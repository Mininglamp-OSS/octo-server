package bot_api

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

func testBotTemplateRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	r := cardtmpl.NewRegistry()
	r.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRoot)
	r.SetDefault(aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersion)
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	return r
}

func testReasoningData(t *testing.T, state string) json.RawMessage {
	t.Helper()
	b, err := aireasoningprocess.Assets.ReadFile(
		aireasoningprocess.HandoffRoot + "/samples/" + state + ".json",
	)
	if err != nil {
		t.Fatalf("read reasoning sample %q: %v", state, err)
	}
	return json.RawMessage(b)
}

func TestBotCardTemplateCatalogCapability(t *testing.T) {
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatalf("newBotCardTemplateCatalog: %v", err)
	}

	got := catalog.Capability()
	if !got.Supported || got.Wire != botTemplateWireV1 {
		t.Fatalf("capability header = %+v", got)
	}
	if len(got.Templates) != 1 {
		t.Fatalf("templates len = %d, want 1", len(got.Templates))
	}
	tpl := got.Templates[0]
	if tpl.ID != string(aireasoningprocess.TemplateID) || tpl.Version != aireasoningprocess.TemplateVersion {
		t.Fatalf("template = %+v", tpl)
	}
	wantViews := []botTemplateViewCapability{
		{Name: "active", States: []string{"answering", "reasoning"}, WireProfile: "octo/v2", SubmitActions: []string{"reasoning_stop"}},
		{Name: "error", States: []string{"error"}, WireProfile: "octo/v2", SubmitActions: []string{"reasoning_retry"}},
		{Name: "result", States: []string{"completed", "stopped"}, WireProfile: "octo/v1", SubmitActions: []string{}},
	}
	if !reflect.DeepEqual(tpl.Views, wantViews) {
		t.Fatalf("views = %#v, want %#v", tpl.Views, wantViews)
	}

	// Capability returns a defensive copy: a caller cannot mutate the catalog.
	got.Templates[0].Views[0].States[0] = "tampered"
	if again := catalog.Capability().Templates[0].Views[0].States[0]; again != "answering" {
		t.Fatalf("catalog capability mutated through response: %q", again)
	}
}

func TestBotCardTemplateCatalogFailsClosed(t *testing.T) {
	registry := testBotTemplateRegistry(t)
	tests := []struct {
		name string
		refs []botTemplateRef
	}{
		{
			name: "duplicate",
			refs: []botTemplateRef{
				{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersion},
				{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersion},
			},
		},
		{
			name: "missing registry entry",
			refs: []botTemplateRef{{ID: "ai.missing", Version: "1.0.0"}},
		},
		{
			name: "implicit version forbidden",
			refs: []botTemplateRef{{ID: aireasoningprocess.TemplateID}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newBotCardTemplateCatalog(registry, tc.refs); err == nil {
				t.Fatal("expected fail-closed catalog error")
			}
		})
	}
}

func TestBotCardTemplateCatalogRenderPayload(t *testing.T) {
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"type": 17,
		"template_ref": map[string]any{
			"id":      string(aireasoningprocess.TemplateID),
			"version": aireasoningprocess.TemplateVersion,
		},
		"state": "reasoning",
		"data":  rawJSONToMap(t, testReasoningData(t, "reasoning")),
		"mention": map[string]any{
			"uids": []any{"u-1"},
		},
		"reply": map[string]any{"message_id": "100"},
	}

	got, err := catalog.RenderPayload(context.Background(), payload, cardtmpl.BuildEnv{
		Lang: "zh-CN", SpaceID: "space-1",
	})
	if err != nil {
		t.Fatalf("RenderPayload: %v", err)
	}
	if got["profile"] != "octo/v2" || got["render_profile"] != "octo-chat/v1" {
		t.Fatalf("profiles = profile:%v render:%v", got["profile"], got["render_profile"])
	}
	if !reflect.DeepEqual(got["template_ref"], payload["template_ref"]) {
		t.Fatalf("server-authored template_ref = %#v", got["template_ref"])
	}
	if _, ok := got["data"]; ok {
		t.Fatal("data must not reach the wire payload")
	}
	if !reflect.DeepEqual(got["mention"], payload["mention"]) || !reflect.DeepEqual(got["reply"], payload["reply"]) {
		t.Fatalf("orthogonal envelope fields not preserved: %#v", got)
	}
	card, _ := got["card"].(map[string]any)
	metadata, _ := card["metadata"].(map[string]any)
	octo, _ := metadata["octo"].(map[string]any)
	template, _ := octo["template"].(map[string]any)
	if template["id"] != string(aireasoningprocess.TemplateID) || template["version"] != aireasoningprocess.TemplateVersion {
		t.Fatalf("authoritative template metadata = %#v", template)
	}
}

