package bot_api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/gin-gonic/gin"
)

type fakeBotCardMutator struct {
	snapshot       carddispatch.CardMutationSnapshot
	snapshotErr    error
	mutateResult   carddispatch.CardMutationResult
	mutateErr      error
	snapshotCalls  int
	mutateRequests []carddispatch.CardMutationRequest
}

func (m *fakeBotCardMutator) Snapshot(_ context.Context, _ carddispatch.CardMutationTarget) (carddispatch.CardMutationSnapshot, error) {
	m.snapshotCalls++
	return m.snapshot, m.snapshotErr
}

func (m *fakeBotCardMutator) Mutate(_ context.Context, request carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error) {
	m.mutateRequests = append(m.mutateRequests, request)
	return m.mutateResult, m.mutateErr
}

func (m *fakeBotCardMutator) WriteCAS(carddispatch.CardMutationCASRequest) (bool, error) {
	return false, nil
}

func TestBotMessageEditRegistryTemplateRendersSameIdentity(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	mutator := &fakeBotCardMutator{
		snapshot:     carddispatch.CardMutationSnapshot{Envelope: initialRegistryEnvelope(t, catalog), CardSeq: 1},
		mutateResult: carddispatch.CardMutationResult{Applied: true},
	}
	ba := &BotAPI{
		Log:           log.NewTLog("BotAPI-template-edit"),
		cardTemplates: catalog,
		cardMutator:   mutator,
	}
	body := registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, true)
	recorder := invokeTemplateEdit(t, ba, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if mutator.snapshotCalls != 1 || len(mutator.mutateRequests) != 1 {
		t.Fatalf("snapshot/mutate calls = %d/%d", mutator.snapshotCalls, len(mutator.mutateRequests))
	}
	request := mutator.mutateRequests[0]
	var frame map[string]any
	if err := json.Unmarshal([]byte(request.ContentEdit), &frame); err != nil {
		t.Fatal(err)
	}
	if frame["profile"] != "octo/v1" || frame["card_seq"] != float64(2) || frame["transient"] != true {
		t.Fatalf("replacement frame = %#v", frame)
	}
	if plain, _ := frame["plain"].(string); plain == "" {
		t.Fatal("replacement plain missing")
	}
	if err := requireEffectiveCardTemplate([]byte(request.ContentEdit), botTemplateRef{
		ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersion,
	}); err != nil {
		t.Fatalf("replacement identity: %v", err)
	}
}

func TestBotMessageEditRegistryTemplateFailsClosed(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		body        func(*testing.T) map[string]any
		snapshot    func(*testing.T) carddispatch.CardMutationSnapshot
		mutateErr   error
		wantMessage string
	}{
		{
			name: "non positive card seq",
			body: func(t *testing.T) map[string]any {
				return registryEditBody(t, "completed", testReasoningData(t, "completed"), 0, false)
			},
		},
		{
			name: "outer data state mismatch",
			body: func(t *testing.T) map[string]any {
				return registryEditBody(t, "error", testReasoningData(t, "completed"), 2, false)
			},
		},
		{
			name: "cross version target",
			body: func(t *testing.T) map[string]any {
				return registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)
			},
			snapshot: func(t *testing.T) carddispatch.CardMutationSnapshot {
				frame := initialRegistryEnvelope(t, catalog)
				var payload map[string]any
				_ = json.Unmarshal(frame, &payload)
				card := payload["card"].(map[string]any)
				metadata := card["metadata"].(map[string]any)
				octo := metadata["octo"].(map[string]any)
				octo["template"].(map[string]any)["version"] = "9.9.9"
				modified, _ := json.Marshal(payload)
				return carddispatch.CardMutationSnapshot{Envelope: modified, CardSeq: 1}
			},
		},
		{
			name: "stale card seq",
			body: func(t *testing.T) map[string]any {
				return registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)
			},
			mutateErr:   carddispatch.ErrCardMutationConflict,
			wantMessage: "stale card_seq",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := carddispatch.CardMutationSnapshot{Envelope: initialRegistryEnvelope(t, catalog), CardSeq: 1}
			if tc.snapshot != nil {
				snapshot = tc.snapshot(t)
			}
			mutator := &fakeBotCardMutator{snapshot: snapshot, mutateErr: tc.mutateErr}
			ba := &BotAPI{
				Log: log.NewTLog("BotAPI-template-edit-invalid"), cardTemplates: catalog, cardMutator: mutator,
			}
			recorder := invokeTemplateEdit(t, ba, tc.body(t))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if tc.wantMessage != "" && !bytes.Contains(recorder.Body.Bytes(), []byte(tc.wantMessage)) {
				t.Fatalf("body=%s, want %q", recorder.Body.String(), tc.wantMessage)
			}
			if tc.mutateErr == nil && len(mutator.mutateRequests) != 0 {
				t.Fatal("invalid Registry edit reached mutation")
			}
		})
	}
}

func TestBotMessageEditRegistryTemplateMapsOwnershipAndLifecycle(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		err  error
	}{
		{name: "not owner", err: carddispatch.ErrCardMutationForbidden},
		{name: "revoked or deleted", err: carddispatch.ErrCardMutationNotFound},
		{name: "non registry target", err: carddispatch.ErrCardMutationInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutator := &fakeBotCardMutator{snapshotErr: tc.err}
			ba := &BotAPI{Log: log.NewTLog("BotAPI-template-edit-guard"), cardTemplates: catalog, cardMutator: mutator}
			recorder := invokeTemplateEdit(t, ba,
				registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false))
			if recorder.Code != http.StatusBadRequest || len(mutator.mutateRequests) != 0 {
				t.Fatalf("status=%d mutate=%d body=%s", recorder.Code, len(mutator.mutateRequests), recorder.Body.String())
			}
		})
	}
}

func initialRegistryEnvelope(t *testing.T, catalog *botCardTemplateCatalog) []byte {
	t.Helper()
	payload, err := catalog.RenderPayload(context.Background(), registrySendBody(t,
		"reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any), cardtmplBuildEnvForTest())
	if err != nil {
		t.Fatal(err)
	}
	payload["card_seq"] = int64(1)
	if err := cardmsg.Finalize(payload); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func registryEditBody(t *testing.T, state string, data json.RawMessage, cardSeq int64, transient bool) map[string]any {
	t.Helper()
	return map[string]any{
		"message_id": "1001", "message_seq": uint32(7),
		"channel_id": "user_creator", "channel_type": common.ChannelTypePerson.Uint8(),
		"template_ref": map[string]any{
			"id": string(aireasoningprocess.TemplateID), "version": aireasoningprocess.TemplateVersion,
		},
		"state": state, "data": rawJSONToMap(t, data), "card_seq": cardSeq, "transient": transient,
	}
}

func invokeTemplateEdit(t *testing.T, ba *BotAPI, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/bot/message/edit", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = request
	ctx := &wkhttp.Context{Context: ginContext}
	ctx.Set(CtxKeyRobotID, "bot-template")
	ctx.Set(CtxKeyBotKind, BotKindUser)
	ctx.Set(CtxKeyRobot, &robotModel{RobotID: "bot-template", CreatorUID: "user_creator"})
	ba.botMessageEdit(ctx)
	return recorder
}

func cardtmplBuildEnvForTest() cardtmpl.BuildEnv {
	return cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-1"}
}

var _ = errors.Is
