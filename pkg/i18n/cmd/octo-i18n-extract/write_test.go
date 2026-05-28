package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGroupByPrefix(t *testing.T) {
	in := []Marker{
		{ID: "err.shared.auth.required", DefaultMessage: "a"},
		{ID: "err.server.thread.not_found", DefaultMessage: "b"},
		{ID: "msg.notify.invite", DefaultMessage: "c"},
		{ID: "err.shared.internal", DefaultMessage: "d"},
	}
	got, err := GroupByPrefix(in)
	if err != nil {
		t.Fatalf("GroupByPrefix: %v", err)
	}
	if n := len(got["shared"]); n != 2 {
		t.Errorf("shared group n=%d want 2", n)
	}
	if n := len(got["server"]); n != 2 {
		t.Errorf("server group n=%d want 2 (server + msg)", n)
	}
}

func TestGroupByPrefix_RejectsUnknown(t *testing.T) {
	_, err := GroupByPrefix([]Marker{{ID: "weird.foo", DefaultMessage: "x"}})
	if err == nil {
		t.Fatal("expected error for unknown prefix")
	}
}

func TestRenderTOML_StableAndIdempotent(t *testing.T) {
	ms := []Marker{
		{ID: "err.shared.b", DefaultMessage: "B with \"quotes\""},
		{ID: "err.shared.a", DefaultMessage: "A\nnewline"},
	}
	out1 := RenderTOML(ms)
	out2 := RenderTOML(ms)
	if out1 != out2 {
		t.Fatalf("RenderTOML not deterministic:\n--1--\n%s\n--2--\n%s", out1, out2)
	}
	// Sorted: "a" must appear before "b".
	if got := out1; !before(got, "err.shared.a", "err.shared.b") {
		t.Fatalf("expected sorted output, got:\n%s", got)
	}
	// Escape sanity: literal newline / quote must not survive raw.
	if contains(out1, "B with \"quotes\"") {
		t.Errorf("quotes not escaped:\n%s", out1)
	}
}

func TestWriteMarkerFile_IdempotentNoChange(t *testing.T) {
	dir := t.TempDir()
	ms := []Marker{{ID: "err.shared.x", DefaultMessage: "X"}}
	path := filepath.Join(dir, "active.en-US.toml")
	changed, err := WriteMarkerFile(path, ms)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if !changed {
		t.Errorf("first write should report changed=true")
	}
	changed, err = WriteMarkerFile(path, ms)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if changed {
		t.Errorf("second write should report changed=false (idempotent)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b) == 0 {
		t.Error("output file is empty")
	}
}

func before(s, a, b string) bool {
	i := indexOf(s, a)
	j := indexOf(s, b)
	return i >= 0 && j >= 0 && i < j
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }
