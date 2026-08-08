package bot_api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
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

// Mutate applies the production marker-preservation rule before recording the
// request, through the same cardmsg.CatalogMarkersPreserved the real
// CardMutator calls.
//
// PR-C review round 8: without this, the fake returned a canned result and the
// real guard never ran, so an edit that the production boundary would refuse
// looked like a pass here. Both blockers in that round lived in exactly this
// seam — the handler suite stubbed the mutator, the mutator suite hand-built
// replacements, and the interaction between a real render output and the real
// guard was covered by neither.
func (m *fakeBotCardMutator) Mutate(_ context.Context, request carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error) {
	if len(m.snapshot.Envelope) > 0 {
		stored, err := cardmsg.CatalogFrameMarkers(m.snapshot.Envelope)
		if err != nil {
			return carddispatch.CardMutationResult{}, err
		}
		next, err := cardmsg.CatalogFrameMarkers([]byte(request.ContentEdit))
		if err != nil {
			return carddispatch.CardMutationResult{}, err
		}
		if err := cardmsg.CatalogMarkersPreserved(stored, next); err != nil {
			return carddispatch.CardMutationResult{}, fmt.Errorf("%w: %v", carddispatch.ErrCardMutationInvalid, err)
		}
	}
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
	}, "bot-template"); err != nil {
		t.Fatalf("replacement identity: %v", err)
	}
}

