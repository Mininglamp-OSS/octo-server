package bot_api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/gin-gonic/gin"
)

func TestSendMessageRegistryTemplateRendersAndDispatches(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	gin.SetMode(gin.TestMode)

	dispatch := &dispatchCapture{}
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-template-send"),
		cardTemplates:    catalog,
		cardConfig:       allCardCapabilitiesOn(),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
		dispatchOverride: dispatch.hook,
	}
	body := registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))
	payload := body["payload"].(map[string]any)
	payload["mention"] = map[string]any{"uids": []any{"u-1"}}
	payload["reply"] = map[string]any{"message_id": "100"}

	recorder := invokeTemplateSend(t, ba, body, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	dispatch.mu.Lock()
	captured := dispatch.captured
	dispatch.mu.Unlock()
	if captured == nil {
		t.Fatal("expected exactly one dispatch")
	}
	var wire map[string]any
	if err := json.Unmarshal(captured.Payload, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["profile"] != "octo/v2" || wire["render_profile"] != "octo-chat/v1" {
		t.Fatalf("profiles = %v / %v", wire["profile"], wire["render_profile"])
	}
	if wire["space_id"] != "space-authoritative" {
		t.Fatalf("space_id = %v", wire["space_id"])
	}
	if plain, _ := wire["plain"].(string); plain == "" {
		t.Fatal("server-authoritative plain missing")
	}
	if ref, ok := wire["template_ref"].(map[string]any); !ok || ref["id"] != string(aireasoningprocess.TemplateID) || ref["version"] != aireasoningprocess.TemplateVersion {
		t.Fatalf("server-authored template_ref missing: %#v", wire["template_ref"])
	}
	if _, ok := wire["data"]; ok {
		t.Fatal("template data leaked to outbound payload")
	}
	if _, ok := wire["mention"].(map[string]any); !ok {
		t.Fatalf("mention not preserved: %#v", wire["mention"])
	}
	if _, ok := wire["reply"].(map[string]any); !ok {
		t.Fatalf("reply not preserved: %#v", wire["reply"])
	}
	for _, forbidden := range [][]byte{
		[]byte("Action.Submit"), []byte("reasoning_stop"), []byte("reasoning_retry"),
		[]byte("stop_reasoning"), []byte("retry_reasoning"), []byte("可以重试"),
	} {
		if bytes.Contains(captured.Payload, forbidden) {
			t.Fatalf("new-send payload contains unsupported control %q", forbidden)
		}
	}
}

func TestSendMessageRawCardAuthorsRenderProfile(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name          string
		renderProfile any
		wantStatus    int
	}{
		{name: "omitted by caller", wantStatus: http.StatusOK},
		{name: "matching legacy input remains compatible", renderProfile: cardmsg.RenderProfileOctoChatV1, wantStatus: http.StatusOK},
		{name: "invalid legacy input still fails closed", renderProfile: "octo-chat/v2", wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatch := &dispatchCapture{}
			ba := &BotAPI{
				Log:              log.NewTLog("BotAPI-raw-card-render-profile"),
				cardConfig:       allCardCapabilitiesOn(),
				spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
				dispatchOverride: dispatch.hook,
			}
			payload := map[string]any{
				"type":         cardmsg.InteractiveCard.Int(),
				"profile":      cardmsg.ProfileV1,
				"card_version": cardmsg.CardVersion,
				"card": map[string]any{
					"type":    "AdaptiveCard",
					"version": cardmsg.CardVersion,
					"body": []any{
						map[string]any{"type": "TextBlock", "text": "server authored render profile"},
					},
				},
			}
			if tc.renderProfile != nil {
				payload["render_profile"] = tc.renderProfile
			}
			body := map[string]any{
				"channel_id":   "user_creator",
				"channel_type": common.ChannelTypePerson.Uint8(),
				"payload":      payload,
			}

			recorder := invokeTemplateSend(t, ba, body, "")
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status=%d, want=%d body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			dispatch.mu.Lock()
			captured := dispatch.captured
			dispatch.mu.Unlock()
			if tc.wantStatus != http.StatusOK {
				if captured != nil {
					t.Fatal("invalid render_profile dispatched a message")
				}
				return
			}
			if captured == nil {
				t.Fatal("expected exactly one dispatch")
			}
			var wire map[string]any
			if err := json.Unmarshal(captured.Payload, &wire); err != nil {
				t.Fatal(err)
			}
			if got := wire["render_profile"]; got != cardmsg.RenderProfileOctoChatV1 {
				t.Fatalf("render_profile = %v, want %q", got, cardmsg.RenderProfileOctoChatV1)
			}
		})
	}
}

