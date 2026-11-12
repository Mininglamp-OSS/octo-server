package permissionbundle

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// SupportedCatalog is trusted startup configuration from the catalog owner
// (XIN-2794), AFTER its source/profile reconciliation and support approval.
// Neither an artifact's format nor passing the structural fragment below
// supplies that approval. There are deliberately no built-in supported pins.
type SupportedCatalog struct {
	Pin      CatalogPin
	Artifact []byte
}

// CatalogTransition is a directed, catalog-owner-approved compatibility pair.
// Matching IDs, versions or semantics_version alone do not approve migration.
type CatalogTransition struct{ From, To CatalogPin }

// ArtifactReleases admits an immutable support list once, before serving.
// Load performs no network/source compilation and never substitutes latest.
type ArtifactReleases struct {
	releases    map[CatalogPin]Release
	intent      map[CatalogPin]map[string]string
	transitions map[CatalogTransition]bool
}

var releaseSchema struct {
	sync.Once
	fragment *jsonschema.Schema
	err      error
}

//go:embed contracts/permission-release-manifest-v1.schema-template.json
var releaseSchemaTemplate []byte

// The selected schema TEMPLATE root contains "not":{} intentionally. Compile
// only the structural definition for defensive validation of independently
// approved bytes; do not alter the template or mistake this for support review.
func releaseFragment() (*jsonschema.Schema, error) {
	releaseSchema.Do(func() {
		var doc map[string]any
		if releaseSchema.err = json.Unmarshal(releaseSchemaTemplate, &doc); releaseSchema.err != nil {
			return
		}
		id := doc["$id"].(string)
		c := jsonschema.NewCompiler()
		if releaseSchema.err = c.AddResource(id, doc); releaseSchema.err == nil {
			releaseSchema.fragment, releaseSchema.err = c.Compile(id + "#/$defs/Release")
		}
	})
	return releaseSchema.fragment, releaseSchema.err
}

// NewArtifactReleases verifies byte pins, structural bounds, content hashes,
// reference closure and ordering before publishing any member of the set.
// Full source-equivalence/program validation remains the catalog owner's
// prerequisite for supplying SupportedCatalog; this is not another publisher.
func NewArtifactReleases(supported []SupportedCatalog, transitions []CatalogTransition) (*ArtifactReleases, error) {
	if len(supported) == 0 {
		return nil, ErrUnavailable
	}
	r := &ArtifactReleases{map[CatalogPin]Release{}, map[CatalogPin]map[string]string{}, map[CatalogTransition]bool{}}
	versions := map[string]bool{}
	for _, s := range supported {
		if versions[s.Pin.Version] {
			return nil, ErrUnavailable // a version can never be rebound to bytes
		}
		// Fixed profiles permit four condition levels nested within the root,
		// profile, branch/action and condition-array records (more than the policy
		// wire's 12 levels). This separate bound leaves policy parsing unchanged.
		v, e := decodeStrictDepth(bytes.NewReader(s.Artifact), 8388608, 32)
		if e != nil || !catalogNumbers(v) || !bytes.Equal(canonical(v), s.Artifact) || hash(s.Artifact) != s.Pin.Digest {
			return nil, ErrUnavailable
		}
		fragment, e := releaseFragment()
		if e != nil || fragment.Validate(v) != nil {
			return nil, ErrUnavailable
		}
		m := v.(map[string]any)
		if m["catalog_version"] != s.Pin.Version {
			return nil, ErrUnavailable
		}
		caps, intent, e := indexRelease(m)
		if e != nil {
			return nil, e
		}
		versions[s.Pin.Version] = true
		r.releases[s.Pin] = Release{s.Pin, bytes.Clone(s.Artifact), caps}
		r.intent[s.Pin] = intent
	}
	for _, pair := range transitions {
		if _, ok := r.releases[pair.From]; !ok {
			return nil, ErrUnavailable
		}
		if _, ok := r.releases[pair.To]; !ok || pair.From == pair.To || r.transitions[pair] {
			return nil, ErrUnavailable
		}
		r.transitions[pair] = true
	}
	return r, nil
}

func (r *ArtifactReleases) Load(ctx context.Context, pin CatalogPin) (Release, error) {
	if r == nil || ctx.Err() != nil {
		return Release{}, ErrUnavailable
	}
	s, ok := r.releases[pin]
	if !ok {
		return Release{}, ErrUnavailable
	}
	copy := Release{s.Pin, bytes.Clone(s.Bytes), make(map[string]bool, len(s.Capabilities))}
	for id := range s.Capabilities {
		copy.Capabilities[id] = true
	}
	return copy, nil
}

func (r *ArtifactReleases) Compatible(ctx context.Context, from, to CatalogPin) error {
	if r == nil || ctx.Err() != nil || r.intent[from] == nil || r.intent[to] == nil || from != to && !r.transitions[CatalogTransition{from, to}] {
		return ErrUnavailable
	}
	return nil
}

func (r *ArtifactReleases) SameIntent(ctx context.Context, from, to CatalogPin, rule Rule) (bool, error) {
	if e := r.Compatible(ctx, from, to); e != nil {
		return false, e
	}
	caps, ok := rule["capabilities"].([]any)
	if !ok || len(caps) == 0 || rule["effect"] != "deny" {
		return false, ErrInvalid
	}
	for _, v := range caps {
		id, ok := v.(string)
		if !ok || r.intent[from][id] == "" || r.intent[from][id] != r.intent[to][id] {
			return false, nil
		}
	}
	return true, nil
}

