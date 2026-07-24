package jsontmpl

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// bigDataArray builds a $data-driven template plus a data array of n items, used
// by the expansion-budget and cancellation tests.
func bigDataArray(n int) (any, Scope) {
	tmpl := map[string]any{
		"items": []any{
			map[string]any{"$data": "${rows}", "type": "TextBlock", "text": "${label}"},
		},
	}
	rows := make([]any, n)
	for i := range rows {
		rows[i] = map[string]any{"label": "x"}
	}
	return tmpl, Scope{Data: map[string]any{"rows": rows}}
}

// TestExpand_UnboundedDataArray_FailsFast asserts a $data array far larger than
// maxExpandNodes is rejected with a bounded budget error rather than expanded in
// full (DoS backstop, PR #654).
func TestExpand_UnboundedDataArray_FailsFast(t *testing.T) {
	tmpl, sc := bigDataArray(maxExpandNodes * 3)
	_, err := Expand(context.Background(), tmpl, sc, noEscape)
	if err == nil {
		t.Fatal("expected expansion-budget error for an oversized $data array")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("want a budget error, got %v", err)
	}
}

// TestExpand_ContextCancelled_Stops asserts a cancelled ctx interrupts expansion
// (R2-7 context propagation, PR #654).
func TestExpand_ContextCancelled_Stops(t *testing.T) {
	tmpl, sc := bigDataArray(1000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: the very first node check must bail out
	_, err := Expand(ctx, tmpl, sc, noEscape)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// parseJSON is a test helper: unmarshal a template/data literal into any.
func parseJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("parseJSON: %v", err)
	}
	return v
}

func dataMap(t *testing.T, s string) map[string]any {
	t.Helper()
	v := parseJSON(t, s)
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("dataMap: not an object: %T", v)
	}
	return m
}

// canonical marshals with sorted keys (encoding/json sorts map keys) so tests
// compare structure, not key order.
func canonical(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestExpand_FieldBinding(t *testing.T) {
	tmpl := parseJSON(t, `{"type":"TextBlock","text":"${title}"}`)
	data := dataMap(t, `{"title":"hello"}`)
	got, err := Expand(context.Background(), tmpl, Scope{Data: data}, noEscape)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := parseJSON(t, `{"type":"TextBlock","text":"hello"}`)
	if canonical(t, got) != canonical(t, want) {
		t.Fatalf("got %s want %s", canonical(t, got), canonical(t, want))
	}
}

func TestExpand_TypedBoolBinding(t *testing.T) {
	tmpl := parseJSON(t, `{"type":"Container","isVisible":"${traceExpanded}"}`)
	data := dataMap(t, `{"traceExpanded":false}`)
	got, err := Expand(context.Background(), tmpl, Scope{Data: data}, noEscape)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	// isVisible must be a real JSON boolean, not the string "false".
	want := parseJSON(t, `{"type":"Container","isVisible":false}`)
	if canonical(t, got) != canonical(t, want) {
		t.Fatalf("got %s want %s", canonical(t, got), canonical(t, want))
	}
}

func TestExpand_DataArrayRepeatsWithIndex(t *testing.T) {
	tmpl := parseJSON(t, `{
		"type":"Container",
		"items":[
			{"$data":"${phases}","type":"TextBlock","text":"${thought}","spacing":"${if($index == 0, 'None', 'Large')}"},
			{"type":"TextBlock","text":"footer"}
		]
	}`)
	data := dataMap(t, `{"phases":[{"thought":"a"},{"thought":"b"}]}`)
	got, err := Expand(context.Background(), tmpl, Scope{Data: data}, noEscape)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := parseJSON(t, `{
		"type":"Container",
		"items":[
			{"type":"TextBlock","text":"a","spacing":"None"},
			{"type":"TextBlock","text":"b","spacing":"Large"},
			{"type":"TextBlock","text":"footer"}
		]
	}`)
	if canonical(t, got) != canonical(t, want) {
		t.Fatalf("\n got %s\nwant %s", canonical(t, got), canonical(t, want))
	}
}

func TestExpand_NestedDataScopes(t *testing.T) {
	tmpl := parseJSON(t, `{
		"$data":"${phases}",
		"type":"Container",
		"items":[
			{"$data":"${actions}","type":"TextBlock","text":"${tool}"}
		]
	}`)
	// wrap in an array so the outer $data repetition is observable
	root := []any{tmpl}
	data := dataMap(t, `{"phases":[{"actions":[{"tool":"x"},{"tool":"y"}]}]}`)
	got, err := Expand(context.Background(), root, Scope{Data: data}, noEscape)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := parseJSON(t, `[{
		"type":"Container",
		"items":[
			{"type":"TextBlock","text":"x"},
			{"type":"TextBlock","text":"y"}
		]
	}]`)
	if canonical(t, got) != canonical(t, want) {
		t.Fatalf("\n got %s\nwant %s", canonical(t, got), canonical(t, want))
	}
}

func TestExpand_DataNotArray_Errors(t *testing.T) {
	tmpl := parseJSON(t, `{"items":[{"$data":"${notarr}","type":"X"}]}`)
	data := dataMap(t, `{"notarr":"scalar"}`)
	if _, err := Expand(context.Background(), tmpl, Scope{Data: data}, noEscape); err == nil {
		t.Fatal("expected error when $data does not resolve to an array")
	}
}

func TestExpand_StaticPassthrough(t *testing.T) {
	tmpl := parseJSON(t, `{"type":"AdaptiveCard","version":"1.5","bleed":true,"spacing":"None"}`)
	got, err := Expand(context.Background(), tmpl, Scope{Data: map[string]any{}}, noEscape)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if canonical(t, got) != canonical(t, tmpl) {
		t.Fatalf("got %s want %s", canonical(t, got), canonical(t, tmpl))
	}
}

func TestExpand_EscapesDataLeaf(t *testing.T) {
	tmpl := parseJSON(t, `{"text":"${detail}"}`)
	data := dataMap(t, `{"detail":"a*b"}`)
	got, err := Expand(context.Background(), tmpl, Scope{Data: data}, markdownish)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := parseJSON(t, `{"text":"a\\*b"}`)
	if canonical(t, got) != canonical(t, want) {
		t.Fatalf("got %s want %s", canonical(t, got), canonical(t, want))
	}
}
