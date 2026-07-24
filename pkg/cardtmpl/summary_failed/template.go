// Package summaryfailed 实现 L2a 平台通知卡 summary.failed@0.1.0。与
// pkg/cardtmpl (L0 基座) 的分层关系见 docs/platform-card-base.md §2 —— 本包只
// 承担 L2a 层职责(Template 接口 + FallbackText + 语言本地化),布局与 URL 白
// 名单一律走基座 helper。
//
// 与 summary.completed 的差异只在 attribution / variant / excerpt 三处;facts /
// deep-link / source 共用 legacy summaryLabelsFor 的同一词表,保证 Kind 之间不
// 因 label 漂移出偏差。
//
// 注册流程 (在 composition root 里):
//
//	registry := cardtmpl.NewRegistry()
//	registry.Register(summaryfailed.New(), summaryfailed.Assets, summaryfailed.HandoffRoot)
package summaryfailed

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

// Assets 内嵌 handoff 契约资源目录,由 Registry.Register 载入。
//
//go:embed all:handoff
var Assets embed.FS

const (
	// HandoffRoot 是 Assets 里契约资源的根路径,交给 Registry.Register 使用。
	HandoffRoot = "handoff/summary.failed@0.1.0"
	// TemplateID 稳定 ID 常量,供 composition root 引用。
	TemplateID cardtmpl.ID = "summary.failed"
	// TemplateVersion 本模板当前版本。
	TemplateVersion = "0.1.0"

	// StateShown 是 v1 展示档视图 "default" 承载的唯一状态。
	StateShown cardtmpl.State = "shown"

	// Variant 是 metadata.octo.variant 值,与 modules/notify.card.go 现值
	// "summary.failed" 一致 —— 迁移前后字节等价的核心不变量。
	Variant = "summary.failed"
)

// Template 是纯展示卡 L2a 实现。持有由 Registry 注入的 TemplateMeta。
type Template struct {
	meta cardtmpl.TemplateMeta
}

// New 构造一个未装配 Meta 的 Template 骨架。Registry.Register 调用 SetMeta 注入。
func New() *Template { return &Template{} }

// SetMeta 满足 cardtmpl 包内 metaSetter 契约。Registry.Register 期一次性调用。
func (t *Template) SetMeta(m cardtmpl.TemplateMeta) { t.meta = m }

// Meta 返回注册期固化静态元数据的防御性深拷贝 —— 与 pilot 保持同口径(R3-2)。
func (t *Template) Meta() cardtmpl.TemplateMeta { return t.meta.Clone() }

// fields 是 shown sample 反序列化后的数据契约。字段与
// handoff/summary.failed@0.1.0/contract/data.schema.json 对齐。
// reason 仅 failed 卡具备,completed 卡的 schema 里没有此字段。
type fields struct {
	TaskNo      string `json:"taskNo"`
	Title       string `json:"title"`
	Reason      string `json:"reason"`
	TimeRange   string `json:"timeRange"`
	Members     int    `json:"members"`
	MsgCount    int    `json:"msgCount"`
	GeneratedAt string `json:"generatedAt"`
}

