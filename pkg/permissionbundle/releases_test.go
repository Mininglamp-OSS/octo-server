package permissionbundle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Deliberately synthetic: these no-op profiles are NOT source-reconciled or a
// supported product release. Only tests supply them as trusted support input.
func syntheticCatalog(t *testing.T, version string) map[string]any {
	t.Helper()
	v, e := decodeStrictDepth(bytes.NewReader(releaseSchemaTemplate), 100000, 32)
	require.NoError(t, e)
	defs := v.(map[string]any)["$defs"].(map[string]any)
	props := defs["Release"].(map[string]any)["properties"].(map[string]any)
	m := map[string]any{}
	for name, v := range props {
		if c, ok := v.(map[string]any)["const"]; ok {
			m[name] = c
		}
	}
	m["catalog_version"] = version
	heads := map[string]any{}
	for name, v := range props["source_heads"].(map[string]any)["properties"].(map[string]any) {
		heads[name] = v.(map[string]any)["const"]
	}
	m["source_heads"] = heads
	blob := []byte("synthetic catalog input")
	digest := hash(blob)
	m["content"] = []any{map[string]any{"kind": "bytes", "sha256": digest, "length": json.Number("23"), "media_type": "text/plain", "data_base64": base64.StdEncoding.EncodeToString(blob)}}
	m["source_files"] = []any{[]any{"synthetic/input.txt", digest}}
	entries := []any{}
	for _, id := range defs["EntryID"].(map[string]any)["enum"].([]any) {
		entries = append(entries, map[string]any{"id": id, "kind": "operation", "target": "synthetic", "aliases": []any{}, "profile": "leaf", "content_refs": []any{digest}})
	}
	m["entries"] = entries
	profiles := []any{}
	for _, name := range []string{"assignee", "leaf", "portable", "portable-bytes", "resource-inventory", "search", "secret", "static-discovery", "static-resource", "static-schema"} {
		profiles = append(profiles, map[string]any{
			"name": name, "execution_contract": name + "-v1", "prepare": []any{}, "dispatch": []any{}, "publish": []any{}, "children": []any{}, "pagination": []any{},
			"branches": []any{map[string]any{"id": "synthetic", "condition": map[string]any{"op": "always"}, "prepare": []any{}, "dispatch": []any{}, "publish": []any{}}},
			"limits":   map[string]any{"page_limit": json.Number("0"), "items_per_page": json.Number("0"), "redirect_hops": json.Number("0"), "local_input_bytes": json.Number("0"), "concurrent_children": json.Number("0")},
		})
	}
	m["profiles"] = profiles
	return m
}

func syntheticSupport(m map[string]any) SupportedCatalog {
	b := canonical(m)
	return SupportedCatalog{CatalogPin{m["catalog_version"].(string), hash(b)}, b}
}

func TestArtifactReleasesPinnedImmutableAndExplicitMigration(t *testing.T) {
	old := syntheticSupport(syntheticCatalog(t, "synthetic-old"))
	next := syntheticSupport(syntheticCatalog(t, "synthetic-next"))
	r, e := NewArtifactReleases([]SupportedCatalog{old, next}, nil)
	require.NoError(t, e)
	ctx := t.Context()
	require.ErrorIs(t, r.Compatible(ctx, old.Pin, next.Pin), ErrUnavailable)
	r, e = NewArtifactReleases([]SupportedCatalog{old, next}, []CatalogTransition{{old.Pin, next.Pin}})
	require.NoError(t, e)
	loaded, e := r.Load(ctx, old.Pin)
	require.NoError(t, e)
	require.Len(t, loaded.Capabilities, 827)
	id := "octo-cli/bot/op/bot.heartbeat"
	deny := Rule{"effect": "deny", "capabilities": []any{id}}
	same, e := r.SameIntent(ctx, old.Pin, next.Pin, deny)
	require.NoError(t, e)
	require.True(t, same)
	p := fixtureBundle(t)
	p.CatalogVersion, p.CatalogDigest = old.Pin.Version, old.Pin.Digest
	rule := fixtureRule("deny")
	rule["capabilities"] = []any{id}
	p.Capability = Stage{"E", []Rule{rule}}
	p, _, e = Seal(p)
	require.NoError(t, e)
	migration := Mutation{SchemaVersion: 1, Action: "migrate_catalog", Expected: p.Expected(), TargetCatalog: &next.Pin, CapabilityRules: p.Capability.Rules}
	migrated, e := Apply(ctx, p, migration, r)
	require.NoError(t, e)
	require.Equal(t, "1", migrated.Revision)
	require.Equal(t, next.Pin, migrated.Pin())
	refs, e := ReferencesToValidate(ctx, p, migrated, migration, r)
	require.NoError(t, e)
	require.Empty(t, refs, "unchanged deny is retained only after approved compatible graph comparison")
	loaded.Bytes[0] = '!'
	delete(loaded.Capabilities, id)
	old.Artifact[0] = '!'
	loaded, e = r.Load(ctx, old.Pin)
	require.NoError(t, e)
	require.Equal(t, byte('{'), loaded.Bytes[0])
	require.True(t, loaded.Capabilities[id])
	require.ErrorIs(t, r.Compatible(ctx, next.Pin, old.Pin), ErrUnavailable)
	_, e = r.Load(ctx, CatalogPin{next.Pin.Version, strings.Repeat("0", 64)})
	require.ErrorIs(t, e, ErrUnavailable)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, e = r.Load(cancelled, next.Pin)
	require.ErrorIs(t, e, ErrUnavailable)

	changed := syntheticCatalog(t, "synthetic-changed")
	changed["entries"].([]any)[0].(map[string]any)["target"] = "changed-child-target"
	other := syntheticSupport(changed)
	r, e = NewArtifactReleases([]SupportedCatalog{next, other}, []CatalogTransition{{next.Pin, other.Pin}})
	require.NoError(t, e)
	same, e = r.SameIntent(ctx, next.Pin, other.Pin, deny)
	require.NoError(t, e)
	require.False(t, same, "even a change to another child disables retained-deny equivalence")
}

