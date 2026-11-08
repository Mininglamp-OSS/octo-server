package permissionbundle

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed contracts/permission-wire-v1.schema.json
var wireSchema []byte

//go:embed contracts/permission-prepare-v1.schema.json
var prepareSchema []byte

type Key struct {
	Origin  string `json:"origin"`
	BotUID  string `json:"bot_uid"`
	SpaceID string `json:"space_id"`
}
type Expected struct {
	Incarnation    string `json:"scope_incarnation"`
	Revision       string `json:"revision"`
	CatalogVersion string `json:"catalog_version"`
	CatalogDigest  string `json:"catalog_digest"`
}
type Rule map[string]any
type Stage struct {
	State string `json:"state"`
	Rules []Rule `json:"rules"`
}
type Bundle struct {
	SchemaVersion  int    `json:"schema_version"`
	Key            Key    `json:"key"`
	Incarnation    string `json:"scope_incarnation"`
	Revision       string `json:"revision"`
	ContextEpoch   string `json:"context_epoch"`
	CatalogVersion string `json:"catalog_version"`
	CatalogDigest  string `json:"catalog_digest"`
	Digest         string `json:"bundle_digest"`
	Invocation     Stage  `json:"invocation"`
	Capability     Stage  `json:"capability"`
}
type Change struct {
	Stage   string   `json:"stage"`
	Action  string   `json:"action"`
	Rules   []Rule   `json:"rules,omitempty"`
	RuleIDs []string `json:"rule_ids,omitempty"`
}
type CatalogPin struct {
	Version string `json:"catalog_version"`
	Digest  string `json:"catalog_digest"`
}
type Mutation struct {
	SchemaVersion   int         `json:"schema_version"`
	Action          string      `json:"action"`
	Expected        Expected    `json:"expected"`
	Changes         []Change    `json:"changes,omitempty"`
	TargetCatalog   *CatalogPin `json:"target_catalog,omitempty"`
	CapabilityRules []Rule      `json:"capability_rules,omitempty"`
}

var schemas struct {
	sync.Once
	p, mutation, prepare *jsonschema.Schema
	descriptor           []byte
	err                  error
}

func loadSchemas() {
	schemas.Do(func() {
		var doc map[string]any
		if err := json.Unmarshal(wireSchema, &doc); err != nil {
			schemas.err = err
			return
		}
		c := jsonschema.NewCompiler()
		const loc = "urn:octo:permission:document:v6:policy-wire"
		if err := c.AddResource(loc, doc); err != nil {
			schemas.err = err
			return
		}
		schemas.p, schemas.err = c.Compile(loc + "#/$defs/P")
		if schemas.err != nil {
			return
		}
		schemas.mutation, schemas.err = c.Compile(loc + "#/$defs/Mutation")
		if schemas.err != nil {
			return
		}
		var prep map[string]any
		if schemas.err = json.Unmarshal(prepareSchema, &prep); schemas.err != nil {
			return
		}
		const prepLoc = "urn:octo:permission:implementation:prepare-v1"
		if schemas.err = c.AddResource(prepLoc, prep); schemas.err != nil {
			return
		}
		schemas.prepare, schemas.err = c.Compile(prepLoc + "#/$defs/Request")
		if schemas.err != nil {
			return
		}
		props := doc["$defs"].(map[string]any)["ContractDescriptor"].(map[string]any)["properties"].(map[string]any)
		d := map[string]any{}
		for k, v := range props {
			if k == "limits" {
				limits := map[string]any{}
				for n, l := range v.(map[string]any)["properties"].(map[string]any) {
					limits[n] = l.(map[string]any)["const"]
				}
				d[k] = limits
			} else {
				d[k] = v.(map[string]any)["const"]
			}
		}
		// Re-decode constants as JSON numbers for the canonical encoder.
		raw, _ := json.Marshal(d)
		v, _ := decodeStrict(bytes.NewReader(raw), 16384)
		schemas.descriptor = canonical(v)
	})
}

