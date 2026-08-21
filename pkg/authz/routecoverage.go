package authz

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	unresolvedRoutePrefix         = "\x00"
	unresolvedPlatformRoutePrefix = "\x01"
)

// ScannedRoute is a statically registered HTTP route whose handler reaches one
// or more direct legacy gates in the same package.
type ScannedRoute struct {
	Method    string
	Path      string
	Module    string
	Handler   string
	GateSites []string
	Source    string
	Line      int
}

// RouteBoundaryExclusion classifies a route that reaches a legacy gate through
// a mixed handler but remains governed by a business ACL branch.
type RouteBoundaryExclusion struct {
	Method  string
	Path    string
	Handler string
	Reason  string
}

type scannedFunction struct {
	symbol       string
	receiverName string
	receiverType string
	body         *ast.BlockStmt
	file         string
	parameters   []*ast.Field
}

// ScanManagerRoutes follows package-local receiver calls from registered route
// handlers to the supplied direct gates. It intentionally does not attempt a
// whole-program call graph.
func ScanManagerRoutes(repositoryRoot string, gates []ScannedGate) ([]ScannedRoute, error) {
	gatesByDirectory := make(map[string][]ScannedGate)
	for _, gate := range gates {
		directory := filepath.Dir(filepath.FromSlash(sourceFile(gate.Source)))
		gatesByDirectory[directory] = append(gatesByDirectory[directory], gate)
	}

	var routes []ScannedRoute
	for directory, directoryGates := range gatesByDirectory {
		scanned, err := scanRouteDirectory(repositoryRoot, directory, directoryGates)
		if err != nil {
			return nil, err
		}
		routes = append(routes, scanned...)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		if routes[i].Method != routes[j].Method {
			return routes[i].Method < routes[j].Method
		}
		return routes[i].Handler < routes[j].Handler
	})
	return routes, nil
}

// ManagerRouteBoundaryExclusions is retained for callers of the original
// coverage API. Business routes are now rejected by platform admission before
// they become scanned routes, so the repository contract has no exclusions.
func ManagerRouteBoundaryExclusions() []RouteBoundaryExclusion {
	return nil
}

func isPlatformOperationRoute(method, routePath, handler string) bool {
	if routePath == "/v1/manager/me" || routePath == "/v1/manager/secrets" || strings.HasPrefix(routePath, "/v1/manager/secrets/") {
		return false
	}
	if routePath == "/v1/manager" || strings.HasPrefix(routePath, "/v1/manager/") {
		return true
	}
	if routePath == "/v1/admin/app_bot" || strings.HasPrefix(routePath, "/v1/admin/app_bot/") {
		return true
	}
	if routePath == "/v1/common/appversion" || strings.HasPrefix(routePath, "/v1/common/appversion/") {
		return true
	}
	return method == http.MethodPost && routePath == "/v1/users/:uid/avatar" && handler == "User.uploadAvatar"
}

func platformCandidatePrefix(prefix string) bool {
	if prefix == unresolvedPlatformRoutePrefix {
		return true
	}
	if prefix == unresolvedRoutePrefix {
		return false
	}
	if prefix == "/v1/manager/secrets" || strings.HasPrefix(prefix, "/v1/manager/secrets/") {
		return false
	}
	return prefix == "/v1/manager" || strings.HasPrefix(prefix, "/v1/manager/") ||
		prefix == "/v1/admin/app_bot" || strings.HasPrefix(prefix, "/v1/admin/app_bot/") ||
		prefix == "/v1/common/appversion" || strings.HasPrefix(prefix, "/v1/common/appversion/")
}

func platformGateSites(method, routePath, handler string, gateSites []string) []string {
	result := append([]string(nil), gateSites...)
	if method != http.MethodPost || routePath != "/v1/users/:uid/avatar" || handler != "User.uploadAvatar" {
		return result
	}
	result = result[:0]
	for _, source := range gateSites {
		if source == "modules/user/api.go::User.uploadAvatar#1" {
			result = append(result, source)
		}
	}
	return result
}

