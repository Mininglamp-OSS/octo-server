package authz

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

type ScannedGate struct {
	Source     string
	Module     string
	Symbol     string
	LegacyGate LegacyGate
	Line       int
}

func ScanDirectGates(repositoryRoot string) ([]ScannedGate, error) {
	modulesRoot := filepath.Join(repositoryRoot, "modules")
	fset := token.NewFileSet()
	var gates []ScannedGate
	err := filepath.WalkDir(modulesRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		matches, err := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path))
		if err != nil {
			return fmt.Errorf("match build constraints for %s: %w", path, err)
		}
		if !matches {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		relative, err := filepath.Rel(repositoryRoot, path)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", path, err)
		}
		module := moduleFromRelativePath(relative)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			symbol := functionSymbol(function)
			ordinal := 0
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				legacyGate, ok := directGate(selector.Sel.Name)
				if !ok {
					return true
				}
				ordinal++
				line := fset.Position(call.Pos()).Line
				source := fmt.Sprintf("%s::%s#%d", filepath.ToSlash(relative), symbol, ordinal)
				gates = append(gates, ScannedGate{Source: source, Module: module, Symbol: symbol, LegacyGate: legacyGate, Line: line})
				return true
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan direct gates: %w", err)
	}
	sort.Slice(gates, func(i, j int) bool { return gates[i].Source < gates[j].Source })
	return gates, nil
}

func ValidateGateInventory(scanned []ScannedGate, declared []GateSite) error {
	scanBySource := make(map[string]ScannedGate, len(scanned))
	for _, gate := range scanned {
		if _, exists := scanBySource[gate.Source]; exists {
			return fmt.Errorf("scanned gate %q is duplicated", gate.Source)
		}
		scanBySource[gate.Source] = gate
	}
	declaredBySource := make(map[string]GateSite, len(declared))
	for i, gate := range declared {
		if _, exists := declaredBySource[gate.Source]; exists {
			return fmt.Errorf("gate_sites[%d].source: duplicate source identity %q", i, gate.Source)
		}
		declaredBySource[gate.Source] = gate
	}
	for _, gate := range scanned {
		declaredGate, exists := declaredBySource[gate.Source]
		if !exists {
			return fmt.Errorf("%s:%d: unregistered direct gate %s", sourceFile(gate.Source), gate.Line, gate.Source)
		}
		if declaredGate.LegacyGate != gate.LegacyGate {
			return fmt.Errorf("gate_sites[%q].legacy_gate: got %q, source uses %q at line %d", gate.Source, declaredGate.LegacyGate, gate.LegacyGate, gate.Line)
		}
		if declaredGate.Module != gate.Module || declaredGate.Symbol != gate.Symbol {
			return fmt.Errorf("gate_sites[%q]: module or symbol does not match source at line %d", gate.Source, gate.Line)
		}
	}
	for _, gate := range declared {
		if _, exists := scanBySource[gate.Source]; !exists {
			return fmt.Errorf("gate_sites[%q]: declared gate does not exist in source", gate.Source)
		}
	}
	return nil
}

func directGate(name string) (LegacyGate, bool) {
	switch name {
	case "CheckLoginRole":
		return LegacyGateAdmin, true
	case "CheckLoginRoleIsSuperAdmin":
		return LegacyGateSuperAdmin, true
	case "CanReadManagerDashboard":
		return LegacyGateManagerConsoleRole, true
	default:
		return "", false
	}
}

func functionSymbol(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return function.Name.Name
	}
	return receiverName(function.Recv.List[0].Type) + "." + function.Name.Name
}

func receiverName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return receiverName(value.X)
	case *ast.IndexExpr:
		return receiverName(value.X)
	case *ast.IndexListExpr:
		return receiverName(value.X)
	default:
		return "unknown"
	}
}

func moduleFromRelativePath(relative string) string {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) >= 2 && parts[0] == "modules" {
		return parts[1]
	}
	return ""
}

func sourceFile(source string) string {
	file, _, _ := strings.Cut(source, "::")
	return file
}
