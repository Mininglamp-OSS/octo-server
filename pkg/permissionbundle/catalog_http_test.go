package permissionbundle

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
)

func catalogQuery(pin CatalogPin, path string) string {
	q := url.Values{"space_id": {"space"}, "format": {CatalogPackageV2}, "catalog_version": {pin.Version}, "catalog_digest": {pin.Digest}}
	if path != "" {
		q.Set("path", path)
	}
	return q.Encode()
}
func catalogStore(t *testing.T, legacyFirst bool) (Store, PackageInput, SupportedSource) {
	t.Helper()
	input, _ := completePackage(t, false)
	legacy := fixtureArtifactV1(t, input)
	r, e := NewCatalogRegistry(t.Context(), []SupportedSource{legacy, packageSource(input)}, []CatalogTransition{{legacy.Pin, input.Pin}, {input.Pin, legacy.Pin}}, nil)
	require.NoError(t, e)
	s := fixtureStore(t, filepath.Join(t.TempDir(), "catalog.sqlite"))
	s.Releases = r
	pin := input.Pin
	if legacyFirst {
		pin = legacy.Pin
	}
	seed(t, s, pin)
	return s, input, legacy
}

func TestCatalogPrivateManifestFileReadsAndEligibility(t *testing.T) {
	s, input, _ := catalogStore(t, false)
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	for _, actor := range []string{"owner", "self"} {
		uid := actor
		if actor == "self" {
			uid = ""
		}
		r.GET("/"+actor+"/catalog", func(c *wkhttp.Context) { s.Serve(c, "bot", uid, "catalog") })
		r.GET("/"+actor+"/catalog/file", func(c *wkhttp.Context) { s.Serve(c, "bot", uid, "catalog_file") })
	}
	for _, actor := range []string{"owner", "self"} {
		for _, path := range []string{"", "index.json", "blobs/0.bin"} {
			endpoint := "/" + actor + "/catalog"
			if path != "" {
				endpoint += "/file"
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest("GET", endpoint+"?"+catalogQuery(input.Pin, path), nil))
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
			require.Equal(t, "identity", rec.Header().Get("Content-Encoding"))
			file := path
			if file == "" {
				file = "manifest.json"
			}
			require.Equal(t, input.Files[file], rec.Body.Bytes())
			if path == "blobs/0.bin" {
				require.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
			} else {
				require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			}
		}
	}
	for name, path := range map[string]string{"legacy_skew": "/self/catalog?space_id=space", "duplicate": "/self/catalog?" + catalogQuery(input.Pin, "") + "&format=" + CatalogPackageV2, "unknown_selector": "/self/catalog?" + catalogQuery(input.Pin, "") + "&extra=1", "wrong_digest": "/self/catalog?" + catalogQuery(CatalogPin{input.Pin.Version, strings.Repeat("0", 64)}, ""), "escape": "/self/catalog/file?" + catalogQuery(input.Pin, "../manifest.json"), "double_escape": "/self/catalog/file?" + catalogQuery(input.Pin, "%2e%2e/manifest.json"), "unlisted": "/self/catalog/file?" + catalogQuery(input.Pin, "blobs/7.bin"), "url": "/self/catalog/file?" + catalogQuery(input.Pin, "https://synthetic.invalid/file")} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
			require.Equal(t, 400, rec.Code)
			require.NotContains(t, rec.Body.String(), "source-only bytes")
		})
	}
	compressed := httptest.NewRequest("GET", "/self/catalog?"+catalogQuery(input.Pin, ""), nil)
	compressed.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, compressed)
	require.Equal(t, 400, rec.Code)
	// A prior manifest response confers no authority lease for later files.
	_, e := s.Session.Exec(`UPDATE space_member SET status=0 WHERE uid='bot'`)
	require.NoError(t, e)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/self/catalog/file?"+catalogQuery(input.Pin, "blobs/0.bin"), nil))
	require.Equal(t, 400, rec.Code)
	require.NotContains(t, rec.Body.String(), "source-only bytes")
}

