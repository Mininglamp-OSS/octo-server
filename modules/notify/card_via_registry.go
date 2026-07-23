package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
	"go.uber.org/zap"
)

// errCardTmplUnavailable 是 preflight / build 通路上 "Registry 未注入 / Template
// 未注册" 的 typed 内部错误。caller (deliverDocsCardNotification) 对它走
// **500 不降级**——这是 composition bug,不应把错请求伪装成成功文本 (§10 / A14)。
// 与 cardtmpl.ErrFieldsInvalid 对齐 typed:前者是 500,后者是 400。
var errCardTmplUnavailable = errors.New("notify: cardtmpl registry/template unavailable (composition bug)")

// templateFallbackText 是 F6 分工:access_requested + gate + Registry-ready 时,
// buildDocsFallbackText 会先调本函数,让 fallback 文本走 pilot Template 的 L0
// 定义。Registry 未注入 / Template 未注册 / mapping/unmarshal 失败 → 返 ok=false,
// caller 兜回 legacy 多行组装。
func templateFallbackText(card *DocsCardFields, lang string) (string, bool) {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return "", false
	}
	tmpl, err := registry.Lookup(docsaccessrequest.TemplateID, "")
	if err != nil {
		return "", false
	}
	fields, err := mapDocsCardFieldsToJSON(card, lang)
	if err != nil {
		return "", false
	}
	text, err := tmpl.FallbackText(docsaccessrequest.StatePending, fields, lang)
	if err != nil || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// preflightDocsAccessRequestSchema 在 access_requested + gate 开的通路上,在
