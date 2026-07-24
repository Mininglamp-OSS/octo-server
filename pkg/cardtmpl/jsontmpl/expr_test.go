package jsontmpl

import "testing"

// identity escaper for tests that don't exercise markdown escaping.
func noEscape(s string) string { return s }

// markdownish is a tiny stand-in for cardtmpl.escapeMarkdown so the jsontmpl
// package stays dependency-free; it escapes only the two metacharacters the
// escaping tests below rely on.
func markdownish(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '*' || r == '_' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}

func TestEvalValue_PlainLiteral(t *testing.T) {
	got, err := EvalValue("Hello world", Scope{}, noEscape)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "Hello world" {
		t.Fatalf("got %#v, want %q", got, "Hello world")
	}
}

func TestEvalValue_WholeStringField_ReturnsString(t *testing.T) {
	sc := Scope{Data: map[string]any{"title": "已深度思考"}}
	got, err := EvalValue("${title}", sc, noEscape)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "已深度思考" {
		t.Fatalf("got %#v, want %q", got, "已深度思考")
	}
}

func TestEvalValue_WholeStringBool_ReturnsNativeBool(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  any
		want bool
	}{
		{"true", true, true},
		{"false", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := Scope{Data: map[string]any{"traceExpanded": tc.val}}
			got, err := EvalValue("${traceExpanded}", sc, noEscape)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			b, ok := got.(bool)
			if !ok {
				t.Fatalf("got %T (%#v), want bool", got, got)
			}
			if b != tc.want {
				t.Fatalf("got %v, want %v", b, tc.want)
			}
		})
	}
}

func TestEvalValue_Interpolation_LiteralPlusData(t *testing.T) {
	sc := Scope{Data: map[string]any{"title": "已深度思考"}}
	got, err := EvalValue("✦  ${title}", sc, noEscape)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "✦  已深度思考" {
		t.Fatalf("got %#v, want %q", got, "✦  已深度思考")
	}
}

func TestEvalValue_IfIndexZero_StringBranches(t *testing.T) {
	cases := []struct {
		name  string
		index int
		expr  string
		want  string
	}{
		{"index0->None", 0, "${if($index == 0, 'None', 'Large')}", "None"},
		{"index2->Large", 2, "${if($index == 0, 'None', 'Large')}", "Large"},
		{"index0->None-small", 0, "${if($index == 0, 'None', 'Small')}", "None"},
		{"index1->Small", 1, "${if($index == 0, 'None', 'Small')}", "Small"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := Scope{Index: tc.index, InLoop: true}
			got, err := EvalValue(tc.expr, sc, noEscape)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %#v, want %q", got, tc.want)
			}
		})
	}
}

func TestEvalValue_IfIndexZero_BoolBranches(t *testing.T) {
	cases := []struct {
		index int
		want  bool
	}{
		{0, false},
		{1, true},
	}
	for _, tc := range cases {
		sc := Scope{Index: tc.index, InLoop: true}
		got, err := EvalValue("${if($index == 0, false, true)}", sc, noEscape)
		if err != nil {
			t.Fatalf("index %d: unexpected err: %v", tc.index, err)
		}
		b, ok := got.(bool)
		if !ok {
			t.Fatalf("index %d: got %T, want bool", tc.index, got)
		}
		if b != tc.want {
			t.Fatalf("index %d: got %v, want %v", tc.index, b, tc.want)
		}
	}
}

func TestEvalValue_EscapesData_WholeString(t *testing.T) {
	sc := Scope{Data: map[string]any{"detail": "a*b_c"}}
	got, err := EvalValue("${detail}", sc, markdownish)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != `a\*b\_c` {
		t.Fatalf("got %#v, want %q", got, `a\*b\_c`)
	}
}

func TestEvalValue_EscapesDataButNotLiteral_Interpolation(t *testing.T) {
	// The literal prefix "*# " is template-authored (trusted) and must NOT be
	// escaped; only the data value ${title} is escaped.
	sc := Scope{Data: map[string]any{"title": "*x*"}}
	got, err := EvalValue("*# ${title}", sc, markdownish)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != `*# \*x\*` {
		t.Fatalf("got %#v, want %q", got, `*# \*x\*`)
	}
}

func TestEvalValue_MissingField_Errors(t *testing.T) {
	if _, err := EvalValue("${nope}", Scope{Data: map[string]any{}}, noEscape); err == nil {
		t.Fatal("expected error for missing field, got nil")
	}
}

func TestEvalValue_IndexOutsideLoop_Errors(t *testing.T) {
	if _, err := EvalValue("${if($index == 0, 'a', 'b')}", Scope{}, noEscape); err == nil {
		t.Fatal("expected error for $index outside loop, got nil")
	}
}

// TestEvalCond_NumericIntVsFloat asserts a condition comparing an int literal to
// a JSON number (float64) still matches — Go's raw `==` is false across the two
// concrete types, which would take the wrong if() branch (PR #654 P3).
func TestEvalCond_NumericIntVsFloat(t *testing.T) {
	sc := Scope{Data: map[string]any{"count": float64(1)}} // JSON numbers unmarshal to float64
	got, err := EvalValue("${if(count == 1, 'yes', 'no')}", sc, noEscape)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got != "yes" {
		t.Fatalf("float64(1) == int(1) should be true, got %#v", got)
	}
}

// TestEvalCond_EqualsInsideStringLiteral asserts the `==` inside a single-quoted
// literal is not mistaken for the comparison operator (PR #654 P3).
func TestEvalCond_EqualsInsideStringLiteral(t *testing.T) {
	sc := Scope{Data: map[string]any{"k": "a==b"}}
	got, err := EvalValue("${if(k == 'a==b', 'match', 'no')}", sc, noEscape)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got != "match" {
		t.Fatalf("quoted '==' literal should compare equal, got %#v", got)
	}
}

// TestEvalCond_UncomparableOperand_Errors asserts comparing an array-valued
// operand with `==` returns a clean error instead of panicking (PR #654 P3).
func TestEvalCond_UncomparableOperand_Errors(t *testing.T) {
	sc := Scope{Data: map[string]any{"arr": []any{1, 2}}}
	if _, err := EvalValue("${if(arr == 1, 'a', 'b')}", sc, noEscape); err == nil {
		t.Fatal("expected error comparing an array operand with ==, got nil")
	}
}
