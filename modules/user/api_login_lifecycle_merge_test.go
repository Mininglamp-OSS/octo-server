package user

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoginLifecycleHelpersRemainIntegrated guards the merge boundary between
// the v3 APP-session lifecycle and login-audit finalization. Both helpers are
// load-bearing and must coexist even when adjacent changes land on main.
func TestLoginLifecycleHelpersRemainIntegrated(t *testing.T) {
	tests := []struct {
		file string
		fn   string
		sink string
	}{
		{file: "api.go", fn: "execLoginAndRespose", sink: "finishSuccessfulLogin"},
		{file: "api.go", fn: "loginWithAuthCode", sink: "finishSuccessfulLogin"},
		{file: "api.go", fn: "loginCheckPhone", sink: "finishSuccessfulLogin"},
		{file: "api.go", fn: "createUserWithRespAndTx", sink: "finishSuccessfulLogin"},
		{file: "api_usernamelogin.go", fn: "usernameLogin", sink: "finishSuccessfulLogin"},
		{file: "api_emaillogin.go", fn: "emailLogin", sink: "finishSuccessfulLogin"},
		// The console sign-in splits credential check (login) from session issue
		// (issueManagerSession); the audit sink lives with the issue.
		{file: "api_manager.go", fn: "issueManagerSession", sink: "recordSuccess"},
		{file: "api_github.go", fn: "githubOAuth", sink: "finishSuccessfulLogin"},
		{file: "api_gitee.go", fn: "giteeOAuth", sink: "finishSuccessfulLogin"},
		{file: "external_login.go", fn: "externalLoginExisting", sink: "finishSuccessfulLogin"},
	}

	for _, tt := range tests {
		t.Run(tt.file+"/"+tt.fn, func(t *testing.T) {
			fn := parseLifecycleFunction(t, tt.file, tt.fn)
			require.True(t, lifecycleBlockCalls(fn.Body, tt.sink),
				"%s/%s must call %s on successful login", tt.file, tt.fn, tt.sink)
		})
	}
}

func TestPostFenceLoginRejectionsAreAudited(t *testing.T) {
	tests := []struct {
		file string
		fn   string
	}{
		{file: "api.go", fn: "login"},
		{file: "api_usernamelogin.go", fn: "usernameLogin"},
		{file: "api_emaillogin.go", fn: "emailLogin"},
		{file: "api_manager.go", fn: "issueManagerSession"},
	}

	for _, tt := range tests {
		t.Run(tt.file+"/"+tt.fn, func(t *testing.T) {
			fn := parseLifecycleFunction(t, tt.file, tt.fn)
			var found bool
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				ifStmt, ok := node.(*ast.IfStmt)
				if !ok || !lifecycleExpressionContainsIdent(ifStmt.Cond, "fencedUID") {
					return true
				}
				found = lifecycleBlockCalls(ifStmt.Body, "recordFailure")
				return !found
			})
			require.True(t, found, "%s/%s must audit the post-fence rejection", tt.file, tt.fn)
		})
	}
}

func TestEmailVerificationFailureIsAudited(t *testing.T) {
	fn := parseLifecycleFunction(t, "api_emaillogin.go", "emailLogin")
	var found bool
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		ifStmt, ok := node.(*ast.IfStmt)
		if !ok || ifStmt.Init == nil || !lifecycleNodeCalls(ifStmt.Init, "Verify") {
			return true
		}
		found = lifecycleBlockCalls(ifStmt.Body, "recordFailure")
		return !found
	})
	require.True(t, found, "emailLogin must audit a rejected verification code")
}

func TestFirstWechatLoginPreservesAuditType(t *testing.T) {
	fn := parseLifecycleFunction(t, "api.go", "wxLogin")
	var found bool
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typeName, ok := literal.Type.(*ast.Ident)
		if !ok || typeName.Name != "createUserModel" {
			return true
		}
		for _, element := range literal.Elts {
			field, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := field.Key.(*ast.Ident)
			value, valueOK := field.Value.(*ast.BasicLit)
			if !ok || key.Name != "LoginType" || !valueOK || value.Kind != gotoken.STRING {
				continue
			}
			loginType, err := strconv.Unquote(value.Value)
			require.NoError(t, err)
			found = loginType == "wechat"
			return !found
		}
		return true
	})
	require.True(t, found, "wxLogin create path must set LoginType to wechat")
}

func parseLifecycleFunction(t *testing.T, path, name string) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(gotoken.NewFileSet(), path, nil, 0)
	require.NoError(t, err)
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("function %s not found in %s", name, path)
	return nil
}

func lifecycleBlockCalls(body ast.Node, name string) bool {
	return lifecycleNodeCalls(body, name)
}

func lifecycleNodeCalls(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && lifecycleCalledFunctionName(call.Fun) == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

func lifecycleCalledFunctionName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}

func lifecycleExpressionContainsIdent(expression ast.Expr, name string) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if ok && ident.Name == name {
			found = true
			return false
		}
		return !found
	})
	return found
}
