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
			if !strings.Contains(text, "Q3 产品规划") || !strings.Contains(text, tc.permission) {
				t.Errorf("RenderCard(%s) dropped source/permission context: %s", tc.state, text)
			}
			if !strings.Contains(text, "申请于") || !strings.Contains(text, "处理于 刚刚") {
				t.Errorf("RenderCard(%s) dropped terminal footer times: %s", tc.state, text)
			}
			if !strings.Contains(text, `"text":"林澈"`) {
				t.Errorf("RenderCard(%s) dropped operator below processed time: %s", tc.state, text)
			}
			if strings.Contains(text, "申请人已获得") || strings.Contains(text, `"style":"good"`) {
				t.Errorf("RenderCard(%s) retained duplicate terminal result block: %s", tc.state, text)
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

func TestV3UsesForgeLayout(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	env := cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "space-1"}

	for _, state := range []cardtmpl.State{
		docsaccessrequest.StatePending,
		docsaccessrequest.StateApproved,
		docsaccessrequest.StateRejected,
	} {
		sample, err := docsaccessrequest.Assets.ReadFile(docsaccessrequest.HandoffRootV3 + "/samples/" + string(state) + ".json")
		if err != nil {
			t.Fatal(err)
		}
		payload, err := r.Render(context.Background(), docsaccessrequest.TemplateID, "", state, sample, env)
		if err != nil {
			t.Fatalf("Render(%s): %v", state, err)
		}
		card := payload["card"].(map[string]any)
		body := card["body"].([]any)
		if len(body) != 3 {
			t.Fatalf("Render(%s) top-level body count = %d, want header/content/footer", state, len(body))
		}
		if header := body[0].(map[string]any); header["type"] != "ColumnSet" || header["spacing"] != "None" {
			t.Errorf("Render(%s) header = %+v", state, header)
		}
		assertForgeSurface(t, state, body[1], "accent")
		assertForgeSurface(t, state, body[2], "emphasis")
		if findNodeByID(card, "octo-badge-neutral-permission") == nil {
			t.Errorf("Render(%s) permission semantic badge missing", state)
		}

		if state == docsaccessrequest.StatePending {
			if _, ok := card["actions"]; ok {
				t.Error("pending Forge layout must keep actions in the inline footer")
			}
			for _, id := range []string{cardtmpl.DocsApproveActionID, cardtmpl.DocsDenyActionID, cardtmpl.DocsDenyReasonInputID} {
				if findNodeByID(card, id) == nil {
					t.Errorf("pending Forge layout missing production id %q", id)
				}
			}
			input := findNodeByID(card, cardtmpl.DocsDenyReasonInputID)
			if input["isVisible"] != false || input["maxLength"] != float64(cardtmpl.MaxExcerptRunes) && input["maxLength"] != cardtmpl.MaxExcerptRunes {
				t.Errorf("deny input contract drift: %+v", input)
			}
		} else {
			if findNodeByID(card, cardtmpl.DocsApproveActionID) != nil || findNodeByID(card, cardtmpl.DocsDenyActionID) != nil {
				t.Errorf("terminal Forge layout retained decision actions for state %s", state)
			}
			if findNodeByID(card, "approval_actions") != nil {
				t.Errorf("terminal Forge layout retained pending action-set id for state %s", state)
			}
		}
	}
}

func assertForgeSurface(t *testing.T, state cardtmpl.State, value any, style string) {
	t.Helper()
	node := value.(map[string]any)
	if node["type"] != "Container" || node["style"] != style || node["bleed"] != true ||
		node["separator"] != true || node["spacing"] != "None" {
		t.Errorf("Render(%s) %s surface = %+v", state, style, node)
	}
}