// ValidateRouteCoverage compares source-derived gated routes with the global
// operation inventory after applying the explicit business-ACL exclusions.
func ValidateRouteCoverage(routes []ScannedRoute, operations []Operation, exclusions []RouteBoundaryExclusion) error {
	routesByKey := make(map[string]ScannedRoute, len(routes))
	for _, route := range routes {
		key := httpRouteKey(route.Method, route.Path)
		if previous, exists := routesByKey[key]; exists {
			return fmt.Errorf("duplicate source route %s %s at %s:%d and %s:%d", route.Method, route.Path, previous.Source, previous.Line, route.Source, route.Line)
		}
		routesByKey[key] = route
	}

	exclusionsByKey := make(map[string]RouteBoundaryExclusion, len(exclusions))
	for i, exclusion := range exclusions {
		if !isHTTPMethod(exclusion.Method) || !normalizedPath(exclusion.Path) || strings.TrimSpace(exclusion.Handler) == "" || strings.TrimSpace(exclusion.Reason) == "" {
			return fmt.Errorf("route boundary exclusions[%d]: method, normalized path, handler and reason are required", i)
		}
		key := httpRouteKey(exclusion.Method, exclusion.Path)
		if _, exists := exclusionsByKey[key]; exists {
			return fmt.Errorf("route boundary exclusions[%d]: duplicate route %s %s", i, exclusion.Method, exclusion.Path)
		}
		exclusionsByKey[key] = exclusion
	}

	operationsByKey := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		key := httpRouteKey(operation.Method, operation.Path)
		if previous, exists := operationsByKey[key]; exists {
			return fmt.Errorf("operations[%q] and operations[%q] declare the same route %s %s", previous.ID, operation.ID, operation.Method, operation.Path)
		}
		operationsByKey[key] = operation
	}

	matchedOperations := make(map[string]struct{}, len(operations))
	matchedExclusions := make(map[string]struct{}, len(exclusions))
	for key, route := range routesByKey {
		if exclusion, exists := exclusionsByKey[key]; exists {
			if route.Handler != exclusion.Handler {
				return fmt.Errorf("route boundary %s %s handler drift: source=%q fixture=%q", route.Method, route.Path, route.Handler, exclusion.Handler)
			}
			if _, declared := operationsByKey[key]; declared {
				return fmt.Errorf("business ACL route %s %s must not be declared as a global operation", route.Method, route.Path)
			}
			matchedExclusions[key] = struct{}{}
			continue
		}

		operation, exists := operationsByKey[key]
		if !exists {
			return fmt.Errorf("%s:%d: unregistered global route %s %s handler %s", route.Source, route.Line, route.Method, route.Path, route.Handler)
		}
		if route.Handler != operation.Handler {
			return fmt.Errorf("operation %q handler drift for %s %s: source=%q manifest=%q", operation.ID, route.Method, route.Path, route.Handler, operation.Handler)
		}
		if route.Module != operation.Module {
			return fmt.Errorf("operation %q module drift for %s %s: source=%q manifest=%q", operation.ID, route.Method, route.Path, route.Module, operation.Module)
		}
		if !equalStringSets(route.GateSites, operation.GateSites) {
			return fmt.Errorf("operation %q gate-site drift for %s %s: source=%v manifest=%v", operation.ID, route.Method, route.Path, route.GateSites, operation.GateSites)
		}
		matchedOperations[key] = struct{}{}
	}

	for key, operation := range operationsByKey {
		if _, exists := matchedOperations[key]; !exists {
			return fmt.Errorf("operation %q has no matching source route %s %s", operation.ID, operation.Method, operation.Path)
		}
	}
	for key, exclusion := range exclusionsByKey {
		if _, exists := matchedExclusions[key]; !exists {
			return fmt.Errorf("route boundary fixture has no matching source route %s %s handler %s", exclusion.Method, exclusion.Path, exclusion.Handler)
		}
	}
	return nil
}

func httpRouteKey(method, path string) string {
	return method + "\x00" + path
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[string]int, len(left))
	for _, value := range left {
		values[value]++
	}
	for _, value := range right {
		values[value]--
		if values[value] < 0 {
			return false
		}
	}
	return true
}

