package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturingCardSender struct {
	mu      sync.Mutex
	targets []carddispatch.Target
	cards   []carddispatch.Card
}

type unusedActionQueue struct{}

func (unusedActionQueue) Enqueue(cardactiondispatch.Event, time.Time) error { return nil }

func (s *capturingCardSender) Send(_ context.Context, target carddispatch.Target, card carddispatch.Card) (*carddispatch.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets = append(s.targets, target)
	s.cards = append(s.cards, card)
	return &carddispatch.Result{MessageID: 1001, MessageSeq: 1}, nil
}

func (s *capturingCardSender) last() carddispatch.Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cards[len(s.cards)-1]
}

func (s *capturingCardSender) lastTarget() carddispatch.Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targets[len(s.targets)-1]
}

func validAccessRequestDocsCard() *DocsCardFields {
	card := validDocsCard()
	card.Kind = DocsCardKindAccessRequested
	card.RequestID = "request-1"
	return card
}

func TestBuildDocsAccessRequestCardProducesValidOctoV2(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "legacy-token")

	document, err := n.buildDocsAccessRequestCard(context.Background(), "space-1", validAccessRequestDocsCard(), "zh-CN")
	require.NoError(t, err)
	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(document, &card))
	require.NoError(t, cardmsg.Validate(map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"card":         card,
	}))
	assert.Contains(t, string(document), `"title":"允许"`)
	assert.Contains(t, string(document), `"title":"拒绝"`)
}

func TestDeliverDocsAccessRequestHonorsApprovalFlag(t *testing.T) {
	tests := []struct {
		name        string
		flag        string
		wantProfile string
	}{
		{name: "enabled sends v2", flag: "true", wantProfile: cardmsg.ProfileV2},
		{name: "disabled preserves v1", flag: "false", wantProfile: cardmsg.ProfileV1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", tt.flag)
			t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "true")
			wk := newWuKongServer()
			defer wk.close()
			ctx := newTestContext(t, wk)
			ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
			n := newTestNotify(ctx, nil, nil, nil, "legacy-token")
			n.botOK.Store(true)
			primeMemberCache(n, "space-1", "user-b")
			capture := &capturingCardSender{}
			n.docsSender = capture

			response, err := n.deliverDocsCardNotification(&NotifyReq{
				SpaceID: "space-1", Service: "docs", Targets: []string{"user-b"}, ActorUID: "user-a",
				DocsCard: validAccessRequestDocsCard(),
			})
			require.NoError(t, err)
			assert.Equal(t, []string{"user-b"}, response.Delivered)
			assert.Equal(t, tt.wantProfile, capture.last().Profile)
		})
	}
}

func TestDocsOutcomeCardsAreV1AndDeniedDoesNotLeakApproverOrReason(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "legacy-token")

	for _, kind := range []string{DocsCardKindAccessGranted, DocsCardKindAccessDenied} {
		card := validDocsCard()
		card.Kind = kind
		card.ActorName = "approver-secret"
		card.Excerpt = "denial-reason-secret"
		document, err := n.buildDocsCard(context.Background(), "space-1", card, "zh-CN")
		require.NoError(t, err)
		assert.NotContains(t, string(document), "approver-secret")
		if kind == DocsCardKindAccessDenied {
			assert.NotContains(t, string(document), "denial-reason-secret")
			assert.Contains(t, string(document), "访问申请已拒绝")
			fallback := buildDocsFallbackText(card, "zh-CN")
			assert.NotContains(t, fallback, "approver-secret")
			assert.NotContains(t, fallback, "denial-reason-secret")
		} else {
			assert.Contains(t, string(document), "文档访问已获批准")
		}
	}
}

