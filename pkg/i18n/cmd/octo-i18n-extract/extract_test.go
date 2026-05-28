package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeFile is a tiny helper that creates a fake Go source under a temp dir
// and returns its path. Tests use this to build representative fixtures of
// the codes.Register / errcode.register call shapes that ExtractFromDir must
// recognize without compiling the fixtures.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestExtractFromDir_RegisterPatterns(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []Marker
	}{
		{
			name: "codes_Register direct",
			src: `package fake
import "net/http"
func _() {
	Register(Code{
		ID:             "err.shared.auth.required",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Please log in to continue.",
	})
}`,
			want: []Marker{{ID: "err.shared.auth.required", DefaultMessage: "Please log in to continue."}},
		},
		{
			name: "errcode_register wrapper + codes.Code",
			src: `package fake
import (
	"net/http"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)
var ErrX = register(codes.Code{
	ID:             "err.server.thread.not_found",
	HTTPStatus:     http.StatusNotFound,
	DefaultMessage: "Thread not found.",
})`,
			want: []Marker{{ID: "err.server.thread.not_found", DefaultMessage: "Thread not found."}},
		},
		{
			name: "multiple in one file",
			src: `package fake
func _() {
	Register(Code{ID: "err.shared.a", DefaultMessage: "A"})
	Register(Code{ID: "err.shared.b", DefaultMessage: "B"})
}`,
			want: []Marker{
				{ID: "err.shared.a", DefaultMessage: "A"},
				{ID: "err.shared.b", DefaultMessage: "B"},
			},
		},
		{
			name: "ignores non-Code composite literals",
			src: `package fake
type Other struct{ ID, DefaultMessage string }
func _() {
	Register(Other{ID: "ignored", DefaultMessage: "x"})
}`,
			want: nil,
		},
		{
			name: "ignores Register call without composite literal",
			src: `package fake
func _() {
	var c Code
	Register(c)
}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "fake.go", tc.src)
			got, err := ExtractFromDir(dir)
			if err != nil {
				t.Fatalf("ExtractFromDir: %v", err)
			}
			sort.Slice(got, func(i, j int) bool { return got[i].ID < got[j].ID })
			sort.Slice(tc.want, func(i, j int) bool { return tc.want[i].ID < tc.want[j].ID })
			if len(got) != len(tc.want) {
				t.Fatalf("len got=%d want=%d (got=%v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].ID != tc.want[i].ID || got[i].DefaultMessage != tc.want[i].DefaultMessage {
					t.Errorf("[%d] got=%+v want=%+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestExtractFromDir_RejectsNonStringLiterals(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.go", `package fake
const msg = "hi"
func _() {
	Register(Code{ID: "err.shared.x", DefaultMessage: msg})
}`)
	_, err := ExtractFromDir(dir)
	if err == nil {
		t.Fatalf("expected error for non-literal DefaultMessage, got nil")
	}
}

func TestExtractFromDir_RejectsDuplicateID(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "dup.go", `package fake
func _() {
	Register(Code{ID: "err.shared.dup", DefaultMessage: "a"})
	Register(Code{ID: "err.shared.dup", DefaultMessage: "b"})
}`)
	_, err := ExtractFromDir(dir)
	if err == nil {
		t.Fatalf("expected error for duplicate ID, got nil")
	}
}

func TestExtractFromDir_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "x_test.go", `package fake
func _() {
	Register(Code{ID: "err.shared.testonly", DefaultMessage: "T"})
}`)
	got, err := ExtractFromDir(dir)
	if err != nil {
		t.Fatalf("ExtractFromDir: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected test file to be skipped, got %v", got)
	}
}

// TestExtractFromDir_RejectsEmptyFields locks the strict-fail upgrade: a
// Code literal missing ID or DefaultMessage is a typo, not a deliberate
// skip signal, and must surface with a file:line position rather than
// silently disappearing into a recall-check mismatch later.
func TestExtractFromDir_RejectsEmptyFields(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "empty_id",
			src: `package fake
func _() {
	Register(Code{ID: "", DefaultMessage: "x"})
}`,
		},
		{
			name: "empty_default_message",
			src: `package fake
func _() {
	Register(Code{ID: "err.shared.x", DefaultMessage: ""})
}`,
		},
		{
			name: "id_only",
			src: `package fake
func _() {
	Register(Code{ID: "err.shared.x"})
}`,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "bad.go", tc.src)
			_, err := ExtractFromDir(dir)
			if err == nil {
				t.Fatal("expected strict-fail on empty field, got nil")
			}
		})
	}
}

// TestExtractFromDir_SkipsHiddenAndUnderscoreDirs codifies the convention
// that test fixtures dropped under `.hidden/` or `_fixtures/` are off-limits
// to extraction (matching `go build`'s dir-skip behavior). Without this
// fixture the convention is only described in a comment and could regress.
func TestExtractFromDir_SkipsHiddenAndUnderscoreDirs(t *testing.T) {
	dir := t.TempDir()
	// Real source under root — should be extracted.
	writeFile(t, dir, "real.go", `package fake
func _() {
	Register(Code{ID: "err.shared.real", DefaultMessage: "R"})
}`)
	for _, sub := range []string{"_fixtures", ".hidden"} {
		subDir := filepath.Join(dir, sub)
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", subDir, err)
		}
		writeFile(t, subDir, "leak.go", `package fake
func _() {
	Register(Code{ID: "err.shared.LEAK_`+sub+`", DefaultMessage: "leak"})
}`)
	}
	got, err := ExtractFromDir(dir)
	if err != nil {
		t.Fatalf("ExtractFromDir: %v", err)
	}
	if len(got) != 1 || got[0].ID != "err.shared.real" {
		t.Fatalf("expected only real.go to extract, got %v", got)
	}
}
