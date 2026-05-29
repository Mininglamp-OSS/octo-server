// Command lint-unregistered-code is the 0.10 #3 gate guarding
// httperr.ResponseErrorL against codes that bypass the pkg/i18n/codes
// registry.
//
// Contract: ResponseErrorL's second argument (the codes.Code) MUST be a
// reference to a registered code — i.e. an errcode.ErrXxx selector, a shared
// code var looked up at init, a local variable, or a function parameter that
// ultimately traces to one of those. The ONE shape it must never be is an
// inline composite literal:
//
//	httperr.ResponseErrorL(c, codes.Code{ID: "err.server.foo"}, nil, nil)  // BANNED
//
// An inline codes.Code{...} is a code that was never passed through
// codes.Register, so:
//   - the 0.8 AST extractor never emits a TOML marker for it (no translation),
//   - pkg/httperr/respond.go's Lookup fails and silently downgrades it to
//     err.shared.internal at runtime.
//
// Catching the literal at CI time turns that silent runtime downgrade into a
// loud compile-time failure, which is the whole point of the registry.
//
// Scope / limitation: this is a single-file AST check, not a type/dataflow
// analysis. A two-step smuggle (`x := codes.Code{...}; ResponseErrorL(c, x,
// …)`) is not flagged here — that residual case is still caught at runtime by
// respond.go's fallback+log and by the 0.8 recall check (every registered code
// must have a marker). The inline literal is the common, test-demonstrated
// vector and the one worth a hard gate.
//
// Usage:
//
//	go run ./tools/lint-unregistered-code [root...]
//
// Defaults to ./modules and ./pkg. Test files are skipped — respond_test.go
// deliberately constructs an inline literal to exercise the runtime fallback.
//
// Exit codes: 0 (clean), 1 (violations), 2 (walk error).
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const respondFunc = "ResponseErrorL"

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"modules", "pkg"}
	}

	violations, err := scanRoots(roots)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		for _, v := range violations {
			fmt.Println(v)
		}
		fmt.Printf("\nFound %d inline error code(s) bypassing the registry.\n", len(violations))
		fmt.Println("Every code must be registered via register() in pkg/errcode (or codes.Register)")
		fmt.Println("so the AST extractor emits a translation marker and the renderer can localize it.")
		fmt.Println("Replace the inline literal with a registered errcode.ErrXxx reference.")
		os.Exit(1)
	}
	fmt.Println("OK: no inline codes passed to ResponseErrorL.")
}

// scanRoots walks the roots and returns one violation message per inline
// codes.Code{...} literal passed as the ResponseErrorL code argument.
func scanRoots(roots []string) ([]string, error) {
	fset := token.NewFileSet()
	var violations []string
	for _, root := range roots {
		walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if name := info.Name(); name == "vendor" || name == "tools" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			violations = append(violations, violationsInFile(fset, f)...)
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk %q: %w", root, walkErr)
		}
	}
	return violations, nil
}

// violationsInFile inspects one parsed file for inline-literal codes passed to
// ResponseErrorL.
func violationsInFile(fset *token.FileSet, f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isResponseErrorL(call.Fun) {
			return true
		}
		// Signature: ResponseErrorL(c, code, params, details). The code is
		// the second positional argument.
		if len(call.Args) < 2 {
			return true
		}
		if isCodeCompositeLit(call.Args[1]) {
			pos := fset.Position(call.Args[1].Pos())
			out = append(out,
				fmt.Sprintf("%s:%d: inline codes.Code{...} passed to %s — register it in pkg/errcode and pass the var",
					filepath.ToSlash(pos.Filename), pos.Line, respondFunc))
		}
		return true
	})
	return out
}

// isResponseErrorL matches both httperr.ResponseErrorL(...) and a bare
// ResponseErrorL(...) (in case a future caller dot-imports or aliases).
func isResponseErrorL(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == respondFunc
	case *ast.Ident:
		return t.Name == respondFunc
	}
	return false
}

// isCodeCompositeLit reports whether the expression is an inline composite
// literal of type Code / codes.Code, unwrapping the equivalent forms a caller
// might use to express the same literal:
//   - &codes.Code{...}      (*ast.UnaryExpr, address-of)
//   - (codes.Code{...})     (*ast.ParenExpr, parenthesized — would otherwise
//                            slip the gate, PR #193 review)
func isCodeCompositeLit(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.ParenExpr:
		return isCodeCompositeLit(t.X)
	case *ast.UnaryExpr:
		return isCodeCompositeLit(t.X)
	case *ast.CompositeLit:
		return isCodeType(t.Type)
	}
	return false
}

// isCodeType matches any type named Code (bare Code or pkg.Code). The match is
// intentionally loose on the package qualifier: at the only call site
// (ResponseErrorL's second argument, whose declared type is codes.Code) a
// composite literal named Code can only be codes.Code, so a false positive is
// not reachable in practice.
func isCodeType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "Code"
	case *ast.Ident:
		return t.Name == "Code"
	}
	return false
}