func TestIntegration_NotifyTokensAreCapabilityScoped(t *testing.T) {
	t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", "false")
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "legacy-token")
	n.docsToken = "docs-token"
	n.botOK.Store(true)
	primeMemberCache(n, "space-1", "user-b")
	n.docsSender = &capturingCardSender{}
	router := buildRouter(n)
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	docsRequest := NotifyReq{
		SpaceID: "space-1", Service: "docs", Targets: []string{"user-b"}, DocsCard: validAccessRequestDocsCard(),
	}
	legacyHeader := http.Header{InternalTokenHeader: []string{"legacy-token"}}
	legacyResponse := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", legacyHeader, docsRequest)
	assert.Equal(t, http.StatusBadRequest, legacyResponse.Code)
	assert.Contains(t, legacyResponse.Body.String(), "err.server.notify.card_not_allowed")

	docsHeader := http.Header{InternalTokenHeader: []string{"docs-token"}}
	docsResponse := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", docsHeader, docsRequest)
	assert.Equal(t, http.StatusOK, docsResponse.Code)

	legacyRequest := NotifyReq{
		SpaceID: "space-1", Service: "other", Targets: []string{"user-b"}, Payload: map[string]interface{}{"type": 1, "content": "x"},
	}
	docsToLegacy := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", docsHeader, legacyRequest)
	assert.Equal(t, http.StatusBadRequest, docsToLegacy.Code)
	assert.Contains(t, docsToLegacy.Body.String(), "err.server.notify.card_not_allowed")
}

func TestIntegration_GenericApprovalTokenIsOwnerBoundAndSendsV2(t *testing.T) {
	t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "true")
	const (
		callbackSecret = "0123456789abcdef0123456789abcdef"
		notifyToken    = "abcdef0123456789abcdef0123456789"
	)
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{
		{
			SenderUID: "notification", Owner: "smart-summary", ActionType: "summary.publish.decision",
			URL:       "https://summary.internal/v1/card-actions/decide",
			SecretEnv: "OCTO_SMART_SUMMARY_CARD_ACTION_SECRET", NotifyTokenEnv: "OCTO_SMART_SUMMARY_NOTIFY_TOKEN",
		},
	}, func(key string) string {
		switch key {
		case "OCTO_SMART_SUMMARY_CARD_ACTION_SECRET":
			return callbackSecret
		case "OCTO_SMART_SUMMARY_NOTIFY_TOKEN":
			return notifyToken
		default:
			return ""
		}
	})
	require.NoError(t, err)
	service, err := cardactiondispatch.NewService(registry, unusedActionQueue{}, ctx)
	require.NoError(t, err)
	capability := cardactiondispatch.NotifyCapability{SenderUID: "notification", Owner: "smart-summary"}
	capture := &capturingCardSender{}
	n := newTestNotify(ctx, nil, nil, nil, "legacy-token")
	n.actionService = service
	n.actionSenders = map[cardactiondispatch.NotifyCapability]carddispatch.Sender{capability: capture}
	n.botOK.Store(true)
	primeMemberCache(n, "space-1", "user-b")
	router := buildRouter(n)
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	request := NotifyReq{
		SpaceID: "space-1", Service: "smart-summary", Targets: []string{"user-b"}, ActorUID: "user-a",
		ApprovalCard: &ApprovalCardFields{
			ActionType: "summary.publish.decision", Title: "Publish summary", Description: "Review it first",
			Data: map[string]string{"task_no": "task-1"},
		},
	}
	actionHeader := http.Header{InternalTokenHeader: []string{notifyToken}}
	response := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", actionHeader, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, capture.cards, 1)
	assert.Equal(t, cardmsg.ProfileV2, capture.last().Profile)
	assert.Contains(t, string(capture.last().Document), `"owner":"smart-summary"`)
	assert.Contains(t, string(capture.last().Document), `"action_type":"summary.publish.decision"`)
	assert.Contains(t, string(capture.last().Document), `"task_no":"task-1"`)

	legacyHeader := http.Header{InternalTokenHeader: []string{"legacy-token"}}
	legacyResponse := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", legacyHeader, request)
	assert.Equal(t, http.StatusBadRequest, legacyResponse.Code)

	request.ApprovalCard.ActionType = "summary.delete.decision"
	unknownAction := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", actionHeader, request)
	assert.Equal(t, http.StatusBadRequest, unknownAction.Code)
	assert.Len(t, capture.cards, 1, "rejected capabilities must not reach transport")

	docsAttempt := NotifyReq{
		SpaceID: "space-1", Service: "docs", Targets: []string{"user-b"}, DocsCard: validAccessRequestDocsCard(),
	}
	docsResponse := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", actionHeader, docsAttempt)
	assert.Equal(t, http.StatusBadRequest, docsResponse.Code)
}