func findNodeByID(value any, id string) map[string]any {
	switch node := value.(type) {
	case map[string]any:
		if node["id"] == id {
			return node
		}
		for _, child := range node {
			if found := findNodeByID(child, id); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range node {
			if found := findNodeByID(child, id); found != nil {
				return found
			}
		}
	}
	return nil
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
	if meta.RenderProfileCompatibility != cardmsg.RenderProfileOctoChatV1 {
		t.Fatalf("Meta() render profile compatibility = %q", meta.RenderProfileCompatibility)
	}
	var manifest struct {
		RenderProfile string `json:"renderProfile"`
	}
	if err := json.Unmarshal(meta.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.RenderProfile != "octo-chat@1.2.0-rc.1" {
		t.Fatalf("manifest renderProfile = %q", manifest.RenderProfile)
	}
}

func TestV3UsesStableRenderProfileWithoutChangingV2(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	env := cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "space-1"}
	v3Sample, err := docsaccessrequest.Assets.ReadFile(docsaccessrequest.HandoffRootV3 + "/samples/pending.json")
	if err != nil {
		t.Fatal(err)
	}
	v3, err := r.Render(context.Background(), docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3,
		docsaccessrequest.StatePending, v3Sample, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := v3["render_profile"]; got != cardmsg.RenderProfileOctoChatV1 {
		t.Errorf("0.3.0 render_profile = %v", got)
	}

	v2Sample, err := docsaccessrequest.Assets.ReadFile(docsaccessrequest.HandoffRoot + "/samples/pending.json")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := r.Render(context.Background(), docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion,
		docsaccessrequest.StatePending, v2Sample, env)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v2["render_profile"]; ok {
		t.Error("frozen 0.2.0 must remain on the legacy renderer")
	}
}

func TestV3PendingRendersRequestedBotNamesAndSourceSpaceWithoutEmptyReason(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	fields := json.RawMessage(`{
		"requestId":"r1","state":"pending",
		"document":{"docId":"d1","title":"测试"},
		"requester":{"name":"超级管理员","sourceSpaceName":"123"},
		"permission":{"roleLabel":"可评论"},
		"requestReason":"",
		"requestedBotNames":["小呆呆","猪猪侠"]
	}`)
	payload, err := r.Render(context.Background(), docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3, docsaccessrequest.StatePending, fields,
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "space-1"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{"申请者：", "超级管理员", "123", "申请权限：", "可评论", "同时申请权限的 AI 助手（2）", "小呆呆、猪猪侠"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "申请原因") {
		t.Fatalf("empty reason section rendered: %s", text)
	}
	if strings.Contains(text, "申请成为此文档") {
		t.Fatalf("duplicate request banner rendered: %s", text)
	}
	if strings.Contains(text, "?sp=") {
		t.Fatalf("Docs deep link retained legacy sp: %s", text)
	}
}

func TestV3PendingCapsVisibleBotNames(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	fields := json.RawMessage(`{
		"requestId":"r-many","state":"pending",
		"document":{"docId":"d-many","title":"测试"},
		"requester":{"name":"申请人","sourceSpaceName":"test Space"},
		"permission":{"roleLabel":"可评论"},
		"requestedBotNames":["助手1","助手2","助手3","助手4","助手5","助手6","助手7","助手8","助手9","助手10"]
	}`)
	payload, err := r.Render(context.Background(), docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3,
		docsaccessrequest.StatePending, fields,
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "space-1"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{"test Space", "申请权限：", "可评论", "同时申请权限的 AI 助手（10）", "等 3 个"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card missing %q: %s", want, text)
		}
	}
	// Full names remain in server-authored submit data so terminal cards preserve
	// context; only the visible TextBlock is capped.
	if !strings.Contains(text, `"text":"助手1、助手2、助手3、助手4、助手5、助手6、助手7，等 3 个"`) ||
		!strings.Contains(text, `"maxLines":2`) {
		t.Fatalf("visible Bot summary was not capped to two lines: %s", text)
	}
}

func TestV3PendingTruncatesSingleOversizedBotNameToOneLine(t *testing.T) {
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	const longName = "这是一个非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常非常长的助手名称"
	fields := json.RawMessage(`{
		"requestId":"r-long","state":"pending",
		"document":{"docId":"d-long","title":"测试"},
		"requester":{"name":"申请人"},
		"permission":{"roleLabel":"只读"},
		"requestedBotNames":["` + longName + `"]
	}`)
	payload, err := r.Render(context.Background(), docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3,
		docsaccessrequest.StatePending, fields,
		cardtmpl.BuildEnv{WebLoginURL: "https://web.example.com", Lang: "zh-CN", SpaceID: "space-1"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, `非常非常非常非…`) || !strings.Contains(text, `"maxLines":2`) {
		t.Fatalf("oversized Bot name was not capped to two visible lines: %s", text)
	}
	if !strings.Contains(text, `"requested_bot_names":["`+longName+`"]`) {
		t.Fatalf("full Bot name was not preserved in action data: %s", text)
	}
}
