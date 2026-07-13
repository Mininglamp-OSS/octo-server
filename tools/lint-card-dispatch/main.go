// Command lint-card-dispatch rejects new in-process type-17 transport owners.
// It is a review backstop for recognizable Go call/data-flow shapes; runtime
// payload passthrough is separately guarded at its ingress.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type AllowlistEntry struct {
	Package  string
	Receiver string
	Function string
	Reason   string
}

var defaultAllowlist = []AllowlistEntry{
	{Package: "bot_api", Receiver: "BotAPI", Function: "sendMessage", Reason: "reviewed authenticated Bot API card ingress"},
	{Package: "robot", Receiver: "Robot", Function: "sendMessage", Reason: "reviewed legacy Robot API card ingress"},
	{Package: "incomingwebhook", Receiver: "IncomingWebhook", Function: "handlePush", Reason: "reviewed Incoming Webhook card ingress"},
	{Package: "carddispatch", Receiver: "producerSender", Function: "Send", Reason: "sole reviewed server-internal card dispatch boundary"},
}

type Finding struct {
	Package  string
	Receiver string
	Function string
	File     string
	Line     int
}

type functionKey struct {
	receiver string
	name     string
}

type functionInfo struct {
	key             functionKey
	file            string
	line            int
	constructsCard  bool
	directTransport bool
	calls           map[functionKey]struct{}
}

type functionAlias struct {
	function  functionKey
	transport bool
}

type packageInfo struct {
	name          string
	functions     map[functionKey][]*functionInfo
	allFunctions  []*functionInfo
	cardConstants map[string]struct{}
}

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"./modules", "./internal"}
	}
	findings, err := Scan(roots, defaultAllowlist)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if len(findings) == 0 {
		return
	}
	for _, finding := range findings {
		symbol := finding.Function
		if finding.Receiver != "" {
			symbol = finding.Receiver + "." + symbol
		}
		fmt.Fprintf(os.Stderr, "%s:%d: %s.%s constructs a type-17 card and reaches SendMessage transport\n",
			finding.File, finding.Line, finding.Package, symbol)
	}
	os.Exit(1)
}

func Scan(roots []string, allowlist []AllowlistEntry) ([]Finding, error) {
	files, err := goFiles(roots)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	packages := make(map[string]*packageInfo)
	parsed := make(map[string]*ast.File, len(files))
	for _, path := range files {
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s: %w", path, parseErr)
		}
		parsed[path] = file
		key := filepath.Dir(path) + "\x00" + file.Name.Name
		pkg := packages[key]
		if pkg == nil {
			pkg = &packageInfo{
				name:          file.Name.Name,
				functions:     make(map[functionKey][]*functionInfo),
				cardConstants: make(map[string]struct{}),
			}
			packages[key] = pkg
		}
		collectCardConstants(file, pkg.cardConstants)
	}

	for path, file := range parsed {
		key := filepath.Dir(path) + "\x00" + file.Name.Name
		pkg := packages[key]
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			position := fset.Position(fn.Pos())
			key := declaredFunctionKey(fn)
			info := &functionInfo{
				key:   key,
				file:  path,
				line:  position.Line,
				calls: make(map[functionKey]struct{}),
			}
			receivers := receiverBindings(fn, key.receiver)
			constants := functionCardConstants(fn.Body, pkg.cardConstants)
			aliases := functionAliases(fn.Body, receivers)
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.CompositeLit:
					if compositeContainsCardType(value, constants) {
						info.constructsCard = true
					}
				case *ast.AssignStmt:
					if assignmentContainsCardType(value, constants) {
						info.constructsCard = true
					}
				case *ast.CallExpr:
					if identifier, ok := value.Fun.(*ast.Ident); ok {
						if alias, exists := aliases[identifier.Name]; exists {
							if alias.transport {
								info.directTransport = true
							} else {
								info.calls[alias.function] = struct{}{}
							}
						} else if called, exists := calledFunctionKey(value.Fun, receivers); exists {
							info.calls[called] = struct{}{}
						}
					} else if called, ok := calledFunctionKey(value.Fun, receivers); ok {
						info.calls[called] = struct{}{}
					}
					name := calledName(value.Fun)
					if name == "SendMessage" || name == "SendMessageWithResult" {
						info.directTransport = true
					}
					if isCardMarkerCall(value.Fun) {
						info.constructsCard = true
					}
				}
				return true
			})
			pkg.functions[info.key] = append(pkg.functions[info.key], info)
			pkg.allFunctions = append(pkg.allFunctions, info)
		}
	}

	allowed := make(map[string]struct{}, len(allowlist))
	for _, entry := range allowlist {
		if entry.Package == "" || entry.Function == "" || entry.Reason == "" {
			continue
		}
		allowed[allowlistKey(entry.Package, functionKey{receiver: entry.Receiver, name: entry.Function})] = struct{}{}
	}

	var findings []Finding
	for _, pkg := range packages {
		propagate(pkg, allowed)
		for _, function := range pkg.allFunctions {
			if !function.constructsCard || !function.directTransport {
				continue
			}
			if isAllowed(allowed, pkg.name, function.key) {
				continue
			}
			findings = append(findings, Finding{
				Package: pkg.name, Receiver: function.key.receiver, Function: function.key.name,
				File: function.file, Line: function.line,
			})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Function < findings[j].Function
	})
	return findings, nil
}

