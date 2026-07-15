package notify

import (
	"context"
	"encoding/json"
	"net/http"
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
	}, []string{"https://summary.internal/v1/card-actions/decide"}, func(key string) string {
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
