package cardtmpl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/jsontmpl"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	JSONTemplateEngineV1     = "octo-json-template/v1"
	CatalogVisibilityPublic  = "public"
	CatalogVisibilityPrivate = "private"

	maxArtifactBundleBytes   = 2 << 20
	maxArtifactDocuments     = 128
	maxArtifactDocumentBytes = 512 << 10
	maxArtifactViews         = 16
	maxArtifactStates        = 64
	maxArtifactSamples       = 64
	maxArtifactGoldens       = 64
	maxArtifactJSONDepth     = 64
	maxArtifactJSONNodes     = 50_000
	artifactSchemaVersion    = 2
	maxArtifactTemplateIDLen = 128
	maxArtifactVersionLen    = 64
	maxArtifactContractLen   = 64
)

var (
	ErrArtifactCompileBusy = errors.New("cardtmpl: artifact compiler busy")
	artifactDocumentKeyRE  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	artifactTemplateIDRE   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9][a-z0-9-]*)+$`)
	artifactVersionRE      = regexp.MustCompile(
		`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
			`(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
			`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`,
	)
	artifactActionTypeRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

	artifactCompileQueue = make(chan struct{}, 16)
	artifactCompileSlots = make(chan struct{}, max(2, runtime.GOMAXPROCS(0)/2))
)

// CatalogDescriptor contains governance metadata outside the frozen template
// manifest. It is part of the canonical artifact identity.
type CatalogDescriptor struct {
	Engine     string `json:"engine"`
	Visibility string `json:"visibility"`
}

// Bundle is the structured, path-free upload contract for JSON templates.
// Map keys are bounded logical document names, not caller-controlled paths.
type Bundle struct {
	Catalog   CatalogDescriptor          `json:"catalog"`
	Manifest  json.RawMessage            `json:"manifest"`
	Schema    json.RawMessage            `json:"schema"`
	Templates map[string]json.RawMessage `json:"templates"`
	Reports   map[string]json.RawMessage `json:"reports,omitempty"`
	Samples   map[string]json.RawMessage `json:"samples"`
	Goldens   map[string]json.RawMessage `json:"goldens,omitempty"`
}

// CanonicalBundle is deterministic JSON used for hashing and immutable
// persistence. Callers must treat the bytes as read-only.
type CanonicalBundle []byte

// CompiledArtifact is the result shared by static registration and the runtime
// validate/publish control plane. It contains no database or activation state.
type CompiledArtifact struct {
	Template        Template
	Meta            TemplateMeta
	Hash            string
	Bundle          CanonicalBundle
	Engine          string
	Visibility      string
	Owner           string
	ContractVersion string

	samples map[string]json.RawMessage
}

// ArtifactValidationError exposes only a bounded category and logical
// document name to HTTP callers. Cause is retained for server logs.
type ArtifactValidationError struct {
	Category string
	Document string
	cause    error
}

func (e *ArtifactValidationError) Error() string {
	if e == nil {
		return "cardtmpl: invalid artifact"
	}
	if e.cause == nil {
		return fmt.Sprintf("cardtmpl: invalid artifact %s (%s)", e.Document, e.Category)
	}
	return fmt.Sprintf("cardtmpl: invalid artifact %s (%s): %v", e.Document, e.Category, e.cause)
}

func (e *ArtifactValidationError) Unwrap() error { return e.cause }

func artifactValidationError(category, document string, err error) error {
	return &ArtifactValidationError{Category: category, Document: document, cause: err}
}

// CompileLimits is explicit so tests and future engine contracts can lower
// budgets. Production callers use DefaultCompileLimits; values above the hard
// defaults are clamped by normalizeCompileLimits.
type CompileLimits struct {
	MaxBundleBytes   int
	MaxDocuments     int
	MaxDocumentBytes int
	MaxViews         int
	MaxStates        int
	MaxSamples       int
	MaxGoldens       int
	MaxJSONDepth     int
	MaxJSONNodes     int

	RequireBoundedSchema bool
	RequireOwner         bool
	RequireProtocol      bool
	RequireStateSamples  bool
}

func DefaultCompileLimits() CompileLimits {
	return CompileLimits{
		MaxBundleBytes:       maxArtifactBundleBytes,
		MaxDocuments:         maxArtifactDocuments,
		MaxDocumentBytes:     maxArtifactDocumentBytes,
		MaxViews:             maxArtifactViews,
		MaxStates:            maxArtifactStates,
		MaxSamples:           maxArtifactSamples,
		MaxGoldens:           maxArtifactGoldens,
		MaxJSONDepth:         maxArtifactJSONDepth,
		MaxJSONNodes:         maxArtifactJSONNodes,
		RequireBoundedSchema: true,
		RequireOwner:         true,
		RequireProtocol:      true,
		RequireStateSamples:  true,
	}
}

func staticCompileLimits() CompileLimits {
	limits := DefaultCompileLimits()
	// Frozen legacy built-ins predate the E3 publisher bounds. They still use
	// the same parser/compiler; only the new-publish governance checks are off.
	limits.RequireBoundedSchema = false
	limits.RequireOwner = false
	limits.RequireProtocol = false
	return limits
}

func normalizeCompileLimits(limits CompileLimits) CompileLimits {
	defaults := DefaultCompileLimits()
	limits.MaxBundleBytes = boundedLimit(limits.MaxBundleBytes, defaults.MaxBundleBytes)
	limits.MaxDocuments = boundedLimit(limits.MaxDocuments, defaults.MaxDocuments)
	limits.MaxDocumentBytes = boundedLimit(limits.MaxDocumentBytes, defaults.MaxDocumentBytes)
	limits.MaxViews = boundedLimit(limits.MaxViews, defaults.MaxViews)
	limits.MaxStates = boundedLimit(limits.MaxStates, defaults.MaxStates)
	limits.MaxSamples = boundedLimit(limits.MaxSamples, defaults.MaxSamples)
	limits.MaxGoldens = boundedLimit(limits.MaxGoldens, defaults.MaxGoldens)
	limits.MaxJSONDepth = boundedLimit(limits.MaxJSONDepth, defaults.MaxJSONDepth)
	limits.MaxJSONNodes = boundedLimit(limits.MaxJSONNodes, defaults.MaxJSONNodes)
	return limits
}

func boundedLimit(value, hardMax int) int {
	if value <= 0 || value > hardMax {
		return hardMax
	}
	return value
}

// DecodeBundleJSON rejects duplicate keys, trailing tokens, invalid UTF-8 and
// out-of-range numbers before converting the request to the typed Bundle.
func DecodeBundleJSON(raw []byte) (Bundle, error) {
	limits := DefaultCompileLimits()
	if len(raw) > limits.MaxBundleBytes {
		return Bundle{}, artifactValidationError("limit", "bundle", fmt.Errorf("body exceeds %d bytes", limits.MaxBundleBytes))
	}
	parsed, err := decodeStrictJSON(raw, jsonBudget{maxDepth: limits.MaxJSONDepth, maxNodes: limits.MaxJSONNodes})
	if err != nil {
		return Bundle{}, artifactValidationError("json", "bundle", err)
	}
	root, ok := parsed.(map[string]any)
	if !ok {
		return Bundle{}, artifactValidationError("shape", "bundle", errors.New("root must be an object"))
	}
	if err := rejectUnknownKeys(root, "catalog", "manifest", "schema", "templates", "reports", "samples", "goldens"); err != nil {
		return Bundle{}, artifactValidationError("shape", "bundle", err)
	}
	if catalog, ok := root["catalog"].(map[string]any); ok {
		if err := rejectUnknownKeys(catalog, "engine", "visibility"); err != nil {
			return Bundle{}, artifactValidationError("shape", "catalog", err)
		}
	}
	canonical, err := marshalCanonicalJSON(parsed)
	if err != nil {
		return Bundle{}, artifactValidationError("canonical", "bundle", err)
	}
	var bundle Bundle
	if err := json.Unmarshal(canonical, &bundle); err != nil {
		return Bundle{}, artifactValidationError("shape", "bundle", err)
	}
	return bundle, nil
}

// DecodeStrictJSON applies the artifact request JSON rules to an enclosing
// control-plane object before decoding it into out. Unknown struct fields are
// rejected in addition to duplicate keys and trailing tokens.
func DecodeStrictJSON(raw []byte, out any) error {
	if out == nil {
		return artifactValidationError("shape", "request", errors.New("decode target is nil"))
	}
	if len(raw) > maxArtifactBundleBytes {
		return artifactValidationError("limit", "request", fmt.Errorf("body exceeds %d bytes", maxArtifactBundleBytes))
	}
	parsed, err := decodeStrictJSON(raw, jsonBudget{maxDepth: maxArtifactJSONDepth, maxNodes: maxArtifactJSONNodes})
	if err != nil {
		return artifactValidationError("json", "request", err)
	}
	canonical, err := marshalCanonicalJSON(parsed)
	if err != nil {
		return artifactValidationError("canonical", "request", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return artifactValidationError("shape", "request", err)
	}
	return nil
}

// LoadJSONBundle converts a reviewed embedded handoff into the same structured
// bundle accepted by the runtime compiler. The schema is read exactly once.
func LoadJSONBundle(assets fs.FS, root string, catalog CatalogDescriptor) (Bundle, error) {
	manifestPath := path.Join(root, "manifest.json")
	manifestRaw, err := fs.ReadFile(assets, manifestPath)
	if err != nil {
		return Bundle{}, artifactValidationError("read", "manifest", err)
	}
	parsed, err := decodeStrictJSON(manifestRaw, jsonBudget{maxDepth: maxArtifactJSONDepth, maxNodes: maxArtifactJSONNodes})
	if err != nil {
		return Bundle{}, artifactValidationError("json", "manifest", err)
	}
	mf, err := decodeManifest(parsed)
	if err != nil {
		return Bundle{}, artifactValidationError("manifest", "manifest", err)
	}

	schemaPath := strings.TrimSpace(mf.DataSchema)
	if schemaPath == "" {
		schemaPath = "contract/data.schema.json"
	}
	schemaRaw, err := readBundleAsset(assets, root, schemaPath)
	if err != nil {
		return Bundle{}, artifactValidationError("read", "schema", err)
	}

	bundle := Bundle{
		Catalog:   catalog,
		Manifest:  append(json.RawMessage(nil), manifestRaw...),
		Schema:    schemaRaw,
		Templates: make(map[string]json.RawMessage, len(mf.Views)),
		Reports:   make(map[string]json.RawMessage),
		Samples:   make(map[string]json.RawMessage),
		Goldens:   make(map[string]json.RawMessage),
	}
	for view, spec := range mf.Views {
		if strings.TrimSpace(spec.Template) == "" {
			return Bundle{}, artifactValidationError("manifest", "templates."+string(view), fmt.Errorf("view %q declares no template path", view))
		}
		templateRaw, readErr := readBundleAsset(assets, root, spec.Template)
		if readErr != nil {
			return Bundle{}, artifactValidationError("read", "templates."+string(view), readErr)
		}
		bundle.Templates[string(view)] = templateRaw
		if spec.WireProfile == profileV2 {
			reportRaw, reportErr := readBundleAsset(assets, root, path.Join("reports", string(view)+".interaction.json"))
			if reportErr != nil {
				return Bundle{}, artifactValidationError("read", "reports."+string(view), reportErr)
			}
			bundle.Reports[string(view)] = reportRaw
		}
		for _, samplePath := range spec.Samples {
			key, keyErr := logicalDocumentKey(samplePath, ".json")
			if keyErr != nil {
				return Bundle{}, artifactValidationError("path", "samples", keyErr)
			}
			sampleRaw, sampleErr := readBundleAsset(assets, root, samplePath)
			if sampleErr != nil {
				return Bundle{}, artifactValidationError("read", "samples."+key, sampleErr)
			}
			if _, duplicate := bundle.Samples[key]; duplicate {
				return Bundle{}, artifactValidationError("manifest", "samples."+key, errors.New("duplicate sample key"))
			}
			bundle.Samples[key] = sampleRaw
		}
	}

	goldenDir := path.Join(root, "goldens")
	entries, err := fs.ReadDir(assets, goldenDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".card.json") {
				continue
			}
			key := strings.TrimSuffix(entry.Name(), ".card.json")
			if !artifactDocumentKeyRE.MatchString(key) {
				return Bundle{}, artifactValidationError("path", "goldens", fmt.Errorf("invalid key %q", key))
			}
			raw, readErr := fs.ReadFile(assets, path.Join(goldenDir, entry.Name()))
			if readErr != nil {
				return Bundle{}, artifactValidationError("read", "goldens."+key, readErr)
			}
			bundle.Goldens[key] = append(json.RawMessage(nil), raw...)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Bundle{}, artifactValidationError("read", "goldens", err)
	}
	return bundle, nil
}

func readBundleAsset(assets fs.FS, root, relative string) (json.RawMessage, error) {
	clean := path.Clean(strings.TrimSpace(relative))
	if clean == "." || clean == "" || clean != relative || !fs.ValidPath(clean) || strings.HasPrefix(clean, "../") {
		return nil, fmt.Errorf("unsafe bundle path %q", relative)
	}
	raw, err := fs.ReadFile(assets, path.Join(root, clean))
	if err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), raw...), nil
}

func logicalDocumentKey(relative, suffix string) (string, error) {
	clean := path.Clean(strings.TrimSpace(relative))
	if clean != relative || !fs.ValidPath(clean) || !strings.HasSuffix(clean, suffix) {
		return "", fmt.Errorf("unsafe document path %q", relative)
	}
	key := strings.TrimSuffix(path.Base(clean), suffix)
	if !artifactDocumentKeyRE.MatchString(key) {
		return "", fmt.Errorf("invalid document key %q", key)
	}
	return key, nil
}

// CompileJSONArtifact validates and compiles an untrusted JSON bundle without
// panicking or accessing databases, the network, or process configuration.
func CompileJSONArtifact(ctx context.Context, bundle Bundle, limits CompileLimits) (*CompiledArtifact, error) {
	limits = normalizeCompileLimits(limits)
	release, err := acquireArtifactCompiler(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bundle.Catalog.Engine != JSONTemplateEngineV1 {
		return nil, artifactValidationError("engine", "catalog", fmt.Errorf("unsupported engine %q", bundle.Catalog.Engine))
	}
	if bundle.Catalog.Visibility != CatalogVisibilityPublic && bundle.Catalog.Visibility != CatalogVisibilityPrivate {
		return nil, artifactValidationError("visibility", "catalog", fmt.Errorf("unsupported visibility %q", bundle.Catalog.Visibility))
	}
	if err := validateBundleDocumentLimits(bundle, limits); err != nil {
		return nil, err
	}

	budget := jsonBudget{maxDepth: limits.MaxJSONDepth, maxNodes: limits.MaxJSONNodes}
	manifestDoc, err := decodeArtifactDocument("manifest", bundle.Manifest, budget)
	if err != nil {
		return nil, err
	}
	mf, err := decodeManifest(manifestDoc)
	if err != nil {
		return nil, artifactValidationError("manifest", "manifest", err)
	}
	if err := validateRuntimeManifest(mf, limits); err != nil {
		return nil, artifactValidationError("manifest", "manifest", err)
	}

	schemaDoc, err := decodeArtifactDocument("schema", bundle.Schema, budget)
	if err != nil {
		return nil, err
	}
	schemaMap, ok := schemaDoc.(map[string]any)
	if !ok {
		return nil, artifactValidationError("schema", "schema", errors.New("root must be an object"))
	}
	if err := rejectExternalSchemaRefs(schemaDoc); err != nil {
		return nil, artifactValidationError("schema", "schema", err)
	}
	schema, err := compileJSONSchema(schemaDoc)
	if err != nil {
		return nil, artifactValidationError("schema", "schema", err)
	}
	aggregateLimits, err := parseAggregateArrayLimits(schemaMap)
	if err != nil {
		return nil, artifactValidationError("constraint", "schema", err)
	}
	if limits.RequireBoundedSchema {
		if err := validateBoundedInputSchema(schemaMap); err != nil {
			return nil, artifactValidationError("bounds", "schema", err)
		}
	}

	parsedTemplates, err := compileBundleTemplates(ctx, mf, bundle.Templates, budget)
	if err != nil {
		return nil, err
	}
	interactions, parsedReports, err := compileBundleReports(mf, bundle.Reports, budget)
	if err != nil {
		return nil, err
	}
	if limits.RequireOwner {
		if err := validateRuntimeActionContract(mf, interactions); err != nil {
			return nil, artifactValidationError("manifest", "manifest", err)
		}
	}
	parsedSamples, sampleAssignments, err := compileBundleSamples(mf, schema, bundle.Samples, budget, limits)
	if err != nil {
		return nil, err
	}
	parsedGoldens, err := compileBundleGoldens(bundle.Goldens, parsedSamples, budget, limits)
	if err != nil {
		return nil, err
	}

	meta, err := assembleTemplateMeta(mf, schema, interactions, bundle.Manifest)
	if err != nil {
		return nil, artifactValidationError("manifest", "manifest", err)
	}
	template := &jsonTemplate{
		meta:                 meta,
		viewAST:              parsedTemplates,
		aggregateArrayLimits: aggregateLimits,
	}
	if err := selfCheckCompiledJSON(ctx, template, meta, bundle.Samples, sampleAssignments); err != nil {
		return nil, artifactValidationError("conformance", err.document, err.cause)
	}
	if err := checkArtifactGoldens(ctx, parsedTemplates, parsedSamples, parsedGoldens, sampleAssignments); err != nil {
		return nil, err
	}

	canonical, err := canonicalizeBundle(bundle, manifestDoc, schemaDoc, parsedTemplates, parsedReports, parsedSamples, parsedGoldens)
	if err != nil {
		return nil, artifactValidationError("canonical", "bundle", err)
	}
	if len(canonical) > limits.MaxBundleBytes {
		return nil, artifactValidationError("limit", "bundle", fmt.Errorf("canonical bundle exceeds %d bytes", limits.MaxBundleBytes))
	}
	digest := sha256.Sum256(canonical)
	return &CompiledArtifact{
		Template:        template,
		Meta:            meta.Clone(),
		Hash:            hex.EncodeToString(digest[:]),
		Bundle:          append(CanonicalBundle(nil), canonical...),
		Engine:          bundle.Catalog.Engine,
		Visibility:      bundle.Catalog.Visibility,
		Owner:           mf.Owner,
		ContractVersion: mf.ContractVersion,
		samples:         cloneRawMessageMap(bundle.Samples),
	}, nil
}

func acquireArtifactCompiler(ctx context.Context) (func(), error) {
	select {
	case artifactCompileQueue <- struct{}{}:
	default:
		return nil, ErrArtifactCompileBusy
	}
	select {
	case artifactCompileSlots <- struct{}{}:
		return func() {
			<-artifactCompileSlots
			<-artifactCompileQueue
		}, nil
	case <-ctx.Done():
		<-artifactCompileQueue
		return nil, ctx.Err()
	}
}

func validateBundleDocumentLimits(bundle Bundle, limits CompileLimits) error {
	documentCount := 2 + len(bundle.Templates) + len(bundle.Reports) + len(bundle.Samples) + len(bundle.Goldens)
	if documentCount > limits.MaxDocuments {
		return artifactValidationError("limit", "bundle", fmt.Errorf("document count %d exceeds %d", documentCount, limits.MaxDocuments))
	}
	if len(bundle.Templates) > limits.MaxViews {
		return artifactValidationError("limit", "templates", fmt.Errorf("view count %d exceeds %d", len(bundle.Templates), limits.MaxViews))
	}
	if len(bundle.Samples) > limits.MaxSamples {
		return artifactValidationError("limit", "samples", fmt.Errorf("sample count %d exceeds %d", len(bundle.Samples), limits.MaxSamples))
	}
	if len(bundle.Goldens) > limits.MaxGoldens {
		return artifactValidationError("limit", "goldens", fmt.Errorf("golden count %d exceeds %d", len(bundle.Goldens), limits.MaxGoldens))
	}
	documents := []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "manifest", raw: bundle.Manifest},
		{name: "schema", raw: bundle.Schema},
	}
	for prefix, docs := range map[string]map[string]json.RawMessage{
		"templates": bundle.Templates,
		"reports":   bundle.Reports,
		"samples":   bundle.Samples,
		"goldens":   bundle.Goldens,
	} {
		for key, raw := range docs {
			if !artifactDocumentKeyRE.MatchString(key) {
				return artifactValidationError("key", prefix, fmt.Errorf("invalid document key %q", key))
			}
			documents = append(documents, struct {
				name string
				raw  json.RawMessage
			}{name: prefix + "." + key, raw: raw})
		}
	}
	total := 0
	for _, document := range documents {
		if len(document.raw) == 0 {
			return artifactValidationError("json", document.name, errors.New("document is empty"))
		}
		if len(document.raw) > limits.MaxDocumentBytes {
			return artifactValidationError("limit", document.name, fmt.Errorf("document exceeds %d bytes", limits.MaxDocumentBytes))
		}
		total += len(document.raw)
	}
	if total > limits.MaxBundleBytes {
		return artifactValidationError("limit", "bundle", fmt.Errorf("document bytes %d exceed %d", total, limits.MaxBundleBytes))
	}
	return nil
}

func decodeArtifactDocument(name string, raw json.RawMessage, budget jsonBudget) (any, error) {
	parsed, err := decodeStrictJSON(raw, budget)
	if err != nil {
		return nil, artifactValidationError("json", name, err)
	}
	return parsed, nil
}

func decodeManifest(parsed any) (manifestFile, error) {
	root, ok := parsed.(map[string]any)
	if !ok {
		return manifestFile{}, errors.New("root must be an object")
	}
	if err := rejectUnknownKeys(root,
		"schemaVersion", "id", "name", "version", "contractVersion", "protocol",
		"adaptiveCardVersion", "renderProfile", "renderProfileCompatibility", "defaultLocale",
		"owner", "actionType", "dataSchema", "views", "sourceLabel", "sourceIconUrl",
	); err != nil {
		return manifestFile{}, err
	}
	views, ok := root["views"].(map[string]any)
	if !ok {
		return manifestFile{}, errors.New("views must be an object")
	}
	for view, raw := range views {
		viewMap, ok := raw.(map[string]any)
		if !ok {
			return manifestFile{}, fmt.Errorf("view %q must be an object", view)
		}
		if err := rejectUnknownKeys(viewMap, "wireProfile", "states", "template", "samples"); err != nil {
			return manifestFile{}, fmt.Errorf("view %q: %w", view, err)
		}
	}
	canonical, err := marshalCanonicalJSON(parsed)
	if err != nil {
		return manifestFile{}, err
	}
	var mf manifestFile
	if err := json.Unmarshal(canonical, &mf); err != nil {
		return manifestFile{}, err
	}
	if strings.TrimSpace(string(mf.ID)) == "" || strings.TrimSpace(mf.Version) == "" {
		return manifestFile{}, errors.New("missing id/version")
	}
	if len(mf.Views) == 0 {
		return manifestFile{}, errors.New("no views")
	}
	return mf, nil
}

func validateRuntimeManifest(mf manifestFile, limits CompileLimits) error {
	if limits.RequireProtocol && mf.SchemaVersion != artifactSchemaVersion {
		return fmt.Errorf("schemaVersion %d is unsupported; want %d", mf.SchemaVersion, artifactSchemaVersion)
	}
	if len(mf.ID) > maxArtifactTemplateIDLen || !artifactTemplateIDRE.MatchString(string(mf.ID)) {
		return fmt.Errorf("id %q is not canonical", mf.ID)
	}
	if len(mf.Version) > maxArtifactVersionLen || !artifactVersionRE.MatchString(mf.Version) {
		return fmt.Errorf("version %q is not canonical semver", mf.Version)
	}
	if mf.ContractVersion != "" && (len(mf.ContractVersion) > maxArtifactContractLen || !artifactVersionRE.MatchString(mf.ContractVersion)) {
		return fmt.Errorf("contractVersion %q is not canonical semver", mf.ContractVersion)
	}
	if limits.RequireOwner && strings.TrimSpace(mf.Owner) == "" {
		return errors.New("owner is required")
	}
	if limits.RequireProtocol && strings.TrimSpace(mf.Protocol) == "" {
		return errors.New("protocol is required")
	}
	if mf.DataSchema != "" && mf.DataSchema != "contract/data.schema.json" {
		return fmt.Errorf("dataSchema %q must reference contract/data.schema.json", mf.DataSchema)
	}
	if limits.RequireProtocol && mf.DataSchema == "" {
		return errors.New("dataSchema must reference contract/data.schema.json")
	}
	if mf.AdaptiveCardVersion != "" && mf.AdaptiveCardVersion != cardmsg.CardVersion {
		return fmt.Errorf("adaptiveCardVersion %q must be %q", mf.AdaptiveCardVersion, cardmsg.CardVersion)
	}
	if limits.RequireProtocol && mf.AdaptiveCardVersion == "" {
		return errors.New("adaptiveCardVersion is required")
	}
	if len(mf.Views) > limits.MaxViews {
		return fmt.Errorf("views exceed %d", limits.MaxViews)
	}
	stateCount := 0
	for view, spec := range mf.Views {
		if !artifactDocumentKeyRE.MatchString(string(view)) {
			return fmt.Errorf("view %q is not a safe token", view)
		}
		if len(spec.States) == 0 {
			return fmt.Errorf("view %q has no states", view)
		}
		stateCount += len(spec.States)
		if stateCount > limits.MaxStates {
			return fmt.Errorf("states exceed %d", limits.MaxStates)
		}
		for _, state := range spec.States {
			if !artifactDocumentKeyRE.MatchString(string(state)) {
				return fmt.Errorf("state %q is not a safe token", state)
			}
		}
	}
	return nil
}

func validateRuntimeActionContract(mf manifestFile, interactions map[ViewKey]InteractionReport) error {
	hasSubmit := false
	for _, report := range interactions {
		for _, action := range report.Actions {
			if action.Type == "Action.Submit" {
				hasSubmit = true
				break
			}
		}
	}
	if !hasSubmit {
		return nil
	}
	if !artifactActionTypeRE.MatchString(mf.ActionType) {
		return errors.New("actionType is required and must be canonical for an interactive template")
	}
	return nil
}

func compileBundleTemplates(ctx context.Context, mf manifestFile, documents map[string]json.RawMessage, budget jsonBudget) (map[ViewKey]any, error) {
	if err := requireExactDocumentKeys("templates", documents, viewKeys(mf.Views)); err != nil {
		return nil, err
	}
	out := make(map[ViewKey]any, len(mf.Views))
	for view, spec := range mf.Views {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		expectedPath := path.Join("templates", string(view)+".template.json")
		if strings.TrimSpace(spec.Template) != expectedPath {
			return nil, artifactValidationError("manifest", "templates."+string(view), fmt.Errorf("template path %q, want %q", spec.Template, expectedPath))
		}
		parsed, err := decodeArtifactDocument("templates."+string(view), documents[string(view)], budget)
		if err != nil {
			return nil, err
		}
		if _, ok := parsed.(map[string]any); !ok {
			return nil, artifactValidationError("shape", "templates."+string(view), errors.New("template root must be an object"))
		}
		if err := jsontmpl.ValidateToggleTargets(parsed); err != nil {
			return nil, artifactValidationError("template", "templates."+string(view), err)
		}
		out[view] = parsed
	}
	return out, nil
}

func compileBundleReports(mf manifestFile, documents map[string]json.RawMessage, budget jsonBudget) (map[ViewKey]InteractionReport, map[string]any, error) {
	expected := make(map[string]struct{})
	for view, spec := range mf.Views {
		if spec.WireProfile == profileV2 {
			expected[string(view)] = struct{}{}
		}
	}
	if err := requireExactDocumentKeys("reports", documents, expected); err != nil {
		return nil, nil, err
	}
	interactions := make(map[ViewKey]InteractionReport, len(expected))
	parsedReports := make(map[string]any, len(expected))
	for key := range expected {
		parsed, err := decodeArtifactDocument("reports."+key, documents[key], budget)
		if err != nil {
			return nil, nil, err
		}
		canonical, err := marshalCanonicalJSON(parsed)
		if err != nil {
			return nil, nil, artifactValidationError("canonical", "reports."+key, err)
		}
		var report InteractionReport
		decoder := json.NewDecoder(bytes.NewReader(canonical))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&report); err != nil {
			return nil, nil, artifactValidationError("report", "reports."+key, err)
		}
		interactions[ViewKey(key)] = report
		parsedReports[key] = parsed
	}
	return interactions, parsedReports, nil
}

type sampleAssignment struct {
	view  ViewKey
	state State
	key   string
}

func compileBundleSamples(
	mf manifestFile,
	schema *jsonschema.Schema,
	documents map[string]json.RawMessage,
	budget jsonBudget,
	limits CompileLimits,
) (map[string]any, []sampleAssignment, error) {
	expected := make(map[string]struct{})
	assignments := make([]sampleAssignment, 0)
	for view, spec := range mf.Views {
		keys := make([]string, 0, len(spec.Samples))
		for _, samplePath := range spec.Samples {
			key, err := logicalDocumentKey(samplePath, ".json")
			if err != nil {
				return nil, nil, artifactValidationError("manifest", "samples", err)
			}
			expectedPath := path.Join("samples", key+".json")
			if samplePath != expectedPath {
				return nil, nil, artifactValidationError("manifest", "samples."+key,
					fmt.Errorf("sample path %q, want %q", samplePath, expectedPath))
			}
			if _, duplicate := expected[key]; duplicate {
				return nil, nil, artifactValidationError("manifest", "samples."+key, errors.New("sample is referenced more than once"))
			}
			expected[key] = struct{}{}
			keys = append(keys, key)
		}
		if limits.RequireStateSamples && len(keys) != len(spec.States) {
			return nil, nil, artifactValidationError("manifest", "samples", fmt.Errorf("view %q has %d states but %d samples; each sample must have one state assignment", view, len(spec.States), len(keys)))
		}
		used := make(map[string]struct{}, len(spec.States))
		for index, state := range spec.States {
			key := string(state)
			if _, exists := expected[key]; !exists {
				if index >= len(keys) {
					continue
				}
				key = keys[index]
			}
			if _, duplicate := used[key]; duplicate {
				return nil, nil, artifactValidationError("manifest", "samples."+key, errors.New("sample cannot cover multiple states in one view"))
			}
			used[key] = struct{}{}
			assignments = append(assignments, sampleAssignment{view: view, state: state, key: key})
		}
	}
	if err := requireExactDocumentKeys("samples", documents, expected); err != nil {
		return nil, nil, err
	}
	parsedSamples := make(map[string]any, len(documents))
	for key, raw := range documents {
		parsed, err := decodeArtifactDocument("samples."+key, raw, budget)
		if err != nil {
			return nil, nil, err
		}
		if err := schema.Validate(parsed); err != nil {
			return nil, nil, artifactValidationError("schema", "samples."+key, err)
		}
		parsedSamples[key] = parsed
	}
	return parsedSamples, assignments, nil
}

func compileBundleGoldens(
	documents map[string]json.RawMessage,
	parsedSamples map[string]any,
	budget jsonBudget,
	limits CompileLimits,
) (map[string]any, error) {
	if len(documents) > limits.MaxGoldens {
		return nil, artifactValidationError("limit", "goldens", fmt.Errorf("goldens exceed %d", limits.MaxGoldens))
	}
	out := make(map[string]any, len(documents))
	for key, raw := range documents {
		if _, ok := parsedSamples[key]; !ok {
			return nil, artifactValidationError("unreferenced", "goldens."+key, errors.New("golden has no matching sample"))
		}
		parsed, err := decodeArtifactDocument("goldens."+key, raw, budget)
		if err != nil {
			return nil, err
		}
		out[key] = parsed
	}
	return out, nil
}

func requireExactDocumentKeys(prefix string, documents map[string]json.RawMessage, expected map[string]struct{}) error {
	for key := range expected {
		if _, ok := documents[key]; !ok {
			return artifactValidationError("missing", prefix+"."+key, errors.New("required document missing"))
		}
	}
	for key := range documents {
		if _, ok := expected[key]; !ok {
			return artifactValidationError("unreferenced", prefix+"."+key, errors.New("document is not referenced by manifest"))
		}
	}
	return nil
}

func viewKeys(views map[ViewKey]manifestView) map[string]struct{} {
	out := make(map[string]struct{}, len(views))
	for view := range views {
		out[string(view)] = struct{}{}
	}
	return out
}

type compiledSelfCheckError struct {
	document string
	cause    error
}

func selfCheckCompiledJSON(
	ctx context.Context,
	template *jsonTemplate,
	meta TemplateMeta,
	samples map[string]json.RawMessage,
	assignments []sampleAssignment,
) *compiledSelfCheckError {
	for _, assignment := range assignments {
		if err := ctx.Err(); err != nil {
			return &compiledSelfCheckError{document: "samples." + assignment.key, cause: err}
		}
		raw := samples[assignment.key]
		payload, err := renderCore(ctx, template, meta, assignment.state, raw, BuildEnv{
			WebLoginURL: "https://selfcheck.internal",
			Lang:        "en-US",
			SpaceID:     "__cardtmpl_compile__",
		})
		if err != nil {
			return &compiledSelfCheckError{document: "samples." + assignment.key, cause: err}
		}
		viewSpec := meta.Views[assignment.view]
		if viewSpec.WireProfile != profileV2 {
			continue
		}
		if meta.ActionContract != nil {
			if err := assertActionContract(payload, *meta.ActionContract); err != nil {
				return &compiledSelfCheckError{document: "samples." + assignment.key, cause: err}
			}
		}
		report, ok := meta.Interaction(assignment.view)
		if !ok {
			return &compiledSelfCheckError{document: "reports." + string(assignment.view), cause: errors.New("interaction report missing")}
		}
		if err := assertInteractionReport(payload, report); err != nil {
			return &compiledSelfCheckError{document: "reports." + string(assignment.view), cause: err}
		}
	}
	return nil
}

func checkArtifactGoldens(
	ctx context.Context,
	templates map[ViewKey]any,
	samples map[string]any,
	goldens map[string]any,
	assignments []sampleAssignment,
) error {
	assignmentByKey := make(map[string]sampleAssignment, len(assignments))
	for _, assignment := range assignments {
		assignmentByKey[assignment.key] = assignment
	}
	for key, golden := range goldens {
		if err := ctx.Err(); err != nil {
			return err
		}
		assignment, ok := assignmentByKey[key]
		if !ok {
			return artifactValidationError("golden", "goldens."+key, errors.New("no state assignment"))
		}
		data, ok := samples[key].(map[string]any)
		if !ok {
			return artifactValidationError("shape", "samples."+key, errors.New("sample root must be an object"))
		}
		expanded, err := jsontmpl.Expand(ctx, templates[assignment.view], jsontmpl.Scope{Data: data}, jsonTemplateEscaper)
		if err != nil {
			return artifactValidationError("template", "templates."+string(assignment.view), err)
		}
		got, err := marshalCanonicalJSON(expanded)
		if err != nil {
			return artifactValidationError("canonical", "templates."+string(assignment.view), err)
		}
		want, err := marshalCanonicalJSON(golden)
		if err != nil {
			return artifactValidationError("canonical", "goldens."+key, err)
		}
		if !bytes.Equal(got, want) {
			return artifactValidationError("golden", "goldens."+key, errors.New("expanded template does not match golden"))
		}
	}
	return nil
}

func canonicalizeBundle(
	bundle Bundle,
	manifest any,
	schema any,
	templates map[ViewKey]any,
	reports map[string]any,
	samples map[string]any,
	goldens map[string]any,
) ([]byte, error) {
	templateDocuments := make(map[string]any, len(templates))
	for key, document := range templates {
		templateDocuments[string(key)] = document
	}
	root := map[string]any{
		"catalog": map[string]any{
			"engine":     bundle.Catalog.Engine,
			"visibility": bundle.Catalog.Visibility,
		},
		"manifest":  manifest,
		"schema":    schema,
		"templates": templateDocuments,
		"reports":   reports,
		"samples":   samples,
		"goldens":   goldens,
	}
	return marshalCanonicalJSON(root)
}

func cloneRawMessageMap(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for key, value := range in {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func compileJSONSchema(document any) (*jsonschema.Schema, error) {
	const schemaURL = "https://octo.internal/card-template/data.schema.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return nil, fmt.Errorf("add schema resource: %w", err)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return schema, nil
}

func parseAggregateArrayLimits(schema map[string]any) ([]jsonTemplateAggregateArrayLimit, error) {
	raw, exists := schema["x-octo-constraints"]
	if !exists {
		return nil, nil
	}
	constraints, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("x-octo-constraints must be an object")
	}
	if err := rejectUnknownKeys(constraints, "aggregateArrayLimits"); err != nil {
		return nil, err
	}
	rawLimits, ok := constraints["aggregateArrayLimits"].([]any)
	if !ok || len(rawLimits) == 0 {
		return nil, errors.New("x-octo-constraints.aggregateArrayLimits must be a non-empty array")
	}
	limits := make([]jsonTemplateAggregateArrayLimit, 0, len(rawLimits))
	seen := make(map[string]struct{}, len(rawLimits))
	for index, rawLimit := range rawLimits {
		entry, ok := rawLimit.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("aggregateArrayLimits[%d] must be an object", index)
		}
		if err := rejectUnknownKeys(entry, "parentArray", "childArray", "maxTotalItems"); err != nil {
			return nil, fmt.Errorf("aggregateArrayLimits[%d]: %w", index, err)
		}
		parent, _ := entry["parentArray"].(string)
		child, _ := entry["childArray"].(string)
		if parent == "" || parent != strings.TrimSpace(parent) {
			return nil, fmt.Errorf("aggregateArrayLimits[%d].parentArray is required and must be trimmed", index)
		}
		if child == "" || child != strings.TrimSpace(child) {
			return nil, fmt.Errorf("aggregateArrayLimits[%d].childArray is required and must be trimmed", index)
		}
		maxItems, err := positiveJSONInt(entry["maxTotalItems"])
		if err != nil {
			return nil, fmt.Errorf("aggregateArrayLimits[%d].maxTotalItems must be positive", index)
		}
		key := parent + "\x00" + child
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("duplicate aggregateArrayLimits target %s[].%s", parent, child)
		}
		seen[key] = struct{}{}
		limit := jsonTemplateAggregateArrayLimit{ParentArray: parent, ChildArray: child, MaxTotalItems: maxItems}
		if err := validateAggregateTarget(schema, limit); err != nil {
			return nil, err
		}
		limits = append(limits, limit)
	}
	return limits, nil
}

func positiveJSONInt(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("not a number")
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 32)
	if err != nil || parsed <= 0 {
		return 0, errors.New("not a positive integer")
	}
	return int(parsed), nil
}

func validateAggregateTarget(schema map[string]any, limit jsonTemplateAggregateArrayLimit) error {
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return errors.New("schema properties must be an object")
	}
	parent, exists := properties[limit.ParentArray]
	if !exists {
		return fmt.Errorf("aggregate parentArray %q not found", limit.ParentArray)
	}
	parentSchema, ok := parent.(map[string]any)
	if !ok || parentSchema["type"] != "array" {
		return fmt.Errorf("aggregate parentArray %q must be an array of objects", limit.ParentArray)
	}
	items, ok := parentSchema["items"].(map[string]any)
	if !ok || items["type"] != "object" {
		return fmt.Errorf("aggregate parentArray %q must be an array of objects", limit.ParentArray)
	}
	itemProperties, ok := items["properties"].(map[string]any)
	if !ok {
		return fmt.Errorf("aggregate parentArray %q item properties missing", limit.ParentArray)
	}
	child, exists := itemProperties[limit.ChildArray]
	if !exists {
		return fmt.Errorf("aggregate childArray %q not found under %q", limit.ChildArray, limit.ParentArray)
	}
	childSchema, ok := child.(map[string]any)
	if !ok || childSchema["type"] != "array" {
		return fmt.Errorf("aggregate childArray %q under %q must be an array", limit.ChildArray, limit.ParentArray)
	}
	return nil
}

func rejectExternalSchemaRefs(value any) error {
	switch node := value.(type) {
	case []any:
		for _, child := range node {
			if err := rejectExternalSchemaRefs(child); err != nil {
				return err
			}
		}
	case map[string]any:
		if raw, exists := node["$ref"]; exists {
			ref, ok := raw.(string)
			if !ok || !strings.HasPrefix(ref, "#") {
				return fmt.Errorf("external $ref %q is forbidden", ref)
			}
		}
		for _, child := range node {
			if err := rejectExternalSchemaRefs(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBoundedInputSchema(root map[string]any) error {
	if root["type"] != "object" {
		return errors.New("root schema type must be object")
	}
	if additional, ok := root["additionalProperties"].(bool); !ok || additional {
		return errors.New("root schema additionalProperties must be false")
	}
	return validateBoundedSchemaNode(root, "$")
}

func validateBoundedSchemaNode(value any, location string) error {
	switch node := value.(type) {
	case []any:
		for index, child := range node {
			if err := validateBoundedSchemaNode(child, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		kinds, err := schemaTypeNames(node["type"], location)
		if err != nil {
			return err
		}
		for _, kind := range kinds {
			switch kind {
			case "string":
				_, hasConst := node["const"]
				enum, hasEnum := node["enum"].([]any)
				if _, err := positiveJSONInt(node["maxLength"]); err != nil && !hasConst && (!hasEnum || len(enum) == 0) {
					return fmt.Errorf("%s string is unbounded", location)
				}
			case "array":
				if _, err := positiveJSONInt(node["maxItems"]); err != nil {
					return fmt.Errorf("%s array is unbounded", location)
				}
				if _, ok := node["items"].(map[string]any); !ok {
					return fmt.Errorf("%s array items schema missing", location)
				}
			case "object":
				if additional, ok := node["additionalProperties"].(bool); !ok || additional {
					return fmt.Errorf("%s object additionalProperties must be false", location)
				}
			}
		}
		if properties, ok := node["properties"].(map[string]any); ok {
			for name, property := range properties {
				if err := validateBoundedPropertySchema(property, location+".properties."+name); err != nil {
					return err
				}
			}
		}
		for key, child := range node {
			if key == "examples" || key == "default" || key == "enum" || key == "const" {
				continue
			}
			if err := validateBoundedSchemaNode(child, location+"."+key); err != nil {
				return err
			}
		}
	}
	return nil
}

func schemaTypeNames(raw any, location string) ([]string, error) {
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{value}, nil
	case []any:
		if len(value) == 0 {
			return nil, fmt.Errorf("%s type list is empty", location)
		}
		out := make([]string, 0, len(value))
		seen := make(map[string]struct{}, len(value))
		for _, item := range value {
			kind, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s type list contains a non-string value", location)
			}
			if _, duplicate := seen[kind]; duplicate {
				return nil, fmt.Errorf("%s type list contains duplicate %q", location, kind)
			}
			seen[kind] = struct{}{}
			out = append(out, kind)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s type must be a string or string array", location)
	}
}

func validateBoundedPropertySchema(value any, location string) error {
	if allowed, ok := value.(bool); ok {
		if !allowed {
			return nil
		}
		return fmt.Errorf("%s must declare a bounded type", location)
	}
	node, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be a schema object", location)
	}
	if _, hasType := node["type"]; hasType {
		return nil
	}
	if _, hasConst := node["const"]; hasConst {
		return nil
	}
	if enum, hasEnum := node["enum"].([]any); hasEnum && len(enum) > 0 {
		return nil
	}
	if ref, hasRef := node["$ref"].(string); hasRef && strings.HasPrefix(ref, "#") {
		return nil
	}
	for _, keyword := range []string{"anyOf", "oneOf"} {
		if alternatives, exists := node[keyword].([]any); exists && len(alternatives) > 0 {
			for index, alternative := range alternatives {
				if err := validateBoundedPropertySchema(alternative, fmt.Sprintf("%s.%s[%d]", location, keyword, index)); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if constraints, exists := node["allOf"].([]any); exists && len(constraints) > 0 {
		for index, constraint := range constraints {
			if validateBoundedPropertySchema(constraint, fmt.Sprintf("%s.allOf[%d]", location, index)) == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("%s must declare a bounded type, enum, const, or local $ref", location)
}

func rejectUnknownKeys(object map[string]any, allowed ...string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range object {
		if _, ok := set[key]; !ok {
			return fmt.Errorf("unknown key %q", key)
		}
	}
	return nil
}

type jsonBudget struct {
	maxDepth int
	maxNodes int
	nodes    int
}

func decodeStrictJSON(raw []byte, budget jsonBudget) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid UTF-8")
	}
	if err := validateJSONUnicodeEscapes(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder, &budget, 1)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("trailing JSON token")
		}
		return nil, fmt.Errorf("trailing JSON token: %w", err)
	}
	return value, nil
}

func validateJSONUnicodeEscapes(raw []byte) error {
	inString := false
	for index := 0; index < len(raw); index++ {
		switch raw[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(raw) {
				continue
			}
			if raw[index+1] != 'u' {
				index++
				continue
			}
			codepoint, err := parseJSONUnicodeEscape(raw, index)
			if err != nil {
				return err
			}
			switch {
			case codepoint >= 0xD800 && codepoint <= 0xDBFF:
				next := index + 6
				if next+5 >= len(raw) || raw[next] != '\\' || raw[next+1] != 'u' {
					return fmt.Errorf("high surrogate \\u%04x is not followed by a low surrogate", codepoint)
				}
				low, err := parseJSONUnicodeEscape(raw, next)
				if err != nil {
					return err
				}
				if low < 0xDC00 || low > 0xDFFF {
					return fmt.Errorf("high surrogate \\u%04x is followed by non-low surrogate \\u%04x", codepoint, low)
				}
				index = next + 5
			case codepoint >= 0xDC00 && codepoint <= 0xDFFF:
				return fmt.Errorf("unpaired low surrogate \\u%04x", codepoint)
			default:
				index += 5
			}
		}
	}
	return nil
}

func parseJSONUnicodeEscape(raw []byte, slashIndex int) (uint64, error) {
	if slashIndex+5 >= len(raw) || raw[slashIndex] != '\\' || raw[slashIndex+1] != 'u' {
		return 0, errors.New("truncated Unicode escape")
	}
	value, err := strconv.ParseUint(string(raw[slashIndex+2:slashIndex+6]), 16, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid Unicode escape: %w", err)
	}
	return value, nil
}

func decodeStrictJSONValue(decoder *json.Decoder, budget *jsonBudget, depth int) (any, error) {
	if depth > budget.maxDepth {
		return nil, fmt.Errorf("JSON depth exceeds %d", budget.maxDepth)
	}
	budget.nodes++
	if budget.nodes > budget.maxNodes {
		return nil, fmt.Errorf("JSON nodes exceed %d", budget.maxNodes)
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				if _, duplicate := object[key]; duplicate {
					return nil, fmt.Errorf("duplicate object key %q", key)
				}
				value, err := decodeStrictJSONValue(decoder, budget, depth+1)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return nil, errors.New("object is not closed")
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for decoder.More() {
				value, err := decodeStrictJSONValue(decoder, budget, depth+1)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return nil, errors.New("array is not closed")
			}
			return array, nil
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", token)
		}
	case json.Number:
		if _, err := canonicalJSONNumber(token); err != nil {
			return nil, err
		}
		return token, nil
	case string, bool, nil:
		return token, nil
	default:
		return nil, fmt.Errorf("unsupported JSON token %T", token)
	}
}

func marshalCanonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := appendCanonicalJSON(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func appendCanonicalJSON(buffer *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if value {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		encoded, err := marshalJSONString(value)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	case json.Number:
		canonical, err := canonicalJSONNumber(value)
		if err != nil {
			return err
		}
		buffer.WriteString(canonical)
	case float64:
		canonical, err := canonicalJSONNumber(json.Number(strconv.FormatFloat(value, 'g', -1, 64)))
		if err != nil {
			return err
		}
		buffer.WriteString(canonical)
	case []any:
		buffer.WriteByte('[')
		for index, child := range value {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := appendCanonicalJSON(buffer, child); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			encoded, err := marshalJSONString(key)
			if err != nil {
				return err
			}
			buffer.Write(encoded)
			buffer.WriteByte(':')
			if err := appendCanonicalJSON(buffer, value[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		parsed, err := decodeStrictJSON(encoded, jsonBudget{maxDepth: maxArtifactJSONDepth, maxNodes: maxArtifactJSONNodes})
		if err != nil {
			return err
		}
		return appendCanonicalJSON(buffer, parsed)
	}
	return nil
}

func marshalJSONString(value string) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func canonicalJSONNumber(number json.Number) (string, error) {
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return "", fmt.Errorf("number %q is outside finite float64 range", number)
	}
	if value == 0 {
		return "0", nil
	}
	if !strings.ContainsAny(number.String(), ".eE") && math.Abs(value) > 1<<53-1 {
		return "", fmt.Errorf("integer %q exceeds exact JSON range", number)
	}
	formatted := strconv.FormatFloat(value, 'g', -1, 64)
	if index := strings.IndexByte(formatted, 'e'); index >= 0 {
		mantissa, exponent := formatted[:index], formatted[index+1:]
		sign := ""
		if strings.HasPrefix(exponent, "+") || strings.HasPrefix(exponent, "-") {
			sign, exponent = exponent[:1], exponent[1:]
		}
		exponent = strings.TrimLeft(exponent, "0")
		if exponent == "" {
			exponent = "0"
		}
		formatted = mantissa + "e" + sign + exponent
	}
	return formatted, nil
}

func utf16Less(left, right string) bool {
	a := utf16.Encode([]rune(left))
	b := utf16.Encode([]rune(right))
	for index := 0; index < len(a) && index < len(b); index++ {
		if a[index] != b[index] {
			return a[index] < b[index]
		}
	}
	return len(a) < len(b)
}
