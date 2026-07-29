package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// TestBuildDocsAccessRequestCardViaRegistry_UnwiredFailsExplicitly 验证 F7 + R2-2:
// DefaultRegistry 未注入 (composition bug) 时,build 通路返回 typed
// errCardTmplUnavailable —— 不再返回 sentinel 让 caller 静默回退到 legacy,
// 也**不是** ErrFieldsInvalid。caller (deliverDocsCardNotification) 对该 typed
// error 走 500 不降级 (见 TestDeliverDocsAccessRequest_UnwiredRegistryFailsClosed),
// 让 wiring 漏洞立即暴露而不是伪装成一条成功文本 DM。
func TestBuildDocsAccessRequestCardViaRegistry_UnwiredFailsExplicitly(t *testing.T) {
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(nil)
	defer cardtmpl.SetDefaultRegistry(prev)

	// 需要 ctx + Log 才不 nil deref;用最小 test notify。
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	n := newTestNotify(ctx, nil, nil, nil, "tk")

	_, _, err := n.buildDocsAccessRequestCardViaRegistry(context.Background(), "s1", validAccessRequestDocsCard(), "zh-CN")
	if err == nil {
		t.Fatalf("want non-nil error when registry unwired")
	}
	// R3-3: 精确锁定 typed errCardTmplUnavailable (500 分类),而非只断言"不是 400"。
	if !errors.Is(err, errCardTmplUnavailable) {
		t.Fatalf("want errCardTmplUnavailable, got %v", err)
	}
	// 且绝不能是 ErrFieldsInvalid (那会被 caller 翻 400,而 wiring bug 应走 500)。
	if errors.Is(err, cardtmpl.ErrFieldsInvalid) {
		t.Fatalf("unwired should NOT be typed ErrFieldsInvalid, got %v", err)
	}
}

