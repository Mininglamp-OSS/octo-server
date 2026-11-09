package permissionbundle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func packageSource(input PackageInput) SupportedSource {
	return SupportedSource{Pin: input.Pin, Format: CatalogPackageV2, Files: input.Files}
}

func repackedFixture(t *testing.T, input PackageInput, version string) PackageInput {
	t.Helper()
	files := map[string][]byte{}
	for path, raw := range input.Files {
		files[path] = append([]byte{}, raw...)
	}
	next := PackageInput{Files: files}
	blob := files["blobs/0.bin"]
	cut := len(blob)
	for path, raw := range files {
		if !strings.HasPrefix(path, "shared/") {
			continue
		}
		m := fixtureObject(t, raw)
		for _, value := range m["roots"].([]any) {
			for _, value := range value.(map[string]any)["parts"].([]any) {
				n, e := strconv.Atoi(string(value.(map[string]any)["offset"].(json.Number)))
				require.NoError(t, e)
				if n >= len(blob)/2 && n < cut {
					cut = n
				}
			}
		}
	}
	require.Greater(t, cut, 0)
	require.Less(t, cut, len(blob))
	files["blobs/0.bin"] = blob[:cut]
	files["blobs/1.bin"] = blob[cut:]
	for path := range files {
		if !strings.HasPrefix(path, "shared/") {
			continue
		}
		changeFixture(t, &next, path, func(m map[string]any) {
			for _, value := range m["roots"].([]any) {
				for _, value := range value.(map[string]any)["parts"].([]any) {
					part := value.(map[string]any)
					n, e := strconv.Atoi(string(part["offset"].(json.Number)))
					require.NoError(t, e)
					if n >= cut {
						part["path"] = "blobs/1.bin"
						part["offset"] = n - cut
					}
				}
			}
		})
	}
	m := fixtureObject(t, files["manifest.json"])
	m["catalog_version"] = version
	listed := []any{}
	for _, path := range sortedByteKeys(files) {
		if path == "manifest.json" {
			continue
		}
		kind, _, _, ok := packagePath(path)
		require.True(t, ok)
		listed = append(listed, map[string]any{"path": path, "kind": kind, "bytes": len(files[path]), "sha256": independentHash(files[path])})
	}
	m["files"] = listed
	files["manifest.json"] = fixtureJSON(t, m)
	next.Pin = CatalogPin{version, independentHash(files["manifest.json"])}
	return next
}
func sortedByteKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Independent test emitter of a real v1 grammar instance. It reads the inert
// fixture files, not VerifiedCatalog or the production v1 normalizer.
func fixtureArtifactV1(t *testing.T, input PackageInput) SupportedSource {
	t.Helper()
	manifest := fixtureObject(t, input.Files["manifest.json"])
	index := fixtureObject(t, input.Files["index.json"])
	roots, rows, content, entries := map[string][]byte{}, map[string]map[string]any{}, map[string]map[string]any{}, map[string]map[string]any{}
	for path, raw := range input.Files {
		if strings.HasPrefix(path, "shared/") {
			m := fixtureObject(t, raw)
			for _, value := range m["roots"].([]any) {
				r := value.(map[string]any)
				b := []byte{}
				for _, value := range r["parts"].([]any) {
					p := value.(map[string]any)
					offset, _ := strconv.Atoi(string(p["offset"].(json.Number)))
					length, _ := strconv.Atoi(string(p["length"].(json.Number)))
					b = append(b, input.Files[p["path"].(string)][offset:offset+length]...)
				}
				roots[r["sha256"].(string)] = b
			}
			for _, value := range m["rows"].([]any) {
				r := value.(map[string]any)
				rows[r["sha256"].(string)] = r
			}
		}
		if strings.HasPrefix(path, "modules/") {
			m := fixtureObject(t, raw)
			for _, value := range m["content"].([]any) {
				c := value.(map[string]any)
				h := ""
				if c["kind"] == "schema_variant" {
					h = independentHash(fixtureJSON(t, c))
				} else {
					h = c["sha256"].(string)
				}
				content[h] = c
			}
			for _, value := range m["entries"].([]any) {
				e := value.(map[string]any)
				entries[e["id"].(string)] = e
			}
		}
	}
	old, remap := map[string]map[string]any{}, map[string]string{}
	put := func(original string, c map[string]any) {
		var h string
		if c["kind"] == "bytes" {
			h = c["sha256"].(string)
		} else {
			delete(c, "sha256")
			h = independentHash(fixtureJSON(t, c))
			if c["kind"] != "schema_variant" {
				c["sha256"] = h
			}
		}
		old[h] = c
		remap[original] = h
	}
	for id, c := range content {
		if c["kind"] == "bytes" {
			ref := c["data_ref"].(string)
			delete(c, "data_ref")
			c["data_base64"] = base64.StdEncoding.EncodeToString(roots[ref])
			put(id, c)
		} else if c["kind"] == "template" {
			ref := c["value_ref"].(string)
			delete(c, "value_ref")
			c["value_base64"] = base64.StdEncoding.EncodeToString(roots[ref])
			put(id, c)
		}
	}
	for id, c := range content {
		switch c["kind"] {
		case "schema_variant":
			delete(c, "selector")
			c["template_hash"] = remap[c["template_hash"].(string)]
			c["parameter_truth_hash"] = remap[c["parameter_truth_hash"].(string)]
			put(id, c)
		case "discovery_index":
			delete(c, "selector")
			rs := []any{}
			for _, ref := range c["rows"].([]any) {
				r := rows[ref.(string)]
				rs = append(rs, map[string]any{"operation_id": r["operation_id"], "match_text": r["match_text"], "row_base64": base64.StdEncoding.EncodeToString(roots[r["row_ref"].(string)])})
			}
			c["rows"] = rs
			put(id, c)
		}
	}
	es := []any{}
	for _, id := range sortedObjectKeys(entries) {
		e := entries[id]
		refs := []string{}
		for _, ref := range e["content_refs"].([]any) {
			refs = append(refs, remap[ref.(string)])
		}
		sort.Strings(refs)
		e["content_refs"] = refs
		es = append(es, e)
	}
	cs := []any{}
	for _, id := range sortedObjectKeys(old) {
		cs = append(cs, old[id])
	}
	manifest["format"] = CatalogArtifactV1
	manifest["contract_revision"] = 2
	manifest["catalog_version"] = "synthetic-complete-artifact-v1"
	delete(manifest, "files")
	delete(manifest["limits"].(map[string]any), "packaging")
	manifest["entries"] = es
	manifest["content"] = cs
	for _, k := range []string{"profiles", "source_files", "exceptions"} {
		manifest[k] = index[k]
	}
	b := fixtureJSON(t, manifest)
	return SupportedSource{Pin: CatalogPin{manifest["catalog_version"].(string), independentHash(b)}, Format: CatalogArtifactV1, Artifact: b}
}

