// Package docsshared 实现 L2a 平台通知卡 docs.shared@0.1.0(v1 纯展示)。
// 结构与 docs_commented 完全对称,只在 variant + attribution 词表上区分 —— 复用
// 基座 assembleResourceCardBody 保证字节等价 legacy modules/notify.buildDocsCard。
package docsshared

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

//go:embed all:handoff
var Assets embed.FS

const (
	HandoffRoot                    = "handoff/docs.shared@0.1.0"
	TemplateID     cardtmpl.ID     = "docs.shared"
	TemplateVersion                = "0.1.0"
	StateShown     cardtmpl.State  = "shown"
	Variant                        = "docs.shared"
)

type Template struct{ meta cardtmpl.TemplateMeta }

func New() *Template                           { return &Template{} }
func (t *Template) SetMeta(m cardtmpl.TemplateMeta) { t.meta = m }
func (t *Template) Meta() cardtmpl.TemplateMeta     { return t.meta.Clone() }

type fields struct {
	DocID     string `json:"docId"`
	Title     string `json:"title"`
	ActorName string `json:"actorName"`
	Excerpt   string `json:"excerpt"`
	UpdatedAt string `json:"updatedAt"`
}

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
		return cardtmpl.BuildResult{}, fmt.Errorf("docs.shared: state %q not declared", state)
	}
	var f fields
	if err := json.Unmarshal(fieldsRaw, &f); err != nil {
		return cardtmpl.BuildResult{}, fmt.Errorf("docs.shared: unmarshal fields: %w", err)
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
		env.Lang, env.WebLoginURL, f.DocID, env.SpaceID, cardtmpl.ResourceCard{
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

func (t *Template) FallbackText(state cardtmpl.State, fieldsRaw json.RawMessage, lang string) (string, error) {
	if state != StateShown {
		return "", fmt.Errorf("docs.shared: fallback for state %q not declared", state)
	}
	var f fields
	if err := json.Unmarshal(fieldsRaw, &f); err != nil {
		return "", fmt.Errorf("docs.shared: unmarshal fields: %w", err)
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

// labelSet 与 modules/notify.docsLabelsFor 的 shared 分支现值逐字节一致(F5)。
type labelSet struct {
	banner     string
	bannerAnon string
	title      string
	actor      string
	updatedAt  string
	kvSep      string
}

func labelsFor(lang string) labelSet {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return labelSet{
			banner: "%s 分享了文档", bannerAnon: "有人分享了文档",
			title: "文档", actor: "操作人", updatedAt: "时间", kvSep: "：",
		}
	}
	return labelSet{
		banner: "%s shared a document", bannerAnon: "A document was shared with you",
		title: "Document", actor: "By", updatedAt: "At", kvSep: ": ",
	}
}

func attributionFor(actor string, labels labelSet) string {
	if actor == "" {
		return labels.bannerAnon
	}
	return fmt.Sprintf(labels.banner, actor)
}

func sourceFor(lang string) cardtmpl.Source {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return cardtmpl.Source{Label: "文档"}
	}
	return cardtmpl.Source{Label: "Docs"}
}
