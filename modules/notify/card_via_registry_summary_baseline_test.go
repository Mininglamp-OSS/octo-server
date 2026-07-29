package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	summarycompleted "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_completed"
	summaryfailed "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_failed"
)

// TestMapSummaryFields_TruncatesDisplayFields 断言 P2-1(PR #650 review):超长
// 展示字段(title/reason/timeRange/generatedAt)被服务端截断到 schema cap 后仍能
// 投递(preflight 通过 + build 成功,不再翻 400);负数 members/msgCount 被 clamp 到
// 0 后省略。而 taskNo(deep-link key)超长仍是 C1 400 —— 绝不静默截断。
// 同时锁 notify 本地 cap 常量与 schema maxLength 对齐:若某个 cap 设大于 schema,
// 截断后仍超 schema → preflight 会 400,本测试即失败。
func TestMapSummaryFields_TruncatesDisplayFields(t *testing.T) {
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

	// 全字段远超 schema cap + 负数计数 → 截断/clamp 后 preflight 通过 + build 成功。
	huge := &SummaryCardFields{
		TaskNo:      "sum_ok",
		Kind:        SummaryCardKindFailed,
		Title:       strings.Repeat("标", 500),  // cap 200
		Reason:      strings.Repeat("因", 1000), // cap 300
		TimeRange:   strings.Repeat("时", 500),  // cap 200
		GeneratedAt: strings.Repeat("刻", 300),  // cap 80
		Members:     -5,
		MsgCount:    -1,
	}
	if err := preflightSummarySchema(context.Background(), huge, summaryfailed.TemplateID); err != nil {
		t.Fatalf("preflight should PASS after truncation, got %v", err)
	}
	if _, _, err := n.buildSummaryCardViaRegistry(context.Background(),
		"space-c", huge, "zh-CN", summaryfailed.TemplateID, summaryfailed.StateShown); err != nil {
		t.Fatalf("build should SUCCEED after truncation, got %v", err)
	}

	// taskNo 超长(>200)→ 仍 C1 400:deep-link key 不截断。
	badTask := &SummaryCardFields{
		TaskNo: strings.Repeat("x", 300), Title: "OK", Kind: SummaryCardKindCompleted,
	}
	err := preflightSummarySchema(context.Background(), badTask, summarycompleted.TemplateID)
	if err == nil || !errorsIsFieldsInvalid(err) {
		t.Fatalf("over-length taskNo must stay C1 400, got %v", err)
	}
}

// TestMapSummaryFields_ClampsOverMaxCounts 断言 Jerry-Xin PR #650 re-review 的阻断
// 项:超过 schema `maximum` 的 members/msgCount 被 clamp 到 max 后仍能投递(preflight
// 通过 + build 成功,不再翻 400 零投递)。clamp 前只 floor 负数、不封上限,一个巨大的
// 正整数计数会被 schema `maximum` 直接 400 掉 —— legacy buildSummaryCard 却会照常投递
// 任意正整数。本 fix 与超长展示字段的截断处置对称,共同保住"通知永不丢"不变量。
// 同时锁 notify 本地计数 cap 与 schema `maximum` 对齐:若某个本地 cap > schema,
// clamp 后仍超 schema → preflight 会 400,本测试即失败。
func TestMapSummaryFields_ClampsOverMaxCounts(t *testing.T) {
	// 单元层:mapSummaryCardFieldsToJSON 把超上限计数 clamp 到 schema max。
	over := &SummaryCardFields{
		TaskNo: "sum_over", Title: "OK", Kind: SummaryCardKindCompleted,
		Members: summaryMembersMax + 1234, MsgCount: summaryMsgCountMax + 1,
	}
	raw, err := mapSummaryCardFieldsToJSON(over)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := int(m["members"].(float64)); got != summaryMembersMax {
		t.Errorf("members = %d, want clamp to %d", got, summaryMembersMax)
	}
	if got := int(m["msgCount"].(float64)); got != summaryMsgCountMax {
		t.Errorf("msgCount = %d, want clamp to %d", got, summaryMsgCountMax)
	}

	// 集成层:超上限计数经 clamp 后 preflight 通过 + build 成功(completed/failed 两个
	// 模板都测),证明 clamp 值确实落在 schema [0, maximum] 内(cap↔schema 对齐锁)。
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
		name       string
		kind       string
		templateID cardtmpl.ID
		state      cardtmpl.State
	}{
		{"completed", SummaryCardKindCompleted, summarycompleted.TemplateID, summarycompleted.StateShown},
		{"failed", SummaryCardKindFailed, summaryfailed.TemplateID, summaryfailed.StateShown},
	}
	for _, s := range specs {
		t.Run(s.name, func(t *testing.T) {
			card := &SummaryCardFields{
				TaskNo: "sum_over", Title: "OK", Kind: s.kind,
				Members: summaryMembersMax + 9, MsgCount: summaryMsgCountMax + 9,
			}
			if s.kind == SummaryCardKindFailed {
				card.Reason = "boom"
			}
			if err := preflightSummarySchema(context.Background(), card, s.templateID); err != nil {
				t.Fatalf("preflight should PASS after count clamp, got %v", err)
			}
			if _, _, err := n.buildSummaryCardViaRegistry(context.Background(),
				"space-c", card, "zh-CN", s.templateID, s.state); err != nil {
				t.Fatalf("build should SUCCEED after count clamp, got %v", err)
			}
		})
	}
}

// TestMapSummaryFields_PreservesTaskNoVerbatim 断言 taskNo 原样透传(不 TrimSpace),
// 与 legacy buildSummaryCard 用原值拼 deep-link 保持字节一致(Jerry-Xin PR #650
// re-review 的 🟡 note:trim 会静默改写身份字段并让 padded 输入相对 legacy 漂移)。
// 空/纯空白 taskNo 在 deliverCardNotification 入口即被挡回,不会到这里。
func TestMapSummaryFields_PreservesTaskNoVerbatim(t *testing.T) {
	const padded = " sum_2026-07-16 " // 前后空白 —— 不应被静默 trim
	card := &SummaryCardFields{TaskNo: padded, Title: "OK", Kind: SummaryCardKindCompleted}
	raw, err := mapSummaryCardFieldsToJSON(card)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := m["taskNo"].(string); got != padded {
		t.Errorf("taskNo = %q, want verbatim %q (no trim)", got, padded)
	}
}

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
	err := preflightSummarySchema(context.Background(), badCompleted, summarycompleted.TemplateID)
	if err == nil {
		t.Fatal("expected ErrFieldsInvalid (completed), got nil")
	}
	if !errorsIsFieldsInvalid(err) {
		t.Errorf("want ErrFieldsInvalid, got %v", err)
	}

	// 同样 taskNo 空 → schema minLength=1 违规(failed 分支)
	badFailed := &SummaryCardFields{TaskNo: "", Title: "T", Kind: SummaryCardKindFailed, Reason: "r"}
	err = preflightSummarySchema(context.Background(), badFailed, summaryfailed.TemplateID)
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
	err := preflightSummarySchema(context.Background(), card, summarycompleted.TemplateID)
	if err == nil {
		t.Fatal("expected errCardTmplUnavailable, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("registry/template unavailable")) {
		t.Errorf("want errCardTmplUnavailable, got %v", err)
	}
}