func propagate(pkg *packageInfo, allowed map[string]struct{}) {
	changed := true
	for changed {
		changed = false
		for _, function := range pkg.allFunctions {
			for called := range function.calls {
				if isAllowed(allowed, pkg.name, called) {
					continue
				}
				for _, callee := range pkg.functions[called] {
					if callee.constructsCard && !function.constructsCard {
						function.constructsCard = true
						changed = true
					}
					if callee.directTransport && !function.directTransport {
						function.directTransport = true
						changed = true
					}
				}
			}
		}
	}
}

func allowlistKey(packageName string, function functionKey) string {
	return packageName + "\x00" + function.receiver + "\x00" + function.name
}

func isAllowed(allowed map[string]struct{}, packageName string, function functionKey) bool {
	_, ok := allowed[allowlistKey(packageName, function)]
	return ok
}

func declaredFunctionKey(fn *ast.FuncDecl) functionKey {
	key := functionKey{name: fn.Name.Name}
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		key.receiver = typeName(fn.Recv.List[0].Type)
	}
	return key
}

func receiverBindings(fn *ast.FuncDecl, receiverType string) map[string]string {
	bindings := make(map[string]string)
	if receiverType == "" || fn.Recv == nil || len(fn.Recv.List) == 0 {
		return bindings
	}
	for _, name := range fn.Recv.List[0].Names {
		bindings[name.Name] = receiverType
	}
	return bindings
}

func functionCardConstants(body *ast.BlockStmt, packageConstants map[string]struct{}) map[string]struct{} {
	constants := make(map[string]struct{}, len(packageConstants))
	for name := range packageConstants {
		constants[name] = struct{}{}
	}

	changed := true
	for changed {
		changed = false
		ast.Inspect(body, func(node ast.Node) bool {
			declaration, ok := node.(*ast.DeclStmt)
			if !ok {
				return true
			}
			generated, ok := declaration.Decl.(*ast.GenDecl)
			if !ok || (generated.Tok != token.CONST && generated.Tok != token.VAR) {
				return true
			}
			for _, spec := range generated.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range values.Names {
					if _, exists := constants[name.Name]; exists || index >= len(values.Values) {
						continue
					}
					if expressionIsCardType(values.Values[index], constants) {
						constants[name.Name] = struct{}{}
						changed = true
					}
				}
			}
			return true
		})
	}
	return constants
}

func functionAliases(body *ast.BlockStmt, receivers map[string]string) map[string]functionAlias {
	aliases := make(map[string]functionAlias)
	ast.Inspect(body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.AssignStmt:
			for index, left := range value.Lhs {
				if index >= len(value.Rhs) {
					continue
				}
				name, ok := left.(*ast.Ident)
				if ok {
					registerFunctionAlias(aliases, name.Name, value.Rhs[index], receivers)
				}
			}
		case *ast.DeclStmt:
			generated, ok := value.Decl.(*ast.GenDecl)
			if !ok {
				return true
			}
			for _, spec := range generated.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range values.Names {
					if index < len(values.Values) {
						registerFunctionAlias(aliases, name.Name, values.Values[index], receivers)
					}
				}
			}
		}
		return true
	})
	return aliases
}

