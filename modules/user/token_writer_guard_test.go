package user

import (
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

func TestDirectTokenWriterGuardRejectsCredentialDeletionMethods(t *testing.T) {
	for _, method := range []string{"Del", "Delete", "Unlink"} {
		t.Run(method, func(t *testing.T) {
			source := fmt.Sprintf(`package fixture
func remove(ctx Context, renamed string) {
	ctx.Cache().%s(ctx.GetConfig().Cache.TokenCachePrefix + renamed)
	ctx.Cache().%s(ctx.GetConfig().Cache.UIDTokenCachePrefix + renamed)
}`, method, method)
			fset := gotoken.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", source, 0)
			require.NoError(t, err)
			require.Len(t, tokenWriterViolations(fset, file, "fixture.go"), 2)
		})
	}
}

func TestDirectTokenWriterGuardRejectsSessionSecurityNamespaceMutations(t *testing.T) {
	tests := []struct {
		method string
		key    string
	}{
		{method: "Set", key: `"opaque-prefix:auth:generation:" + subject`},
		{method: "ZAdd", key: `"opaque-prefix:auth:sessions:" + subject`},
		{method: "Del", key: `"opaque-prefix:auth:legacy-deny:" + subject`},
		{method: "Expire", key: `"opaque-prefix:auth:rollout-control"`},
		{method: "SetNX", key: `"opaque-prefix:auth:migration:lock"`},
	}
	for _, tt := range tests {
		t.Run(tt.method+"_"+tt.key, func(t *testing.T) {
			source := fmt.Sprintf(`package fixture
func mutate(client Client, subject string) {
	client.%s(%s, value)
}`, tt.method, tt.key)
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
		method := selector.Sel.Name
		credentialWrite := isCredentialWriteMethod(method) && expressionContainsTokenPrefix(call.Args[0])
		securityStateMutation := isSecurityStateMutationMethod(method) && expressionContainsSessionSecurityNamespace(call.Args[0])
		if !credentialWrite && !securityStateMutation {
			return true
		}
		position := fset.Position(call.Pos())
		violations = append(violations, fmt.Sprintf("%s:%d calls %s with a token session security key", path, position.Line, method))
		return true
	})
	return violations
}

func isCredentialWriteMethod(method string) bool {
	switch method {
	case "Set", "SetAndExpire", "SetNX", "Persist", "Expire", "ExpireAt":
		return true
	default:
		return false
	}
}

func isSecurityStateMutationMethod(method string) bool {
	switch method {
	case "Set", "SetAndExpire", "SetNX", "Persist", "Expire", "ExpireAt",
		"Del", "Delete", "Unlink", "Rename", "Move", "Copy",
		"ZAdd", "ZRem", "ZRemRangeByScore", "HSet", "HDel":
		return true
	default:
		return false
	}
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

func expressionContainsSessionSecurityNamespace(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != gotoken.STRING {
			return !found
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			return !found
		}
		for _, namespace := range []string{
			"auth:generation:",
			"auth:sessions:",
			"auth:legacy-deny:",
			"auth:rollout-control",
			"auth:migration:",
		} {
			if strings.Contains(value, namespace) {
				found = true
				return false
			}
		}
		return !found
	})
	return found
}
