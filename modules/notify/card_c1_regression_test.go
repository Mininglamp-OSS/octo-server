package notify

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/prometheus/client_golang/prometheus"
)

func TestDeliverDocsAccessRequest_BlockBetweenPreflightAndRenderFailsClosed(t *testing.T) {
	t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", "true")
	t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "true")
	static, err := cardtmpl.NewStaticCatalog(freshPilotRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	cardtmpl.SetDefaultCatalog(&notifyFailureCatalog{
		Catalog: static, renderErr: cardtmpl.ErrRuntimeCatalogBlocked,
	})
	t.Cleanup(func() { cardtmpl.SetDefaultCatalog(nil) })

	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "tk")
	n.botOK.Store(true)
	primeMemberCache(n, "space-1", "user-b")
	capture := &capturingCardSender{}
	n.docsSender = capture

	resp, err := n.deliverDocsCardNotification(context.Background(), &NotifyReq{
		SpaceID: "space-1", Service: "docs", Targets: []string{"user-b"}, ActorUID: "user-a",
		DocsCard: validAccessRequestDocsCard(),
	})
	if !errors.Is(err, cardtmpl.ErrRuntimeCatalogBlocked) {
		t.Fatalf("delivery error = %v, want ErrRuntimeCatalogBlocked (resp=%v)", err, resp)
	}
	if resp != nil || len(capture.cards) != 0 || atomic.LoadInt32(&wk.messageCount) != 0 {
		t.Fatalf("blocked runtime delivered content: resp=%v cards=%d text=%d",
			resp, len(capture.cards), atomic.LoadInt32(&wk.messageCount))
	}
}

// TestDeliverDocsAccessRequest_UnwiredRegistryFailsClosed 是 R2-2 的 caller 级锁:
// gate 开 + DefaultRegistry 未注入 (composition bug) → deliverDocsCardNotification
// 在 preflight 阶段 (memberCache / docsSender gate 之前) 返回 typed
// errCardTmplUnavailable (api.go 兜底走 500),resp=nil,零投递 —— 绝不把 wiring
// 漏洞伪装成一条成功文本 DM (§10 / A14)。
func TestDeliverDocsAccessRequest_UnwiredRegistryFailsClosed(t *testing.T) {
	t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", "true")
	t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "true")
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(nil)
	defer cardtmpl.SetDefaultRegistry(prev)

	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "tk")
	n.botOK.Store(true)
	primeMemberCache(n, "space-1", "user-b")
	capture := &capturingCardSender{}
	n.docsSender = capture

	resp, err := n.deliverDocsCardNotification(context.Background(), &NotifyReq{
		SpaceID: "space-1", Service: "docs", Targets: []string{"user-b"}, ActorUID: "user-a",
		DocsCard: validAccessRequestDocsCard(),
	})
	if err == nil {
		t.Fatalf("want fail-close error, got nil (resp=%v)", resp)
	}
	if !errors.Is(err, errCardTmplUnavailable) {
		t.Fatalf("want errCardTmplUnavailable (500), got %v", err)
	}
	if resp != nil {
		t.Errorf("want nil resp on fail-close, got %v", resp)
	}
	if got := len(capture.cards); got != 0 {
		t.Errorf("want zero delivery on composition bug, got %d cards", got)
	}
}

// TestDeliverDocsAccessRequest_MalformedAvatarRejected400 是 R3-1 的 caller 级锁:
// avatar = "https://?x=1" (host 为空,能过 schema 的正则 pattern) → preflight 的
// URL parser 拦截 → C1 400 (errNotifyCardInvalid),resp=nil,零投递 (既不发卡也
// 不降级文本)。这是本轮修复的核心:坏 URL 字段不再漏到 Build 被误判成 render_error。
func TestDeliverDocsAccessRequest_MalformedAvatarRejected400(t *testing.T) {
	t.Setenv("OCTO_DOCS_APPROVAL_CARD_ENABLED", "true")
	t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "true")
	installTestCardTmplRegistry(t)

	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "tk")
	n.botOK.Store(true)
	primeMemberCache(n, "space-1", "user-b")
	capture := &capturingCardSender{}
	n.docsSender = capture

	card := validAccessRequestDocsCard()
	card.ActorAvatarURL = "https://?x=1" // 空 host,滑过 schema 正则,必须被 preflight parser 拦截

	resp, err := n.deliverDocsCardNotification(context.Background(), &NotifyReq{
		SpaceID: "space-1", Service: "docs", Targets: []string{"user-b"}, ActorUID: "user-a",
		DocsCard: card,
	})
	if !errors.Is(err, errNotifyCardInvalid) {
		t.Fatalf("want errNotifyCardInvalid (C1 400), got err=%v resp=%v", err, resp)
	}
	if resp != nil {
		t.Errorf("want nil resp on C1 reject, got %v", resp)
	}
	if got := len(capture.cards); got != 0 {
		t.Errorf("want zero delivery on C1 reject, got %d cards", got)
	}
}

// TestPreflightDocsAccessRequestSchema_AvatarSemantics 单测 preflight 的 avatar
// 语义校验:host 为空 / 非 https 的 URL 一律 ErrFieldsInvalid;合法 https 与空串通过。
func TestPreflightDocsAccessRequestSchema_AvatarSemantics(t *testing.T) {
	installTestCardTmplRegistry(t)

	bad := []string{
		"https://?x=1",  // 空 host (query 紧跟 //)
		"https://#frag", // 空 host (fragment 紧跟 //)
		"http://ok.com", // 非 https (schema 正则先拦)
		"ftp://x/y",     // 非 https
		"https:///path", // 首字符斜杠,host 空
	}
	for _, u := range bad {
		card := validAccessRequestDocsCard()
		card.ActorAvatarURL = u
		if err := preflightDocsAccessRequestSchema(context.Background(), "space-preflight", card); !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
			t.Errorf("avatar %q: want ErrFieldsInvalid, got %v", u, err)
		}
	}

	ok := []string{"", "https://cdn.example.com/a.png"}
	for _, u := range ok {
		card := validAccessRequestDocsCard()
		card.ActorAvatarURL = u
		if err := preflightDocsAccessRequestSchema(context.Background(), "space-preflight", card); err != nil {
			t.Errorf("avatar %q: want pass, got %v", u, err)
		}
	}
}

// TestPreflight_EmitsFieldsInvalidMetric 验证 R2-8:preflight 阶段的 400 (Render
// 未触发) 也会打 dmwork_cardtmpl_build_total{result=fields_invalid} —— 否则最
// "合法"的 400 路径在指标里完全不可见。
func TestPreflight_EmitsFieldsInvalidMetric(t *testing.T) {
	installTestCardTmplRegistry(t)
	reg := prometheus.NewRegistry()
	cardtmpl.SetGlobalMetrics(cardtmpl.NewMetrics(reg))
	defer cardtmpl.SetGlobalMetrics(nil)

	card := validAccessRequestDocsCard()
	card.Title = "" // schema violation: document.title minLength=1
	if err := preflightDocsAccessRequestSchema(context.Background(), "space-preflight", card); !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
		t.Fatalf("want ErrFieldsInvalid, got %v", err)
	}

	if got := gatherCounterTotal(t, reg, "dmwork_cardtmpl_build_total"); got < 1 {
		t.Errorf("preflight schema failure did not emit build_total metric, got %v", got)
	}
}

// gatherCounterTotal 汇总某 counter 全部 label 组合的当前值。
func gatherCounterTotal(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}