func TestCatalogMigrationCASDenyRetentionReadbackAndRollback(t *testing.T) {
	s, input, legacy := catalogStore(t, true)
	initial, e := s.Get(t.Context(), fixtureKey, "owner")
	require.NoError(t, e)
	require.Contains(t, string(initial.Contract), `"catalog_format":"permission-release-manifest-v1"`)
	rule := fixtureRule("deny")
	rule["caller"] = map[string]any{"kind": "uids", "uids": []any{"departed"}}
	rule["capabilities"] = []any{"octo-cli/bot/op/bot.heartbeat"}
	invocation := fixtureRule("deny")
	invocation["caller"] = rule["caller"]
	activate := Mutation{SchemaVersion: 1, Action: "mutate", Expected: initial.Bundle.Expected(), Changes: []Change{{Stage: "invocation", Action: "activate", Rules: []Rule{invocation}}, {Stage: "capability", Action: "activate", Rules: []Rule{rule}}}}
	active, e := s.Mutate(t.Context(), fixtureKey, "owner", bytes.NewReader(fixtureJSON(t, activate)))
	require.NoError(t, e)
	_, e = s.Session.Exec(`UPDATE space_member SET status=0 WHERE uid='departed'`)
	require.NoError(t, e)
	migration := Mutation{SchemaVersion: 1, Action: "migrate_catalog", Expected: active.Bundle.Expected(), TargetCatalog: &input.Pin, CapabilityRules: active.Bundle.Capability.Rules}
	saved, e := s.Mutate(t.Context(), fixtureKey, "owner", bytes.NewReader(fixtureJSON(t, migration)))
	require.NoError(t, e)
	require.Equal(t, "2", saved.Bundle.Revision)
	require.Equal(t, active.Bundle.ContextEpoch, saved.Bundle.ContextEpoch)
	require.Equal(t, active.Bundle.Invocation, saved.Bundle.Invocation)
	require.Equal(t, active.Bundle.Capability, saved.Bundle.Capability)
	require.Contains(t, string(saved.Contract), `"catalog_format":"permission-catalog-package-v2"`)
	readback, e := s.Get(t.Context(), fixtureKey, "owner")
	require.NoError(t, e)
	require.Equal(t, saved, readback)
	_, e = s.Mutate(t.Context(), fixtureKey, "owner", bytes.NewReader(fixtureJSON(t, migration)))
	require.ErrorIs(t, e, ErrConflict)
	allow := fixtureRule("allow")
	allow["caller"] = rule["caller"]
	allow["capabilities"] = rule["capabilities"]
	changed := Mutation{SchemaVersion: 1, Action: "mutate", Expected: saved.Bundle.Expected(), Changes: []Change{{Stage: "capability", Action: "replace_rules", Rules: []Rule{allow}}}}
	_, e = s.Mutate(t.Context(), fixtureKey, "owner", bytes.NewReader(fixtureJSON(t, changed)))
	require.ErrorIs(t, e, ErrReference)
	rollback := Mutation{SchemaVersion: 1, Action: "migrate_catalog", Expected: saved.Bundle.Expected(), TargetCatalog: &legacy.Pin, CapabilityRules: saved.Bundle.Capability.Rules}
	back, e := s.Mutate(t.Context(), fixtureKey, "owner", bytes.NewReader(fixtureJSON(t, rollback)))
	require.NoError(t, e)
	require.Equal(t, "3", back.Bundle.Revision)
	require.Equal(t, active.Bundle.ContextEpoch, back.Bundle.ContextEpoch)
	require.Equal(t, active.Bundle.Capability, back.Bundle.Capability)
	r := wkhttp.New()
	r.GET("/catalog", func(c *wkhttp.Context) { s.Serve(c, "bot", "", "catalog") })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/catalog?space_id=space", nil))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, legacy.Artifact, rec.Body.Bytes())
	var descriptor map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(back.Contract, &descriptor))
	require.Len(t, descriptor, 7)
}

func TestCatalogTrustedStartupAndClosedReaderDelta(t *testing.T) {
	s, input, _ := catalogStore(t, false)
	ctx := &config.Context{}
	require.Nil(t, RuntimeFor(ctx).Store())
	require.NoError(t, Install(ctx, s, input.Pin, nil))
	require.NotNil(t, RuntimeFor(ctx).Store())
	require.False(t, RuntimeFor(ctx).HasFacts())
	response, e := s.Get(t.Context(), fixtureKey, "owner")
	require.NoError(t, e)
	raw, e := os.ReadFile("contracts/permission-wire-catalog-v2.schema.json")
	require.NoError(t, e)
	doc := fixtureObject(t, raw)
	original := fixtureObject(t, wireSchema)
	// Independently establish the exact normative schema delta.
	delete(doc, "$id")
	delete(doc, "$comment")
	delete(original, "$id")
	delete(original, "$comment")
	doc["$defs"].(map[string]any)["ContractDescriptor"].(map[string]any)["properties"].(map[string]any)["catalog_format"] = map[string]any{"const": CatalogArtifactV1}
	require.Equal(t, original, doc)
	var schemaDoc any
	require.NoError(t, json.Unmarshal(raw, &schemaDoc))
	compiler := jsonschema.NewCompiler()
	const id = "urn:octo:permission:reader:policy-wire-catalog-v2"
	require.NoError(t, compiler.AddResource(id, schemaDoc))
	schema, e := compiler.Compile(id + "#/$defs/OwnerResponse")
	require.NoError(t, e)
	value, e := decodeStrict(bytes.NewReader(fixtureJSON(t, response)), 393216)
	require.NoError(t, e)
	require.NoError(t, schema.Validate(value))
	value.(map[string]any)["contract"].(map[string]any)["catalog_format"] = "unknown"
	require.Error(t, schema.Validate(value))
}
