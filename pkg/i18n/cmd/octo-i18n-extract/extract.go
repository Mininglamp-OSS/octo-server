package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Marker is the minimal go-i18n message marker emitted per registered Code.
// We only carry ID + DefaultMessage because the AST extractor is strictly a
// source-of-truth dump for translators (D18); HTTPStatus / SafeDetailKeys /
// Internal don't belong in the runtime TOML.
type Marker struct {
	// ID is the stable i18n key (e.g. "err.shared.auth.required").
	ID string
	// DefaultMessage is the en-US source text the marker exposes as `other`.
	DefaultMessage string
	// Pos is the file:line where the Register call was found; used purely
	// for human-readable error messages on duplicate / non-literal failures.
	Pos string
}

// recognizedFuncNames is the closed set of call expressions the extractor
// treats as a Code registration. Both shapes appear in-repo today:
//
//   - pkg/i18n/codes/shared.go uses package-local `Register(Code{...})`
//   - pkg/errcode/server.go uses a wrapper `register(codes.Code{...})`
//
// Adding a new wrapper requires updating this set AND adding a test fixture;
// silently accepting any *register* name would let typos pass through.
var recognizedFuncNames = map[string]struct{}{
	"Register": {},
	"register": {},
}

// ExtractFromDir walks `root` and extracts a Marker for every Code{...}
// composite literal passed to one of recognizedFuncNames.
//
// Strictness rationale (these conditions fail the run, not warn):
//   - ID / DefaultMessage MUST be basic string literals; computed values
//     defeat the static guarantee that markers match codes.Register input.
//   - Duplicate IDs MUST fail; downstream goi18n merge produces undefined
//     output on collisions.
//
// Test files (`*_test.go`) and directories whose name begins with `.` or `_`
// are skipped (the latter mirrors `go build`'s convention so test fixtures
// under `_fixtures/` don't pollute the marker set).
func ExtractFromDir(root string) ([]Marker, error) {
	fset := token.NewFileSet()
	var markers []Marker

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		fileMarkers, err := extractFromFile(fset, file)
		if err != nil {
			return err
		}
		markers = append(markers, fileMarkers...)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	if err := checkDuplicates(markers); err != nil {
		return nil, err
	}

	sort.SliceStable(markers, func(i, j int) bool { return markers[i].ID < markers[j].ID })
	return markers, nil
}

func extractFromFile(fset *token.FileSet, file *ast.File) ([]Marker, error) {
	var (
		out  []Marker
		ferr error
	)
	ast.Inspect(file, func(n ast.Node) bool {
		// Once ferr is set we stop creating new state, but ast.Inspect's
		// `return false` only skips the current subtree — siblings still
		// trigger the visitor, which re-checks ferr and short-circuits. Net
		// effect: first error wins, no further work happens, traversal walks
		// the rest of the file harmlessly. Acceptable for tool-binary use.
		if ferr != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isRecognizedRegisterCall(call) || len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.CompositeLit)
		if !ok || !isCodeType(lit.Type) {
			return true
		}
		id, msg, err := readCodeLiteral(lit)
		if err != nil {
			ferr = fmt.Errorf("%s: %w", fset.Position(call.Pos()), err)
			return false
		}
		// Missing ID or DefaultMessage is a hard error, not a silent skip.
		// Register() also panics at runtime, but relying on that means a
		// typo'd Code literal disappears from the marker set and surfaces
		// only as a recall-check mismatch in main.go with no source
		// position. Failing here matches the rest of this file's strictness
		// rationale and gives reviewers an actionable file:line.
		if id == "" || msg == "" {
			ferr = fmt.Errorf("%s: Code literal is missing ID or DefaultMessage; both are required for a marker", fset.Position(call.Pos()))
			return false
		}
		out = append(out, Marker{
			ID:             id,
			DefaultMessage: msg,
			Pos:            fset.Position(call.Pos()).String(),
		})
		return true
	})
	return out, ferr
}

func isRecognizedRegisterCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		_, ok := recognizedFuncNames[fn.Name]
		return ok
	case *ast.SelectorExpr:
		_, ok := recognizedFuncNames[fn.Sel.Name]
		return ok
	}
	return false
}

// isCodeType returns true when the composite literal's type is `Code` or a
// selector ending in `.Code`. Anything else (e.g. `Other{}` inside a fake
// Register call) is ignored — false positives would leak unrelated structs
// into the marker output.
func isCodeType(t ast.Expr) bool {
	switch tt := t.(type) {
	case *ast.Ident:
		return tt.Name == "Code"
	case *ast.SelectorExpr:
		return tt.Sel.Name == "Code"
	}
	return false
}

func readCodeLiteral(lit *ast.CompositeLit) (id, msg string, err error) {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "ID":
			s, ok := basicStringLit(kv.Value)
			if !ok {
				return "", "", fmt.Errorf("Code.ID must be a string literal")
			}
			id = s
		case "DefaultMessage":
			s, ok := basicStringLit(kv.Value)
			if !ok {
				return "", "", fmt.Errorf("Code.DefaultMessage must be a string literal (id=%q)", id)
			}
			msg = s
		}
	}
	return id, msg, nil
}

func basicStringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func checkDuplicates(ms []Marker) error {
	seen := make(map[string]string, len(ms))
	for _, m := range ms {
		if prev, ok := seen[m.ID]; ok {
			return fmt.Errorf("duplicate Code.ID %q registered at %s and %s", m.ID, prev, m.Pos)
		}
		seen[m.ID] = m.Pos
	}
	return nil
}