// Build 渲染纯展示卡业务片段。字节等价于 legacy modules/notify.buildSummaryCard
// 对 failed 分支的输出:attribution 换成"总结生成失败",excerpt = "失败原因：" + reason
// (reason 非空时),facts 仍按 timeRange/members/msgCount/generatedAt 填充(与
// completed 同 label 表)。
func (t *Template) Build(
	ctx context.Context,
	state cardtmpl.State,
	fieldsRaw json.RawMessage,
	env cardtmpl.BuildEnv,
) (cardtmpl.BuildResult, error) {
	if err := ctx.Err(); err != nil {
		return cardtmpl.BuildResult{}, err
	}
	if state != StateShown {
		return cardtmpl.BuildResult{}, fmt.Errorf("summary.failed: state %q not declared", state)
	}
	var f fields
	if err := json.Unmarshal(fieldsRaw, &f); err != nil {
		return cardtmpl.BuildResult{}, fmt.Errorf("summary.failed: unmarshal fields: %w", err)
	}
	labels := labelsFor(env.Lang)
	facts := make([]cardtmpl.Fact, 0, 4)
	if tr := strings.TrimSpace(f.TimeRange); tr != "" {
		facts = append(facts, cardtmpl.Fact{Title: labels.timeRange, Value: tr})
	}
	if f.Members > 0 {
		facts = append(facts, cardtmpl.Fact{Title: labels.members, Value: fmt.Sprintf(labels.membersValue, f.Members)})
	}
	if f.MsgCount > 0 {
		facts = append(facts, cardtmpl.Fact{Title: labels.msgCount, Value: fmt.Sprintf(labels.msgCountValue, f.MsgCount)})
	}
	if gen := strings.TrimSpace(f.GeneratedAt); gen != "" {
		facts = append(facts, cardtmpl.Fact{Title: labels.generatedAt, Value: gen})
	}
	excerpt := ""
	if reason := strings.TrimSpace(f.Reason); reason != "" {
		excerpt = labels.failedPrefix + reason
	}
	source := sourceFor(env.Lang)
	body, deepLink, err := cardtmpl.BuildSummaryResourceCardBodyWithLang(
		env.Lang, env.WebLoginURL, f.TaskNo, env.SpaceID, cardtmpl.ResourceCard{
			Title:       f.Title,
			Attribution: labels.failedBanner,
			Excerpt:     excerpt,
			Facts:       facts,
			Variant:     Variant,
			Source:      source,
		})
	if err != nil {
		return cardtmpl.BuildResult{}, err
	}
	return cardtmpl.BuildResult{
		Body:     body,
		Variant:  Variant,
		DeepLink: deepLink,
		Source:   &source,
	}, nil
}

// FallbackText 与 legacy modules/notify.buildSummaryFallbackText 对 failed
// 分支的输出对齐:failedHeadline(标题) -> "失败原因：..."(reason 非空时);
// completed 分支里的 timeRange/members/... 各 fact 行 legacy 不追加到 failed
// 文本,这里同样跳过。每行 sanitizeLine 消除内部换行注入。
func (t *Template) FallbackText(state cardtmpl.State, fieldsRaw json.RawMessage, lang string) (string, error) {
	if state != StateShown {
		return "", fmt.Errorf("summary.failed: fallback for state %q not declared", state)
	}
	var f fields
	if err := json.Unmarshal(fieldsRaw, &f); err != nil {
		return "", fmt.Errorf("summary.failed: unmarshal fields: %w", err)
	}
	labels := labelsFor(lang)
	var b strings.Builder
	fmt.Fprintf(&b, labels.failedHeadline, cardtmpl.SanitizeLine(f.Title))
	if reason := cardtmpl.SanitizeLine(f.Reason); reason != "" {
		fmt.Fprintf(&b, "\n%s%s", labels.failedPrefix, reason)
	}
	return b.String(), nil
}

// labelSet 是本卡按语言本地化的文案集。与 modules/notify.summaryLabelsFor 现值
// 保持逐字节一致(F5:英文卡片不能带中文来源;词表漂移会被字节等价基线抓到)。
type labelSet struct {
	failedBanner   string // "总结生成失败" / "Summary failed"
	failedHeadline string // "你的总结「%s」生成失败。" / "Your summary \"%s\" failed to generate."
	failedPrefix   string // "失败原因：" / "Reason: "
	timeRange      string
	members        string
	membersValue   string
	msgCount       string
	msgCountValue  string
	generatedAt    string
	kvSep          string
}

func labelsFor(lang string) labelSet {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return labelSet{
			failedBanner:   "总结生成失败",
			failedHeadline: "你的总结「%s」生成失败。",
			failedPrefix:   "失败原因：",
			timeRange:      "时间范围",
			members:        "参与成员",
			membersValue:   "%d 人",
			msgCount:       "消息数量",
			msgCountValue:  "%d 条",
			generatedAt:    "生成时间",
			kvSep:          "：",
		}
	}
	return labelSet{
		failedBanner:   "Summary failed",
		failedHeadline: "Your summary \"%s\" failed to generate.",
		failedPrefix:   "Reason: ",
		timeRange:      "Time range",
		members:        "Participants",
		membersValue:   "%d",
		msgCount:       "Messages",
		msgCountValue:  "%d",
		generatedAt:    "Generated at",
		kvSep:          ": ",
	}
}

// sourceFor 返回 metadata.octo.source 的本地化默认值 —— 与 legacy
// summaryLabelsFor.sourceLabel 保持逐字节一致(与 summary.completed 同源)。
func sourceFor(lang string) cardtmpl.Source {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return cardtmpl.Source{Label: "智能总结"}
	}
	return cardtmpl.Source{Label: "Smart Summary"}
}