func TestCatalogRegistryAtomicSemanticPairsAndLegacyReader(t *testing.T) {
	input, _ := completePackage(t, false)
	repacked := repackedFixture(t, input, "synthetic-repacked-v2")
	a, e := VerifyCatalogPackage(t.Context(), input, nil)
	require.NoError(t, e)
	b, e := VerifyCatalogPackage(t.Context(), repacked, nil)
	require.NoError(t, e)
	require.True(t, semanticEqual(a, b))
	legacy := fixtureArtifactV1(t, input)
	r, e := NewCatalogRegistry(t.Context(), []SupportedSource{legacy, packageSource(input), packageSource(repacked)}, []CatalogTransition{{legacy.Pin, input.Pin}, {input.Pin, repacked.Pin}, {repacked.Pin, input.Pin}}, nil)
	require.NoError(t, e)
	format, e := r.CatalogFormat(t.Context(), legacy.Pin)
	require.NoError(t, e)
	require.Equal(t, CatalogArtifactV1, format)
	format, e = r.CatalogFormat(t.Context(), input.Pin)
	require.NoError(t, e)
	require.Equal(t, CatalogPackageV2, format)
	require.ErrorIs(t, r.Compatible(t.Context(), input.Pin, legacy.Pin), ErrUnavailable)
	oldState := r.current.Load()
	bad := repackedFixture(t, input, "synthetic-failed-final-file")
	bad.Files["shared/00.json"][0] = '!'
	require.Error(t, r.Replace(t.Context(), []SupportedSource{legacy, packageSource(input), packageSource(bad)}, nil, nil))
	require.Same(t, oldState, r.current.Load())
	changed := repackedFixture(t, input, "synthetic-changed-meaning")
	changeFixture(t, &changed, "index.json", func(m map[string]any) {
		for _, value := range m["profiles"].([]any) {
			p := value.(map[string]any)
			if p["name"] == "leaf" {
				p["prepare"].([]any)[0].(map[string]any)["condition"].(map[string]any)["literal"] = "changed"
			}
		}
	})
	rebindFixture(t, &changed)
	e = r.Replace(t.Context(), []SupportedSource{packageSource(input), packageSource(changed)}, []CatalogTransition{{input.Pin, changed.Pin}}, nil)
	require.ErrorContains(t, e, "directed pair changes semantic view")
	require.Same(t, oldState, r.current.Load())
	// Readers never observe the partially verified replacement.
	var wg sync.WaitGroup
	errors := make(chan error, 80)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				loaded, e := r.Load(context.Background(), input.Pin)
				if e != nil {
					errors <- e
				} else if loaded.Pin != input.Pin || independentHash(loaded.Bytes) != input.Pin.Digest {
					errors <- ErrUnavailable
				}
			}
		}()
	}
	require.NoError(t, r.Replace(t.Context(), []SupportedSource{packageSource(input), packageSource(repacked)}, []CatalogTransition{{input.Pin, repacked.Pin}}, nil))
	wg.Wait()
	close(errors)
	for e := range errors {
		require.NoError(t, e)
	}
	_, e = r.Load(t.Context(), legacy.Pin)
	require.ErrorIs(t, e, ErrUnavailable, "removed pin never falls back to last good")
}