func TestBotRawCardEditRenderProfilePreservesLargeCardSeq(t *testing.T) {
	const contentEdit = `{"type":17,"profile":"octo/v2","card_version":"1.5","card_seq":9007199254740993,"card":{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"progress"}]}}`
	normalized, err := cardmsg.NormalizeContentEdit(contentEdit)
	if err != nil {
		t.Fatal(err)
	}
	authored, err := authorBotRawCardRenderProfile(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := cardmsg.CardSeqFromContentEdit(authored); !ok || got != 9007199254740993 {
		t.Fatalf("card_seq = %d, ok=%v; want 9007199254740993", got, ok)
	}
	var wire map[string]any
	if err := json.Unmarshal([]byte(authored), &wire); err != nil {
		t.Fatal(err)
	}
	if got := wire["render_profile"]; got != cardmsg.RenderProfileOctoChatV1 {
		t.Fatalf("render_profile = %v, want %q", got, cardmsg.RenderProfileOctoChatV1)
	}
}

func TestSendMessageRegistryTemplateRejectsHistoricalVersionsWithoutDispatch(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	gin.SetMode(gin.TestMode)

	for _, historical := range []struct {
		version string
		root    string
	}{
		{aireasoningprocess.TemplateVersionV1, aireasoningprocess.HandoffRootV1},
		{aireasoningprocess.TemplateVersionV2, aireasoningprocess.HandoffRootV2},
		{aireasoningprocess.TemplateVersionV3, aireasoningprocess.HandoffRootV3},
	} {
		t.Run(historical.version, func(t *testing.T) {
			dispatch := &dispatchCapture{}
			catalog, err := newBotCardTemplateCatalogWithPolicy(testBotTemplateRegistry(t), defaultBotTemplatePolicy())
			if err != nil {
				t.Fatal(err)
			}
			ba := &BotAPI{
				Log:              log.NewTLog("BotAPI-template-send-historical"),
				cardTemplates:    catalog,
				cardConfig:       allCardCapabilitiesOn(),
				spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
				dispatchOverride: dispatch.hook,
			}
			body := registrySendBodyVersion(t, historical.version,
				"reasoning", testReasoningDataVersion(t, historical.root, "reasoning"))

			recorder := invokeTemplateSend(t, ba, body, "")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			dispatch.mu.Lock()
			defer dispatch.mu.Unlock()
			if dispatch.captured != nil {
				t.Fatal("historical template ref dispatched a new message")
			}
		})
	}
}

func TestRawContentEditCannotForgeRegistryProvenance(t *testing.T) {
	raw := imCardEnvelope("raw", 1)
	raw["template_ref"] = map[string]any{
		"id": string(aireasoningprocess.TemplateID), "version": aireasoningprocess.TemplateVersion,
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !contentEditHasCatalogMarker(string(encoded)) {
		t.Fatal("raw content_edit provenance marker was not detected")
	}
	if !cardEnvelopeHasCatalogMarker(encoded) {
		t.Fatal("Registry-authored target provenance marker was not detected")
	}
	if contentEditHasCatalogMarker(string(mustJSON(t, imCardEnvelope("raw", 1)))) {
		t.Fatal("ordinary raw content_edit falsely detected as Registry provenance")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSendMessageRegistryTemplateRejectsInvalidWithoutDispatch(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	gin.SetMode(gin.TestMode)
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "outer data state mismatch", mutate: func(payload map[string]any) { payload["state"] = "completed" }},
		{name: "registered but unlisted", mutate: func(payload map[string]any) {
			payload["template_ref"] = map[string]any{"id": "docs.access-request", "version": "0.3.0"}
		}},
		{name: "raw and registry both present", mutate: func(payload map[string]any) { payload["card"] = map[string]any{} }},
		{name: "render owned field", mutate: func(payload map[string]any) { payload["plain"] = "forged" }},
		{name: "render owned render profile", mutate: func(payload map[string]any) { payload["render_profile"] = "octo-chat/v1" }},
		{name: "aggregate action overflow", mutate: func(payload map[string]any) {
			payload["data"].(map[string]any)["phases"] = reasoningPhasesForBotTest(4, 2, 2, 2, 2, 2)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dispatch := &dispatchCapture{}
			ba := &BotAPI{
				Log:              log.NewTLog("BotAPI-template-send-invalid"),
				cardTemplates:    catalog,
				cardConfig:       allCardCapabilitiesOn(),
				spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
				dispatchOverride: dispatch.hook,
			}
			body := registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))
			tc.mutate(body["payload"].(map[string]any))
			recorder := invokeTemplateSend(t, ba, body, "")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			dispatch.mu.Lock()
			captured := dispatch.captured
			dispatch.mu.Unlock()
			if captured != nil {
				t.Fatal("invalid template request dispatched a message")
			}
		})
	}
}

