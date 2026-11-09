package permissionbundle

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const CatalogPackageV2 = "permission-catalog-package-v2"
const CatalogArtifactV1 = "permission-release-manifest-v1"

// PackageInput belongs to trusted private staging, not an HTTP/model request.
// Files includes manifest.json and every listed file, with no other keys.
type PackageInput struct {
	Pin   CatalogPin
	Files map[string][]byte
}

// SlotCell identifies one source-valid joint configuration. The source binding
// must return BOTH slot domains for the same cell, never independent maxima.
// Its values come from source reconciliation in trusted startup configuration;
// the package cannot supply its own oracle or claim its own support status.
type SlotCell struct {
	Pin          CatalogPin
	EntryID      string
	Facade       string
	Resources    bool
	ManagedMask  int
	ServerFacade string
}
type SlotValues func(context.Context, SlotCell) (map[string]any, error)

type PackageLedger struct {
	StoredBytes    int `json:"stored_bytes"`
	MetadataNodes  int `json:"metadata_nodes"`
	PreBytes       int `json:"pre_bytes"`
	PostBytes      int `json:"post_bytes"`
	PreNodes       int `json:"pre_nodes"`
	PostNodes      int `json:"post_nodes"`
	MaxDepth       int `json:"max_depth"`
	Parts          int `json:"parts"`
	Roots          int `json:"roots"`
	SharedRows     int `json:"shared_rows"`
	Content        int `json:"content_records"`
	SchemaCells    int `json:"schema_cells"`
	DiscoveryCells int `json:"discovery_cells"`
	StaticCells    int `json:"static_cells"`
}

// VerifiedCatalog exposes copies only. Semantic bytes and every file remain
// private and immutable after complete validation; loading grants no authority.
type VerifiedCatalog struct {
	pin      CatalogPin
	files    map[string][]byte
	entries  map[string]map[string]any
	content  map[string]map[string]any
	roots    map[string][]byte
	rows     map[string]map[string]any
	profiles map[string]map[string]any
	index    map[string]any
	manifest map[string]any
	semantic []byte
	ledger   PackageLedger
	selected map[string]string
	rendered map[string]string
}

func (v *VerifiedCatalog) Pin() CatalogPin       { return v.pin }
func (v *VerifiedCatalog) Ledger() PackageLedger { return v.ledger }
func (v *VerifiedCatalog) File(path string) ([]byte, bool) {
	b, ok := v.files[path]
	return bytes.Clone(b), ok
}
func (v *VerifiedCatalog) SemanticDigest() string { return hash(v.semantic) }

//go:embed contracts/permission-catalog-package-v2.schema.json
var packageSchemaBytes []byte

// This projection preserves exact identities/targets/aliases from the approved
// source-complete candidate; it is an identity oracle, not a supported catalog.
//
//go:embed contracts/permission-catalog-identities-v1.json
var catalogIdentities []byte

var packageSchemas struct {
	sync.Once
	byKind     map[string]*jsonschema.Schema
	identities map[string]map[string]any
	err        error
}

func loadPackageSchemas() {
	packageSchemas.Do(func() {
		var inherited, doc any
		if packageSchemas.err = json.Unmarshal(releaseSchemaTemplate, &inherited); packageSchemas.err != nil {
			return
		}
		if packageSchemas.err = json.Unmarshal(packageSchemaBytes, &doc); packageSchemas.err != nil {
			return
		}
		c := jsonschema.NewCompiler()
		if packageSchemas.err = c.AddResource(inherited.(map[string]any)["$id"].(string), inherited); packageSchemas.err != nil {
			return
		}
		id := doc.(map[string]any)["$id"].(string)
		if packageSchemas.err = c.AddResource(id, doc); packageSchemas.err != nil {
			return
		}
		packageSchemas.byKind = map[string]*jsonschema.Schema{}
		for k, def := range map[string]string{"manifest": "Manifest", "index": "Index", "module": "Module", "shared": "Shared"} {
			packageSchemas.byKind[k], packageSchemas.err = c.Compile(id + "#/$defs/" + def)
			if packageSchemas.err != nil {
				return
			}
		}
		v, e := decodeStrictDepth(bytes.NewReader(catalogIdentities), 524288, 32)
		if e != nil {
			packageSchemas.err = e
			return
		}
		packageSchemas.identities = map[string]map[string]any{}
		for _, v := range v.([]any) {
			e := v.(map[string]any)
			packageSchemas.identities[e["id"].(string)] = e
		}
	})
}

var packageModules = []string{"_global", "bot", "docs", "drive", "event", "file", "group", "html", "loop", "mail", "marketplace", "message", "thread"}
var serverFacades = []string{"three", "two", "both"}