func TestCatalogDirectoryRejectsAliasesAndSnapshotsExactFiles(t *testing.T) {
	input, _ := completePackage(t, false)
	for _, name := range []string{"valid", "symlink", "hardlink", "extra_file", "empty_directory"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for path, b := range input.Files {
				full := filepath.Join(root, filepath.FromSlash(path))
				require.NoError(t, os.MkdirAll(filepath.Dir(full), 0700))
				require.NoError(t, os.WriteFile(full, b, 0600))
			}
			file := filepath.Join(root, "index.json")
			switch name {
			case "symlink":
				require.NoError(t, os.Rename(file, filepath.Join(t.TempDir(), "index.json")))
				require.NoError(t, os.Symlink("manifest.json", file))
			case "hardlink":
				require.NoError(t, os.Link(file, filepath.Join(t.TempDir(), "alias")))
			case "extra_file":
				require.NoError(t, os.WriteFile(filepath.Join(root, "extra"), []byte("x"), 0600))
			case "empty_directory":
				require.NoError(t, os.Mkdir(filepath.Join(root, "shared-extra"), 0700))
			}
			got, e := ReadCatalogDirectory(t.Context(), input.Pin, root)
			if name != "valid" {
				require.Error(t, e)
				return
			}
			require.NoError(t, e)
			v, e := VerifyCatalogPackage(t.Context(), got, nil)
			require.NoError(t, e)
			require.Equal(t, input.Pin, v.Pin())
			require.NoError(t, os.WriteFile(file, []byte("changed"), 0600))
			b, _ := v.File("index.json")
			require.Equal(t, input.Files["index.json"], b)
		})
	}
}