func registerFunctionAlias(aliases map[string]functionAlias, name string, expression ast.Expr, receivers map[string]string) {
	if identifier, ok := expression.(*ast.Ident); ok {
		if alias, exists := aliases[identifier.Name]; exists {
			aliases[name] = alias
			return
		}
	}
	if isTransportReference(expression) {
		aliases[name] = functionAlias{transport: true}
		return
	}
	if function, ok := calledFunctionKey(expression, receivers); ok {
		aliases[name] = functionAlias{function: function}
	}
}

func isTransportReference(expression ast.Expr) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return selector.Sel.Name == "SendMessage" || selector.Sel.Name == "SendMessageWithResult"
}

func calledFunctionKey(expression ast.Expr, receivers map[string]string) (functionKey, bool) {
	switch value := expression.(type) {
	case *ast.Ident:
		return functionKey{name: value.Name}, true
	case *ast.SelectorExpr:
		receiver, ok := value.X.(*ast.Ident)
		if !ok {
			return functionKey{}, false
		}
		receiverType := receivers[receiver.Name]
		if receiverType == "" {
			return functionKey{}, false
		}
		return functionKey{receiver: receiverType, name: value.Sel.Name}, true
	default:
		return functionKey{}, false
	}
}

func typeName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return typeName(value.X)
	case *ast.IndexExpr:
		return typeName(value.X)
	case *ast.IndexListExpr:
		return typeName(value.X)
	case *ast.ParenExpr:
		return typeName(value.X)
	default:
		return ""
	}
}

func goFiles(roots []string) ([]string, error) {
	var files []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				switch entry.Name() {
				case ".git", ".context", "testdata", "vendor":
					if path != root {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", root, err)
		}
	}
	sort.Strings(files)
	return files, nil
}

func collectCardConstants(file *ast.File, out map[string]struct{}) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range valueSpec.Names {
				if index < len(valueSpec.Values) && expressionIsCardType(valueSpec.Values[index], out) {
					out[name.Name] = struct{}{}
				}
			}
		}
	}
}

func compositeContainsCardType(literal *ast.CompositeLit, constants map[string]struct{}) bool {
	for _, element := range literal.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok || stringLiteral(pair.Key) != "type" {
			continue
		}
		if expressionIsCardType(pair.Value, constants) {
			return true
		}
	}
	return false
}

func assignmentContainsCardType(assignment *ast.AssignStmt, constants map[string]struct{}) bool {
	for index, left := range assignment.Lhs {
		if index >= len(assignment.Rhs) {
			continue
		}
		mapIndex, ok := left.(*ast.IndexExpr)
		if !ok || stringLiteral(mapIndex.Index) != "type" {
			continue
		}
		if expressionIsCardType(assignment.Rhs[index], constants) {
			return true
		}
	}
	return false
}

func expressionIsCardType(expression ast.Expr, constants map[string]struct{}) bool {
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind != token.INT {
			return false
		}
		n, err := strconv.ParseInt(value.Value, 0, 64)
		return err == nil && n == 17
	case *ast.Ident:
		_, ok := constants[value.Name]
		return ok
	case *ast.SelectorExpr:
		return value.Sel.Name == "InteractiveCard"
	case *ast.CallExpr:
		return isInteractiveCardIntCall(value)
	default:
		return false
	}
}

func isInteractiveCardIntCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Int" {
		return false
	}
	receiver, ok := selector.X.(*ast.SelectorExpr)
	return ok && receiver.Sel.Name == "InteractiveCard"
}

func isCardMarkerCall(expression ast.Expr) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	base, ok := selector.X.(*ast.Ident)
	if !ok || base.Name != "cardmsg" {
		return false
	}
	switch selector.Sel.Name {
	case "Validate", "Finalize", "RecheckPayloadSize":
		return true
	default:
		return false
	}
}

func calledName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func stringLiteral(expression ast.Expr) string {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return ""
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return ""
	}
	return value
}
