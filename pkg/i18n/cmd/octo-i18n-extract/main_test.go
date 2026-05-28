package main

// Unit coverage for the load-bearing safety properties in main.go that the
// initial PR only exercised indirectly via `make i18n-extract`. Reviewer
// feedback on PR #186 (yujiawei P2) called out that verifyRecall,
// checkOnDisk, and collectMarkers' cross-root duplicate path deserve
// direct tests because they are what makes the 100% recall guarantee
// enforceable rather than aspirational.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyRecall(t *testing.T) {
	cases := []struct {
		name       string
		markers    []Marker
		registered []string
		wantErr    bool
		wantSubstr []string // each substring must appear in the error message
	}{
		{
			name:       "both_empty",
			markers:    nil,
			registered: nil,
			wantErr:    false,
		},
		{
			name:       "exact_match",
			markers:    []Marker{{ID: "err.shared.a"}, {ID: "err.shared.b"}},
			registered: []string{"err.shared.b", "err.shared.a"},
			wantErr:    false,
		},
		{
			name:       "missing_from_ast",
			markers:    []Marker{{ID: "err.shared.a"}},
			registered: []string{"err.shared.a", "err.shared.b"},
			wantErr:    true,
			wantSubstr: []string{"missing-from-AST", "err.shared.b"},
		},
		{
			name:       "extra_in_ast",
			markers:    []Marker{{ID: "err.shared.a"}, {ID: "err.shared.z"}},
			registered: []string{"err.shared.a"},
			wantErr:    true,
			wantSubstr: []string{"extra-only-in-AST", "err.shared.z"},
		},
		{
			name:       "both_missing_and_extra",
			markers:    []Marker{{ID: "err.shared.a"}, {ID: "err.shared.z"}},
			registered: []string{"err.shared.a", "err.shared.b"},
			wantErr:    true,
			wantSubstr: []string{"missing-from-AST", "err.shared.b", "extra-only-in-AST", "err.shared.z"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := verifyRecall(tc.markers, tc.registered)
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifyRecall err=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			for _, s := range tc.wantSubstr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error message missing %q\n  full: %s", s, err.Error())
				}
			}
		})
	}
}

func TestCheckOnDisk(t *testing.T) {
	dir := t.TempDir()
	ms := []Marker{{ID: "err.shared.x", DefaultMessage: "X"}}

	// State 1: file absent → diff
	absent := filepath.Join(dir, "absent.toml")
	diff, err := checkOnDisk(absent, ms)
	if err != nil {
		t.Fatalf("checkOnDisk(absent): %v", err)
	}
	if !diff {
		t.Error("absent file must report diff=true")
	}

	// State 2: file matches → no diff
	matching := filepath.Join(dir, "match.toml")
	if err := os.WriteFile(matching, []byte(RenderTOML(ms)), 0o644); err != nil {
		t.Fatalf("seed match file: %v", err)
	}
	diff, err = checkOnDisk(matching, ms)
	if err != nil {
		t.Fatalf("checkOnDisk(match): %v", err)
	}
	if diff {
		t.Error("matching file must report diff=false")
	}

	// State 3: file differs → diff
	stale := filepath.Join(dir, "stale.toml")
	if err := os.WriteFile(stale, []byte("# stale unrelated content\n"), 0o644); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	diff, err = checkOnDisk(stale, ms)
	if err != nil {
		t.Fatalf("checkOnDisk(stale): %v", err)
	}
	if !diff {
		t.Error("stale file must report diff=true")
	}
}

func TestCollectMarkers_CrossRootDuplicateID(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	writeFile(t, root1, "a.go", `package fake
func _() {
	Register(Code{ID: "err.shared.dup", DefaultMessage: "from root1"})
}`)
	writeFile(t, root2, "b.go", `package fake
func _() {
	Register(Code{ID: "err.shared.dup", DefaultMessage: "from root2"})
}`)

	_, err := collectMarkers([]string{root1, root2})
	if err == nil {
		t.Fatal("expected cross-root duplicate to fail, got nil")
	}
	// Error message must point at both occurrences so reviewers can locate them.
	for _, want := range []string{"err.shared.dup", root1, root2} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("cross-root error missing %q\n  full: %s", want, err.Error())
		}
	}
}

func TestCollectMarkers_MergesDistinctRoots(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()
	writeFile(t, root1, "a.go", `package fake
func _() {
	Register(Code{ID: "err.shared.a", DefaultMessage: "A"})
}`)
	writeFile(t, root2, "b.go", `package fake
func _() {
	Register(Code{ID: "err.server.b", DefaultMessage: "B"})
}`)

	ms, err := collectMarkers([]string{root1, root2})
	if err != nil {
		t.Fatalf("collectMarkers: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("len=%d want 2", len(ms))
	}
	// Sorted output is part of the contract — main.go relies on it for
	// stable downstream rendering and recall diffs.
	if ms[0].ID >= ms[1].ID {
		t.Errorf("collectMarkers output not sorted: %v", ms)
	}
}