func scanRouteDirectory(repositoryRoot, relativeDirectory string, gates []ScannedGate) ([]ScannedRoute, error) {
	directory := filepath.Join(repositoryRoot, relativeDirectory)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("scan route directory %s: %w", relativeDirectory, err)
	}

	fset := token.NewFileSet()
	functions := make(map[string]scannedFunction)
	var routeFunctions []scannedFunction
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		matches, err := build.Default.MatchFile(directory, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("match build constraints for %s: %w", filepath.Join(relativeDirectory, entry.Name()), err)
		}
		if !matches {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		relative, err := filepath.Rel(repositoryRoot, path)
		if err != nil {
			return nil, fmt.Errorf("relative path for %s: %w", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			info := scannedFunction{
				symbol:       functionSymbol(function),
				receiverName: functionReceiverName(function),
				receiverType: functionReceiverType(function),
				body:         function.Body,
				file:         filepath.ToSlash(relative),
			}
			if function.Type.Params != nil {
				info.parameters = function.Type.Params.List
			}
			if _, exists := functions[info.symbol]; exists {
				return nil, fmt.Errorf("duplicate function symbol %s in %s", info.symbol, relativeDirectory)
			}
			functions[info.symbol] = info
			if function.Name.Name == "Route" && info.receiverType != "" {
				routeFunctions = append(routeFunctions, info)
			}
		}
	}

	direct := make(map[string][]string)
	for _, gate := range gates {
		direct[gate.Symbol] = append(direct[gate.Symbol], gate.Source)
	}
	edges := buildPackageCallGraph(functions)
	reachableSets := make(map[string]map[string]struct{}, len(functions))
	for symbol := range functions {
		reachableSets[symbol] = make(map[string]struct{})
		for _, source := range direct[symbol] {
			reachableSets[symbol][source] = struct{}{}
		}
	}
	for changed := true; changed; {
		changed = false
		for symbol, targets := range edges {
			for _, target := range targets {
				for source := range reachableSets[target] {
					if _, exists := reachableSets[symbol][source]; !exists {
						reachableSets[symbol][source] = struct{}{}
						changed = true
					}
				}
			}
		}
	}
	reachable := make(map[string][]string, len(functions))
	for symbol, sources := range reachableSets {
		for source := range sources {
			reachable[symbol] = append(reachable[symbol], source)
		}
		sort.Strings(reachable[symbol])
	}

	module := moduleFromRelativePath(filepath.ToSlash(relativeDirectory) + "/placeholder.go")
	platformDelegates := platformDelegatedFunctions(routeFunctions, functions)
	routeSymbols := make(map[string]struct{}, len(routeFunctions))
	var routes []ScannedRoute
	for _, function := range routeFunctions {
		routeSymbols[function.symbol] = struct{}{}
		functionRoutes, err := scanRouteFunction(fset, module, function, functions, reachable, true, false)
		if err != nil {
			return nil, err
		}
		routes = append(routes, functionRoutes...)
	}
	var symbols []string
	for symbol := range functions {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	for _, symbol := range symbols {
		if _, isRoute := routeSymbols[symbol]; isRoute {
			continue
		}
		_, platformContext := platformDelegates[symbol]
		if _, err := scanRouteFunction(fset, module, functions[symbol], functions, reachable, false, platformContext); err != nil {
			return nil, err
		}
	}
	return routes, nil
}

func platformDelegatedFunctions(routeFunctions []scannedFunction, functions map[string]scannedFunction) map[string]struct{} {
	result := make(map[string]struct{})
	for _, function := range routeFunctions {
		prefixes := make(map[string]string)
		for _, field := range function.parameters {
			for _, name := range field.Names {
				prefixes[name.Name] = ""
			}
		}
		aliases := receiverAliasSet(function)
		ast.Inspect(function.body, func(node ast.Node) bool {
			if assignment, ok := node.(*ast.AssignStmt); ok {
				registerGroupPrefixes(assignment, prefixes)
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			target := calledFunctionSymbol(call.Fun, aliases, function.receiverType, functions)
			if target == "" {
				return true
			}
			for _, argument := range call.Args {
				identifier, ok := argument.(*ast.Ident)
				if ok && platformCandidatePrefix(prefixes[identifier.Name]) {
					result[target] = struct{}{}
					break
				}
			}
			return true
		})
	}

	edges := buildPackageCallGraph(functions)
	for changed := true; changed; {
		changed = false
		for source := range result {
			for _, target := range edges[source] {
				if _, exists := result[target]; !exists {
					result[target] = struct{}{}
					changed = true
				}
			}
		}
	}
	return result
}

func calledFunctionSymbol(expression ast.Expr, receiverAliases map[string]struct{}, receiverType string, functions map[string]scannedFunction) string {
	switch called := expression.(type) {
	case *ast.SelectorExpr:
		identifier, ok := called.X.(*ast.Ident)
		if !ok {
			return ""
		}
		if _, ok := receiverAliases[identifier.Name]; !ok {
			return ""
		}
		target := receiverType + "." + called.Sel.Name
		if _, exists := functions[target]; exists {
			return target
		}
	case *ast.Ident:
		if _, exists := functions[called.Name]; exists {
			return called.Name
		}
	}
	return ""
}

func buildPackageCallGraph(functions map[string]scannedFunction) map[string][]string {
	edges := make(map[string][]string, len(functions))
	for symbol, function := range functions {
		seen := make(map[string]struct{})
		aliases := receiverAliasSet(function)
		ast.Inspect(function.body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			target := ""
			switch called := call.Fun.(type) {
			case *ast.SelectorExpr:
				identifier, ok := called.X.(*ast.Ident)
				if ok {
					_, ok = aliases[identifier.Name]
				}
				if ok {
					target = function.receiverType + "." + called.Sel.Name
				}
			case *ast.Ident:
				target = called.Name
			}
			if _, exists := functions[target]; exists {
				seen[target] = struct{}{}
			}
			return true
		})
		for target := range seen {
			edges[symbol] = append(edges[symbol], target)
		}
		sort.Strings(edges[symbol])
	}
	return edges
}

func scanRouteFunction(fset *token.FileSet, module string, function scannedFunction, functions map[string]scannedFunction, reachable map[string][]string, allowRegistration, platformContext bool) ([]ScannedRoute, error) {
	prefixes := make(map[string]string)
	receiverAliases := receiverAliasSet(function)
	for _, field := range function.parameters {
		for _, name := range field.Names {
			prefixes[name.Name] = ""
		}
	}

	var routes []ScannedRoute
	var scanErr error
	ast.Inspect(function.body, func(node ast.Node) bool {
		if scanErr != nil {
			return false
		}
		if assignment, ok := node.(*ast.AssignStmt); ok {
			gatedPlatformGroup := assignmentHasGatedGroupMiddleware(assignment, prefixes, receiverAliases, function.receiverType, functions, reachable)
			registerGroupPrefixes(assignment, prefixes)
			if gatedPlatformGroup {
				position := fset.Position(assignment.Pos())
				scanErr = fmt.Errorf("%s:%d: gated group middleware is not supported", function.file, position.Line)
				return false
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		position := fset.Position(call.Pos())
		base, baseOK := selector.X.(*ast.Ident)
		prefix := unresolvedRoutePrefix
		prefixOK := false
		if baseOK {
			prefix, prefixOK = prefixes[base.Name]
		}
		routePath := ""
		pathOK := false
		if len(call.Args) > 0 {
			routePath, pathOK = stringLiteral(call.Args[0])
		}
		fullPath := ""
		if prefixOK && prefix != unresolvedRoutePrefix && prefix != unresolvedPlatformRoutePrefix && pathOK {
			fullPath = joinRoutePath(prefix, routePath)
		}
		platformCandidate := platformContext || platformCandidatePrefix(prefix) || expressionContainsPlatformPrefix(selector.X)
		platformShape := platformCandidate
		if fullPath != "" {
			platformShape = isPlatformOperationRoute(selector.Sel.Name, fullPath, "")
		}
		if !isHTTPMethod(selector.Sel.Name) {
			if selector.Sel.Name != "Group" && len(call.Args) >= 2 && platformShape {
				scanErr = fmt.Errorf("%s:%d: unsupported route registration verb %s", function.file, position.Line, selector.Sel.Name)
				return false
			}
			return true
		}
		if len(call.Args) < 2 {
			return true
		}

		handlerExpression := call.Args[len(call.Args)-1]
		handler, handlerOK := routeHandlerSymbol(handlerExpression, receiverAliases, function.receiverType, functions)
		platformRoute := handlerOK && fullPath != "" && isPlatformOperationRoute(selector.Sel.Name, fullPath, handler)
		if handlerOK && fullPath != "" && !platformRoute && !platformContext {
			return true
		}
		for _, middlewareExpression := range call.Args[1 : len(call.Args)-1] {
			if expressionMentionsGatedHandler(middlewareExpression, receiverAliases, function.receiverType, functions, reachable) || expressionContainsRecognizedGate(middlewareExpression) {
				if platformRoute || platformCandidate {
					scanErr = fmt.Errorf("%s:%d: gated middleware before route handler is not supported", function.file, position.Line)
					return false
				}
				return true
			}
		}
		if !handlerOK {
			if platformShape {
				scanErr = fmt.Errorf("%s:%d: cannot resolve route handler for platform route", function.file, position.Line)
				return false
			}
			return true
		}
		gateSites := reachable[handler]
		if len(gateSites) == 0 {
			return true
		}
		if !allowRegistration {
			if platformContext || platformRoute {
				scanErr = fmt.Errorf("%s:%d: gated route registration outside Route is not supported", function.file, position.Line)
				return false
			}
			return true
		}
		if !baseOK {
			if platformCandidate {
				scanErr = fmt.Errorf("%s:%d: cannot resolve route base for gated handler %s", function.file, position.Line, handler)
				return false
			}
			return true
		}
		if !prefixOK {
			if platformCandidate {
				scanErr = fmt.Errorf("%s:%d: cannot resolve route prefix variable %s for gated handler %s", function.file, position.Line, base.Name, handler)
				return false
			}
			return true
		}
		if !pathOK {
			if platformCandidate {
				scanErr = fmt.Errorf("%s:%d: cannot resolve route path for gated handler %s", function.file, position.Line, handler)
				return false
			}
			return true
		}
		if prefix == unresolvedRoutePrefix || prefix == unresolvedPlatformRoutePrefix {
			if platformCandidate {
				scanErr = fmt.Errorf("%s:%d: cannot resolve route group prefix for gated handler %s", function.file, position.Line, handler)
				return false
			}
			return true
		}
		if !platformRoute {
			return true
		}
		routes = append(routes, ScannedRoute{
			Method: selector.Sel.Name, Path: fullPath, Module: module,
			Handler: handler, GateSites: platformGateSites(selector.Sel.Name, fullPath, handler, gateSites), Source: function.file, Line: position.Line,
		})
		return true
	})
	return routes, scanErr
}

func assignmentHasGatedGroupMiddleware(assignment *ast.AssignStmt, prefixes map[string]string, receiverAliases map[string]struct{}, receiverType string, functions map[string]scannedFunction, reachable map[string][]string) bool {
	for _, expression := range assignment.Rhs {
		call, ok := expression.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Group" {
			continue
		}
		base, baseOK := selector.X.(*ast.Ident)
		prefix := unresolvedRoutePrefix
		if baseOK {
			prefix = prefixes[base.Name]
		}
		part, partOK := stringLiteral(call.Args[0])
		platformGroup := platformCandidatePrefix(prefix) || expressionContainsPlatformPrefix(selector.X)
		if partOK && prefix != unresolvedRoutePrefix && prefix != unresolvedPlatformRoutePrefix {
			platformGroup = platformCandidatePrefix(joinRoutePath(prefix, part))
		}
		for _, middleware := range call.Args[1:] {
			if platformGroup && (expressionMentionsGatedHandler(middleware, receiverAliases, receiverType, functions, reachable) || expressionContainsRecognizedGate(middleware)) {
				return true
			}
		}
	}
	return false
}

func expressionContainsPlatformPrefix(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && platformCandidatePrefix(pathpkg.Clean(value)) {
			found = true
			return false
		}
		return true
	})
	return found
}