// TestMapDocsCardFieldsToJSON_ShapesToSchema 验证 mapping 产物是能过 pilot schema
// 的合法 JSON。断言必填字段(requestId/state/document.title)与嵌套结构存在。
func TestMapDocsCardFieldsToJSON_ShapesToSchema(t *testing.T) {
	card := validAccessRequestDocsCard()
	raw, err := mapDocsCardFieldsToJSON(card, "zh-CN")
	if err != nil {
		t.Fatalf("mapDocsCardFieldsToJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, _ := m["requestId"].(string); got == "" {
		t.Errorf("requestId empty: %v", m["requestId"])
	}
	if got, _ := m["state"].(string); got != "pending" {
		t.Errorf("state != pending: %v", m["state"])
	}
	doc, _ := m["document"].(map[string]any)
	if doc == nil {
		t.Fatalf("document missing")
	}
	if got, _ := doc["title"].(string); got == "" {
		t.Errorf("document.title empty")
	}
	if got, _ := doc["docId"].(string); got == "" {
		t.Errorf("document.docId empty (mapping should carry DocsCardFields.DocID)")
	}
}

// TestBuildDocsAccessRequestCardViaRegistry_HappyPath 端到端验证:
// 装配一个内嵌 pilot 的 Registry → mapping → Render → 断言产物含
// metadata.octo.template 与合法 webUrl,业务字段回填正确。
func TestBuildDocsAccessRequestCardViaRegistry_HappyPath(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := freshPilotRegistry(t)
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	n := newTestNotify(ctx, nil, nil, nil, "tk")
	card := validAccessRequestDocsCard()

	doc, renderProfile, err := n.buildDocsAccessRequestCardViaRegistry(context.Background(), "space-1", card, "zh-CN")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if renderProfile != "octo-chat/v1" {
		t.Fatalf("render profile = %q, want octo-chat/v1", renderProfile)
	}
	var ac map[string]any
	if err := json.Unmarshal(doc, &ac); err != nil {
		t.Fatalf("parse ac: %v", err)
	}
	metadata, _ := ac["metadata"].(map[string]any)
	octo, _ := metadata["octo"].(map[string]any)
	tpl, _ := octo["template"].(map[string]any)
	if id, _ := tpl["id"].(string); id != string(docsaccessrequest.TemplateID) {
		t.Errorf("template.id: %v", id)
	}
	if v, _ := tpl["version"].(string); v != docsaccessrequest.TemplateVersionV3 {
		t.Errorf("template.version: %v", v)
	}
	if proto, _ := octo["protocol"].(string); proto != cardtmpl.Protocol {
		t.Errorf("octo.protocol: %v", proto)
	}
	webURL, _ := metadata["webUrl"].(string)
	if !strings.HasPrefix(webURL, "https://im.example.com/d/") {
		t.Errorf("webUrl not derived from WebLoginURL: %q", webURL)
	}
}

func TestBuildDocsAccessRequestUsesRuntimeCatalogPrincipal(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	registry := freshPilotRegistry(t)
	static, err := cardtmpl.NewStaticCatalog(registry)
	if err != nil {
		t.Fatal(err)
	}
	spy := &notifyCatalogSpy{Catalog: static}
	cardtmpl.SetDefaultCatalog(spy)
	t.Cleanup(func() { cardtmpl.SetDefaultCatalog(nil) })

	n := newTestNotify(ctx, nil, nil, nil, "tk")
	if _, _, err := n.buildDocsAccessRequestCardViaRegistry(
		context.Background(), "space-1", validAccessRequestDocsCard(), "zh-CN",
	); err != nil {
		t.Fatalf("build: %v", err)
	}
	if spy.lastRender.Access.Purpose != cardtmpl.CatalogPurposeNewSend ||
		spy.lastRender.Access.Principal.Kind != cardtmpl.CatalogPrincipalInternalProducer ||
		spy.lastRender.Access.Principal.ID != docsNotifyProducerID ||
		spy.lastRender.Access.Principal.SpaceID != "space-1" {
		t.Fatalf("catalog access = %+v", spy.lastRender.Access)
	}
}

func TestNotifyCatalogOperationsUseBoundedContexts(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	static, err := cardtmpl.NewStaticCatalog(freshPilotRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	spy := &notifyDeadlineCatalog{Catalog: static}
	cardtmpl.SetDefaultCatalog(spy)
	t.Cleanup(func() { cardtmpl.SetDefaultCatalog(nil) })
	card := validAccessRequestDocsCard()

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if err := preflightDocsAccessRequestSchema(parent, card); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if _, ok := templateFallbackText(card, "zh-CN"); !ok {
		t.Fatal("template fallback was unavailable")
	}
	n := newTestNotify(ctx, nil, nil, nil, "tk")
	if _, _, err := n.buildDocsAccessRequestCardViaRegistry(
		context.Background(), "space-1", card, "zh-CN",
	); err != nil {
		t.Fatalf("build: %v", err)
	}
	if !spy.metaDefaultDeadline || !spy.metaDefaultCanceled || !spy.fallbackDeadline || !spy.renderDeadline {
		t.Fatalf("catalog deadline/cancel meta=%v/%v fallback=%v render=%v, want all true",
			spy.metaDefaultDeadline, spy.metaDefaultCanceled, spy.fallbackDeadline, spy.renderDeadline)
	}
}

func TestPreflightDocsAccessRequestPreservesRuntimeCatalogFailures(t *testing.T) {
	static, err := cardtmpl.NewStaticCatalog(freshPilotRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{
		cardtmpl.ErrRuntimeCatalogBlocked,
		cardtmpl.ErrRuntimeCatalogDisabled,
		cardtmpl.ErrRuntimeCatalogNotAuthorized,
		cardtmpl.ErrRuntimeCatalogIntegrity,
		cardtmpl.ErrRuntimeCatalogUnavailable,
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			cardtmpl.SetDefaultCatalog(&notifyFailureCatalog{Catalog: static, metaErr: cause})
			t.Cleanup(func() { cardtmpl.SetDefaultCatalog(nil) })

			err := preflightDocsAccessRequestSchema(context.Background(), validAccessRequestDocsCard())
			if !errors.Is(err, cause) {
				t.Fatalf("preflight error = %v, want cause %v", err, cause)
			}
			if errors.Is(err, errCardTmplUnavailable) {
				t.Fatalf("runtime rejection was flattened to composition error: %v", err)
			}
		})
	}
}

// TestBuildDocsAccessRequestCardViaRegistry_FieldsInvalid 验证 C1 policy:
// 传入 schema 违规字段 → 返回 typed cardtmpl.ErrFieldsInvalid,
// caller (card.go 分支) 会翻成 errNotifyCardInvalid → 400 零投递。
func TestBuildDocsAccessRequestCardViaRegistry_FieldsInvalid(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := freshPilotRegistry(t)
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	n := newTestNotify(ctx, nil, nil, nil, "tk")
	card := validAccessRequestDocsCard()
	card.Title = "" // schema violation: document.title minLength=1

	_, _, err := n.buildDocsAccessRequestCardViaRegistry(context.Background(), "space-1", card, "zh-CN")
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
		t.Fatalf("want cardtmpl.ErrFieldsInvalid, got %v", err)
	}
}

