package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docscommented "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_commented"
	docsshared "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_shared"
)

// TestMapDocsDisplayFields_TruncatesDisplayFields 断言 P1(PR #649 review):超长
// 展示字段(title/actorName/excerpt/updatedAt)被服务端截断到 schema cap 后仍能
// 投递(preflight 通过 + build 成功,不再翻 400 零投递);而 docId(deep-link key)
// 超长仍是 C1 400 —— 绝不静默截断。同时锁 notify 本地 cap 与 schema maxLength 对齐:
// 若某 cap 设大于 schema,截断后仍超 schema → preflight 会 400,本测试即失败。
func TestMapDocsDisplayFields_TruncatesDisplayFields(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := cardtmpl.NewRegistry()
	registry.Register(docscommented.New(), docscommented.Assets, docscommented.HandoffRoot)
	registry.SetDefault(docscommented.TemplateID, docscommented.TemplateVersion)
	registry.Register(docsshared.New(), docsshared.Assets, docsshared.HandoffRoot)
	registry.SetDefault(docsshared.TemplateID, docsshared.TemplateVersion)
	registry.Freeze()
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	n := newTestNotify(ctx, nil, nil, nil, "tk")

	// 全展示字段远超 schema cap → 截断后 preflight 通过 + build 成功(投递不丢)。
	huge := &DocsCardFields{
		DocID:     "d_ok",
		Kind:      DocsCardKindCommented,
		Title:     strings.Repeat("标", 500),  // cap 200
		ActorName: strings.Repeat("名", 400),  // cap 120
		Excerpt:   strings.Repeat("评", 1000), // cap 300
		UpdatedAt: strings.Repeat("时", 300),  // cap 80
	}
	if err := preflightDocsDisplaySchema(context.Background(), "space-preflight", huge, docscommented.TemplateID); err != nil {
		t.Fatalf("preflight should PASS after truncation, got %v", err)
	}
	if _, _, err := n.buildDocsDisplayCardViaRegistry(context.Background(),
		"space-c", huge, "zh-CN", docscommented.TemplateID, docscommented.StateShown); err != nil {
		t.Fatalf("build should SUCCEED after truncation, got %v", err)
	}

	// docId 超长(>200)→ 仍 C1 400:deep-link key 不截断。
	badDoc := &DocsCardFields{
		DocID: strings.Repeat("x", 300), Title: "OK", Kind: DocsCardKindShared,
	}
	err := preflightDocsDisplaySchema(context.Background(), "space-preflight", badDoc, docsshared.TemplateID)
	if err == nil || !errorsIsFieldsInvalid(err) {
		t.Fatalf("over-length docId must stay C1 400, got %v", err)
	}
}

// TestBuildDocsDisplayCards_MigrationBaseline 是 roadmap C 的**规范化 JSON 等价**
// 基线(canonical-JSON equivalence,非字面 wire 字节):对 docs.commented /
// docs.shared 两张 v1 展示卡,断言 legacy buildDocsCard 与 Registry.Render 出的
// card 节点,在删除 metadata.octo.{protocol,template} 后,经 unmarshal→canonical
// (sorted-key)→remarshal 得到的 JSON 完全相等 —— body / facts / actions /
// attribution / variant / source / webUrl 均一致。round-trip 会归一化 key 序 /
// 空白 / 转义形态,所以它锁的是**语义/结构等价**(词表 / 排序 / 别名 / 结构漂移
// 都会失败),不是逐字节一致(P2, PR #649 review:命名别 over-claim 成 byte-equal)。
//
// 与 A11 (access_requested) 的两处不同:
//   - v1 展示卡没有 view_document action id (只有 access-request 交互契约要求),
//     所以只需要 strip metadata.octo.{protocol,template};
//   - 每张卡独立跑 —— 两卡的 variant 不同,不共用 fixture。
func TestBuildDocsDisplayCards_MigrationBaseline(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := cardtmpl.NewRegistry()
	registry.Register(docscommented.New(), docscommented.Assets, docscommented.HandoffRoot)
	registry.SetDefault(docscommented.TemplateID, docscommented.TemplateVersion)
	registry.Register(docsshared.New(), docsshared.Assets, docsshared.HandoffRoot)
	registry.SetDefault(docsshared.TemplateID, docsshared.TemplateVersion)
	registry.Freeze()
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	n := newTestNotify(ctx, nil, nil, nil, "tk")

	cases := []struct {
		name       string
		card       *DocsCardFields
		templateID cardtmpl.ID
		state      cardtmpl.State
	}{
		{
			name: "commented_zh_full",
			card: &DocsCardFields{
				DocID: "d_2026-q3-okr", Title: "2026 Q3 OKR", Kind: DocsCardKindCommented,
				ActorName: "张三", Excerpt: "我在 KR3 段落补了数据。", UpdatedAt: "2026-07-16 11:00",
			},
			templateID: docscommented.TemplateID, state: docscommented.StateShown,
		},
		{
			name: "commented_zh_anon_no_ts",
			card: &DocsCardFields{
				DocID: "d_2026-q3-okr", Title: "2026 Q3 OKR", Kind: DocsCardKindCommented,
				ActorName: "", Excerpt: "匿名评论。", UpdatedAt: "",
			},
			templateID: docscommented.TemplateID, state: docscommented.StateShown,
		},
		{
			name: "shared_zh_full",
			card: &DocsCardFields{
				DocID: "d_2026-q3-okr", Title: "2026 Q3 OKR", Kind: DocsCardKindShared,
				ActorName: "李四", Excerpt: "先分享一版。", UpdatedAt: "2026-07-16 10:30",
			},
			templateID: docsshared.TemplateID, state: docsshared.StateShown,
		},
		{
			name: "shared_zh_anon_no_excerpt",
			card: &DocsCardFields{
				DocID: "d_2026-q3-okr", Title: "2026 Q3 OKR", Kind: DocsCardKindShared,
				ActorName: "", Excerpt: "", UpdatedAt: "2026-07-16 10:30",
			},
			templateID: docsshared.TemplateID, state: docsshared.StateShown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacyRaw, err := n.buildDocsCard(context.Background(), "space-c", tc.card, "zh-CN")
			if err != nil {
				t.Fatalf("legacy build: %v", err)
			}
			var legacy map[string]any
			if err := json.Unmarshal(legacyRaw, &legacy); err != nil {
				t.Fatalf("unmarshal legacy: %v", err)
			}

			newRaw, _, err := n.buildDocsDisplayCardViaRegistry(context.Background(),
				"space-c", tc.card, "zh-CN", tc.templateID, tc.state)
			if err != nil {
				t.Fatalf("new build: %v", err)
			}
			var newer map[string]any
			if err := json.Unmarshal(newRaw, &newer); err != nil {
				t.Fatalf("unmarshal new: %v", err)
			}

			stripped := stripOctoNewFields(newer)
			legacyCanon := canonicalJSON(t, legacy)
			newCanon := canonicalJSON(t, stripped)
			if !bytes.Equal(legacyCanon, newCanon) {
				t.Fatalf("baseline drift for %s.\n--- legacy ---\n%s\n--- new (stripped) ---\n%s",
					tc.name, legacyCanon, newCanon)
			}
		})
	}
}

