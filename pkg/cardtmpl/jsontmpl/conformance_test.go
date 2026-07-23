package jsontmpl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestConformance_ReasoningGoldens is the engine's byte-equivalence oracle: for
// every sample in the ai.reasoning-process handoff, expand the state's view
// template and assert the result matches the shipped golden (canonical JSON —
// sorted keys — since key order is not semantically meaningful and Go maps are
// unordered). This proves the ACT subset the engine implements reproduces the
// authoring tool's compiled output. Escaping is identity here: the goldens bind
// data literally (decision D6; cardmsg.Validate is the security backstop at the
// Registry layer, not markdown-escaping in the engine).
func TestConformance_ReasoningGoldens(t *testing.T) {
	root := filepath.Join("testdata", "ai.reasoning-process@0.1.0")

	// state → view is read from the manifest (views[].states + views[].template),
	// never guessed from sample file names (brief acceptance D1).
	stateToView, viewToTemplate := loadViewMaps(t, filepath.Join(root, "manifest.json"))

	// sample file name == golden file name in this handoff.
	samples := []string{"reasoning", "answering", "completed", "stopped", "error"}

	for _, name := range samples {
		t.Run(name, func(t *testing.T) {
			data := readObject(t, filepath.Join(root, "samples", name+".json"))
			state, _ := data["state"].(string)
			view, ok := stateToView[state]
			if !ok {
				t.Fatalf("manifest declares no view for state %q", state)
			}
			tmplPath, ok := viewToTemplate[view]
			if !ok || tmplPath == "" {
				t.Fatalf("manifest view %q declares no template", view)
			}
			tmpl := readJSON(t, filepath.Join(root, tmplPath))

			got, err := Expand(tmpl, Scope{Data: data}, func(s string) string { return s })
			if err != nil {
				t.Fatalf("expand %s (view %s): %v", name, view, err)
			}
			golden := readJSON(t, filepath.Join(root, "goldens", name+".card.json"))

			if g, w := canonJSON(t, got), canonJSON(t, golden); g != w {
				t.Fatalf("golden mismatch for %s (view %s)\n--- got ---\n%s\n--- want ---\n%s", name, view, g, w)
			}
		})
	}
}

// loadViewMaps reads manifest.views into state→view and view→templatePath maps.
func loadViewMaps(t *testing.T, manifestPath string) (map[string]string, map[string]string) {
	t.Helper()
	m := readObject(t, manifestPath)
	views, ok := m["views"].(map[string]any)
	if !ok {
		t.Fatalf("manifest has no views object")
	}
	stateToView := map[string]string{}
	viewToTemplate := map[string]string{}
	for view, raw := range views {
		v, _ := raw.(map[string]any)
		if tpl, _ := v["template"].(string); tpl != "" {
			viewToTemplate[view] = tpl
		}
		states, _ := v["states"].([]any)
		for _, s := range states {
			if st, ok := s.(string); ok {
				stateToView[st] = view
			}
		}
	}
	if len(stateToView) == 0 {
		t.Fatalf("manifest views declare no states")
	}
	return stateToView, viewToTemplate
}

func readJSON(t *testing.T, path string) any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return v
}

func readObject(t *testing.T, path string) map[string]any {
	t.Helper()
	v := readJSON(t, path)
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not a JSON object", path)
	}
	return m
}

// canonJSON marshals with sorted keys (recursively) so comparison is order-
// independent.
func canonJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(sortKeys(v), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func sortKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		ks := make([]string, 0, len(x))
		for k := range x {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		m := make(map[string]any, len(x))
		for _, k := range ks {
			m[k] = sortKeys(x[k])
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = sortKeys(x[i])
		}
		return out
	default:
		return v
	}
}
