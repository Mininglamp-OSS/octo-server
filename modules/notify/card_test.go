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

func TestSendSummaryTextUsesNotificationBot(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	n := newTestNotify(ctx, nil, nil, nil, "tk")

	require.NoError(t, n.sendSummaryText("uid_a", "spc_1", "summary ready"))
	body, ok := wk.lastMessage.Load().([]byte)
	require.True(t, ok, "WuKongIM request body must be captured")
	var req map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, NotifyBotUIDValue, req["from_uid"])
}

// When no card sender is wired (or the card feature is off), a card request must
// still deliver — as a plain-text DM from the notification bot — never be dropped.
func TestDeliverCardNotification_DegradesToTextWhenSenderUnavailable(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	n := newTestNotify(ctx, nil, nil, nil, "tk")
	n.cardSender = nil  // no producer bound → cannot build a card
	n.botOK.Store(true) // notification bot already provisioned
	primeMemberCache(n, "spc_1", "uid_a")

	resp, err := n.deliverCardNotification(&NotifyReq{
		SpaceID: "spc_1", Service: "summary-service", Targets: []string{"uid_a"}, Card: validCard(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"uid_a"}, resp.Delivered)
	assert.Empty(t, resp.Filtered)
	assert.Equal(t, int32(1), atomic.LoadInt32(&wk.messageCount), "exactly one text DM should be sent")
}

// A non-member target must be excluded from delivery and reported in Filtered,
// never delivered — this locks the member-verify pre-filter. A regression that
// dropped it would otherwise pass every other card-path unit test (they only
// seed the delivered member). carddispatch re-verifies membership independently
// (Decision 3), but the pre-filter is the first gate and worth pinning. Uses
// the text-fallback path (nil sender) so no carddispatch mock is needed; the
// assertion is that the stranger reaches neither Delivered nor transport.
func TestDeliverCardNotification_NonMemberExcluded(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	n := newTestNotify(ctx, nil, nil, nil, "tk")
	n.cardSender = nil
	n.botOK.Store(true)
	primeMemberCache(n, "spc_1", "uid_member") // uid_stranger is NOT a member

	resp, err := n.deliverCardNotification(&NotifyReq{
		SpaceID: "spc_1", Service: "summary-service",
		Targets: []string{"uid_member", "uid_stranger"}, Card: validCard(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"uid_member"}, resp.Delivered)
	assert.Equal(t, "not_space_member", resp.Filtered["uid_stranger"])
	assert.NotContains(t, resp.Delivered, "uid_stranger")
	assert.Equal(t, int32(1), atomic.LoadInt32(&wk.messageCount), "only the member gets a DM")
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

// validDocsCard returns a minimal well-formed docs card request field set.
func validDocsCard() *DocsCardFields {
	return &DocsCardFields{
		DocID:     "doc_abcd",
		Kind:      DocsCardKindShared,
		Title:     "产品设计方案",
		ActorName: "Alice",
		Excerpt:   "Q3 上线计划已确认",
		UpdatedAt: "2026-07-13 15:04",
	}
}

func TestDeliverDocsCardNotification_ValidationRejectsBadFields(t *testing.T) {
	n := &Notify{} // validation short-circuits before any DB/cache access

	cases := map[string]NotifyReq{
		"missing docs card": {SpaceID: "s", Targets: []string{"u"}},
		"missing space":     {Targets: []string{"u"}, DocsCard: validDocsCard()},
		"empty targets":     {SpaceID: "s", Targets: nil, DocsCard: validDocsCard()},
		"missing doc_id":    {SpaceID: "s", Targets: []string{"u"}, DocsCard: &DocsCardFields{Kind: DocsCardKindShared, Title: "t"}},
		"missing title":     {SpaceID: "s", Targets: []string{"u"}, DocsCard: &DocsCardFields{DocID: "d", Kind: DocsCardKindShared}},
		"unknown kind":      {SpaceID: "s", Targets: []string{"u"}, DocsCard: &DocsCardFields{DocID: "d", Kind: "weird", Title: "t"}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			r := req
			_, err := n.deliverDocsCardNotification(&r)
			require.ErrorIs(t, err, errNotifyCardInvalid)
		})
	}
}

func TestBuildDocsCard_ProducesValidOctoV1(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "tk")

	for _, kind := range []string{DocsCardKindShared, DocsCardKindCommented, DocsCardKindAccessRequested} {
		t.Run(kind, func(t *testing.T) {
			card := validDocsCard()
			card.Kind = kind
			doc, err := n.buildDocsCard(context.Background(), "spc_1", card, "zh-CN")
			require.NoError(t, err)

			var cardObj map[string]interface{}
			require.NoError(t, json.Unmarshal(doc, &cardObj))
			envelope := map[string]interface{}{
				"type":         cardmsg.InteractiveCard.Int(),
				"card_version": cardmsg.CardVersion,
				"profile":      cardmsg.ProfileV1,
				"card":         cardObj,
			}
			require.NoError(t, cardmsg.Validate(envelope), "docs template output must pass octo/v1 Validate")
			// Deep link built from WebLoginURL origin only, /d/{doc_id}?sp={space}.
			assert.Contains(t, string(doc), "https://im.example.com/d/doc_abcd?sp=spc_1")
			assert.NotContains(t, string(doc), "/login")
			// Variant identifier is embedded in metadata.octo.variant.
			metadata, _ := cardObj["metadata"].(map[string]interface{})
			octo, _ := metadata["octo"].(map[string]interface{})
			assert.Equal(t, "docs."+kind, octo["variant"], "variant identifier reserved namespace")
			assert.Contains(t, metadata["webUrl"], "/d/doc_abcd")
		})
	}
}

func TestBuildDocsCard_AttributionFallsBackWithoutActor(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "tk")

	card := validDocsCard()
	card.ActorName = ""
	doc, err := n.buildDocsCard(context.Background(), "spc_1", card, "zh-CN")
	require.NoError(t, err)
	assert.Contains(t, string(doc), "有人分享了文档", "anonymous attribution must be used when actor is unknown")
	assert.NotContains(t, string(doc), " 分享了文档", "no leading-space placeholder for missing actor")
}

