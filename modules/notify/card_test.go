package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validCard returns a minimal well-formed completed card request field set.
func validCard() *SummaryCardFields {
	return &SummaryCardFields{
		TaskNo:      "TN_abcd",
		Kind:        SummaryCardKindCompleted,
		Title:       "产品周会纪要",
		TimeRange:   "2026-07-06 10:00 ~ 2026-07-13 10:00",
		Members:     5,
		MsgCount:    128,
		GeneratedAt: "2026-07-13 15:04",
	}
}

func assertReadinessIsRaceSafe(t *testing.T, ready *atomic.Bool) {
	t.Helper()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(readyValue bool) {
			defer wg.Done()
			ready.Store(readyValue)
		}(i%2 == 0)
		go func() {
			defer wg.Done()
			_ = ready.Load()
		}()
	}

	wg.Wait()
}

func TestSummaryBotReadinessIsRaceSafe(t *testing.T) {
	var n Notify
	assertReadinessIsRaceSafe(t, &n.summaryBotOK)
}

func TestNotifyBotReadinessIsRaceSafe(t *testing.T) {
	var n Notify
	assertReadinessIsRaceSafe(t, &n.botOK)
}

func TestDeliverCardNotification_ValidationRejectsBadFields(t *testing.T) {
	n := &Notify{} // validation short-circuits before any DB/cache access

	cases := map[string]NotifyReq{
		"missing card":    {SpaceID: "s", Targets: []string{"u"}},
		"missing space":   {Targets: []string{"u"}, Card: validCard()},
		"empty targets":   {SpaceID: "s", Targets: nil, Card: validCard()},
		"missing task_no": {SpaceID: "s", Targets: []string{"u"}, Card: &SummaryCardFields{Kind: SummaryCardKindCompleted, Title: "t"}},
		"missing title":   {SpaceID: "s", Targets: []string{"u"}, Card: &SummaryCardFields{TaskNo: "n", Kind: SummaryCardKindCompleted}},
		"unknown kind":    {SpaceID: "s", Targets: []string{"u"}, Card: &SummaryCardFields{TaskNo: "n", Kind: "weird", Title: "t"}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			r := req
			_, err := n.deliverCardNotification(&r)
			require.ErrorIs(t, err, errNotifyCardInvalid)
		})
	}
}

func TestBuildSummaryCard_ProducesValidOctoV1(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "tk")

	for _, kind := range []string{SummaryCardKindCompleted, SummaryCardKindFailed} {
		t.Run(kind, func(t *testing.T) {
			card := validCard()
			card.Kind = kind
			if kind == SummaryCardKindFailed {
				card.Reason = "AI 处理失败，请稍后重试"
			}
			doc, err := n.buildSummaryCard(context.Background(), "spc_1", card, "zh-CN")
			require.NoError(t, err)

			var cardObj map[string]interface{}
			require.NoError(t, json.Unmarshal(doc, &cardObj))
			envelope := map[string]interface{}{
				"type":         cardmsg.InteractiveCard.Int(),
				"card_version": cardmsg.CardVersion,
				"profile":      cardmsg.ProfileV1,
				"card":         cardObj,
			}
			require.NoError(t, cardmsg.Validate(envelope), "template output must pass octo/v1 Validate")
			// Deep link built from WebLoginURL origin only, /s/{task_no}?sp={space}.
			assert.Contains(t, string(doc), "https://im.example.com/s/TN_abcd?sp=spc_1")
			assert.NotContains(t, string(doc), "/login")
		})
	}
}

func TestBuildSummaryCard_RejectsNonHTTPSDeepLinkOrigin(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "http://im.example.com" // not https
	n := newTestNotify(ctx, nil, nil, nil, "tk")

	_, err := n.buildSummaryCard(context.Background(), "spc_1", validCard(), "zh-CN")
	require.Error(t, err, "a non-https origin must fail card build so the caller degrades to text")
}

func TestBuildSummaryFallbackText(t *testing.T) {
	completed := buildSummaryFallbackText(validCard(), "zh-CN")
	assert.Contains(t, completed, "你的总结「产品周会纪要」已生成完成。")
	assert.Contains(t, completed, "时间范围：2026-07-06 10:00 ~ 2026-07-13 10:00")
	assert.Contains(t, completed, "参与成员：5 人")
	assert.Contains(t, completed, "消息数量：128 条")

	failed := &SummaryCardFields{TaskNo: "n", Kind: SummaryCardKindFailed, Title: "周报", Reason: "超时"}
	text := buildSummaryFallbackText(failed, "zh-CN")
	assert.Contains(t, text, "你的总结「周报」生成失败。")
	assert.Contains(t, text, "失败原因：超时")
}

// When no card sender is wired (or the card feature is off), a card request must
// still deliver — as a plain-text DM from the summary bot — never be dropped.
func TestDeliverCardNotification_DegradesToTextWhenSenderUnavailable(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	n := newTestNotify(ctx, nil, nil, nil, "tk")
	n.cardSender = nil         // no producer bound → cannot build a card
	n.summaryBotOK.Store(true) // summary bot already provisioned
	primeMemberCache(n, "spc_1", "uid_a")

	resp, err := n.deliverCardNotification(&NotifyReq{
		SpaceID: "spc_1", Service: "summary-service", Targets: []string{"uid_a"}, Card: validCard(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"uid_a"}, resp.Delivered)
	assert.Empty(t, resp.Filtered)
	assert.Equal(t, int32(1), atomic.LoadInt32(&wk.messageCount), "exactly one text DM should be sent")
}

// payload and card are mutually exclusive on the single endpoint (contract).
func TestIntegration_NotifyRejectsPayloadAndCardTogether(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	r := buildRouter(n)
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	h := http.Header{}
	h.Set(InternalTokenHeader, "tk")

	for name, payload := range map[string]map[string]interface{}{
		"non-empty payload": {"type": 1, "content": "ok"},
		"empty payload":     {},
	} {
		t.Run(name, func(t *testing.T) {
			w := doJSONRequest(t, r, http.MethodPost, "/v1/internal/notify", h, NotifyReq{
				SpaceID: "spc_1", Service: "summary-service", Targets: []string{"uid_a"},
				Payload: payload, Card: validCard(),
			})
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "err.server.notify.card_invalid")
			assert.Zero(t, atomic.LoadInt32(&wk.messageCount))
		})
	}
}

func TestIntegration_NotifyBatchRejectsCardField(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	r := buildRouter(n)
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	h := http.Header{}
	h.Set(InternalTokenHeader, "tk")

	w := doJSONRequest(t, r, http.MethodPost, "/v1/internal/notify/batch", h, BatchNotifyReq{Notifications: []NotifyReq{
		{SpaceID: "spc_1", Service: "summary-service", Targets: []string{"uid_a"}, Payload: map[string]interface{}{"type": 1, "content": "ok"}},
		{SpaceID: "spc_1", Service: "summary-service", Targets: []string{"uid_b"}, Card: validCard()},
	}})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "err.server.notify.card_invalid")
	assert.Zero(t, atomic.LoadInt32(&wk.messageCount))
}