func TestArtifactReleasesDescriptorHashClosure(t *testing.T) {
	m := syntheticCatalog(t, "synthetic-descriptor")
	truth := m["content"].([]any)[0].(map[string]any)["sha256"]
	template := map[string]any{"kind": "template", "media_type": "application/json", "renderer": "go-json-marshalindent-2sp-html-escape-v1", "value_base64": base64.StdEncoding.EncodeToString([]byte("{}")), "slots": []any{}}
	templateHash := hash(canonical(template))
	template["sha256"] = templateHash
	variant := map[string]any{"kind": "schema_variant", "operation_id": "synthetic", "facade": "describe_op", "template_hash": templateHash, "parameter_truth_hash": truth}
	cs := append(m["content"].([]any), template, variant)
	sort.Slice(cs, func(i, j int) bool { return contentTestHash(cs[i]) < contentTestHash(cs[j]) })
	m["content"] = cs
	m["entries"].([]any)[0].(map[string]any)["content_refs"] = []any{contentTestHash(variant)}
	_, e := NewArtifactReleases([]SupportedCatalog{syntheticSupport(m)}, nil)
	require.NoError(t, e)
	template["value_base64"] = base64.StdEncoding.EncodeToString([]byte(`{"changed":true}`))
	_, e = NewArtifactReleases([]SupportedCatalog{syntheticSupport(m)}, nil)
	require.ErrorIs(t, e, ErrUnavailable, "a newly pinned outer artifact cannot hide changed content behind an old hash")
}

func TestArtifactReleasesRejectCandidateCorruptionAndOpenReferences(t *testing.T) {
	for _, name := range []string{"candidate", "wrong_version", "wrong_digest", "noncanonical", "duplicate_key", "fractional_metadata", "content_corruption", "base64_newline", "missing_content", "duplicate_source", "unsorted_entries", "duplicate_profile", "dangling_phase", "missing_variant_template"} {
		t.Run(name, func(t *testing.T) {
			m := syntheticCatalog(t, "synthetic-negative")
			c := m["content"].([]any)[0].(map[string]any)
			s := syntheticSupport(m)
			switch name {
			case "candidate":
				m["format"] = "permission-release-candidate-source-v1"
			case "content_corruption":
				c["data_base64"] = base64.StdEncoding.EncodeToString([]byte("altered"))
			case "base64_newline":
				c["data_base64"] = c["data_base64"].(string) + "\n"
			case "missing_content":
				m["entries"].([]any)[0].(map[string]any)["content_refs"] = []any{strings.Repeat("0", 64)}
			case "duplicate_source":
				m["source_files"] = append(m["source_files"].([]any), m["source_files"].([]any)[0])
			case "unsorted_entries":
				es := m["entries"].([]any)
				es[0], es[1] = es[1], es[0]
			case "duplicate_profile":
				ps := m["profiles"].([]any)
				ps[1] = ps[0]
			case "dangling_phase":
				m["profiles"].([]any)[0].(map[string]any)["branches"].([]any)[0].(map[string]any)["dispatch"] = []any{"missing"}
			case "missing_variant_template":
				variant := map[string]any{"kind": "schema_variant", "operation_id": "synthetic", "facade": "describe_op", "template_hash": strings.Repeat("0", 64), "parameter_truth_hash": c["sha256"]}
				cs := append(m["content"].([]any), variant)
				sort.Slice(cs, func(i, j int) bool { return contentTestHash(cs[i]) < contentTestHash(cs[j]) })
				m["content"] = cs
			}
			s = syntheticSupport(m)
			switch name {
			case "wrong_version":
				s.Pin.Version = "different"
			case "wrong_digest":
				s.Pin.Digest = strings.Repeat("0", 64)
			case "noncanonical":
				s.Artifact = append(s.Artifact, '\n')
				s.Pin.Digest = hash(s.Artifact)
			case "duplicate_key":
				s.Artifact = append([]byte(`{"format":"permission-release-manifest-v1",`), s.Artifact[1:]...)
				s.Pin.Digest = hash(s.Artifact)
			case "fractional_metadata":
				s.Artifact = bytes.Replace(s.Artifact, []byte(`"contract_revision":2`), []byte(`"contract_revision":2.0`), 1)
				s.Pin.Digest = hash(s.Artifact)
			}
			_, e := NewArtifactReleases([]SupportedCatalog{s}, nil)
			require.ErrorIs(t, e, ErrUnavailable)
		})
	}
	_, e := NewArtifactReleases(nil, nil)
	require.ErrorIs(t, e, ErrUnavailable)
	s := syntheticSupport(syntheticCatalog(t, "synthetic-duplicate-version"))
	_, e = NewArtifactReleases([]SupportedCatalog{s, s}, nil)
	require.ErrorIs(t, e, ErrUnavailable)
	_, e = NewArtifactReleases([]SupportedCatalog{s}, []CatalogTransition{{s.Pin, fixturePin}})
	require.ErrorIs(t, e, ErrUnavailable)
}

func contentTestHash(v any) string {
	m := v.(map[string]any)
	if h, ok := m["sha256"].(string); ok {
		return h
	}
	return hash(canonical(m))
}
