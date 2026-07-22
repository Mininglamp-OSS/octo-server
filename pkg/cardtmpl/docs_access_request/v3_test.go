package docsaccessrequest_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

func TestV3RegistersPendingAndResultWithoutMutatingV2(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()

	if got := docsaccessrequest.TemplateVersion; got != "0.2.0" {
		t.Fatalf("frozen TemplateVersion = %q, want 0.2.0", got)
	}
	env := cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "space-1"}
	for _, tc := range []struct {
		state      cardtmpl.State
		profile    string
		variant    string
		permission string
	}{
		{docsaccessrequest.StatePending, cardmsg.ProfileV2, docsaccessrequest.Variant, ""},
		{docsaccessrequest.StateApproved, cardmsg.ProfileV1, docsaccessrequest.VariantApproved, "协作权限"},
		{docsaccessrequest.StateRejected, cardmsg.ProfileV1, docsaccessrequest.VariantRejected, "查看权限"},
	} {
		sample, err := docsaccessrequest.Assets.ReadFile(docsaccessrequest.HandoffRootV3 + "/samples/" + string(tc.state) + ".json")
		if err != nil {
			t.Fatalf("read sample %s: %v", tc.state, err)
		}
		card, profile, err := r.RenderCard(context.Background(), docsaccessrequest.TemplateID, "", tc.state, sample, env)
		if err != nil {
			t.Fatalf("RenderCard(%s): %v", tc.state, err)
		}
		if profile != tc.profile {
			t.Errorf("RenderCard(%s) profile = %q, want %q", tc.state, profile, tc.profile)
		}
		var document map[string]any
		if err := json.Unmarshal(card, &document); err != nil {
			t.Fatalf("decode %s card: %v", tc.state, err)
		}
		metadata, _ := document["metadata"].(map[string]any)
		octo, _ := metadata["octo"].(map[string]any)
		template, _ := octo["template"].(map[string]any)
		if template["version"] != docsaccessrequest.TemplateVersionV3 {
			t.Errorf("RenderCard(%s) template.version = %v", tc.state, template["version"])
		}
		if octo["variant"] != tc.variant {
			t.Errorf("RenderCard(%s) variant = %v, want %q", tc.state, octo["variant"], tc.variant)
		}
		if tc.profile == cardmsg.ProfileV1 {
			if _, ok := document["actions"]; ok {
				t.Errorf("RenderCard(%s) result view must not contain top-level actions", tc.state)
			}
			text := string(card)
			if !strings.Contains(text, "申请原因") || !strings.Contains(text, "林澈") {
				t.Errorf("RenderCard(%s) dropped result handoff context: %s", tc.state, text)
			}
			if !strings.Contains(text, "Q3 产品规划") || !strings.Contains(text, tc.permission) ||
				!strings.Contains(text, "处理于 刚刚") {
				t.Errorf("RenderCard(%s) dropped source/permission context: %s", tc.state, text)
			}
			if tc.state == docsaccessrequest.StateRejected && !strings.Contains(text, "信息不完整") {
				t.Errorf("RenderCard(rejected) dropped rejection reason: %s", text)
			}
		}
	}
}

func TestV3ResultAcceptsPublishedV2ActionDataMinimum(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	fields := json.RawMessage(`{
		"requestId":"request-1","state":"rejected",
		"document":{"docId":"doc-1","title":"Roadmap"},
		"requester":{"name":"Alice"},
		"decision":{"operatorName":"reviewer-1","rejectionReason":"scope mismatch"}
	}`)
	card, profile, err := r.RenderCard(context.Background(), docsaccessrequest.TemplateID, "",
		docsaccessrequest.StateRejected, fields,
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "en", SpaceID: "space-1"})
	if err != nil {
		t.Fatalf("RenderCard(minimum legacy fields): %v", err)
	}
	if profile != cardmsg.ProfileV1 {
		t.Fatalf("profile = %q, want %q", profile, cardmsg.ProfileV1)
	}
	if len(card) == 0 {
		t.Fatal("result card is empty")
	}
}

func TestV3PendingCarriesResultContextInServerAuthoredActionData(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	sample, err := docsaccessrequest.Assets.ReadFile(docsaccessrequest.HandoffRootV3 + "/samples/pending.json")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := r.Render(context.Background(), docsaccessrequest.TemplateID, "", docsaccessrequest.StatePending, sample,
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "space-1"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(payload)
	data, found := cardmsg.SubmitAction(raw, cardtmpl.DocsApproveActionID)
	if !found {
		t.Fatal("approve action missing")
	}
	for _, key := range []string{"actor_avatar_url", "request_reason", "requested_at_display", "message_time_display", "permission_label", "permission_role_label", "source_name"} {
		if _, ok := data[key]; !ok {
			t.Errorf("V3 action data missing %q: %+v", key, data)
		}
	}
}

func TestV3FallbackTextLocalizesResultsAndRejectsUnknownState(t *testing.T) {
	tmpl := docsaccessrequest.NewV3()
	fields := json.RawMessage(`{
		"requestId":"request-1","state":"approved",
		"document":{"docId":"doc-1","title":"Roadmap"},
		"decision":{"operatorName":"reviewer-1"}
	}`)
	text, err := tmpl.FallbackText(docsaccessrequest.StateApproved, fields, "en")
	if err != nil {
		t.Fatalf("FallbackText(approved): %v", err)
	}
	if text != "Roadmap - Approved" {
		t.Fatalf("FallbackText(approved) = %q", text)
	}
	if _, err := tmpl.FallbackText(cardtmpl.State("future"), fields, "en"); err == nil {
		t.Fatal("FallbackText(unknown state) error = nil")
	}
	if _, err := tmpl.FallbackText(docsaccessrequest.StateRejected, json.RawMessage(`{`), "en"); err == nil {
		t.Fatal("FallbackText(invalid fields) error = nil")
	}
}

func TestV3MetaIsPopulatedByRegistry(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	tmpl, err := r.Lookup(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	if err != nil {
		t.Fatal(err)
	}
	meta := tmpl.Meta()
	if meta.ID != docsaccessrequest.TemplateID || meta.Version != docsaccessrequest.TemplateVersionV3 {
		t.Fatalf("Meta() = %+v", meta)
	}
}
