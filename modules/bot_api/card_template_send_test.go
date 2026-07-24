package bot_api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if !contentEditHasTemplateRef(string(encoded)) {
		t.Fatal("raw content_edit provenance marker was not detected")
	}
	if !cardEnvelopeHasTemplateRef(encoded) {
		t.Fatal("Registry-authored target provenance marker was not detected")
	}
	if contentEditHasTemplateRef(string(mustJSON(t, imCardEnvelope("raw", 1)))) {
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dispatch := &dispatchCapture{}
			ba := &BotAPI{
				Log:              log.NewTLog("BotAPI-template-send-invalid"),
				cardTemplates:    catalog,
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
	t.Helper()
	return map[string]any{
		"channel_id":   "user_creator",
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload": map[string]any{
			"type": cardmsg.InteractiveCard.Int(),
			"template_ref": map[string]any{
				"id": string(aireasoningprocess.TemplateID), "version": aireasoningprocess.TemplateVersion,
			},
			"state": state,
			"data":  rawJSONToMap(t, data),
		},
	}
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
