package permissionbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func independentHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func fixtureJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	require.NoError(t, e)
	return b
}
func fixtureObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	require.NoError(t, d.Decode(&m))
	return m
}

func completePackage(t *testing.T, slots bool) (PackageInput, map[string]any) {
	t.Helper()
	suffix := ""
	if slots {
		suffix = "-slots"
	}
	f, e := os.Open("testdata/package-v2-complete" + suffix + ".tar.gz")
	require.NoError(t, e)
	defer f.Close()
	z, e := gzip.NewReader(f)
	require.NoError(t, e)
	defer z.Close()
	tarReader := tar.NewReader(z)
	files := map[string][]byte{}
	for {
		h, e := tarReader.Next()
		if e == io.EOF {
			break
		}
		require.NoError(t, e)
		require.Equal(t, byte(tar.TypeReg), h.Typeflag)
		b, e := io.ReadAll(tarReader)
		require.NoError(t, e)
		files[h.Name] = b
	}
	m := fixtureObject(t, files["manifest.json"])
	raw, e := os.ReadFile("testdata/package-v2-ledger" + suffix + ".json")
	require.NoError(t, e)
	ledger := fixtureObject(t, raw)
	return PackageInput{CatalogPin{m["catalog_version"].(string), independentHash(files["manifest.json"])}, files}, ledger
}

func fixtureJointSlots(_ context.Context, c SlotCell) (map[string]any, error) {
	names := []string{}
	for i, n := range []string{"space_id", "channel_id", "channel_type"} {
		if c.ManagedMask&(1<<i) != 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return map[string]any{"facade_hint": c.ServerFacade, "server_managed_argument_names": names}, nil
}

func TestCatalogPackageCompleteIndependentLedgers(t *testing.T) {
	loadPackageSchemas()
	require.NoError(t, packageSchemas.err)
	for _, slots := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "slots"}[slots], func(t *testing.T) {
			input, expected := completePackage(t, slots)
			v, e := VerifyCatalogPackage(t.Context(), input, fixtureJointSlots)
			require.NoError(t, e)
			actual := fixtureObject(t, fixtureJSON(t, v.Ledger()))
			for _, key := range []string{"stored_bytes", "metadata_nodes", "pre_bytes", "post_bytes", "pre_nodes", "post_nodes", "max_depth", "parts", "roots", "shared_rows", "content_records", "schema_cells", "discovery_cells", "static_cells"} {
				require.Equal(t, expected[key], actual[key], key)
			}
			require.Len(t, v.entries, 827)
			require.Len(t, v.profiles, 10)
			require.LessOrEqual(t, v.ledger.MaxDepth, 32)
			require.Equal(t, expected["manifest_sha256"], v.Pin().Digest)
			require.Equal(t, []byte("\xff\x00opaque $static_ref\n"), v.roots[independentHash([]byte("\xff\x00opaque $static_ref\n"))])
			root := expected["raw_template_sha256"].(string)
			require.Contains(t, string(v.roots[root]), `"number":1e0`)
			require.Contains(t, string(v.roots[root]), `"a":null`)
			key := cellKey("octo-cli/bot/schema/bot.heartbeat", "describe_op", 1, 7, 0)
			require.Equal(t, expected["rendered_template_sha256"], v.rendered[key])
			file, ok := v.File("manifest.json")
			require.True(t, ok)
			file[0] = '!'
			input.Files["manifest.json"][0] = '!'
			again, _ := v.File("manifest.json")
			require.Equal(t, byte('{'), again[0])
			t.Logf("synthetic only: %+v semantic=%s", v.Ledger(), v.SemanticDigest())
		})
	}
}

func rebindFixture(t *testing.T, input *PackageInput) {
	t.Helper()
	m := fixtureObject(t, input.Files["manifest.json"])
	for _, value := range m["files"].([]any) {
		f := value.(map[string]any)
		raw := input.Files[f["path"].(string)]
		f["bytes"] = len(raw)
		f["sha256"] = independentHash(raw)
	}
	input.Files["manifest.json"] = fixtureJSON(t, m)
	input.Pin.Digest = independentHash(input.Files["manifest.json"])
}
func changeFixture(t *testing.T, input *PackageInput, path string, change func(map[string]any)) {
	m := fixtureObject(t, input.Files[path])
	change(m)
	input.Files[path] = fixtureJSON(t, m)
}