func Descriptor() []byte   { loadSchemas(); return bytes.Clone(schemas.descriptor) }
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func U64(s string) (uint64, error) {
	if s == "" || len(s) > 20 || len(s) > 1 && s[0] == '0' {
		return 0, ErrInvalid
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrInvalid
		}
	}
	n, e := strconv.ParseUint(s, 10, 64)
	if e != nil {
		return 0, ErrInvalid
	}
	return n, nil
}
func ValidNativeID(s string) bool { return validText(s, 40, 160) }
func validText(s string, scalars, octets int) bool {
	if s == "" || len(s) > octets || !utf8.ValidString(s) || utf8.RuneCountInString(s) > scalars || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func ValidOrigin(s string) bool {
	if len(s) == 0 || len(s) > 2048 || strings.Contains(s, "#") {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}
func (k Key) Valid() bool {
	return ValidOrigin(k.Origin) && ValidNativeID(k.BotUID) && ValidNativeID(k.SpaceID)
}
func (p Bundle) Expected() Expected {
	return Expected{p.Incarnation, p.Revision, p.CatalogVersion, p.CatalogDigest}
}
func (p Bundle) Pin() CatalogPin { return CatalogPin{p.CatalogVersion, p.CatalogDigest} }

func asObject(v any) (map[string]any, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, ErrInvalid
	}
	x, e := decodeStrict(bytes.NewReader(b), MaxBundleBytes)
	if e != nil {
		return nil, e
	}
	m, ok := x.(map[string]any)
	if !ok {
		return nil, ErrInvalid
	}
	return m, nil
}
func ParseMutation(r io.Reader) (Mutation, error) {
	v, e := decodeStrict(r, MaxBundleBytes)
	if e != nil {
		return Mutation{}, e
	}
	loadSchemas()
	if schemas.err != nil {
		return Mutation{}, ErrUnavailable
	}
	m, ok := v.(map[string]any)
	if ok && m["schema_version"] != json.Number("1") {
		if n, yes := m["schema_version"].(json.Number); yes {
			if _, e := U64(string(n)); e == nil {
				return Mutation{}, ErrVersion
			}
		}
	}
	if !ok || m["schema_version"] != json.Number("1") || schemas.mutation.Validate(v) != nil {
		return Mutation{}, ErrInvalid
	}
	if e = semantic(v); e != nil {
		return Mutation{}, e
	}
	if e = normalizeMutation(m); e != nil {
		return Mutation{}, e
	}
	var out Mutation
	if json.Unmarshal(canonical(m), &out) != nil {
		return Mutation{}, ErrInvalid
	}
	return out, nil
}

func ParsePreparation(r io.Reader) (Preparation, error) {
	v, e := decodeStrict(r, 32768)
	if e != nil {
		return Preparation{}, e
	}
	loadSchemas()
	if schemas.err != nil {
		return Preparation{}, ErrUnavailable
	}
	if schemas.prepare.Validate(v) != nil || semantic(v) != nil {
		return Preparation{}, ErrInvalid
	}
	var req Preparation
	if json.Unmarshal(canonical(v), &req) != nil {
		return Preparation{}, ErrInvalid
	}
	for _, uid := range req.PeerUIDs {
		if !ValidNativeID(uid) {
			return Preparation{}, ErrInvalid
		}
	}
	return req, nil
}
func ParseBundle(r io.Reader) (Bundle, error) {
	v, e := decodeStrict(r, MaxBundleBytes)
	if e != nil {
		return Bundle{}, ErrUnavailable
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Bundle{}, ErrUnavailable
	}
	loadSchemas()
	if schemas.err != nil || m["schema_version"] != json.Number("1") || schemas.p.Validate(v) != nil || semantic(v) != nil {
		return Bundle{}, ErrUnavailable
	}
	if normalizeBundle(m) != nil {
		return Bundle{}, ErrUnavailable
	}
	digest := m["bundle_digest"].(string)
	delete(m, "bundle_digest")
	if hash(canonical(m)) != digest {
		return Bundle{}, ErrUnavailable
	}
	m["bundle_digest"] = digest
	var p Bundle
	if json.Unmarshal(canonical(m), &p) != nil {
		return Bundle{}, ErrUnavailable
	}
	return p, nil
}

// Seal validates the complete resulting object, fills only group defaults and
// computes its digest. Callers still need current native/reference authority.
func Seal(p Bundle) (Bundle, []byte, error) {
	p.Digest = strings.Repeat("0", 64)
	m, e := asObject(p)
	if e != nil {
		return Bundle{}, nil, e
	}
	loadSchemas()
	if schemas.err != nil {
		return Bundle{}, nil, ErrUnavailable
	}
	if schemas.p.Validate(m) != nil || semantic(m) != nil || normalizeBundle(m) != nil {
		return Bundle{}, nil, ErrInvalid
	}
	delete(m, "bundle_digest")
	m["bundle_digest"] = hash(canonical(m))
	b := canonical(m)
	if len(b) > MaxBundleBytes {
		return Bundle{}, nil, ErrInvalid
	}
	// Unmarshal into a fresh value. Reusing p would reuse its rule maps and
	// silently mutate the caller's stored intent during normalization/cloning.
	var sealed Bundle
	if json.Unmarshal(b, &sealed) != nil {
		return Bundle{}, nil, ErrInvalid
	}
	return sealed, b, nil
}

func semantic(v any) error {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if s, ok := v.(string); ok {
				switch k {
				case "revision", "context_epoch":
					if _, e := U64(s); e != nil {
						return e
					}
				case "origin":
					if !ValidOrigin(s) {
						return ErrInvalid
					}
				case "bot_uid", "space_id":
					if !ValidNativeID(s) {
						return ErrInvalid
					}
				case "id":
					kind, _ := x["kind"].(string)
					switch kind {
					case "dm":
						if !validText(s, 100, 400) {
							return ErrInvalid
						}
					case "group":
						if !ValidNativeID(s) {
							return ErrInvalid
						}
					case "thread":
						if !validText(s, 256, 256) {
							return ErrInvalid
						}
					}
				}
			}
			if k == "uids" {
				for _, uid := range v.([]any) {
					if !ValidNativeID(uid.(string)) {
						return ErrInvalid
					}
				}
			}
			if e := semantic(v); e != nil {
				return e
			}
		}
	case []any:
		for _, v := range x {
			if e := semantic(v); e != nil {
				return e
			}
		}
	}
	return nil
}
func sortUnique(a []any) error {
	sort.Slice(a, func(i, j int) bool { return bytes.Compare(canonical(a[i]), canonical(a[j])) < 0 })
	for i := 1; i < len(a); i++ {
		if bytes.Equal(canonical(a[i-1]), canonical(a[i])) {
			return ErrInvalid
		}
	}
	return nil
}
func normalizeRules(a []any) error {
	seen := map[string]bool{}
	for _, v := range a {
		r := v.(map[string]any)
		id := r["rule_id"].(string)
		if seen[id] {
			return ErrInvalid
		}
		seen[id] = true
		caller := r["caller"].(map[string]any)
		if u, ok := caller["uids"]; ok {
			if sortStrings(u.([]any)) != nil {
				return ErrInvalid
			}
		}
		source := r["source"].(map[string]any)
		if refs, ok := source["refs"]; ok {
			for _, ref := range refs.([]any) {
				m := ref.(map[string]any)
				if m["kind"] == "group" {
					if _, ok := m["include_descendants"]; !ok {
						m["include_descendants"] = false
					}
				}
			}
			if sortUnique(refs.([]any)) != nil {
				return ErrInvalid
			}
		}
		if types, ok := source["conversation_types"]; ok {
			if sortStrings(types.([]any)) != nil {
				return ErrInvalid
			}
		}
		if caps, ok := r["capabilities"]; ok {
			if sortStrings(caps.([]any)) != nil {
				return ErrInvalid
			}
		}
	}
	sort.Slice(a, func(i, j int) bool {
		return a[i].(map[string]any)["rule_id"].(string) < a[j].(map[string]any)["rule_id"].(string)
	})
	return nil
}
func sortStrings(a []any) error {
	sort.Slice(a, func(i, j int) bool { return a[i].(string) < a[j].(string) })
	for i := 1; i < len(a); i++ {
		if a[i] == a[i-1] {
			return ErrInvalid
		}
	}
	return nil
}
func normalizeBundle(m map[string]any) error {
	selectors, caps := 0, 0
	for _, name := range []string{"invocation", "capability"} {
		rules := m[name].(map[string]any)["rules"].([]any)
		if normalizeRules(rules) != nil {
			return ErrInvalid
		}
		for _, v := range rules {
			r := v.(map[string]any)
			c := r["caller"].(map[string]any)
			s := r["source"].(map[string]any)
			if a, ok := c["uids"]; ok {
				selectors += len(a.([]any))
			}
			if a, ok := s["refs"]; ok {
				selectors += len(a.([]any))
			}
			if a, ok := s["conversation_types"]; ok {
				selectors += len(a.([]any))
			}
			if a, ok := r["capabilities"]; ok {
				caps += len(a.([]any))
			}
		}
	}
	if selectors > 4096 || caps > 1024 {
		return ErrInvalid
	}
	return nil
}
func normalizeMutation(m map[string]any) error {
	if m["action"] == "migrate_catalog" {
		return normalizeRules(m["capability_rules"].([]any))
	}
	seen := map[string]bool{}
	for _, v := range m["changes"].([]any) {
		c := v.(map[string]any)
		s := c["stage"].(string)
		if seen[s] {
			return ErrInvalid
		}
		seen[s] = true
		if a, ok := c["rules"]; ok {
			if normalizeRules(a.([]any)) != nil {
				return ErrInvalid
			}
		}
	}
	return nil
}
