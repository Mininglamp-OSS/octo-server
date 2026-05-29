package main

import (
	"go/ast"
	"go/parser"
	"os"
	"path/filepath"
	"testing"
)

func parseExpr(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, err := parser.ParseExpr(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return e
}

// scanSrc writes src to a temp .go file and returns the violation count.
func scanSrc(t *testing.T, src string) int {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := scanRoots([]string{dir})
	if err != nil {
		t.Fatalf("scanRoots: %v", err)
	}
	return len(v)
}

func TestIsResponseErrorL(t *testing.T) {
	cases := map[string]bool{
		"httperr.ResponseErrorL": true,
		"ResponseErrorL":         true,
		"ResponseError":          false,
		"ResponseErrorf":         false,
		"foo.ResponseErrorL":     true,
	}
	for expr, want := range cases {
		if got := isResponseErrorL(parseExpr(t, expr)); got != want {
			t.Errorf("isResponseErrorL(%q) = %v, want %v", expr, got, want)
		}
	}
}

func TestIsCodeCompositeLit(t *testing.T) {
	cases := map[string]bool{
		`codes.Code{ID: "x"}`:        true,
		`Code{ID: "x"}`:              true,
		`&codes.Code{ID: "x"}`:       true,
		`(codes.Code{ID: "x"})`:      true, // parenthesized — must not slip the gate
		`(&codes.Code{ID: "x"})`:     true,
		"errcode.ErrUserStoreFailed": false,
		"code":                       false,
		"someStruct{A: 1}":           false,
	}
	for expr, want := range cases {
		if got := isCodeCompositeLit(parseExpr(t, expr)); got != want {
			t.Errorf("isCodeCompositeLit(%q) = %v, want %v", expr, got, want)
		}
	}
}

func TestScanRoots_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	// A _test.go file with an inline literal must be ignored (respond_test.go
	// legitimately exercises the runtime fallback this way).
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(`package x
func TestY() { httperr.ResponseErrorL(c, codes.Code{ID: "x"}, nil, nil) }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := scanRoots([]string{dir})
	if err != nil {
		t.Fatalf("scanRoots: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("violations=%d, want 0 (_test.go skipped)", len(v))
	}
}

func TestViolationDetection(t *testing.T) {
	t.Run("registered reference passes", func(t *testing.T) {
		if n := scanSrc(t, `package x
func h(c *Ctx) { httperr.ResponseErrorL(c, errcode.ErrUserStoreFailed, nil, nil) }
`); n != 0 {
			t.Fatalf("violations=%d, want 0 (registered ref)", n)
		}
	})

	t.Run("local var / param passes", func(t *testing.T) {
		if n := scanSrc(t, `package x
func h(c *Ctx, code codes.Code) { httperr.ResponseErrorL(c, code, nil, nil) }
`); n != 0 {
			t.Fatalf("violations=%d, want 0 (param ref)", n)
		}
	})

	t.Run("inline literal flagged", func(t *testing.T) {
		if n := scanSrc(t, `package x
func h(c *Ctx) { httperr.ResponseErrorL(c, codes.Code{ID: "err.server.foo"}, nil, nil) }
`); n != 1 {
			t.Fatalf("violations=%d, want 1 (inline literal)", n)
		}
	})

	t.Run("pointer inline literal flagged", func(t *testing.T) {
		if n := scanSrc(t, `package x
func h(c *Ctx) { httperr.ResponseErrorL(c, &codes.Code{ID: "x"}, nil, nil) }
`); n != 1 {
			t.Fatalf("violations=%d, want 1 (&codes.Code literal)", n)
		}
	})

	t.Run("parenthesized inline literal flagged", func(t *testing.T) {
		// PR #193 review: parens must not let a literal slip the gate.
		if n := scanSrc(t, `package x
func h(c *Ctx) { httperr.ResponseErrorL(c, (codes.Code{ID: "x"}), nil, nil) }
`); n != 1 {
			t.Fatalf("violations=%d, want 1 (parenthesized literal)", n)
		}
	})

	t.Run("unrelated call ignored", func(t *testing.T) {
		if n := scanSrc(t, `package x
func h(c *Ctx) { other.DoThing(c, codes.Code{ID: "x"}) }
`); n != 0 {
			t.Fatalf("violations=%d, want 0 (not ResponseErrorL)", n)
		}
	})
}