func TestBotMessageEditRegistryTemplateKeepsHistoricalVersionsEditable(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalogWithPolicy(testBotTemplateRegistry(t), defaultBotTemplatePolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, historical := range []struct {
		version string
		root    string
	}{
		{aireasoningprocess.TemplateVersionV1, aireasoningprocess.HandoffRootV1},
		{aireasoningprocess.TemplateVersionV2, aireasoningprocess.HandoffRootV2},
		{aireasoningprocess.TemplateVersionV3, aireasoningprocess.HandoffRootV3},
	} {
		t.Run(historical.version, func(t *testing.T) {
			mutator := &fakeBotCardMutator{
				snapshot: carddispatch.CardMutationSnapshot{
					Envelope: initialRegistryEnvelopeVersion(t, catalog, historical.version, "reasoning"),
					CardSeq:  1,
				},
				mutateResult: carddispatch.CardMutationResult{Applied: true},
			}
			ba := &BotAPI{
				Log:           log.NewTLog("BotAPI-template-edit-historical"),
				cardTemplates: catalog,
				cardMutator:   mutator,
			}
			body := registryEditBodyVersion(t, historical.version, "completed",
				testReasoningDataVersion(t, historical.root, "completed"), 2, false)

			recorder := invokeTemplateEdit(t, ba, body)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if mutator.snapshotCalls != 1 || len(mutator.mutateRequests) != 1 {
				t.Fatalf("snapshot/mutate calls = %d/%d", mutator.snapshotCalls, len(mutator.mutateRequests))
			}
			if err := requireEffectiveCardTemplate([]byte(mutator.mutateRequests[0].ContentEdit), botTemplateRef{
				ID: aireasoningprocess.TemplateID, Version: historical.version,
			}, "bot-template"); err != nil {
				t.Fatalf("historical replacement identity: %v", err)
			}
		})
	}
}

func TestBotMessageEditRegistryTemplatePreservesAuthoritativeSpaceAcrossEdits(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	effective := initialRegistryEnvelope(t, catalog)
	tests := []struct {
		state     string
		cardSeq   int64
		transient bool
	}{
		{state: "answering", cardSeq: 2, transient: true},
		{state: "completed", cardSeq: 3, transient: false},
	}
	for _, tc := range tests {
		mutator := &fakeBotCardMutator{
			snapshot:     carddispatch.CardMutationSnapshot{Envelope: effective, CardSeq: tc.cardSeq - 1},
			mutateResult: carddispatch.CardMutationResult{Applied: true},
		}
		ba := &BotAPI{
			Log: log.NewTLog("BotAPI-template-edit-space"), cardTemplates: catalog, cardMutator: mutator,
		}
		recorder := invokeTemplateEdit(t, ba,
			registryEditBody(t, tc.state, testReasoningData(t, tc.state), tc.cardSeq, tc.transient))
		if recorder.Code != http.StatusOK || len(mutator.mutateRequests) != 1 {
			t.Fatalf("state=%s status=%d mutations=%d body=%s",
				tc.state, recorder.Code, len(mutator.mutateRequests), recorder.Body.String())
		}
		contentEdit := mutator.mutateRequests[0].ContentEdit
		var frame map[string]any
		if err := json.Unmarshal([]byte(contentEdit), &frame); err != nil {
			t.Fatalf("state=%s decode replacement: %v", tc.state, err)
		}
		if got := frame["space_id"]; got != cardtmplBuildEnvForTest().SpaceID {
			t.Fatalf("state=%s space_id=%v, want authoritative %q",
				tc.state, got, cardtmplBuildEnvForTest().SpaceID)
		}
		effective = []byte(contentEdit)
	}
}

func TestBotMessageEditRegistryTemplateRejectsUnlistedRefBeforeTargetLookup(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	validSnapshot := carddispatch.CardMutationSnapshot{
		Envelope: initialRegistryEnvelope(t, catalog), CardSeq: 1,
	}
	tests := []struct {
		name        string
		snapshot    carddispatch.CardMutationSnapshot
		snapshotErr error
	}{
		{name: "valid target", snapshot: validSnapshot},
		{name: "missing target", snapshotErr: carddispatch.ErrCardMutationNotFound},
		{name: "foreign target", snapshotErr: carddispatch.ErrCardMutationForbidden},
	}
	var firstEnvelope []byte
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutator := &fakeBotCardMutator{snapshot: tc.snapshot, snapshotErr: tc.snapshotErr}
			ba := &BotAPI{
				Log: log.NewTLog("BotAPI-template-edit-unlisted"), cardTemplates: catalog, cardMutator: mutator,
			}
			body := registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)
			body["template_ref"] = map[string]any{
				"id": string(docsaccessrequest.TemplateID), "version": docsaccessrequest.TemplateVersionV3,
			}

			recorder := invokeTemplateEdit(t, ba, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var envelope errEnvelope
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v; body=%s", err, recorder.Body.String())
			}
			if envelope.Msg != errcode.ErrBotAPICardInvalid.DefaultMessage || envelope.Status != http.StatusBadRequest {
				t.Fatalf("error envelope=%+v, want card-invalid; body=%s", envelope, recorder.Body.String())
			}
			if mutator.snapshotCalls != 0 || len(mutator.mutateRequests) != 0 {
				t.Fatalf("unlisted ref reached target state: snapshots=%d mutations=%d",
					mutator.snapshotCalls, len(mutator.mutateRequests))
			}
			if firstEnvelope == nil {
				firstEnvelope = append([]byte(nil), recorder.Body.Bytes()...)
			} else if !bytes.Equal(firstEnvelope, recorder.Body.Bytes()) {
				t.Fatalf("target state changed rejection envelope:\nfirst=%s\ncurrent=%s",
					firstEnvelope, recorder.Body.Bytes())
			}
		})
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
			name: "aggregate action overflow",
			body: func(t *testing.T) map[string]any {
				body := registryEditBody(t, "reasoning", testReasoningData(t, "reasoning"), 2, false)
				body["data"].(map[string]any)["phases"] = reasoningPhasesForBotTest(4, 2, 2, 2, 2, 2)
				return body
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
			name: "post build render failure",
			body: func(t *testing.T) map[string]any {
				body := registryEditBody(t, "reasoning", testReasoningData(t, "reasoning"), 2, false)
				body["data"].(map[string]any)["progressText"] = "[tap](javascript:alert(1))"
				return body
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
		{
			name: "ownership changes between snapshot and mutate",
			body: func(t *testing.T) map[string]any {
				return registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)
			},
			mutateErr: carddispatch.ErrCardMutationForbidden,
		},
		{
			name: "target revoked between snapshot and mutate",
			body: func(t *testing.T) map[string]any {
				return registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)
			},
			mutateErr: carddispatch.ErrCardMutationNotFound,
		},
		{
			name: "replacement rejected by mutator",
			body: func(t *testing.T) map[string]any {
				return registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)
			},
			mutateErr: carddispatch.ErrCardMutationInvalid,
		},
		{
			name: "mutation infrastructure failure",
			body: func(t *testing.T) map[string]any {
				return registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)
			},
			mutateErr: errors.New("store unavailable"),
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
		{name: "snapshot infrastructure failure", err: errors.New("lookup unavailable")},
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

