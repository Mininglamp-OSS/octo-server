package cardtmpl

// PR-C D5 evidence for the safe export projection.
//
// Two properties matter more than the field-by-field shape. First, what is
// *absent*: view templates, goldens and the canonical bundle must never appear,
// because those are the documents a caller could use to clone or probe a card
// rather than build against it. Second, that samples are opt-in per artifact —
// a fixture is caller-authored data and the default has to be "do not ship it".

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func compileBundleForExport(t *testing.T, bundle Bundle) *CompiledArtifact {
	t.Helper()
	artifact, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
	if err != nil {
		t.Fatalf("CompileJSONArtifact: %v", err)
	}
	return artifact
}

func TestSafeExportOmitsTemplatesGoldensAndBundle(t *testing.T) {
	artifact := compileBundleForExport(t, validJSONArtifactBundle())
	export := artifact.Meta.Export()
	if export == nil {
		t.Fatal("compiled artifact has no export projection")
	}
	encoded, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	// The golden body text and the template's substitution syntax are the two
	// strings that would prove a leak of either document.
	for _, forbidden := range []string{"${title}", "AdaptiveCard", "canonical"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("export leaked %q: %s", forbidden, encoded)
		}
	}
	if export.ID != "test.runtime-card" || export.Version != "1.0.0" ||
		export.Owner != "ai" || export.Engine != JSONTemplateEngineV1 {
		t.Fatalf("export identity = %+v", export)
	}
	if len(export.Views) != 1 || export.Views[0].Name != "main" ||
		len(export.Views[0].States) != 1 || export.Views[0].States[0] != "shown" {
		t.Fatalf("export views = %+v", export.Views)
	}
	if len(export.Manifest) == 0 || len(export.Schema) == 0 {
		t.Fatal("export dropped the reviewed manifest/schema bytes")
	}
}

func TestSafeExportShipsNoSamplesUnlessTheManifestOptsIn(t *testing.T) {
	artifact := compileBundleForExport(t, validJSONArtifactBundle())
	export := artifact.Meta.Export()
	if len(export.Samples) != 0 {
		t.Fatalf("samples exported without an allowlist: %+v", export.Samples)
	}
	// An empty map rather than null: a caller should not have to distinguish
	// "no exportable samples" from "field missing".
	encoded, _ := json.Marshal(export)
	if !bytes.Contains(encoded, []byte(`"samples":{}`)) {
		t.Fatalf("samples should serialize as an empty object: %s", encoded)
	}

	opted := validJSONArtifactBundle()
	opted.Manifest = json.RawMessage(strings.Replace(string(opted.Manifest),
		`"owner":"ai",`, `"owner":"ai","export":{"samples":["shown"]},`, 1))
	export = compileBundleForExport(t, opted).Meta.Export()
	if len(export.Samples) != 1 {
		t.Fatalf("allowlisted sample missing: %+v", export.Samples)
	}
	if !bytes.Contains(export.Samples["shown"], []byte("hello")) {
		t.Fatalf("allowlisted sample content = %s", export.Samples["shown"])
	}
}

// An allowlist naming a sample that does not exist is a manifest bug. Failing
// the compile surfaces it at publish review; serving an empty samples map
// instead would hide it until someone noticed the docs were thin.
func TestSafeExportRejectsAnAllowlistNamingAMissingSample(t *testing.T) {
	bundle := validJSONArtifactBundle()
	bundle.Manifest = json.RawMessage(strings.Replace(string(bundle.Manifest),
		`"owner":"ai",`, `"owner":"ai","export":{"samples":["does-not-exist"]},`, 1))
	if _, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits()); err == nil {
		t.Fatal("compile accepted an allowlist naming a missing sample")
	}
}

func TestSafeExportRejectsUnknownExportKeys(t *testing.T) {
	bundle := validJSONArtifactBundle()
	bundle.Manifest = json.RawMessage(strings.Replace(string(bundle.Manifest),
		`"owner":"ai",`, `"owner":"ai","export":{"samples":["shown"],"goldens":["shown"]},`, 1))
	if _, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits()); err == nil {
		t.Fatal("compile accepted an unknown export key")
	}
}