// TestIntegration_ApprovalCardCustomActionsRenderInOrder verifies the
// http-actions follow-up wire path: a caller-supplied 1-5 actions slice ends
// up as server-built Action.Submit buttons with the reserved owner/action_type
// metadata injected and the router-owned action IDs, without leaking a URL.
func TestIntegration_ApprovalCardCustomActionsRenderInOrder(t *testing.T) {
	t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "true")
	const (
		callbackSecret = "0123456789abcdef0123456789abcdef"
		notifyToken    = "abcdef0123456789abcdef0123456789"
	)
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{
		{
			SenderUID: "notification", Owner: "tasks", ActionType: "task.execute.decision",
			URL:       "https://tasks.internal/v1/card-actions/decide",
			SecretEnv: "OCTO_TASKS_CARD_ACTION_SECRET", NotifyTokenEnv: "OCTO_TASKS_NOTIFY_TOKEN",
		},
	}, func(key string) string {
		switch key {
		case "OCTO_TASKS_CARD_ACTION_SECRET":
			return callbackSecret
		case "OCTO_TASKS_NOTIFY_TOKEN":
			return notifyToken
		default:
			return ""
		}
	})
	require.NoError(t, err)
	service, err := cardactiondispatch.NewService(registry, unusedActionQueue{}, ctx)
	require.NoError(t, err)
	capability := cardactiondispatch.NotifyCapability{SenderUID: "notification", Owner: "tasks"}
	capture := &capturingCardSender{}
	n := newTestNotify(ctx, nil, nil, nil, "legacy-token")
	n.actionService = service
	n.actionSenders = map[cardactiondispatch.NotifyCapability]carddispatch.Sender{capability: capture}
	n.botOK.Store(true)
	primeMemberCache(n, "space-1", "user-b")
	router := buildRouter(n)
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	request := NotifyReq{
		SpaceID: "space-1", Service: "tasks", Targets: []string{"user-b"}, ActorUID: "user-a",
		ApprovalCard: &ApprovalCardFields{
			ActionType: "task.execute.decision", Title: "Execute task", Description: "Pick one",
			Data: map[string]string{"task_id": "task-1"},
			Actions: []ApprovalCardAction{
				{Decision: "execute", Title: "Execute"},
				{Decision: "reject", Title: "Reject"},
				{Decision: "cancel", Title: "Cancel"},
			},
		},
	}
	header := http.Header{InternalTokenHeader: []string{notifyToken}}
	response := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", header, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, capture.cards, 1)

	document := capture.last().Document
	assert.NotContains(t, string(document), "http")
	assert.Contains(t, string(document), `"id":"approval-execute"`)
	assert.Contains(t, string(document), `"id":"approval-reject"`)
	assert.Contains(t, string(document), `"id":"approval-cancel"`)
	assert.Contains(t, string(document), `"title":"Execute"`)
	assert.Contains(t, string(document), `"title":"Cancel"`)
	assert.NotContains(t, string(document), `"id":"approval-approve"`)

	var card map[string]interface{}
	require.NoError(t, json.Unmarshal(document, &card))
	actions, _ := card["actions"].([]interface{})
	require.Len(t, actions, 3)
	for _, value := range actions {
		action, _ := value.(map[string]interface{})
		data, _ := action["data"].(map[string]interface{})
		assert.Equal(t, "tasks", data["owner"])
		assert.Equal(t, "task.execute.decision", data["action_type"])
		assert.Equal(t, "task-1", data["task_id"])
	}

	invalid := request
	invalid.ApprovalCard = &ApprovalCardFields{
		ActionType: "task.execute.decision", Title: "Execute task",
		Actions: []ApprovalCardAction{
			{Decision: "Execute", Title: "bad"},
		},
	}
	rejected := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", header, invalid)
	assert.Equal(t, http.StatusBadRequest, rejected.Code)
	assert.Len(t, capture.cards, 1, "invalid decisions must not reach transport")

	// Verify the on-the-wire JSON boundary: an explicit "actions": [] is a
	// caller bug, not a fallback to approve/deny. Send raw JSON to make sure
	// nil vs non-nil-empty is preserved through gin's binder.
	rawEmpty := `{"space_id":"space-1","service":"tasks","targets":["user-b"],"actor_uid":"user-a",` +
		`"approval_card":{"action_type":"task.execute.decision","title":"Execute task",` +
		`"description":"Pick one","data":{"task_id":"task-1"},"actions":[]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/notify", strings.NewReader(rawEmpty))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(InternalTokenHeader, notifyToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Len(t, capture.cards, 1, "explicit empty actions must not fall back to approve/deny")

	// Sanity: omitting the field ("actions" not present) is equivalent to
	// nil and still succeeds via the localized approve/deny template.
	rawOmitted := `{"space_id":"space-1","service":"tasks","targets":["user-b"],"actor_uid":"user-a",` +
		`"approval_card":{"action_type":"task.execute.decision","title":"Execute task",` +
		`"description":"Pick one","data":{"task_id":"task-1"}}}`
	reqOmit := httptest.NewRequest(http.MethodPost, "/v1/internal/notify", strings.NewReader(rawOmitted))
	reqOmit.Header.Set("Content-Type", "application/json")
	reqOmit.Header.Set(InternalTokenHeader, notifyToken)
	recOmit := httptest.NewRecorder()
	router.ServeHTTP(recOmit, reqOmit)
	assert.Equal(t, http.StatusOK, recOmit.Code, recOmit.Body.String())
	require.Len(t, capture.cards, 2)
	assert.Contains(t, string(capture.last().Document), `"id":"approval-approve"`,
		"omitted actions must reach the localized approve/deny template")

	// Wire equivalence: explicit "actions": null MUST decode to a nil slice
	// (Go's encoding/json contract) and therefore reach the same legacy
	// approve/deny template as an omitted field. This pins the doc claim that
	// null and omit are interchangeable on the wire.
	rawNull := `{"space_id":"space-1","service":"tasks","targets":["user-b"],"actor_uid":"user-a",` +
		`"approval_card":{"action_type":"task.execute.decision","title":"Execute task",` +
		`"description":"Pick one","data":{"task_id":"task-1"},"actions":null}}`
	reqNull := httptest.NewRequest(http.MethodPost, "/v1/internal/notify", strings.NewReader(rawNull))
	reqNull.Header.Set("Content-Type", "application/json")
	reqNull.Header.Set(InternalTokenHeader, notifyToken)
	recNull := httptest.NewRecorder()
	router.ServeHTTP(recNull, reqNull)
	assert.Equal(t, http.StatusOK, recNull.Code, recNull.Body.String())
	require.Len(t, capture.cards, 3)
	assert.Contains(t, string(capture.last().Document), `"id":"approval-approve"`,
		`explicit "actions": null must decode to nil and take the legacy approve/deny template`)
}

func TestIntegration_MissingDocsTokenFailsClosed(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	n := newTestNotify(ctx, nil, nil, nil, "legacy-token")
	n.docsToken = ""
	router := buildRouter(n)
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	header := http.Header{InternalTokenHeader: []string{"legacy-token"}}
	response := doJSONRequest(t, router, http.MethodPost, "/v1/internal/notify", header, NotifyReq{
		SpaceID: "space-1", Service: "docs", Targets: []string{"user-b"}, DocsCard: validAccessRequestDocsCard(),
	})
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), "err.server.notify.card_not_allowed")
}
