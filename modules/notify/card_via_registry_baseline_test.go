package notify

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// TestBuildDocsAccessRequestCard_MigrationBaseline 验证 A11:
// 对同一 DocsCardFields 输入,新链路 (Registry.Render + pilot Template) 与
// legacy 链路 (buildDocsAccessRequestCard) 输出在**删除 metadata.octo.protocol
// 与 metadata.octo.template 两个新字段**后字节等价。
//
// 这是 pilot 迁移的"零功能漂移"证明 —— 只增新 metadata,不动其余字段。
func TestBuildDocsAccessRequestCard_MigrationBaseline(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
	registry.Freeze()
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	n := newTestNotify(ctx, nil, nil, nil, "tk")
	card := validAccessRequestDocsCard()

	legacyRaw, err := n.buildDocsAccessRequestCard(context.Background(), "space-1", card, "zh-CN")
	if err != nil {
		t.Fatalf("legacy build: %v", err)
	}
	newRaw, err := n.buildDocsAccessRequestCardViaRegistry(context.Background(), "space-1", card, "zh-CN")
	if err != nil {
		t.Fatalf("new build: %v", err)
	}

	// Parse both to maps for structural comparison (byte comparison is too strict:
	// legacy has an implicit AC version=1.5 + metadata layout; new inherits.)
	var legacy, newer map[string]any
	if err := json.Unmarshal(legacyRaw, &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if err := json.Unmarshal(newRaw, &newer); err != nil {
		t.Fatalf("unmarshal new: %v", err)
	}
	// legacy 侧 view_document Action.OpenUrl 现在也带 id (A15c 修正),两侧应等价。

	// Strip new-only metadata subfields.
	stripOctoNewFields(newer)

	if !reflect.DeepEqual(legacy, newer) {
		t.Errorf("A11 baseline drift after stripping new metadata fields.\nlegacy=%s\nnew=%s",
			pretty(legacy), pretty(newer))
	}

	// AC 版本一致
	if v, _ := newer["version"].(string); v != cardmsg.CardVersion {
		t.Errorf("new card.version = %q, want %q", v, cardmsg.CardVersion)
	}
}

func stripOctoNewFields(card map[string]any) {
	md, _ := card["metadata"].(map[string]any)
	if md == nil {
		return
	}
	octo, _ := md["octo"].(map[string]any)
	if octo == nil {
		return
	}
	delete(octo, "protocol")
	delete(octo, "template")
	if len(octo) == 0 {
		delete(md, "octo")
	}
}

func pretty(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
