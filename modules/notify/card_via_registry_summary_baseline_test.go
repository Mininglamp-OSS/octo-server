package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	summarycompleted "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_completed"
	summaryfailed "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_failed"
)

// TestBuildSummaryCards_MigrationBaseline 是 roadmap C PR-2 的字节等价基线:
// 对 summary.completed / summary.failed 两张 v1 展示卡,断言 legacy
// buildSummaryCard 与 Registry.Render 出的 card 节点在删除
// metadata.octo.{protocol,template} 之后 canonical JSON 字节完全相等 —— body /
// facts / actions / attribution / variant / source / webUrl 均一致。任何漂移
// (词表 / 排序 / 别名 / 结构)都会失败。
//
// 与 PR-1 (docs.commented/shared) 同 pattern:v1 展示卡无 action id,只需 strip
// metadata.octo.{protocol,template};stripOctoNewFields 与 canonicalJSON 复用
// PR-1 已在 card_via_registry_baseline_test.go 建立的 helper。
//
// 4 fixtures 覆盖(baseline 全走 zh-CN;lang 不同的分支由 Template 单测覆盖):
//   - completed_zh_full:completed 带 timeRange/members/msgCount/generatedAt(生产典型形态)
//   - completed_zh_minimal:completed 只带 taskNo+title(下限)
//   - failed_zh_with_reason:failed 带 reason(生产典型形态)
//   - failed_zh_full:failed 同时携带 reason + facts(边界 —— 失败卡也允许带 facts)
//
// baseline 不测 en 分支的原因:legacy buildSummaryCard 的按钮 label 走
// i18n.OutboundLanguage(ctx),背景 ctx 默认解析为 zh,与 Registry 路径的
// env.Lang 直传解耦。en 展示分支的字节形状由 Template 单测(summary_completed /
// summary_failed 的 TestRenderEnglishSourceLocalized)独立覆盖,这里只锁"同语言
// 下 legacy vs Registry 字节等价"。
func TestBuildSummaryCards_MigrationBaseline(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := cardtmpl.NewRegistry()
	registry.Register(summarycompleted.New(), summarycompleted.Assets, summarycompleted.HandoffRoot)
	registry.SetDefault(summarycompleted.TemplateID, summarycompleted.TemplateVersion)
	registry.Register(summaryfailed.New(), summaryfailed.Assets, summaryfailed.HandoffRoot)
	registry.SetDefault(summaryfailed.TemplateID, summaryfailed.TemplateVersion)
	registry.Freeze()
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	n := newTestNotify(ctx, nil, nil, nil, "tk")

	cases := []struct {
		name       string
		card       *SummaryCardFields
		lang       string
		templateID cardtmpl.ID
		state      cardtmpl.State
	}{
		{
			name: "completed_zh_full",
			card: &SummaryCardFields{
				TaskNo: "sum_2026-07-16-daily", Kind: SummaryCardKindCompleted,
				Title: "2026-07-16 每日总结", TimeRange: "2026-07-16 00:00 – 24:00",
				Members: 12, MsgCount: 348, GeneratedAt: "2026-07-16 23:59",
			},
			lang:       "zh-CN",
			templateID: summarycompleted.TemplateID, state: summarycompleted.StateShown,
		},
		{
			name: "completed_zh_minimal",
			card: &SummaryCardFields{
				TaskNo: "sum_2026-07-16-daily", Kind: SummaryCardKindCompleted,
				Title: "2026-07-16 每日总结",
			},
			lang:       "zh-CN",
			templateID: summarycompleted.TemplateID, state: summarycompleted.StateShown,
		},
		{
			name: "failed_zh_with_reason",
			card: &SummaryCardFields{
				TaskNo: "sum_2026-07-16-daily", Kind: SummaryCardKindFailed,
				Title: "2026-07-16 每日总结", Reason: "上游模型 429,3 次重试仍失败",
			},
			lang:       "zh-CN",
			templateID: summaryfailed.TemplateID, state: summaryfailed.StateShown,
		},
		{
			name: "failed_zh_full",
			card: &SummaryCardFields{
				TaskNo: "sum_2026-07-16-daily", Kind: SummaryCardKindFailed,
				Title: "2026-07-16 每日总结", Reason: "上游模型 429",
				TimeRange: "2026-07-16 00:00 – 24:00", Members: 12, MsgCount: 348,
				GeneratedAt: "2026-07-16 23:59",
			},
			lang:       "zh-CN",
			templateID: summaryfailed.TemplateID, state: summaryfailed.StateShown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacyRaw, err := n.buildSummaryCard(context.Background(), "space-c", tc.card, tc.lang)
			if err != nil {
				t.Fatalf("legacy build: %v", err)
			}
			var legacy map[string]any
			if err := json.Unmarshal(legacyRaw, &legacy); err != nil {
				t.Fatalf("unmarshal legacy: %v", err)
			}

			newRaw, _, err := n.buildSummaryCardViaRegistry(context.Background(),
				"space-c", tc.card, tc.lang, tc.templateID, tc.state)
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

// TestBuildSummaryCards_MetadataOctoInjected 断言迁移后 Registry.Render 的 card
// 节点确实带上了 §5 强制注入的 metadata.octo.{protocol,template},与基线 stripped
// 前的原始字节形成互补校验。
func TestBuildSummaryCards_MetadataOctoInjected(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	registry := cardtmpl.NewRegistry()
	registry.Register(summarycompleted.New(), summarycompleted.Assets, summarycompleted.HandoffRoot)
	registry.SetDefault(summarycompleted.TemplateID, summarycompleted.TemplateVersion)
	registry.Register(summaryfailed.New(), summaryfailed.Assets, summaryfailed.HandoffRoot)
	registry.SetDefault(summaryfailed.TemplateID, summaryfailed.TemplateVersion)
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
		{summarycompleted.TemplateID, summarycompleted.TemplateVersion, summarycompleted.StateShown, SummaryCardKindCompleted},
		{summaryfailed.TemplateID, summaryfailed.TemplateVersion, summaryfailed.StateShown, SummaryCardKindFailed},
	}
	for _, s := range specs {
		card := &SummaryCardFields{TaskNo: "t", Title: "T", Kind: s.kind}
		if s.kind == SummaryCardKindFailed {
			card.Reason = "upstream failure"
		}
		newRaw, _, err := n.buildSummaryCardViaRegistry(context.Background(),
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

// TestPreflightSummarySchema_C1RejectsEmptyTaskNo 断言 C1 preflight 对 schema
// 违规(空 taskNo)返回 typed ErrFieldsInvalid,不静默降级。
func TestPreflightSummarySchema_C1RejectsEmptyTaskNo(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.Register(summarycompleted.New(), summarycompleted.Assets, summarycompleted.HandoffRoot)
	registry.SetDefault(summarycompleted.TemplateID, summarycompleted.TemplateVersion)
	registry.Register(summaryfailed.New(), summaryfailed.Assets, summaryfailed.HandoffRoot)
	registry.SetDefault(summaryfailed.TemplateID, summaryfailed.TemplateVersion)
	registry.Freeze()
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(registry)
	defer cardtmpl.SetDefaultRegistry(prev)

	// taskNo 空 → schema minLength=1 违规(completed 分支)
	badCompleted := &SummaryCardFields{TaskNo: "", Title: "T", Kind: SummaryCardKindCompleted}
	err := preflightSummarySchema(badCompleted, summarycompleted.TemplateID)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid (completed), got nil")
	}
	if !errorsIsFieldsInvalid(err) {
		t.Errorf("want ErrFieldsInvalid, got %v", err)
	}

	// 同样 taskNo 空 → schema minLength=1 违规(failed 分支)
	badFailed := &SummaryCardFields{TaskNo: "", Title: "T", Kind: SummaryCardKindFailed, Reason: "r"}
	err = preflightSummarySchema(badFailed, summaryfailed.TemplateID)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid (failed), got nil")
	}
	if !errorsIsFieldsInvalid(err) {
		t.Errorf("want ErrFieldsInvalid, got %v", err)
	}
}

// TestPreflightSummarySchema_F7RegistryUnwired 断言 Registry 未注入时,preflight
// 返 typed errCardTmplUnavailable(composition bug → 500,不静默降级)。
func TestPreflightSummarySchema_F7RegistryUnwired(t *testing.T) {
	prev := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(nil)
	defer cardtmpl.SetDefaultRegistry(prev)

	card := &SummaryCardFields{TaskNo: "t", Title: "T", Kind: SummaryCardKindCompleted}
	err := preflightSummarySchema(card, summarycompleted.TemplateID)
	if err == nil {
		t.Fatal("expected errCardTmplUnavailable, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("registry/template unavailable")) {
		t.Errorf("want errCardTmplUnavailable, got %v", err)
	}
}
