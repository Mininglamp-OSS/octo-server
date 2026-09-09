// Package docscommented 实现 L2a 平台通知卡 docs.commented@0.1.0。与
// pkg/cardtmpl (L0 基座) 的分层关系见 docs/platform-card-base.md §2 —— 本包只
// 承担 L2a 层职责(Template 接口 + FallbackText + 语言本地化),布局与 URL 白
// 名单一律走基座 helper。
//
// 注册流程 (在 composition root 里):
//
//	registry := cardtmpl.NewRegistry()
//	registry.Register(docscommented.New(), docscommented.Assets, docscommented.HandoffRoot)
package docscommented

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
	HandoffRoot = "handoff/docs.commented@0.1.0"
	// TemplateID 稳定 ID 常量,供 composition root 引用。
	TemplateID cardtmpl.ID = "docs.commented"
	// TemplateVersion 本模板当前版本。
	TemplateVersion = "0.1.0"

	// StateShown 是 v1 展示档视图 "default" 承载的唯一状态。
	StateShown cardtmpl.State = "shown"

	// Variant 是 metadata.octo.variant 值,与 modules/notify.card.go
	// docsAttributionAndVariant 现值一致 —— 迁移前后字节等价的核心不变量。
	Variant = "docs.commented"
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
// handoff/docs.commented@0.1.0/contract/data.schema.json 对齐。
type fields struct {
	DocID     string `json:"docId"`
	Title     string `json:"title"`
	ActorName string `json:"actorName"`
	Excerpt   string `json:"excerpt"`
	UpdatedAt string `json:"updatedAt"`
}

// Build 渲染纯展示卡业务片段。字节等价于 legacy modules/notify.buildDocsCard
// 对 docs.commented 分支的输出 —— body/facts/actions 完全复用基座
// assembleResourceCardBody(通过 BuildDocsResourceCardBodyWithLang 入口),差集
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
		return cardtmpl.BuildResult{}, fmt.Errorf("docs.commented: state %q not declared", state)
	}
	var f fields
	if err := json.Unmarshal(fieldsRaw, &f); err != nil {
		return cardtmpl.BuildResult{}, fmt.Errorf("docs.commented: unmarshal fields: %w", err)
	}
	labels := labelsFor(env.Lang)
	facts := make([]cardtmpl.Fact, 0, 2)
	if actor := strings.TrimSpace(f.ActorName); actor != "" {
		facts = append(facts, cardtmpl.Fact{Title: labels.actor, Value: actor})
	}
	if ts := strings.TrimSpace(f.UpdatedAt); ts != "" {
		facts = append(facts, cardtmpl.Fact{Title: labels.updatedAt, Value: ts})
	}
	attribution := attributionFor(strings.TrimSpace(f.ActorName), labels)
	source := sourceFor(env.Lang)
	body, deepLink, err := cardtmpl.BuildDocsResourceCardBodyWithLang(
		env.Lang, env.WebLoginURL, f.DocID, cardtmpl.ResourceCard{
			Title:       f.Title,
			Attribution: attribution,
			Excerpt:     strings.TrimSpace(f.Excerpt),
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

// FallbackText 与 legacy modules/notify.buildDocsFallbackText 对 commented
// 分支的输出对齐:attribution -> title -> excerpt -> updatedAt(每行 sanitizeLine
// 消除内部换行注入);缺项跳过。actorName 空时用匿名兜底 attribution。
func (t *Template) FallbackText(state cardtmpl.State, fieldsRaw json.RawMessage, lang string) (string, error) {
	if state != StateShown {
		return "", fmt.Errorf("docs.commented: fallback for state %q not declared", state)
	}
	var f fields
	if err := json.Unmarshal(fieldsRaw, &f); err != nil {
		return "", fmt.Errorf("docs.commented: unmarshal fields: %w", err)
	}
	labels := labelsFor(lang)
	attribution := attributionFor(cardtmpl.SanitizeLine(f.ActorName), labels)
	var b strings.Builder
	b.WriteString(attribution)
	if title := cardtmpl.SanitizeLine(f.Title); title != "" {
		fmt.Fprintf(&b, "\n%s%s%s", labels.title, labels.kvSep, title)
	}
	if excerpt := cardtmpl.SanitizeLine(f.Excerpt); excerpt != "" {
		fmt.Fprintf(&b, "\n%s", excerpt)
	}
	if ts := cardtmpl.SanitizeLine(f.UpdatedAt); ts != "" {
		fmt.Fprintf(&b, "\n%s%s%s", labels.updatedAt, labels.kvSep, ts)
	}
	return b.String(), nil
}

// labelSet 是本卡按语言本地化的文案集。与 modules/notify.docsLabelsFor 现值
// 保持逐字节一致 —— 一处改词表两处都要跟(将来 G4 折叠到 i18n locale)。
type labelSet struct {
	banner     string // "%s 评论了文档"(actor 非空时)
	bannerAnon string // "有新评论"(actor 空)
	title      string // FactSet/Fallback 的键名"文档"
	actor      string // FactSet 的键名"操作人"
	updatedAt  string // FactSet/Fallback 的键名"时间"
	kvSep      string // "："/": "
}

func labelsFor(lang string) labelSet {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return labelSet{
			banner: "%s 评论了文档", bannerAnon: "有新评论",
			title: "文档", actor: "操作人", updatedAt: "时间", kvSep: "：",
		}
	}
	return labelSet{
		banner: "%s commented on a document", bannerAnon: "A new comment on a document",
		title: "Document", actor: "By", updatedAt: "At", kvSep: ": ",
	}
}

func attributionFor(actor string, labels labelSet) string {
	if actor == "" {
		return labels.bannerAnon
	}
	return fmt.Sprintf(labels.banner, actor)
}

// sourceFor 返回 metadata.octo.source 的本地化默认值 —— 与 pilot / legacy
// docsLabelsFor.sourceLabel 保持逐字节一致(F5:英文卡片不能带中文来源)。
func sourceFor(lang string) cardtmpl.Source {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return cardtmpl.Source{Label: "文档"}
	}
	return cardtmpl.Source{Label: "Docs"}
}
