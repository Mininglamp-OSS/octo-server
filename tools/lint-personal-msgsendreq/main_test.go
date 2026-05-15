package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// walkComposites runs the same AST inspection the binary does and reports
// per-composite results to a callback. Lives in *_test.go so it doesn't add
// dead code to the production binary.
func walkComposites(f *ast.File, cb func(typeMatched, valueMatched bool)) {
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typeMatched := isMsgSendReqType(cl.Type)
		valueMatched := false
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
				valueMatched = true
			}
		}
		cb(typeMatched, valueMatched)
		return true
	})
}

func parse(t *testing.T, src string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

// PERSONAL literal must be detected (canonical migration target).
func TestDetectsDirectPersonalMsgSendReq(t *testing.T) {
	f := parse(t, `package x
import (
	"x/common"
	"x/config"
)
func _() {
	_ = &config.MsgSendReq{
		ChannelType: common.ChannelTypePerson.Uint8(),
	}
}
`)
	hits := 0
	walkComposites(f, func(typeMatched, valueMatched bool) {
		if typeMatched && valueMatched {
			hits++
		}
	})
	if hits != 1 {
		t.Fatalf("expected 1 PERSONAL MsgSendReq detection, got %d", hits)
	}
}

// GROUP literal must NOT trip the lint (out of scope for the PERSONAL builder).
func TestIgnoresGroupMsgSendReq(t *testing.T) {
	f := parse(t, `package x
import (
	"x/common"
	"x/config"
)
func _() {
	_ = &config.MsgSendReq{
		ChannelType: common.ChannelTypeGroup.Uint8(),
	}
}
`)
	hits := 0
	walkComposites(f, func(typeMatched, valueMatched bool) {
		if typeMatched && valueMatched {
			hits++
		}
	})
	if hits != 0 {
		t.Fatalf("expected 0 detections on GROUP literal, got %d", hits)
	}
}

// Variable channel_type (e.g. message.ChannelType passthrough) must NOT trip
// the lint — those sites need a runtime PERSONAL branch, which the code
// already does (see modules/robot/event.go after the migration).
func TestIgnoresVariableChannelType(t *testing.T) {
	f := parse(t, `package x
import "x/config"
func _(ct uint8) {
	_ = &config.MsgSendReq{
		ChannelType: ct,
	}
}
`)
	hits := 0
	walkComposites(f, func(typeMatched, valueMatched bool) {
		if typeMatched && valueMatched {
			hits++
		}
	})
	if hits != 0 {
		t.Fatalf("expected 0 detections on variable channel_type, got %d", hits)
	}
}
