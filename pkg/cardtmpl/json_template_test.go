package cardtmpl

import (
	"context"
	"embed"
	"encoding/json"
	"strings"
	"testing"
)

//go:embed testdata
var jsonCardTestData embed.FS

// registerJSONResult captures a RegisterJSON attempt without aborting the test
// binary on the register-time panics RegisterJSON uses for fail-close.
func tryRegisterJSON(root string) (rec any) {
	defer func() { rec = recover() }()
	reg := NewRegistry()
	reg.RegisterJSON(jsonCardTestData, root)
	return nil
}

// TestRegisterJSON_MinimalV1_RendersThroughPipeline is the end-to-end wiring
// proof: a JSON-mode v1 card registers, freezes, and Renders through the exact
// same renderCore + cardmsg.Validate pipeline as Go cards. It exercises typed
// bool binding (isVisible), $data repetition, $index, and the D7 no-deep-link
// path (metadata.webUrl omitted).
func TestRegisterJSON_MinimalV1_RendersThroughPipeline(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterJSON(jsonCardTestData, "testdata/test.jsoncard@0.1.0")
	reg.SetDefault("test.jsoncard", "0.1.0")
	reg.Freeze()

	fields := json.RawMessage(`{"title":"冒烟","expanded":true,"rows":[{"label":"a"},{"label":"b"}]}`)
	payload, err := reg.Render(context.Background(), "test.jsoncard", "0.1.0", "shown", fields,
		BuildEnv{Lang: "zh-CN", SpaceID: "s1"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if got := payload["type"]; got == nil {
		t.Fatal("payload missing type")
	}
	if got, _ := payload["profile"].(string); got != profileV1 {
		t.Fatalf("profile = %q, want %q", got, profileV1)
	}
	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatal("payload.card not an object")
	}
	// D7: no deep link → metadata.webUrl omitted, octo still present.
	md, _ := card["metadata"].(map[string]any)
	if _, has := md["webUrl"]; has {
		t.Errorf("metadata.webUrl should be omitted when card has no deep link")
	}
	if _, has := md["octo"]; !has {
		t.Errorf("metadata.octo must be injected")
	}
	body, _ := card["body"].([]any)
	if len(body) != 2 {
		t.Fatalf("body len = %d, want 2 (title + rows_panel)", len(body))
	}
	// typed bool binding: rows_panel.isVisible must be a real JSON bool.
	panel, _ := body[1].(map[string]any)
	vis, ok := panel["isVisible"].(bool)
	if !ok || !vis {
		t.Fatalf("rows_panel.isVisible = %#v, want bool true", panel["isVisible"])
	}
	items, _ := panel["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("rows_panel items = %d, want 2 ($data repeat)", len(items))
	}
	// $index → first row spacing None, second Small.
	first, _ := items[0].(map[string]any)
	if first["spacing"] != "None" {
		t.Errorf("row0 spacing = %v, want None", first["spacing"])
	}
	second, _ := items[1].(map[string]any)
	if second["spacing"] != "Small" {
		t.Errorf("row1 spacing = %v, want Small", second["spacing"])
	}
}

// TestRegisterJSON_C1_RejectsBadFields asserts schema-invalid fields fail with
// ErrFieldsInvalid (C1 zero-delivery), same as Go cards.
func TestRegisterJSON_C1_RejectsBadFields(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterJSON(jsonCardTestData, "testdata/test.jsoncard@0.1.0")
	reg.SetDefault("test.jsoncard", "0.1.0")
	reg.Freeze()

	// missing required "expanded"
	bad := json.RawMessage(`{"title":"x"}`)
	_, err := reg.Render(context.Background(), "test.jsoncard", "0.1.0", "shown", bad,
		BuildEnv{Lang: "zh-CN", SpaceID: "s1"})
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid for schema-invalid fields, got nil")
	}
	if !strings.Contains(err.Error(), ErrFieldsInvalid.Error()) {
		t.Fatalf("want ErrFieldsInvalid, got %v", err)
	}
}

// TestRegisterJSON_FallbackText_DerivesPlain asserts FallbackText compiles the
// view and reduces it to plain text carrying the card's title.
func TestRegisterJSON_FallbackText_DerivesPlain(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterJSON(jsonCardTestData, "testdata/test.jsoncard@0.1.0")
	reg.Freeze()
	tmpl, err := reg.Lookup("test.jsoncard", "0.1.0")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	fields := json.RawMessage(`{"title":"冒烟","expanded":true,"rows":[{"label":"a"}]}`)
	txt, err := tmpl.FallbackText("shown", fields, "zh-CN")
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	if !strings.Contains(txt, "冒烟") {
		t.Fatalf("fallback text %q should contain the title", txt)
	}
}

