// Command lint-personal-msgsendreq scans .go files under one or more roots and
// rejects direct config.MsgSendReq{} composite literals that explicitly set
// ChannelType to common.ChannelTypePerson (or its .Uint8() variant). PERSONAL
// DM dispatchers MUST go through config.NewPersonalMsgSendReq() in octo-lib so
// payload.space_id authoritative semantics are uniformly applied.
//
// Background: Mininglamp-OSS/octo-server#37 (defense-in-depth wrapper for
// PERSONAL MsgSendReq, follow-up to PR#35 / Mininglamp-OSS/octo-server#33).
//
// Usage:
//
//	go run ./tools/lint-personal-msgsendreq [root...]
//
// Defaults to scanning ./modules. Test files (*_test.go) are skipped because
// tests sometimes need to construct adversarial / golden literals directly to
// exercise the builder's fail-closed behavior.
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
	"strings"
)

// channelTypePersonValue is the numeric value that a direct integer literal
// would have to use to bypass the symbolic ChannelTypePerson detection.
// octo-lib's common.ChannelType iota starts at ChannelTypeNone=0, so
// ChannelTypePerson==1. This is part of the public protocol surface and is
// not expected to drift; if it ever does, the constant must be updated here
// AND in the test fixture below.
const channelTypePersonValue = "1"

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"modules"}
	}

	fset := token.NewFileSet()
	var violations []string

	for _, root := range roots {
		walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				// Defense-in-depth: skip vendored code and the lint tool itself.
				name := info.Name()
				if name == "vendor" || name == "tools" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				// Don't fail the lint on parse errors — `go build` / `go vet`
				// is the canonical syntax check; we just want a clean signal
				// from the AST when it parses.
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if !isMsgSendReqType(cl.Type) {
					return true
				}
				for _, e := range cl.Elts {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || key.Name != "ChannelType" {
						continue
					}
					if containsPersonChannelType(kv.Value) {
						pos := fset.Position(cl.Pos())
						violations = append(violations,
							fmt.Sprintf("%s:%d: direct PERSONAL MsgSendReq literal — use config.NewPersonalMsgSendReq()",
								pos.Filename, pos.Line))
					}
				}
				return true
			})
			return nil
		})
		if walkErr != nil {
			fmt.Fprintf(os.Stderr, "walk %q failed: %v\n", root, walkErr)
			os.Exit(2)
		}
	}

	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Println(v)
		}
		fmt.Printf("\nFound %d direct PERSONAL MsgSendReq literal(s).\n", len(violations))
		fmt.Println("Use config.NewPersonalMsgSendReq(channelID, fromUID, payloadMap, senderSpaceID, opts) instead.")
		fmt.Println("See https://github.com/Mininglamp-OSS/octo-server/issues/37.")
		os.Exit(1)
	}
	fmt.Println("OK: no direct PERSONAL MsgSendReq literals.")
}

// isMsgSendReqType reports whether the composite literal type is
// (*octolib*).MsgSendReq, MsgSendReq, or any selector ending in .MsgSendReq.
func isMsgSendReqType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "MsgSendReq"
	case *ast.Ident:
		return t.Name == "MsgSendReq"
	case *ast.StarExpr:
		return isMsgSendReqType(t.X)
	}
	return false
}

// containsPersonChannelType reports whether the expression refers to
// ChannelTypePerson anywhere (e.g. common.ChannelTypePerson.Uint8(),
// ChannelTypePerson.Uint8(), or just ChannelTypePerson) — OR uses the raw
// numeric literal that ChannelTypePerson resolves to (currently 1). The
// numeric form is included so a `ChannelType: 1` literal cannot quietly
// bypass the guard; the lint is meant to be a durable CI backstop.
func containsPersonChannelType(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.Ident:
			if t.Name == "ChannelTypePerson" {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if t.Sel != nil && t.Sel.Name == "ChannelTypePerson" {
				found = true
				return false
			}
		case *ast.BasicLit:
			if t.Kind == token.INT && t.Value == channelTypePersonValue {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
