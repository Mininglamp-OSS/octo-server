package user

import (
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionTokenWritersUseSessionStore(t *testing.T) {
	root := tokenWriterRepoRoot(t)
	authPackage := filepath.Join(root, "pkg", "auth")
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".context", "vendor", "node_modules":
				return filepath.SkipDir
			}
			if path == authPackage {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
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
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "Set", "SetAndExpire", "SetNX", "Persist", "Expire", "ExpireAt":
		default:
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

func tokenWriterRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test directory")
		}
		dir = parent
	}
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
