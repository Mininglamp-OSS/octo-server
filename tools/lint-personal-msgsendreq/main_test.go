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

// Numeric literal `ChannelType: 1` must be detected. ChannelTypePerson is
// the iota value 1 in octo-lib/common.ChannelType, so a hand-written numeric
// literal would otherwise quietly bypass the symbolic guard. Reviewer ask
// (Jerry-Xin, PR#44 R1): make this lint a durable CI backstop.
func TestDetectsNumericPersonChannelType(t *testing.T) {
	f := parse(t, `package x
import "x/config"
func _() {
	_ = &config.MsgSendReq{
		ChannelType: 1,
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
		t.Fatalf("expected 1 numeric PERSONAL detection, got %d", hits)
	}
}

// uint8(1) cast literal — same trap as bare `1`, just dressed in a cast.
func TestDetectsNumericPersonChannelTypeWithCast(t *testing.T) {
	f := parse(t, `package x
import "x/config"
func _() {
	_ = &config.MsgSendReq{
		ChannelType: uint8(1),
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
		t.Fatalf("expected 1 numeric-cast PERSONAL detection, got %d", hits)
	}
}

// Numeric literal for GROUP (==2) must NOT trip the lint — GROUP DM is out
// of scope for the PERSONAL builder migration.
func TestIgnoresNumericGroupChannelType(t *testing.T) {
	f := parse(t, `package x
import "x/config"
func _() {
	_ = &config.MsgSendReq{
		ChannelType: 2,
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
		t.Fatalf("expected 0 detections on numeric GROUP literal, got %d", hits)
	}
}

// Numeric literal in a non-MsgSendReq composite must NOT trip the lint
// (defense-in-depth: the type guard in walkComposites must still gate the
// numeric value match).
func TestIgnoresNumericChannelTypeOnUnrelatedStruct(t *testing.T) {
	f := parse(t, `package x
type SomethingElse struct{ ChannelType uint8 }
func _() {
	_ = &SomethingElse{ChannelType: 1}
}
`)
	hits := 0
	walkComposites(f, func(typeMatched, valueMatched bool) {
		if typeMatched && valueMatched {
			hits++
		}
	})
	if hits != 0 {
		t.Fatalf("expected 0 detections on unrelated struct, got %d", hits)
	}
}