func TestSendMessageRegistryTemplateKeepsOBOCardExclusion(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-template-send-obo"),
		cardTemplates:    catalog,
		cardConfig:       allCardCapabilitiesOn(),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
		dispatchOverride: dispatch.hook,
	}
	recorder := invokeTemplateSend(t, ba,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning")), "user_creator")
	if recorder.Code != http.StatusBadRequest || !bytes.Contains(recorder.Body.Bytes(), []byte("cannot be sent on behalf")) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	dispatch.mu.Lock()
	defer dispatch.mu.Unlock()
	if dispatch.captured != nil {
		t.Fatal("OBO template request dispatched a message")
	}
}

func registrySendBody(t *testing.T, state string, data json.RawMessage) map[string]any {
	return registrySendBodyVersion(t, aireasoningprocess.TemplateVersion, state, data)
}

func registrySendBodyVersion(t *testing.T, version, state string, data json.RawMessage) map[string]any {
	t.Helper()
	return map[string]any{
		"channel_id":   "user_creator",
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload": map[string]any{
			"type": cardmsg.InteractiveCard.Int(),
			"template_ref": map[string]any{
				"id": string(aireasoningprocess.TemplateID), "version": version,
			},
			"state": state,
			"data":  rawJSONToMap(t, data),
		},
	}
}

func reasoningPhasesForBotTest(actionCounts ...int) []any {
	phases := make([]any, 0, len(actionCounts))
	for _, actionCount := range actionCounts {
		actions := make([]any, 0, actionCount)
		for i := 0; i < actionCount; i++ {
			actions = append(actions, map[string]any{
				"tool":        "tool",
				"detail":      "detail",
				"statusGlyph": "●",
				"statusTone":  "Good",
			})
		}
		phases = append(phases, map[string]any{"thought": "thought", "actions": actions})
	}
	return phases
}