func TestBuildDocsCard_RejectsNonHTTPSDeepLinkOrigin(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "http://im.example.com" // not https
	n := newTestNotify(ctx, nil, nil, nil, "tk")

	_, err := n.buildDocsCard(context.Background(), "spc_1", validDocsCard(), "zh-CN")
	require.Error(t, err, "a non-https origin must fail docs card build so the caller degrades to text")
}

func TestBuildDocsFallbackText(t *testing.T) {
	text := buildDocsFallbackText(validDocsCard(), "zh-CN")
	assert.Contains(t, text, "Alice 分享了文档")
	assert.Contains(t, text, "文档：产品设计方案")
	assert.Contains(t, text, "Q3 上线计划已确认")
	assert.Contains(t, text, "时间：2026-07-13 15:04")
}

// Text fallback path when the docs card sender is unavailable: still delivers,
// never silently dropped. Mirrors the summary DegradesToText test.
func TestDeliverDocsCardNotification_DegradesToTextWhenSenderUnavailable(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	n := newTestNotify(ctx, nil, nil, nil, "tk")
	n.docsSender = nil // no producer bound → cannot build a card
	n.botOK.Store(true)
	primeMemberCache(n, "spc_1", "uid_a")

	resp, err := n.deliverDocsCardNotification(&NotifyReq{
		SpaceID: "spc_1", Service: "docs-service", Targets: []string{"uid_a"}, DocsCard: validDocsCard(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"uid_a"}, resp.Delivered)
	assert.Empty(t, resp.Filtered)
	assert.Equal(t, int32(1), atomic.LoadInt32(&wk.messageCount), "exactly one text DM should be sent")
}

// Docs path: same non-member pre-filter guarantee as the summary path above.
func TestDeliverDocsCardNotification_NonMemberExcluded(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	n := newTestNotify(ctx, nil, nil, nil, "tk")
	n.docsSender = nil
	n.botOK.Store(true)
	primeMemberCache(n, "spc_1", "uid_member") // uid_stranger is NOT a member

	resp, err := n.deliverDocsCardNotification(&NotifyReq{
		SpaceID: "spc_1", Service: "docs-service",
		Targets: []string{"uid_member", "uid_stranger"}, DocsCard: validDocsCard(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"uid_member"}, resp.Delivered)
	assert.Equal(t, "not_space_member", resp.Filtered["uid_stranger"])
	assert.NotContains(t, resp.Delivered, "uid_stranger")
	assert.Equal(t, int32(1), atomic.LoadInt32(&wk.messageCount), "only the member gets a DM")
}

// A newline embedded in a caller field must not inject a spoofed line into the
// plain-text fallback DM: sanitizeLine collapses control chars to spaces.
func TestBuildDocsFallbackText_StripsNewlineInjection(t *testing.T) {
	card := validDocsCard()
	card.ActorName = "Alice\n系统管理员"
	card.Title = "标题\n伪造：越权提示"
	text := buildDocsFallbackText(card, "zh-CN")
	assert.NotContains(t, text, "\n伪造", "an embedded title newline must not start a new line")
	assert.Contains(t, text, "Alice 系统管理员 分享了文档", "actor newline collapses to a space in the attribution")
}

// The single-endpoint mutex also covers DocsCard combined with either Payload
// or the summary Card field.
func TestIntegration_NotifyMutexRejectsDocsCardCombinations(t *testing.T) {
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

	cases := map[string]NotifyReq{
		"payload + docs card": {SpaceID: "spc_1", Service: "svc", Targets: []string{"uid_a"},
			Payload: map[string]interface{}{"type": 1, "content": "ok"}, DocsCard: validDocsCard()},
		"summary card + docs card": {SpaceID: "spc_1", Service: "svc", Targets: []string{"uid_a"},
			Card: validCard(), DocsCard: validDocsCard()},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			w := doJSONRequest(t, r, http.MethodPost, "/v1/internal/notify", h, req)
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

	// Both the summary Card and the docs DocsCard fields are single-endpoint
	// only; either one in a batch entry short-circuits the whole batch.
	cases := map[string][]NotifyReq{
		"summary card in batch": {
			{SpaceID: "spc_1", Service: "summary-service", Targets: []string{"uid_a"}, Payload: map[string]interface{}{"type": 1, "content": "ok"}},
			{SpaceID: "spc_1", Service: "summary-service", Targets: []string{"uid_b"}, Card: validCard()},
		},
		"docs card in batch": {
			{SpaceID: "spc_1", Service: "docs-service", Targets: []string{"uid_a"}, Payload: map[string]interface{}{"type": 1, "content": "ok"}},
			{SpaceID: "spc_1", Service: "docs-service", Targets: []string{"uid_b"}, DocsCard: validDocsCard()},
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			w := doJSONRequest(t, r, http.MethodPost, "/v1/internal/notify/batch", h, BatchNotifyReq{Notifications: entries})
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "err.server.notify.card_invalid")
			assert.Zero(t, atomic.LoadInt32(&wk.messageCount))
		})
	}
}
