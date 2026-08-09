package user

import (
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionTokenWritersUseSessionStore(t *testing.T) {
	var violations []string
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || filepath.Ext(path) == ".test" || filepath.Base(path) == "token_writer_guard_test.go" {
			return nil
		}
		if len(path) >= len("_test.go") && path[len(path)-len("_test.go"):] == "_test.go" {
			return nil
		}
		found, err := directTokenWriterViolations(path)
		if err != nil {
			return err
		}
		violations = append(violations, found...)
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, violations, "production token cache writes must go through pkg/auth Session Store")
}

func TestDirectTokenWriterGuardHandlesFormattingAndRenames(t *testing.T) {
	source := `package fixture
func write(ctx Context, renamed string) {
	ctx.Cache().SetAndExpire(
		ctx.GetConfig().Cache.TokenCachePrefix + renamed,
		"payload",
		minute,
	)
}`
	fset := gotoken.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, 0)
	require.NoError(t, err)
	require.Len(t, tokenWriterViolations(fset, file, "fixture.go"), 1)
}

func TestDirectTokenWriterGuardRejectsDeadlineMutationMethods(t *testing.T) {
	for _, method := range []string{"Persist", "Expire", "ExpireAt"} {
		t.Run(method, func(t *testing.T) {
			source := fmt.Sprintf(`package fixture
func write(ctx Context, renamed string) {
	ctx.Cache().%s(ctx.GetConfig().Cache.TokenCachePrefix + renamed, deadline)
}`, method)
			fset := gotoken.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", source, 0)
			require.NoError(t, err)
			require.Len(t, tokenWriterViolations(fset, file, "fixture.go"), 1)
		})
	}
}

func directTokenWriterViolations(path string) ([]string, error) {
	fset := gotoken.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	return tokenWriterViolations(fset, file, path), nil
}

func tokenWriterViolations(fset *gotoken.FileSet, file *ast.File, path string) []string {
	var violations []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (selector.Sel.Name != "Set" && selector.Sel.Name != "SetAndExpire" && selector.Sel.Name != "SetNX") {
			return true
		}
		if expressionContainsTokenPrefix(call.Args[0]) {
			position := fset.Position(call.Pos())
			violations = append(violations, fmt.Sprintf("%s:%d calls %s with a token cache prefix", path, position.Line, selector.Sel.Name))
		}
		return true
	})
	return violations
}

func expressionContainsTokenPrefix(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && (selector.Sel.Name == "TokenCachePrefix" || selector.Sel.Name == "UIDTokenCachePrefix") {
			found = true
			return false
		}
		return !found
	})
	return found
}