func indexRelease(m map[string]any) (map[string]bool, map[string]string, error) {
	content := map[string]map[string]any{}
	previous := ""
	for _, v := range m["content"].([]any) {
		c := v.(map[string]any)
		digest := ""
		switch c["kind"] {
		case "bytes":
			b, e := catalogBlob(c["data_base64"].(string))
			if e != nil || c["length"] != json.Number(strconv.Itoa(len(b))) {
				return nil, nil, ErrUnavailable
			}
			digest = hash(b)
		default:
			d := map[string]any{}
			for k, v := range c {
				if k != "sha256" {
					d[k] = v
				}
			}
			digest = hash(canonical(d))
			if c["kind"] == "template" {
				_, e := catalogBlob(c["value_base64"].(string))
				if e != nil {
					return nil, nil, e
				}
				// Template syntax/slot rendering belongs to the owner's source
				// reconciliation. Here its exact inert bytes are hash-bound.
			}
			if c["kind"] == "discovery_index" {
				for _, v := range c["rows"].([]any) {
					if _, e := catalogBlob(v.(map[string]any)["row_base64"].(string)); e != nil {
						return nil, nil, e
					}
				}
			}
		}
		if digest <= previous || c["kind"] != "schema_variant" && c["sha256"] != digest {
			return nil, nil, ErrUnavailable
		}
		previous = digest
		content[digest] = c
	}
	for _, c := range content {
		if c["kind"] == "schema_variant" && (content[c["template_hash"].(string)]["kind"] != "template" || content[c["parameter_truth_hash"].(string)]["kind"] != "bytes") {
			return nil, nil, ErrUnavailable
		}
	}
	previous = ""
	for _, v := range m["source_files"].([]any) {
		p := v.([]any)
		path, digest := p[0].(string), p[1].(string)
		if path == "" || path <= previous || content[digest]["kind"] != "bytes" {
			return nil, nil, ErrUnavailable
		}
		previous = path
	}
	profiles := map[string]map[string]any{}
	previous = ""
	for _, v := range m["profiles"].([]any) {
		p := v.(map[string]any)
		name := p["name"].(string)
		if name <= previous {
			return nil, nil, ErrUnavailable
		}
		previous = name
		profiles[name] = p
	}
	caps, intent := map[string]bool{}, map[string]string{}
	previous = ""
	for _, v := range m["entries"].([]any) {
		e := v.(map[string]any)
		id := e["id"].(string)
		if id <= previous || profiles[e["profile"].(string)] == nil {
			return nil, nil, ErrUnavailable
		}
		previous = id
		refs := e["content_refs"].([]any)
		priorRef := ""
		for _, v := range refs {
			ref := v.(string)
			if ref <= priorRef || content[ref] == nil {
				return nil, nil, ErrUnavailable
			}
			priorRef = ref
		}
		// Conservative equivalence: no textual-ID inference. The entry binds its
		// content hashes (including transitive descriptor hashes), complete fixed
		// program and all its child descriptors. Child closure is added below.
		caps[id] = true
	}
	for _, p := range profiles {
		if !catalogProfileReferences(p, caps) {
			return nil, nil, ErrUnavailable
		}
	}
	// Whole entry table is intentionally included: any changed child, alias,
	// content binding or target disables retained-deny equivalence. An approved
	// compatible migration can still proceed with current reference validation.
	global := map[string]any{}
	for _, k := range []string{"semantics_version", "identity_index_sha256", "normalization", "limits", "exceptions", "source_heads", "profiles", "entries"} {
		global[k] = m[k]
	}
	fingerprint := hash(canonical(global))
	for id := range caps {
		intent[id] = fingerprint
	}
	return caps, intent, nil
}

func catalogProfileReferences(p map[string]any, caps map[string]bool) bool {
	for _, v := range p["children"].([]any) {
		if !caps[v.(string)] {
			return false
		}
	}
	all := map[string]bool{}
	for _, phase := range []string{"prepare", "dispatch", "publish"} {
		actions, referenced := map[string]bool{}, map[string]bool{}
		for _, v := range p[phase].([]any) {
			a := v.(map[string]any)
			id, cap := a["id"].(string), a["capability"].(string)
			if all[id] || cap != "self" && !caps[cap] {
				return false
			}
			all[id], actions[id] = true, true
		}
		for _, v := range p["branches"].([]any) {
			for _, v := range v.(map[string]any)[phase].([]any) {
				id := v.(string)
				if !actions[id] {
					return false
				}
				referenced[id] = true
			}
		}
		if len(actions) != len(referenced) {
			return false
		}
	}
	if len(all) > 64 {
		return false
	}
	for _, v := range p["pagination"].([]any) {
		if !all[v.(map[string]any)["action_id"].(string)] {
			return false
		}
	}
	return true
}

func catalogBlob(s string) ([]byte, error) {
	b, e := base64.StdEncoding.Strict().DecodeString(s)
	if e != nil || base64.StdEncoding.EncodeToString(b) != s {
		return nil, ErrUnavailable
	}
	return b, nil
}

func catalogNumbers(v any) bool {
	switch x := v.(type) {
	case json.Number:
		n, e := U64(string(x))
		return e == nil && n <= 9007199254740991
	case map[string]any:
		for _, v := range x {
			if !catalogNumbers(v) {
				return false
			}
		}
	case []any:
		for _, v := range x {
			if !catalogNumbers(v) {
				return false
			}
		}
	}
	return true
}