// ---- helpers ----

func freshPilotRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	r := cardtmpl.NewRegistry()
	r.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	r.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	r.Freeze()
	return r
}

// installTestCardTmplRegistry 装配一个 fresh Registry 并注入到 cardtmpl.DefaultRegistry,
// t.Cleanup 恢复原值。任何走 Registry.Render 的 test (v2 分支) 都要调本 helper,
// 否则 R2-2 fail-close 会返 500 error 让 test 崩掉。
func installTestCardTmplRegistry(t *testing.T) {
	t.Helper()
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(freshPilotRegistry(t))
	t.Cleanup(func() { cardtmpl.SetDefaultRegistry(prev) })
}

type notifyCatalogSpy struct {
	cardtmpl.Catalog
	lastRender cardtmpl.CatalogRenderRequest
}

func (s *notifyCatalogSpy) Render(ctx context.Context, request cardtmpl.CatalogRenderRequest) (map[string]any, error) {
	s.lastRender = request
	return s.Catalog.Render(ctx, request)
}

type notifyDeadlineCatalog struct {
	cardtmpl.Catalog
	metaDefaultDeadline bool
	metaDefaultCanceled bool
	fallbackDeadline    bool
	renderDeadline      bool
}

type notifyFailureCatalog struct {
	cardtmpl.Catalog
	metaErr   error
	renderErr error
}

func (c *notifyFailureCatalog) MetaDefault(
	ctx context.Context,
	request cardtmpl.CatalogDefaultRequest,
) (cardtmpl.TemplateMeta, error) {
	if c.metaErr != nil {
		return cardtmpl.TemplateMeta{}, c.metaErr
	}
	return c.Catalog.MetaDefault(ctx, request)
}

func (c *notifyFailureCatalog) Render(
	ctx context.Context,
	request cardtmpl.CatalogRenderRequest,
) (map[string]any, error) {
	if c.renderErr != nil {
		return nil, c.renderErr
	}
	return c.Catalog.Render(ctx, request)
}

func (s *notifyDeadlineCatalog) MetaDefault(
	ctx context.Context,
	request cardtmpl.CatalogDefaultRequest,
) (cardtmpl.TemplateMeta, error) {
	_, s.metaDefaultDeadline = ctx.Deadline()
	s.metaDefaultCanceled = errors.Is(ctx.Err(), context.Canceled)
	return s.Catalog.MetaDefault(ctx, request)
}

func (s *notifyDeadlineCatalog) FallbackText(
	ctx context.Context,
	request cardtmpl.CatalogFallbackRequest,
) (string, error) {
	_, s.fallbackDeadline = ctx.Deadline()
	return s.Catalog.FallbackText(ctx, request)
}

func (s *notifyDeadlineCatalog) Render(
	ctx context.Context,
	request cardtmpl.CatalogRenderRequest,
) (map[string]any, error) {
	_, s.renderDeadline = ctx.Deadline()
	return s.Catalog.Render(ctx, request)
}
