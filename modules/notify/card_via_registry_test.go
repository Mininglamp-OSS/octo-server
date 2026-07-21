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

// TestBuildDocsAccessRequestCardViaRegistry_UnwiredFallback 验证 DefaultRegistry
// 未注入时返回 sentinel error;caller 依赖它判定是否降级到 legacy builder。
// 这是"新 pilot 不接管失败旧路径也能继续跑"的兼容契约。
func TestBuildDocsAccessRequestCardViaRegistry_UnwiredFallback(t *testing.T) {
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(nil)
	defer cardtmpl.SetDefaultRegistry(prev)

	n := &Notify{} // ctx nil ok, function should short-circuit on nil registry
	_, err := n.buildDocsAccessRequestCardViaRegistry(context.Background(), "s1", validAccessRequestDocsCard(), "zh-CN")
	if err == nil || !errors.Is(err, errCardTmplRegistryUnwired) {
		t.Fatalf("want errCardTmplRegistryUnwired, got %v", err)
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

	doc, err := n.buildDocsAccessRequestCardViaRegistry(context.Background(), "space-1", card, "zh-CN")
	if err != nil {
		t.Fatalf("build: %v", err)
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
	if v, _ := tpl["version"].(string); v != docsaccessrequest.TemplateVersion {
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

	_, err := n.buildDocsAccessRequestCardViaRegistry(context.Background(), "space-1", card, "zh-CN")
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
	r.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	r.Freeze()
	return r
}
