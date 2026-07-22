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

// F7 已删 errCardTmplRegistryUnwired sentinel:composition root 未注入 Registry
// 现在直接作为 render_error 分类 (返回 non-typed error),而不是让 caller 静默回退
// 到 legacy 通路——那样会遮蔽 wiring 漏洞。所有 test 必须显式 SetDefaultRegistry。

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
// 在 memberCache / docsSender / cardmsg.Enabled 任意 gate 之前独立跑一次
// pilot Template 的 InputSchema 校验。命中 → 返回 typed 错(caller 翻 400 零投递,
// C1 policy)。Registry 未注入 (composition bug) → 返回 nil 让下游按现网走,
// 稍后 render 阶段仍会失败并 fallback 到 legacy;这里选 nil 而非 fail,是为了
// 在 test 环境和 gate-off 部署中不引入新硬依赖。
func preflightDocsAccessRequestSchema(card *DocsCardFields) error {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return nil // 无 registry 就没有 schema,fallback 到 legacy 走原路径
	}
	tmpl, err := registry.Lookup(docsaccessrequest.TemplateID, "")
	if err != nil {
		return nil // 同上
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
		return fmt.Errorf("%w: %v", cardtmpl.ErrFieldsInvalid, err)
	}
	return nil
}

// buildDocsAccessRequestCardViaRegistry 走 L0 Registry.Render 生成 docs.access-request
// 卡片。相较 buildDocsAccessRequestCard 差异:
//   - metadata.octo 新增 {protocol, template:{id,version}} 由基座强制注入;
//   - schema 校验失败返回 typed cardtmpl.ErrFieldsInvalid (由 caller 翻 400,C1);
//   - render 级错沿用 buildErr 语义,caller 降级为纯文本。
// buildDocsAccessRequestCardViaRegistry 走 L0 Registry.Render 生成
// docs.access-request 卡片。相较 buildDocsAccessRequestCard 差异:
//   - metadata.octo 新增 {protocol, template:{id,version}} 由基座强制注入;
//   - schema 校验失败返回 typed cardtmpl.ErrFieldsInvalid (由 caller 翻 400,C1);
//   - render 级错沿用 buildErr 语义,caller 降级为纯文本。
//
// F7: DefaultRegistry nil 不再返回 sentinel 让 caller 回退到 legacy —— composition
// bug 必须显式暴露。返回 non-typed error,caller 走 render_error 降级为文本 DM,
// 同时打 ERROR 日志促使 SRE 修 wiring。原 errCardTmplRegistryUnwired 走的 "test
// 环境未 wire → fallback legacy" 通路已删,test 必须自行 SetDefaultRegistry。
func (n *Notify) buildDocsAccessRequestCardViaRegistry(
	ctx context.Context, spaceID string, card *DocsCardFields, lang string,
) (json.RawMessage, error) {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		n.Error("cardtmpl DefaultRegistry unwired — composition bug",
			zap.String("space_id", spaceID), zap.String("doc_id", card.DocID))
		return nil, errors.New("notify: cardtmpl default registry unwired (composition bug)")
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