func TestCatalogPackageRejectsCorruptAndPartialLoads(t *testing.T) {
	for _, name := range []string{"missing_file", "extra_file", "tampered_file", "wrong_pin", "duplicate_key", "new_null_path", "new_null_selector", "missing_inherited_literal", "identity_target", "root_range", "blob_padding", "part_fraction", "selector_gap", "selector_overlap", "source_only_missing"} {
		t.Run(name, func(t *testing.T) {
			input, _ := completePackage(t, false)
			switch name {
			case "missing_file":
				delete(input.Files, "index.json")
			case "extra_file":
				input.Files["unlisted.json"] = []byte(`{}`)
			case "tampered_file":
				input.Files["blobs/0.bin"][0] ^= 1
			case "wrong_pin":
				input.Pin.Digest = independentHash([]byte("wrong"))
			case "duplicate_key":
				input.Files["index.json"] = append([]byte(`{"kind":"index",`), input.Files["index.json"][1:]...)
				rebindFixture(t, &input)
			case "new_null_path":
				changeFixture(t, &input, "manifest.json", func(m map[string]any) { m["files"].([]any)[0].(map[string]any)["path"] = nil })
				input.Pin.Digest = independentHash(input.Files["manifest.json"])
			case "new_null_selector":
				changeFixture(t, &input, "index.json", func(m map[string]any) { m["static_bindings"].([]any)[0].(map[string]any)["selector"] = nil })
				rebindFixture(t, &input)
			case "missing_inherited_literal":
				changeFixture(t, &input, "index.json", func(m map[string]any) {
					for _, v := range m["profiles"].([]any) {
						p := v.(map[string]any)
						if p["name"] == "leaf" {
							delete(p["prepare"].([]any)[0].(map[string]any)["condition"].(map[string]any), "literal")
						}
					}
				})
				rebindFixture(t, &input)
			case "identity_target":
				changeFixture(t, &input, "modules/bot/0.json", func(m map[string]any) { m["entries"].([]any)[0].(map[string]any)["target"] = "changed" })
				rebindFixture(t, &input)
			case "root_range", "part_fraction":
				changeFixture(t, &input, "shared/00.json", func(m map[string]any) {
					for _, v := range m["roots"].([]any) {
						r := v.(map[string]any)
						if len(r["parts"].([]any)) > 0 {
							part := r["parts"].([]any)[0].(map[string]any)
							if name == "root_range" {
								part["offset"] = 1048576
							} else {
								part["length"] = json.Number("1.0")
							}
							break
						}
					}
				})
				rebindFixture(t, &input)
			case "blob_padding":
				input.Files["blobs/0.bin"] = append(input.Files["blobs/0.bin"], 0)
				rebindFixture(t, &input)
			case "selector_gap":
				changeFixture(t, &input, "index.json", func(m map[string]any) {
					m["static_bindings"].([]any)[0].(map[string]any)["selector"].(map[string]any)["resources_bits"] = 1
				})
				rebindFixture(t, &input)
			case "selector_overlap":
				changeFixture(t, &input, "index.json", func(m map[string]any) {
					a := m["static_bindings"].([]any)
					copy := fixtureObject(t, fixtureJSON(t, a[0]))
					copy["selector"].(map[string]any)["resources_bits"] = 1
					m["static_bindings"] = append([]any{copy}, a...)
				})
				rebindFixture(t, &input)
			case "source_only_missing":
				index := fixtureObject(t, input.Files["index.json"])
				id := index["source_files"].([]any)[1].([]any)[1]
				changeFixture(t, &input, "modules/_global/0.json", func(m map[string]any) {
					kept := []any{}
					for _, v := range m["content"].([]any) {
						if v.(map[string]any)["sha256"] != id {
							kept = append(kept, v)
						}
					}
					m["content"] = kept
				})
				rebindFixture(t, &input)
			}
			v, e := VerifyCatalogPackage(t.Context(), input, fixtureJointSlots)
			require.ErrorIs(t, e, ErrUnavailable)
			require.Nil(t, v)
		})
	}
}

func TestCatalogSlotAssignmentsValidatedBeforeRenderCache(t *testing.T) {
	raw := []byte(`{"hint":{"$octo_slot":"hint"}}`)
	root := independentHash(raw)
	v := &VerifiedCatalog{roots: map[string][]byte{root: raw}, content: map[string]map[string]any{
		"template": {"kind": "template", "value_ref": root, "slots": []any{map[string]any{"id": "hint", "pointer": "/hint", "domain": "facade_hint", "max_length": json.Number("64")}}},
	}}
	calls := 0
	p := &packageCheck{v: v, ctx: t.Context(), uses: map[string]int{root: 1}, rootNodes: map[string]int{root: 3}, templateCells: map[string][]SlotCell{"template": {{}, {}}}, slots: func(context.Context, SlotCell) (map[string]any, error) {
		calls++
		var hint any = "hint"
		if calls == 2 {
			hint = json.RawMessage(`"hint"`)
		}
		return map[string]any{"facade_hint": hint}, nil
	}}
	require.ErrorContains(t, p.postSlots(), "facade slot type")
	require.Equal(t, 2, calls)
}

func TestCatalogPackageNullRawDomainsAndSourceSlots(t *testing.T) {
	input, _ := completePackage(t, true)
	_, e := VerifyCatalogPackage(t.Context(), input, nil)
	require.ErrorContains(t, e, "source joint slot domain unavailable")
	_, e = VerifyCatalogPackage(t.Context(), input, func(context.Context, SlotCell) (map[string]any, error) {
		return map[string]any{"facade_hint": nil, "server_managed_argument_names": []string{}}, nil
	})
	require.ErrorContains(t, e, "facade slot type")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, e = VerifyCatalogPackage(cancelled, input, fixtureJointSlots)
	require.ErrorIs(t, e, ErrUnavailable)
	for _, raw := range [][]byte{[]byte(`{"z":null,"a":1e0,"v":"<>&"}`), []byte(`[null,true,1.00,"\u003c"]`), []byte("{\n\"é\":\"\u2028\",\"n\":-0.50e+1,\"a\":[]}"), []byte(`{"x":{"a":[1,2,{}]}}`)} {
		b, e := json.MarshalIndent(json.RawMessage(raw), "", "  ")
		require.NoError(t, e)
		require.Equal(t, len(b), renderedSize(raw))
	}
}