func registerGroupPrefixes(assignment *ast.AssignStmt, prefixes map[string]string) {
	for i, expression := range assignment.Rhs {
		if i >= len(assignment.Lhs) {
			break
		}
		name, ok := assignment.Lhs[i].(*ast.Ident)
		if !ok {
			continue
		}
		call, ok := expression.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Group" {
			continue
		}
		base, ok := selector.X.(*ast.Ident)
		if !ok {
			continue
		}
		prefix, exists := prefixes[base.Name]
		part, partOK := stringLiteral(call.Args[0])
		if !exists {
			if partOK && platformCandidatePrefix(pathpkg.Clean(part)) {
				prefixes[name.Name] = unresolvedPlatformRoutePrefix
			} else {
				prefixes[name.Name] = unresolvedRoutePrefix
			}
			continue
		}
		if !partOK || prefix == unresolvedRoutePrefix || prefix == unresolvedPlatformRoutePrefix {
			if platformCandidatePrefix(prefix) {
				prefixes[name.Name] = unresolvedPlatformRoutePrefix
			} else {
				prefixes[name.Name] = unresolvedRoutePrefix
			}
			continue
		}
		prefixes[name.Name] = joinRoutePath(prefix, part)
	}
}

func routeHandlerSymbol(expression ast.Expr, receiverAliases map[string]struct{}, receiverType string, functions map[string]scannedFunction) (string, bool) {
	switch value := expression.(type) {
	case *ast.SelectorExpr:
		identifier, ok := value.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		if _, ok := receiverAliases[identifier.Name]; !ok {
			return "", false
		}
		symbol := receiverType + "." + value.Sel.Name
		_, exists := functions[symbol]
		return symbol, exists
	case *ast.Ident:
		_, exists := functions[value.Name]
		return value.Name, exists
	default:
		return "", false
	}
}