func TestBotCardTemplateCatalogInvalidAndInternalRenderClassification(t *testing.T) {
	registry := testBotTemplateRegistry(t)
	catalog, err := newBotCardTemplateCatalog(registry, defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	if got := (*botCardTemplateCatalog)(nil).Capability(); got.Supported || got.Wire != botTemplateWireV1 || got.Templates == nil {
		t.Fatalf("nil catalog capability = %+v", got)
	}
	if _, err := newBotCardTemplateCatalog(registry, nil); err == nil {
		t.Fatal("empty catalog must fail closed")
	}
	if _, err := newBotCardTemplateCatalog(nil, defaultBotTemplateRefs()); !errors.Is(err, errBotTemplateCatalogUnavailable) {
		t.Fatalf("nil registry error = %v", err)
	}

	base := func() map[string]any {
		return map[string]any{
			"type": 17,
			"template_ref": map[string]any{
				"id": string(aireasoningprocess.TemplateID), "version": aireasoningprocess.TemplateVersion,
			},
			"state": "reasoning",
			"data":  rawJSONToMap(t, testReasoningData(t, "reasoning")),
		}
	}
	invalid := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong type", mutate: func(p map[string]any) { p["type"] = 1 }},
		{name: "missing ref", mutate: func(p map[string]any) { delete(p, "template_ref") }},
		{name: "extra ref field", mutate: func(p map[string]any) { p["template_ref"].(map[string]any)["view"] = "active" }},
		{name: "blank state", mutate: func(p map[string]any) { p["state"] = " " }},
		{name: "schema invalid", mutate: func(p map[string]any) { delete(p["data"].(map[string]any), "title") }},
		{name: "unknown state", mutate: func(p map[string]any) {
			p["state"] = "unknown"
			p["data"].(map[string]any)["state"] = "unknown"
		}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			payload := base()
			tc.mutate(payload)
			if _, err := catalog.RenderPayload(context.Background(), payload, cardtmpl.BuildEnv{}); !errors.Is(err, errBotTemplateRequestInvalid) {
				t.Fatalf("error = %v, want request invalid", err)
			}
		})
	}

	// Schema-valid markdown reaches cardmsg.Validate and is a post-build render
	// failure, not a caller-schema error. It remains zero-write and maps to the
	// generic internal facade until the bounded successor schema lands.
	malicious := base()
	malicious["data"].(map[string]any)["progressText"] = "[tap](javascript:alert(1))"
	_, err = catalog.RenderPayload(context.Background(), malicious, cardtmpl.BuildEnv{})
	if err == nil || errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("post-build error classification = %v", err)
	}
}

func TestBotCardTemplateCatalogRejectsCallerControlledInvalidModes(t *testing.T) {
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	valid := func() map[string]any {
		return map[string]any{
			"type": float64(17),
			"template_ref": map[string]any{
				"id": string(aireasoningprocess.TemplateID), "version": aireasoningprocess.TemplateVersion,
			},
			"state": "reasoning",
			"data":  rawJSONToMap(t, testReasoningData(t, "reasoning")),
		}
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "registered but unlisted", mutate: func(p map[string]any) {
			p["template_ref"] = map[string]any{"id": string(docsaccessrequest.TemplateID), "version": docsaccessrequest.TemplateVersionV3}
		}},
		{name: "raw and registry both present", mutate: func(p map[string]any) { p["card"] = map[string]any{"type": "AdaptiveCard"} }},
		{name: "state mismatch", mutate: func(p map[string]any) { p["state"] = "completed" }},
		{name: "render owned plain", mutate: func(p map[string]any) { p["plain"] = "forged" }},
		{name: "render owned profile", mutate: func(p map[string]any) { p["profile"] = "octo/v1" }},
		{name: "render owned space", mutate: func(p map[string]any) { p["space_id"] = "forged" }},
		{name: "unknown envelope field", mutate: func(p map[string]any) { p["content"] = "smuggled" }},
		{name: "data must be object", mutate: func(p map[string]any) { p["data"] = []any{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := valid()
			tc.mutate(p)
			if _, err := catalog.RenderPayload(context.Background(), p, cardtmpl.BuildEnv{}); !errors.Is(err, errBotTemplateRequestInvalid) {
				t.Fatalf("error = %v, want errBotTemplateRequestInvalid", err)
			}
		})
	}
}

func TestEffectiveCardTemplateIdentity(t *testing.T) {
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := catalog.RenderPayload(context.Background(), map[string]any{
		"type": float64(17),
		"template_ref": map[string]any{
			"id": string(aireasoningprocess.TemplateID), "version": aireasoningprocess.TemplateVersion,
		},
		"state": "reasoning",
		"data":  rawJSONToMap(t, testReasoningData(t, "reasoning")),
	}, cardtmpl.BuildEnv{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := botTemplateRef{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersion}
	if err := requireEffectiveCardTemplate(raw, want); err != nil {
		t.Fatalf("requireEffectiveCardTemplate: %v", err)
	}
	delete(payload, "template_ref")
	metadataOnly, _ := json.Marshal(payload)
	if err := requireEffectiveCardTemplate(metadataOnly, want); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("metadata-only target error = %v", err)
	}
	if err := requireEffectiveCardTemplate(raw, botTemplateRef{ID: want.ID, Version: "9.9.9"}); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("mismatch error = %v", err)
	}
	if err := requireEffectiveCardTemplate([]byte(`{"type":17,"card":{}}`), want); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("non-registry error = %v", err)
	}
}

func rawJSONToMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return out
}
