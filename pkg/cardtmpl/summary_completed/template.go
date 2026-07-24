// Package summarycompleted 实现 L2a 平台通知卡 summary.completed@0.1.0。与
// pkg/cardtmpl (L0 基座) 的分层关系见 docs/platform-card-base.md §2 —— 本包只
// 承担 L2a 层职责(Template 接口 + FallbackText + 语言本地化),布局与 URL 白
// 名单一律走基座 helper。
//
// 注册流程 (在 composition root 里):
//
//	registry := cardtmpl.NewRegistry()
//	registry.Register(summarycompleted.New(), summarycompleted.Assets, summarycompleted.HandoffRoot)
package summarycompleted

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
	HandoffRoot = "handoff/summary.completed@0.1.0"
	// TemplateID 稳定 ID 常量,供 composition root 引用。
	TemplateID cardtmpl.ID = "summary.completed"
	// TemplateVersion 本模板当前版本。
	TemplateVersion = "0.1.0"

	// StateShown 是 v1 展示档视图 "default" 承载的唯一状态。
	StateShown cardtmpl.State = "shown"

	// Variant 是 metadata.octo.variant 值,与 modules/notify.card.go 现值
	// "summary.completed" 一致 —— 迁移前后字节等价的核心不变量。
	Variant = "summary.completed"
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
// handoff/summary.completed@0.1.0/contract/data.schema.json 对齐。
type fields struct {
	TaskNo      string `json:"taskNo"`
	Title       string `json:"title"`
	TimeRange   string `json:"timeRange"`
	Members     int    `json:"members"`
	MsgCount    int    `json:"msgCount"`
	GeneratedAt string `json:"generatedAt"`
}

// Build 渲染纯展示卡业务片段。字节等价于 legacy modules/notify.buildSummaryCard
// 对 completed 分支的输出 —— body/facts/actions 完全复用基座
// assembleResourceCardBody(通过 BuildSummaryResourceCardBodyWithLang 入口),差集
// 仅在 metadata.octo.{protocol,template} + envelope 层的 render_profile。
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
		return cardtmpl.BuildResult{}, fmt.Errorf("summary.completed: state %q not declared", state)
	}
	var f fields
	if err := json.Unmarshal(fieldsRaw, &f); err != nil {
		return cardtmpl.BuildResult{}, fmt.Errorf("summary.completed: unmarshal fields: %w", err)
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
	source := sourceFor(env.Lang)
	body, deepLink, err := cardtmpl.BuildSummaryResourceCardBodyWithLang(
		env.Lang, env.WebLoginURL, f.TaskNo, env.SpaceID, cardtmpl.ResourceCard{
			Title:       f.Title,
			Attribution: labels.completedBanner,
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

// FallbackText 与 legacy modules/notify.buildSummaryFallbackText 对 completed
// 分支的输出对齐:headline(标题) -> timeRange -> members -> msgCount -> generatedAt,
// 每行 sanitizeLine 消除内部换行注入;缺项跳过。members / msgCount 走整数,直接
// 拼 fmt 模板,不需要 sanitize。
func (t *Template) FallbackText(state cardtmpl.State, fieldsRaw json.RawMessage, lang string) (string, error) {
	if state != StateShown {
		return "", fmt.Errorf("summary.completed: fallback for state %q not declared", state)
	}
	var f fields
	if err := json.Unmarshal(fieldsRaw, &f); err != nil {
		return "", fmt.Errorf("summary.completed: unmarshal fields: %w", err)
	}
	labels := labelsFor(lang)
	var b strings.Builder
	fmt.Fprintf(&b, labels.completedHeadline, cardtmpl.SanitizeLine(f.Title))
	if tr := cardtmpl.SanitizeLine(f.TimeRange); tr != "" {
		fmt.Fprintf(&b, "\n%s%s%s", labels.timeRange, labels.kvSep, tr)
	}
	if f.Members > 0 {
		fmt.Fprintf(&b, "\n%s%s"+labels.membersValue, labels.members, labels.kvSep, f.Members)
	}
	if f.MsgCount > 0 {
		fmt.Fprintf(&b, "\n%s%s"+labels.msgCountValue, labels.msgCount, labels.kvSep, f.MsgCount)
	}
	if gen := cardtmpl.SanitizeLine(f.GeneratedAt); gen != "" {
		fmt.Fprintf(&b, "\n%s%s%s", labels.generatedAt, labels.kvSep, gen)
	}
	return b.String(), nil
}

// labelSet 是本卡按语言本地化的文案集。与 modules/notify.summaryLabelsFor 现值
// 保持逐字节一致 —— 一处改词表两处都要跟(将来 G4 折叠到 i18n locale)。
type labelSet struct {
	completedBanner   string // "总结已生成完成" / "Summary ready"
	completedHeadline string // headline 模板 "你的总结「%s」已生成完成。" / "Your summary \"%s\" is ready."
	timeRange         string // "时间范围" / "Time range"
	members           string // "参与成员" / "Participants"
	membersValue      string // "%d 人" / "%d"
	msgCount          string // "消息数量" / "Messages"
	msgCountValue     string // "%d 条" / "%d"
	generatedAt       string // "生成时间" / "Generated at"
	kvSep             string // "：" / ": "
}

func labelsFor(lang string) labelSet {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return labelSet{
			completedBanner:   "总结已生成完成",
			completedHeadline: "你的总结「%s」已生成完成。",
			timeRange:         "时间范围",
			members:           "参与成员",
			membersValue:      "%d 人",
			msgCount:          "消息数量",
			msgCountValue:     "%d 条",
			generatedAt:       "生成时间",
			kvSep:             "：",
		}
	}
	return labelSet{
		completedBanner:   "Summary ready",
		completedHeadline: "Your summary \"%s\" is ready.",
		timeRange:         "Time range",
		members:           "Participants",
		membersValue:      "%d",
		msgCount:          "Messages",
		msgCountValue:     "%d",
		generatedAt:       "Generated at",
		kvSep:             ": ",
	}
}

// sourceFor 返回 metadata.octo.source 的本地化默认值 —— 与 legacy
// summaryLabelsFor.sourceLabel 保持逐字节一致(F5:英文卡片不能带中文来源)。
func sourceFor(lang string) cardtmpl.Source {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return cardtmpl.Source{Label: "智能总结"}
	}
	return cardtmpl.Source{Label: "Smart Summary"}
}