// Visibility fails closed. An artifact that declared nothing, or declared
// something unrecognised, is private — the alternative turns every future
// omission into a disclosure.
func TestSafeExportVisibilityFailsClosed(t *testing.T) {
	for _, declared := range []string{"", "internal", "PUBLIC"} {
		if got := normalizeExportVisibility(declared); got != CatalogVisibilityPrivate {
			t.Fatalf("visibility %q normalized to %q, want private", declared, got)
		}
	}
	if got := normalizeExportVisibility(CatalogVisibilityPublic); got != CatalogVisibilityPublic {
		t.Fatalf("public visibility normalized to %q", got)
	}
}

// The hash is the static ETag, so it must be stable across builds of the same
// artifact and must move when anything a caller can observe changes.
func TestSafeExportHashIsDeterministicAndCoversVisibility(t *testing.T) {
	first := compileBundleForExport(t, validJSONArtifactBundle()).Meta.Export()
	second := compileBundleForExport(t, validJSONArtifactBundle()).Meta.Export()
	if first.Hash == "" || first.Hash != second.Hash {
		t.Fatalf("export hash is not deterministic: %q vs %q", first.Hash, second.Hash)
	}

	public := validJSONArtifactBundle()
	public.Catalog.Visibility = CatalogVisibilityPublic
	changed := compileBundleForExport(t, public).Meta.Export()
	if changed.Hash == first.Hash {
		t.Fatal("export hash ignored a visibility change")
	}
}

// Clone must be deep: the projection is shared by every request for that
// template, so a handler that mutated a returned map would corrupt all of them.
func TestSafeExportCloneIsDeep(t *testing.T) {
	bundle := validJSONArtifactBundle()
	bundle.Manifest = json.RawMessage(strings.Replace(string(bundle.Manifest),
		`"owner":"ai",`, `"owner":"ai","export":{"samples":["shown"]},`, 1))
	meta := compileBundleForExport(t, bundle).Meta

	first := meta.Export()
	first.Samples["shown"] = json.RawMessage(`{"tampered":true}`)
	first.Views[0].States[0] = "tampered"

	second := meta.Export()
	if bytes.Contains(second.Samples["shown"], []byte("tampered")) {
		t.Fatalf("sample mutation escaped the clone: %s", second.Samples["shown"])
	}
	if second.Views[0].States[0] != "shown" {
		t.Fatalf("view mutation escaped the clone: %+v", second.Views[0])
	}
}

// A frozen static card cannot declare an export allowlist, so its exported
// sample set is permanently empty — and it is still private by default.
func TestStaticRegistryExportIsPrivateWithNoSamples(t *testing.T) {
	registry := runtimeStaticRegistry(t)
	template, err := registry.Lookup("test.runtime-parity", "1.0.0")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	export := template.Meta().Export()
	if export == nil {
		t.Fatal("static template has no export projection")
	}
	if export.Visibility != CatalogVisibilityPrivate {
		t.Fatalf("static visibility = %q, want private", export.Visibility)
	}
	if len(export.Samples) != 0 {
		t.Fatalf("static export carried samples: %+v", export.Samples)
	}
}

func TestRegistryStaticCatalogReportsVisibilityAndDefault(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterJSON(jsonCardTestData, "testdata/test.runtime-parity@1.0.0")
	registry.SetDefault("test.runtime-parity", "1.0.0")

	entries := registry.StaticCatalog()
	if len(entries) != 1 {
		t.Fatalf("static catalog = %+v", entries)
	}
	if entries[0].Visibility != CatalogVisibilityPrivate || !entries[0].IsDefault {
		t.Fatalf("entry = %+v, want private default", entries[0])
	}
	if entries[0].ExportHash == "" {
		t.Fatal("entry has no export hash to use as an ETag")
	}

	registry.SetCatalogVisibility("test.runtime-parity", "1.0.0", CatalogVisibilityPublic)
	entries = registry.StaticCatalog()
	if entries[0].Visibility != CatalogVisibilityPublic {
		t.Fatalf("visibility after publish = %q", entries[0].Visibility)
	}
	if entries[0].ExportHash == "" {
		t.Fatal("publishing dropped the export hash")
	}

	registry.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("SetCatalogVisibility succeeded after Freeze")
		}
	}()
	registry.SetCatalogVisibility("test.runtime-parity", "1.0.0", CatalogVisibilityPrivate)
}