func TestBotMessageEditRegistryTemplateDisabledAndWiringFailures(t *testing.T) {
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	body := registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)

	t.Run("disabled", func(t *testing.T) {
		t.Setenv(cardmsg.EnvEnabled, "")
		mutator := &fakeBotCardMutator{}
		ba := &BotAPI{Log: log.NewTLog("BotAPI-template-edit-disabled"), cardTemplates: catalog, cardMutator: mutator}
		recorder := invokeTemplateEdit(t, ba, body)
		if recorder.Code != http.StatusBadRequest || mutator.snapshotCalls != 0 {
			t.Fatalf("status=%d snapshots=%d body=%s", recorder.Code, mutator.snapshotCalls, recorder.Body.String())
		}
	})

	t.Run("catalog missing", func(t *testing.T) {
		t.Setenv(cardmsg.EnvEnabled, "true")
		mutator := &fakeBotCardMutator{}
		ba := &BotAPI{Log: log.NewTLog("BotAPI-template-edit-unwired"), cardMutator: mutator}
		recorder := invokeTemplateEdit(t, ba, body)
		if recorder.Code != http.StatusBadRequest || mutator.snapshotCalls != 0 {
			t.Fatalf("status=%d snapshots=%d body=%s", recorder.Code, mutator.snapshotCalls, recorder.Body.String())
		}
	})

	t.Run("malformed ref", func(t *testing.T) {
		t.Setenv(cardmsg.EnvEnabled, "true")
		mutator := &fakeBotCardMutator{}
		ba := &BotAPI{Log: log.NewTLog("BotAPI-template-edit-ref"), cardTemplates: catalog, cardMutator: mutator}
		bad := registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)
		bad["template_ref"].(map[string]any)["view"] = "result"
		recorder := invokeTemplateEdit(t, ba, bad)
		if recorder.Code != http.StatusBadRequest || mutator.snapshotCalls != 0 {
			t.Fatalf("status=%d snapshots=%d body=%s", recorder.Code, mutator.snapshotCalls, recorder.Body.String())
		}
	})
}

func TestBotMessageEditRegistryTemplateRejectsDualModeByFieldPresence(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	mutator := &fakeBotCardMutator{}
	ba := &BotAPI{
		Log: log.NewTLog("BotAPI-template-edit-dual-mode"), cardTemplates: catalog, cardMutator: mutator,
	}
	body := registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false)
	body["content_edit"] = " "

	recorder := invokeTemplateEdit(t, ba, body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if mutator.snapshotCalls != 0 || len(mutator.mutateRequests) != 0 {
		t.Fatalf("dual-mode request reached mutation path: snapshots=%d mutate=%d",
			mutator.snapshotCalls, len(mutator.mutateRequests))
	}
}