// TestBuildDocsDisplayCards_MetadataOctoInjected 断言迁移后 Registry.Render 的
// card 节点确实带上了 §5 强制注入的 metadata.octo.{protocol,template},与基线
// stripped 前的原始字节形成互补校验。
func TestBuildDocsDisplayCards_MetadataOctoInjected(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := cardtmpl.NewRegistry()
	registry.Register(docscommented.New(), docscommented.Assets, docscommented.HandoffRoot)
	registry.SetDefault(docscommented.TemplateID, docscommented.TemplateVersion)
	registry.Register(docsshared.New(), docsshared.Assets, docsshared.HandoffRoot)
	registry.SetDefault(docsshared.TemplateID, docsshared.TemplateVersion)
	registry.Freeze()
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	n := newTestNotify(ctx, nil, nil, nil, "tk")
	specs := []struct {
		id      cardtmpl.ID
		version string
		state   cardtmpl.State
		kind    string
	}{
		{docscommented.TemplateID, docscommented.TemplateVersion, docscommented.StateShown, DocsCardKindCommented},
		{docsshared.TemplateID, docsshared.TemplateVersion, docsshared.StateShown, DocsCardKindShared},
	}
	for _, s := range specs {
		card := &DocsCardFields{
			DocID: "d_1", Title: "T", Kind: s.kind, ActorName: "A", Excerpt: "E", UpdatedAt: "U",
		}
		newRaw, _, err := n.buildDocsDisplayCardViaRegistry(context.Background(),
			"space-c", card, "zh-CN", s.id, s.state)
		if err != nil {
			t.Fatalf("%s build: %v", s.id, err)
		}
		var m map[string]any
		if err := json.Unmarshal(newRaw, &m); err != nil {
			t.Fatalf("%s unmarshal: %v", s.id, err)
		}
		octo := m["metadata"].(map[string]any)["octo"].(map[string]any)
		if octo["protocol"] != "octo-card@1.0" {
			t.Errorf("%s protocol = %v", s.id, octo["protocol"])
		}
		tpl := octo["template"].(map[string]any)
		if tpl["id"] != string(s.id) || tpl["version"] != s.version {
			t.Errorf("%s template = %+v", s.id, tpl)
		}
	}
}

// TestPreflightDocsDisplaySchema_C1RejectsEmptyDocID 断言 C1 preflight 对
// schema 违规(空 docId)返回 typed ErrFieldsInvalid,不静默降级。
func TestPreflightDocsDisplaySchema_C1RejectsEmptyDocID(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.Register(docscommented.New(), docscommented.Assets, docscommented.HandoffRoot)
	registry.SetDefault(docscommented.TemplateID, docscommented.TemplateVersion)
	registry.Freeze()
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	// docId 空 → schema minLength=1 违规
	card := &DocsCardFields{DocID: "", Title: "T", Kind: DocsCardKindCommented}
	err := preflightDocsDisplaySchema(context.Background(), "space-preflight", card, docscommented.TemplateID)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid, got nil")
	}
	if !errorsIsFieldsInvalid(err) {
		t.Errorf("want ErrFieldsInvalid, got %v", err)
	}
}

// errorsIsFieldsInvalid 匹配 pilot preflight 相同的 wrap 语义。
func errorsIsFieldsInvalid(err error) bool {
	return err != nil && (bytes.Contains([]byte(err.Error()), []byte("fields did not pass input schema")) ||
		bytes.Contains([]byte(err.Error()), []byte("cardtmpl: fields did not pass input schema")))
}