// TestRegisterJSON_MarkdownLinkInjection_RejectedByValidate proves decision D6's
// security model: the engine binds data literally (no markdown escaping), and a
// caller-injected dangerous markdown link in a text field is caught by the L0
// backstop cardmsg.Validate (positive URL allowlist over TextBlock markdown
// links), which renderCore runs on the compiled card → ErrRenderFailed.
//
// The fields are FULLY valid (rows present) so Build SUCCEEDS and the injected
// link actually reaches renderCore step-8 cardmsg.Validate — the layer under
// test. An earlier version omitted "rows", so Build failed first with
// `field "rows" not found` and the assertion passed vacuously without ever
// exercising Validate (both build failures and Validate failures wrap
// ErrRenderFailed). This test now asserts the cardmsg.Validate wrapper specifically
// and adds a benign-title control proving the link is the sole cause (PR #654 P1).
func TestRegisterJSON_MarkdownLinkInjection_RejectedByValidate(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterJSON(jsonCardTestData, "testdata/test.jsoncard@0.1.0")
	reg.SetDefault("test.jsoncard", "0.1.0")
	reg.Freeze()

	// title carries a javascript: markdown link; schema permits it (plain string),
	// so it must be the render-layer Validate that rejects it. rows/expanded are
	// present so Build succeeds and the link reaches step 8.
	const validTail = `,"expanded":true,"rows":[{"label":"a"}]}`
	malicious := json.RawMessage(`{"title":"[tap](javascript:alert(1))"` + validTail)
	_, err := reg.Render(context.Background(), "test.jsoncard", "0.1.0", "shown", malicious,
		BuildEnv{Lang: "zh-CN", SpaceID: "s1"})
	if err == nil {
		t.Fatal("expected ErrRenderFailed (Validate rejects non-https markdown link), got nil — engine has an injection gap")
	}
	if !strings.Contains(err.Error(), ErrRenderFailed.Error()) {
		t.Fatalf("want ErrRenderFailed, got %v", err)
	}
	// Must be the step-8 cardmsg.Validate rejection, NOT an earlier Build failure —
	// both wrap ErrRenderFailed, so pinning the Validate wrapper is the whole point.
	if !strings.Contains(err.Error(), "cardmsg.Validate") {
		t.Fatalf("injection must be caught by cardmsg.Validate (render step 8), got %v", err)
	}

	// Benign-title control: identical fields, safe title → renders cleanly. Proves
	// the injected link (not a missing/!valid field) is the sole cause above.
	benign := json.RawMessage(`{"title":"tap here"` + validTail)
	if _, err := reg.Render(context.Background(), "test.jsoncard", "0.1.0", "shown", benign,
		BuildEnv{Lang: "zh-CN", SpaceID: "s1"}); err != nil {
		t.Fatalf("benign control must render cleanly, got %v", err)
	}
}

// TestRegisterJSON_ViewMissingTemplate_FailClose asserts a JSON-mode view that
// declares no template path panics at register time (fail-close), never at
// first render.
func TestRegisterJSON_ViewMissingTemplate_FailClose(t *testing.T) {
	rec := tryRegisterJSON("testdata/test.notmpl@0.1.0")
	if rec == nil {
		t.Fatal("expected register-time panic for view without a template path")
	}
	if !strings.Contains(toStr(rec), "no template path") {
		t.Fatalf("want 'no template path' panic, got %v", rec)
	}
}

// TestRegisterJSON_InteractionReportDrift_FailClose locks the production
// guarantee consumed by the Bot template catalog: advertised Submit ids come
// from the interaction report, so a report that differs from the rendered
// sample must fail at registration instead of publishing a false capability.
func TestRegisterJSON_InteractionReportDrift_FailClose(t *testing.T) {
	rec := tryRegisterJSON("testdata/test.reportdrift@0.1.0")
	if rec == nil {
		t.Fatal("expected register-time panic for interaction report drift")
	}
	if !strings.Contains(toStr(rec), "interaction") {
		t.Fatalf("want interaction drift panic, got %v", rec)
	}
}

// TestRegisterJSON_InlineActionInteractionReportDrift_FailClose proves the
// registration guard sees Action.Submit when it is attached to Input.* via
// inlineAction. Without that traversal, this fixture registers successfully
// while publishing an interaction report that omits a real Submit action.
func TestRegisterJSON_InlineActionInteractionReportDrift_FailClose(t *testing.T) {
	rec := tryRegisterJSON("testdata/test.inlineaction-reportdrift@0.1.0")
	if rec == nil {
		t.Fatal("expected register-time panic for inlineAction interaction report drift")
	}
	got := toStr(rec)
	if !strings.Contains(got, "Action.Submit ids") || !strings.Contains(got, "inline_action") {
		t.Fatalf("want inlineAction Submit drift panic, got %v", rec)
	}
}

// TestAssertActionContract_InlineAction keeps the action-contract walker in
// lockstep with the interaction-report walker for the same supported surface.
func TestAssertActionContract_InlineAction(t *testing.T) {
	payload := map[string]any{
		"card": map[string]any{
			"body": []any{
				map[string]any{
					"type": "Input.Text",
					"id":   "query",
					"inlineAction": map[string]any{
						"type": "Action.Submit",
						"id":   "inline_action",
						"data": map[string]any{
							"owner":       "ai",
							"action_type": "reasoning.control",
						},
					},
				},
			},
		},
	}

	err := assertActionContract(payload, TemplateActionContract{
		Owner:      "ai",
		ActionType: "reasoning.control",
	})
	if err != nil {
		t.Fatalf("inlineAction Submit must satisfy action contract: %v", err)
	}
}

func toStr(v any) string {
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return fmtSprint(v)
}