func packagePath(path string) (kind, module string, shard int, ok bool) {
	if path == "index.json" {
		return "index", "", 0, true
	}
	p := strings.Split(path, "/")
	if len(p) == 2 && p[0] == "blobs" && len(p[1]) == 5 && p[1][1:] == ".bin" && p[1][0] >= '0' && p[1][0] <= '7' {
		return "blob", "", int(p[1][0] - '0'), true
	}
	if len(p) == 2 && p[0] == "shared" && len(p[1]) == 7 && p[1][2:] == ".json" && p[1][0] >= '0' && p[1][0] <= '1' && p[1][1] >= '0' && p[1][1] <= '9' {
		n := int(p[1][0]-'0')*10 + int(p[1][1]-'0')
		return "shared", "", n, n <= 15
	}
	if len(p) == 3 && p[0] == "modules" && len(p[2]) == 6 && p[2][1:] == ".json" && p[2][0] >= '0' && p[2][0] <= '7' {
		for _, m := range packageModules {
			if p[1] == m {
				return "module", m, int(p[2][0] - '0'), true
			}
		}
	}
	return "", "", 0, false
}

func packagingInt(v any, max int) (int, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, ErrUnavailable
	}
	u, e := U64(string(n))
	if e != nil || u > uint64(max) {
		return 0, ErrUnavailable
	}
	return int(u), nil
}

// Inherited integer values retain their original schema domain; the lexical
// decimal-only rule belongs to NEW packaging fields, not inherited values.
func inheritedInt(v any, max int) (int, error) {
	s, ok := v.(json.Number)
	if !ok {
		return 0, ErrUnavailable
	}
	n, e := boundedInteger(string(s), uint64(max))
	return int(n), e
}

// Decide the exact bounded integer value without constructing huge powers of
// ten from an adversarial exponent. Syntax is already checked by decodeBudget.
func boundedInteger(s string, max uint64) (uint64, error) {
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}
	mantissa, exponent := s, ""
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa, exponent = s[:i], s[i+1:]
	}
	fraction := 0
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fraction = len(mantissa) - i - 1
	}
	digits := strings.ReplaceAll(mantissa, ".", "")
	if digits == "" {
		return 0, ErrUnavailable
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, ErrUnavailable
		}
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, nil
	}
	if negative {
		return 0, ErrUnavailable
	}
	sign := 1
	if strings.HasPrefix(exponent, "-") {
		sign = -1
		exponent = exponent[1:]
	} else if strings.HasPrefix(exponent, "+") {
		exponent = exponent[1:]
	}
	shift, capShift := 0, len(mantissa)+32
	for _, c := range exponent {
		if c < '0' || c > '9' {
			return 0, ErrUnavailable
		}
		if shift < capShift {
			shift = shift*10 + int(c-'0')
			if shift > capShift {
				shift = capShift
			}
		}
	}
	scale := sign*shift - fraction
	if scale < 0 {
		need := -scale
		if need > len(digits) || len(digits)-len(strings.TrimRight(digits, "0")) < need {
			return 0, ErrUnavailable
		}
		digits = digits[:len(digits)-need]
		scale = 0
	}
	if len(digits)+scale > len(strconv.FormatUint(max, 10)) {
		return 0, ErrUnavailable
	}
	n, e := strconv.ParseUint(digits, 10, 64)
	if e != nil {
		return 0, ErrUnavailable
	}
	for i := 0; i < scale; i++ {
		if n > max/10 {
			return 0, ErrUnavailable
		}
		n *= 10
	}
	if n > max {
		return 0, ErrUnavailable
	}
	return n, nil
}

// Validate inherited numeric VALUES with bounded decimals in a separate schema
// projection. Keep the original metadata bytes/numbers for commitments, and
// never apply this projection to raw payloads. New packaging lexical checks
// still inspect the original numbers.
func schemaValue(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		n, e := boundedInteger(string(x), 9007199254740991)
		if e != nil {
			return nil, e
		}
		return json.Number(strconv.FormatUint(n, 10)), nil
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, v := range x {
			value, e := schemaValue(v)
			if e != nil {
				return nil, e
			}
			m[k] = value
		}
		return m, nil
	case []any:
		a := make([]any, len(x))
		for i, v := range x {
			value, e := schemaValue(v)
			if e != nil {
				return nil, e
			}
			a[i] = value
		}
		return a, nil
	default:
		return v, nil
	}
}
func sortedObjectKeys(m map[string]map[string]any) []string {
	a := make([]string, 0, len(m))
	for k := range m {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}
func descriptorID(m map[string]any) string {
	if m["kind"] == "bytes" {
		h, _ := m["sha256"].(string)
		return h
	}
	d := map[string]any{}
	for k, v := range m {
		if k != "sha256" {
			d[k] = v
		}
	}
	return hash(canonical(d))
}
func objectWithout(m map[string]any, keys ...string) map[string]any {
	d := map[string]any{}
	for k, v := range m {
		keep := true
		for _, skip := range keys {
			if k == skip {
				keep = false
			}
		}
		if keep {
			d[k] = v
		}
	}
	return d
}
func cellKey(id, facade string, r, m, f int) string {
	return fmt.Sprintf("%s|%s|%d|%d|%d", id, facade, r, m, f)
}

// addWork checks before any expansion/allocation, using the selected limits.
func addWork(total *int, n, limit int) error {
	if n < 0 || *total > limit-n {
		return ErrUnavailable
	}
	*total += n
	return nil
}