// The 2 MiB cap has to fail the *build*, not the response. A projection that
// only blew up on the way out would reach a client as a truncated body — and
// because the projection is built once at registration or compile time, the
// failure belongs there, where an operator publishing the artifact sees it.
func TestSafeExportRefusesToBuildAnOversizedProjection(t *testing.T) {
	artifact := compileBundleForExport(t, validJSONArtifactBundle())
	meta := artifact.Meta

	// One sample past the cap. Samples are the only unbounded input a manifest
	// controls, which is why the allowlist is opt-in in the first place.
	oversized := json.RawMessage(`{"blob":"` + strings.Repeat("a", maxSafeExportBytes+1024) + `"}`)
	_, err := buildSafeExport(meta, JSONTemplateEngineV1, CatalogVisibilityPrivate, "docs",
		json.RawMessage(`{"type":"object"}`), nil,
		map[string]json.RawMessage{"huge": oversized}, []string{"huge"})
	if err == nil {
		t.Fatal("an oversized projection was built")
	}
	if !strings.Contains(err.Error(), "export") {
		t.Fatalf("err = %v, want the failure to name the export projection", err)
	}

	// Just under the cap still builds, so the guard is a bound rather than a
	// blanket refusal of samples.
	modest := json.RawMessage(`{"blob":"` + strings.Repeat("a", 1024) + `"}`)
	export, err := buildSafeExport(meta, JSONTemplateEngineV1, CatalogVisibilityPrivate, "docs",
		json.RawMessage(`{"type":"object"}`), nil,
		map[string]json.RawMessage{"modest": modest}, []string{"modest"})
	if err != nil {
		t.Fatalf("a projection under the cap was refused: %v", err)
	}
	if _, ok := export.Samples["modest"]; !ok {
		t.Fatalf("the allowlisted sample is missing: %+v", export.Samples)
	}
}

// TestPublishedManifestDropsPathsAndTheSampleInventory is review P2-3
// (yujiawei). The sample opt-in gates sample *bodies* and always did; what
// leaked was the manifest announcing the inventory those bodies belong to.
//
// The assertion is on the serialized bytes rather than on struct fields,
// because the defect was a pass-through: any check that decoded into a known
// shape would have looked at exactly the fields that were fine.
func TestPublishedManifestDropsPathsAndTheSampleInventory(t *testing.T) {
	raw := json.RawMessage(`{
        "schemaVersion": 1,
        "id": "docs.pilot",
        "name": "Pilot",
        "version": "0.4.0",
        "contractVersion": "1.0.0",
        "protocol": "octo-card@1.0",
        "adaptiveCardVersion": "1.5",
        "defaultLocale": "zh-CN",
        "renderProfile": "octo-chat@1.2.0-rc.1",
        "renderProfileCompatibility": "octo-chat@1.2",
        "owner": "docs",
        "actionType": "access_request.decision",
        "dataSchema": "contract/data.schema.json",
        "views": {"result": {
            "template": "templates/pending.template.json",
            "samples": ["samples/approved.json", "samples/rejected.json"]
        }},
        "export": {"samples": ["pending"]}
    }`)

	published, err := publishedManifest(raw)
	if err != nil {
		t.Fatalf("publishedManifest: %v", err)
	}
	body := string(published)

	// Nothing that names where a document lives, and nothing that enumerates
	// the fixtures the manifest did not opt into exporting.
	for _, forbidden := range []string{
		"samples/approved.json", "samples/rejected.json",
		"templates/pending.template.json", "contract/data.schema.json",
		"dataSchema", "views", "export",
		// renderProfile is provenance-only for the exact Forge artifact; the
		// stable renderProfileCompatibility is the published one, and a
		// substring check would match both, so this asserts the exact value.
		"octo-chat@1.2.0-rc.1",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("published manifest leaked %q: %s", forbidden, body)
		}
	}

	// And it still carries the contract a caller came for.
	for _, required := range []string{
		`"id":"docs.pilot"`, `"version":"0.4.0"`, `"contractVersion":"1.0.0"`,
		`"protocol":"octo-card@1.0"`, `"actionType":"access_request.decision"`,
		`"renderProfileCompatibility":"octo-chat@1.2"`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("published manifest dropped %s: %s", required, body)
		}
	}
}
