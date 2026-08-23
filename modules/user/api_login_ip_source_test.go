package user

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLoginAuditUsesWKHTTPClientIP(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list user Go files: %v", err)
	}

	// Any exception must name the production file and explain why it is safe.
	exemptions := map[string]string{}
	safeCalls := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		if reason, exempt := exemptions[file]; exempt {
			if strings.TrimSpace(reason) == "" {
				t.Fatalf("%s has an empty client-IP source exemption", file)
			}
			continue
		}
		legacy, safe := clientIPCallCounts(t, file)
		if legacy > 0 {
			t.Fatalf("%s contains %d util.GetClientPublicIP call(s); add a justified exemption or use wkhttp.ClientIP", file, legacy)
		}
		safeCalls += safe
	}
	// Tripwire, not a budget: raise it deliberately when a new call site is
	// reviewed and confirmed to use the safe API. 15 → 17 added the two
	// manager second-factor endpoints (api_manager_2fa.go).
	if safeCalls != 17 {
		t.Fatalf("user production wkhttp.ClientIP calls = %d, want 17", safeCalls)
	}
}

func clientIPCallCounts(t *testing.T, file string) (legacy, safe int) {
	t.Helper()
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	parsed, err := parser.ParseFile(gotoken.NewFileSet(), file, source, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	aliases := map[string]string{}
	for _, spec := range parsed.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("parse import in %s: %v", file, err)
		}
		if importPath != "github.com/Mininglamp-OSS/octo-lib/pkg/util" &&
			importPath != "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp" {
			continue
		}
		alias := filepath.Base(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		aliases[alias] = importPath
	}

	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch aliases[ident.Name] {
		case "github.com/Mininglamp-OSS/octo-lib/pkg/util":
			if selector.Sel.Name == "GetClientPublicIP" {
				legacy++
			}
		case "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp":
			if selector.Sel.Name == "ClientIP" {
				safe++
			}
		}
		return true
	})
	return legacy, safe
}
