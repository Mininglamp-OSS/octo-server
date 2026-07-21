package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// errCardTmplRegistryUnwired 由 buildDocsAccessRequestCardViaRegistry 返回,表示
// composition root 未注入 cardtmpl.DefaultRegistry (例如集成测试没走 main.go 的
// installCardTmplRegistry)。caller 检查该 sentinel 后 fallback 到 legacy builder,
// 让 pilot 落地可 bisect (旧路径始终可回)。
var errCardTmplRegistryUnwired = errors.New("notify: cardtmpl default registry unwired")

// buildDocsAccessRequestCardViaRegistry 走 L0 Registry.Render 生成 docs.access-request
// 卡片。相较 buildDocsAccessRequestCard 差异:
//   - metadata.octo 新增 {protocol, template:{id,version}} 由基座强制注入;
//   - schema 校验失败返回 typed cardtmpl.ErrFieldsInvalid (由 caller 翻 400,C1);
//   - render 级错沿用 buildErr 语义,caller 降级为纯文本。
func (n *Notify) buildDocsAccessRequestCardViaRegistry(
	ctx context.Context, spaceID string, card *DocsCardFields, lang string,
) (json.RawMessage, error) {
	registry := cardtmpl.DefaultRegistry()
	if registry == nil {
		return nil, errCardTmplRegistryUnwired
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