// memberCache / docsSender / cardmsg.Enabled 任意 gate 之前独立跑一次 pilot
// Template 的 InputSchema 校验。返回值分类:
//   - schema 不合法 → cardtmpl.ErrFieldsInvalid (400 零投递,C1)
//   - Registry / Template 未注入 → errCardTmplUnavailable (500 不降级)
//   - nil → 通过
//
// R2-2 修正:前版当 registry nil 时返 nil 让 caller 继续走 v2 分支,后者最终降级
// 文本 —— 那样"错请求"仍然会成功送达一条文本,与 §10 / A14 冲突。现在 preflight
// 阶段就报 500,让 wiring bug 立刻可见。
func preflightDocsAccessRequestSchema(card *DocsCardFields) error {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return fmt.Errorf("%w: default registry not wired", errCardTmplUnavailable)
	}
	tmpl, err := registry.Lookup(docsaccessrequest.TemplateID, "")
	if err != nil {
		return fmt.Errorf("%w: lookup %s: %v", errCardTmplUnavailable, docsaccessrequest.TemplateID, err)
	}
	fields, err := mapDocsCardFieldsToJSON(card, "zh-CN") // preflight 只关心 schema shape,lang 无差异
	if err != nil {
		return fmt.Errorf("%w: map: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	var parsed any
	if err := json.Unmarshal(fields, &parsed); err != nil {
		return fmt.Errorf("%w: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	if err := tmpl.Meta().InputSchema.Validate(parsed); err != nil {
		// R2-8: preflight schema 失败也打 fields_invalid metric (Render 未被触发,
		// 否则 metric 永远漏这条最"合法"的 400 路径)。
		cardtmpl.RecordFieldsInvalid(docsaccessrequest.TemplateID, tmpl.Meta().Version)
		return fmt.Errorf("%w: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	// R3-1: schema 的 avatarUrl pattern 只做粗粒度前缀防护,正则无法可靠判定 host
	// 存在 —— "https://?x" / "https://#y" 的 host 为空却能过 pattern,随后在 Build
	// 内 requireHTTPS 才失败、被误分类成 render_error 降级文本 (绕过 C1 400)。
	// 这里用与 Build 同一个 URL parser (cardtmpl.AbsoluteHTTPSURL) 在 ingress 前置
	// 断言绝对 https,把坏字段确定性收敛成 C1 400,零缝隙、零降级。空头像合法 (省略头像列)。
	//
	// 维护约束:本校验按字段硬编码 (当前 pilot 唯一的用户可控 URL 字段是 avatar;
	// Source.IconURL 由服务端置空)。**L1 schema 若新增任何 URL 字段,必须在此同步
	// 补 AbsoluteHTTPSURL 校验**,否则该字段又会退回"schema 过 → Build 失败 →
	// render_error 降级"的同类缝隙。未来可由 Template 声明"须绝对 https 的字段集"
	// 让基座统一前置校验,消除这条硬编码耦合。
	if avatar := strings.TrimSpace(card.ActorAvatarURL); avatar != "" {
		if err := cardtmpl.AbsoluteHTTPSURL(avatar); err != nil {
			cardtmpl.RecordFieldsInvalid(docsaccessrequest.TemplateID, tmpl.Meta().Version)
			return fmt.Errorf("%w: actor_avatar_url: %v", cardtmpl.ErrFieldsInvalid, err)
		}
	}
	return nil
}

// buildDocsAccessRequestCardViaRegistry 走 L0 Registry.Render 生成
// docs.access-request 卡片。相较 buildDocsAccessRequestCard 差异:
//   - metadata.octo 新增 {protocol, template:{id,version}} 由基座强制注入;
//   - schema 校验失败返回 typed cardtmpl.ErrFieldsInvalid (由 caller 翻 400,C1);
//   - Registry/Template 未注入返 errCardTmplUnavailable (R2-2:caller 翻 500 不降级);
//   - 其他 render 级错 non-typed,caller 走 render_error 降级为纯文本 (F6/§10)。
//
// R2-2 更正:DefaultRegistry nil 走 typed errCardTmplUnavailable —— 与 preflight
// 同分类,让 wiring bug 在 caller 那里映射到 500 不降级。
func (n *Notify) buildDocsAccessRequestCardViaRegistry(
	ctx context.Context, spaceID string, card *DocsCardFields, lang string,
) (json.RawMessage, error) {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		n.Error("cardtmpl DefaultRegistry unwired — composition bug",
			zap.String("space_id", spaceID), zap.String("doc_id", card.DocID))
		return nil, fmt.Errorf("%w: default registry not wired", errCardTmplUnavailable)
	}

	fields, err := mapDocsCardFieldsToJSON(card, lang)
	if err != nil {
		return nil, fmt.Errorf("notify: map DocsCardFields to schema JSON: %w", err)
	}

	env := cardtmpl.BuildEnv{
		WebLoginURL: n.ctx.GetConfig().External.WebLoginURL,
		Lang:        lang,
		SpaceID:     spaceID,
	}
	cardDoc, _, err := registry.RenderCard(ctx,
		docsaccessrequest.TemplateID, "",
		docsaccessrequest.StatePending, fields, env)
	if err != nil {
		return nil, err
	}
	return cardDoc, nil
}

// mapDocsCardFieldsToJSON 把扁平 DocsCardFields 映射成 pilot data.schema.json 期望的
// 嵌套 JSON 形状。服务端字典字段(permission/document.sourceName/requestedAtDisplay/
// messageTimeDisplay)按当前收件人语言从本地化词表补齐;不接受调用方传入。
//
// state 恒 "pending" (本 PR pilot 只注册 pending view;approved/rejected 由
// standard_action_finalizer 生成,不走 ingress)。
func mapDocsCardFieldsToJSON(card *DocsCardFields, lang string) (json.RawMessage, error) {
	if card == nil {
		return nil, errors.New("mapDocsCardFieldsToJSON: nil card")
	}
	labels := docsLabelsFor(lang)

	m := map[string]any{
		"requestId": strings.TrimSpace(card.RequestID),
		"state":     "pending",
		"document": map[string]any{
			"docId":      strings.TrimSpace(card.DocID),
			"title":      strings.TrimSpace(card.Title),
			"sourceName": labels.sourceLabel, // "文档" / "Docs" — 与 legacy source label 同源
		},
		"requester": map[string]any{
			"name":      strings.TrimSpace(card.ActorName),
			"avatarUrl": strings.TrimSpace(card.ActorAvatarURL),
		},
		// permission 服务端字典缺省留空; pilot Template pendingLabels 默认词表
		// ("查看者" / "viewer") 与 legacy requestBannerSuffix 现值一致,保证
		// 迁移前后 banner 字节等价。
		"requestReason":      strings.TrimSpace(card.Excerpt),
		"requestedAtDisplay": strings.TrimSpace(card.UpdatedAt),
		"messageTimeDisplay": strings.TrimSpace(card.UpdatedAt),
	}
	// document.url 由服务端拼接 (docsDeepLink 在 cardtmpl 内做),这里留空,
	// schema 允许空;pilot Template Build 内部不读它,读的是 requestId + WebLoginURL。
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal mapped fields: %w", err)
	}
	return json.RawMessage(raw), nil
}