func initialRegistryEnvelope(t *testing.T, catalog *botCardTemplateCatalog) []byte {
	t.Helper()
	payload, err := catalog.RenderPayload(context.Background(), registrySendBody(t,
		"reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any), cardtmplBuildEnvForTest())
	if err != nil {
		t.Fatal(err)
	}
	payload["space_id"] = cardtmplBuildEnvForTest().SpaceID
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

func initialRegistryEnvelopeVersion(
	t *testing.T,
	catalog *botCardTemplateCatalog,
	version string,
	state string,
) []byte {
	t.Helper()
	root := testReasoningRootCurrent
	switch version {
	case aireasoningprocess.TemplateVersionV1:
		root = aireasoningprocess.HandoffRootV1
	case aireasoningprocess.TemplateVersionV2:
		root = aireasoningprocess.HandoffRootV2
	case aireasoningprocess.TemplateVersionV3:
		root = aireasoningprocess.HandoffRootV3
	}
	data := testReasoningDataVersion(t, root, state)
	env := cardtmplBuildEnvForTest()
	payload, err := catalog.catalog.Render(context.Background(), cardtmpl.CatalogRenderRequest{
		Access: botCatalogAccess(cardtmpl.CatalogPurposeHistoricalEdit, "test-bot", env.SpaceID),
		ID:     aireasoningprocess.TemplateID, Version: version,
		State: cardtmpl.State(state), Fields: data, Env: env,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload["template_ref"] = map[string]any{
		"id": string(aireasoningprocess.TemplateID), "version": version,
	}
	payload["space_id"] = cardtmplBuildEnvForTest().SpaceID
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
	return registryEditBodyVersion(t, aireasoningprocess.TemplateVersion, state, data, cardSeq, transient)
}

func registryEditBodyVersion(
	t *testing.T,
	version string,
	state string,
	data json.RawMessage,
	cardSeq int64,
	transient bool,
) map[string]any {
	t.Helper()
	return map[string]any{
		"message_id": "1001", "message_seq": uint32(7),
		"channel_id": "user_creator", "channel_type": common.ChannelTypePerson.Uint8(),
		"template_ref": map[string]any{
			"id": string(aireasoningprocess.TemplateID), "version": version,
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

// ---- PR-C Slice 1 (D3): Bot template edit re-authors the same-identity
// provenance, rejects cross-principal stored frames, and raw edit cannot
// forge or overwrite the server-only markers. ----

func markedRegistryEnvelope(t *testing.T, catalog *botCardTemplateCatalog, botID string) []byte {
	t.Helper()
	env := cardtmplBuildEnvForTest()
	payload, err := catalog.RenderPayloadForPrincipal(context.Background(), botID, registrySendBody(t,
		"reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any), env)
	if err != nil {
		t.Fatal(err)
	}
	payload["space_id"] = env.SpaceID
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

func TestBotMessageEditRegistryTemplateReAuthorsSameProvenance(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	mutator := &fakeBotCardMutator{
		snapshot:     carddispatch.CardMutationSnapshot{Envelope: markedRegistryEnvelope(t, catalog, "bot-template"), CardSeq: 1},
		mutateResult: carddispatch.CardMutationResult{Applied: true},
	}
	ba := &BotAPI{
		Log:           log.NewTLog("BotAPI-template-edit-provenance"),
		cardTemplates: catalog,
		cardMutator:   mutator,
	}
	recorder := invokeTemplateEdit(t, ba, registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	markers, err := cardmsg.CatalogFrameMarkers([]byte(mutator.mutateRequests[0].ContentEdit))
	if err != nil {
		t.Fatalf("replacement markers: %v", err)
	}
	if !markers.HasProvenance || markers.Provenance.PrincipalType != cardmsg.CatalogPrincipalWireBot ||
		markers.Provenance.PrincipalID != "bot-template" {
		t.Fatalf("replacement provenance = %+v", markers)
	}
	stored, err := cardmsg.CatalogFrameMarkers(mutator.snapshot.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if markers.Provenance != stored.Provenance {
		t.Fatalf("provenance changed across edit: stored=%+v replacement=%+v", stored.Provenance, markers.Provenance)
	}
}

func TestBotMessageEditRegistryTemplateRejectsForeignStoredProvenance(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	// Same template, but the stored frame is signed by a different principal.
	// Even if a Snapshot ownership gap ever let the lookup succeed, the stored
	// provenance must fail the edit closed.
	envelope := markedRegistryEnvelope(t, catalog, "another-bot")
	mutator := &fakeBotCardMutator{
		snapshot:     carddispatch.CardMutationSnapshot{Envelope: envelope, CardSeq: 1},
		mutateResult: carddispatch.CardMutationResult{Applied: true},
	}
	ba := &BotAPI{
		Log:           log.NewTLog("BotAPI-template-edit-foreign"),
		cardTemplates: catalog,
		cardMutator:   mutator,
	}
	recorder := invokeTemplateEdit(t, ba, registryEditBody(t, "completed", testReasoningData(t, "completed"), 2, false))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("foreign provenance edit status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(mutator.mutateRequests) != 0 {
		t.Fatal("foreign provenance edit reached mutation")
	}
}

func TestBotRawEditRejectsCatalogProvenanceMarkers(t *testing.T) {
	// The raw-edit guard treats either server-only marker as Registry
	// authorship: a target carrying only catalog_provenance is still not
	// raw-editable, and a raw content_edit carrying it is a forgery.
	if !cardEnvelopeHasCatalogMarker([]byte(`{"type":17,"catalog_provenance":{"version":1}}`)) {
		t.Fatal("provenance-marked target not recognized as Registry-authored")
	}
	if !contentEditHasCatalogMarker(`{"type":17,"catalog_provenance":{"version":1,"principal_type":"bot","principal_id":"x","space_id":""}}`) {
		t.Fatal("raw content_edit forging catalog_provenance not recognized")
	}
	if cardEnvelopeHasCatalogMarker([]byte(`{"type":17,"card":{"body":[]}}`)) {
		t.Fatal("legacy raw frame misclassified as Registry-authored")
	}
}