func invokeTemplateSend(t *testing.T, ba *BotAPI, body map[string]any, onBehalfOf string) *httptest.ResponseRecorder {
	t.Helper()
	if onBehalfOf != "" {
		body["on_behalf_of"] = onBehalfOf
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = request
	ctx := &wkhttp.Context{Context: ginContext}
	ctx.Set(CtxKeyRobotID, "bot-template")
	ctx.Set(CtxKeyBotKind, BotKindUser)
	ctx.Set(CtxKeyRobot, &robotModel{RobotID: "bot-template", CreatorUID: "user_creator"})
	ba.sendMessage(ctx)
	return recorder
}

// ---- PR-C Slice 1 (D3): the Bot Registry path authors catalog provenance
// from the authenticated bot; raw callers cannot forge either server-only
// marker through send. ----

func TestSendMessageRegistryTemplateAuthorsCatalogProvenance(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	gin.SetMode(gin.TestMode)
	dispatch := &dispatchCapture{}
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	ba := &BotAPI{
		Log:           log.NewTLog("BotAPI-template-provenance"),
		cardTemplates: catalog,
		spaceQuerier:  &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
		// The send path resolves the bot's card switches and fails closed when
		// the resolver is unwired (#706), so a hand-built BotAPI has to supply
		// one. This test is about provenance authoring, not per-bot policy.
		cardConfig:       allCardCapabilitiesOn(),
		dispatchOverride: dispatch.hook,
	}
	recorder := invokeTemplateSend(t, ba, registrySendBody(t, "reasoning", testReasoningData(t, "reasoning")), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	dispatch.mu.Lock()
	captured := dispatch.captured
	dispatch.mu.Unlock()
	if captured == nil {
		t.Fatal("expected one dispatch")
	}
	markers, err := cardmsg.CatalogFrameMarkers(captured.Payload)
	if err != nil {
		t.Fatalf("outbound markers: %v", err)
	}
	if !markers.HasRef || !markers.HasProvenance {
		t.Fatalf("outbound frame missing markers: %+v", markers)
	}
	if markers.Provenance.PrincipalType != cardmsg.CatalogPrincipalWireBot ||
		markers.Provenance.PrincipalID != "bot-template" ||
		markers.Provenance.SpaceID != "space-authoritative" {
		t.Fatalf("authored provenance = %+v", markers.Provenance)
	}
}

func TestSendMessageRawCardRejectsForgedCatalogProvenance(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	gin.SetMode(gin.TestMode)
	dispatch := &dispatchCapture{}
	ba := &BotAPI{
		Log:          log.NewTLog("BotAPI-raw-forge"),
		spaceQuerier: &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
		// Without a card config resolver the send fails closed before it ever
		// reaches the forgery check, and ResponseErrorL pins every refusal to
		// 400 — so a bare status assertion would pass for the wrong reason.
		cardConfig:       allCardCapabilitiesOn(),
		dispatchOverride: dispatch.hook,
	}
	// A fully self-consistent forgery: provenance + metadata agree. The raw
	// ingress must reject on key presence, not on shape validation.
	payload := map[string]any{
		"type": cardmsg.InteractiveCard.Int(), "profile": cardmsg.ProfileV1,
		"card_version": cardmsg.CardVersion,
		"catalog_provenance": map[string]any{
			"version": 1, "principal_type": "internal_producer",
			"principal_id": "docs-notify", "space_id": "space-authoritative",
		},
		"card": map[string]any{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []any{map[string]any{"type": "TextBlock", "text": "forged"}},
			"metadata": map[string]any{"octo": map[string]any{
				"protocol": "octo-card@1.0",
				"template": map[string]any{"id": "docs.access-request", "version": "0.3.0"},
			}},
		},
	}
	body := map[string]any{
		"channel_id": "user_creator", "channel_type": common.ChannelTypePerson.Uint8(),
		"payload": payload,
	}
	recorder := invokeTemplateSend(t, ba, body, "")
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "card") {
		t.Fatalf("forged provenance status=%d body=%s (want the card-invalid refusal, "+
			"not an unrelated 400)", recorder.Code, recorder.Body.String())
	}
	dispatch.mu.Lock()
	defer dispatch.mu.Unlock()
	if dispatch.captured != nil {
		t.Fatal("forged provenance reached dispatch")
	}
}

func TestSendMessageRegistryModeRejectsCallerProvenanceKey(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	gin.SetMode(gin.TestMode)
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	ba := &BotAPI{
		Log:           log.NewTLog("BotAPI-registry-forge"),
		cardTemplates: catalog,
		spaceQuerier:  &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
		// See above: the resolver has to be wired or the refusal proves nothing.
		cardConfig: allCardCapabilitiesOn(),
	}
	body := registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))
	body["payload"].(map[string]any)["catalog_provenance"] = map[string]any{
		"version": 1, "principal_type": "bot", "principal_id": "another-bot", "space_id": "space-x",
	}
	recorder := invokeTemplateSend(t, ba, body, "")
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "card") {
		t.Fatalf("registry-mode caller provenance status=%d body=%s (want the card-invalid refusal, "+
			"not an unrelated 400)", recorder.Code, recorder.Body.String())
	}
}