func expressionMentionsGatedHandler(expression ast.Expr, receiverAliases map[string]struct{}, receiverType string, functions map[string]scannedFunction, reachable map[string][]string) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		value, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		if symbol, ok := routeHandlerSymbol(value, receiverAliases, receiverType, functions); ok && len(reachable[symbol]) > 0 {
			found = true
			return false
		}
		return true
	})
	return found
}

func receiverAliasSet(function scannedFunction) map[string]struct{} {
	aliases := make(map[string]struct{})
	if function.receiverName == "" {
		return aliases
	}
	aliases[function.receiverName] = struct{}{}
	for changed := true; changed; {
		changed = false
		ast.Inspect(function.body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, right := range assignment.Rhs {
				if i >= len(assignment.Lhs) {
					break
				}
				rightName, ok := right.(*ast.Ident)
				if !ok {
					continue
				}
				if _, ok := aliases[rightName.Name]; !ok {
					continue
				}
				leftName, ok := assignment.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if _, exists := aliases[leftName.Name]; !exists {
					aliases[leftName.Name] = struct{}{}
					changed = true
				}
			}
			return true
		})
	}
	return aliases
}

func expressionContainsRecognizedGate(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if _, ok := directGate(selector.Sel.Name); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

func functionReceiverName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 || len(function.Recv.List[0].Names) == 0 {
		return ""
	}
	return function.Recv.List[0].Names[0].Name
}

func functionReceiverType(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return ""
	}
	return receiverName(function.Recv.List[0].Type)
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func isHTTPMethod(value string) bool {
	switch value {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

func joinRoutePath(prefix, suffix string) string {
	joined := "/" + strings.Trim(strings.TrimSuffix(prefix, "/")+"/"+strings.TrimPrefix(suffix, "/"), "/")
	return pathpkg.Clean(joined)
}
