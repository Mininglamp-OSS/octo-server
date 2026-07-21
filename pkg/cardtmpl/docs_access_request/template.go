// Package docsaccessrequest 实现 L2a pilot 卡片 docs.access-request@0.2.0。
// 与 pkg/cardtmpl (L0 基座) 的分层关系见 docs/platform-card-base.md §2。
//
// 注册流程 (在 composition root 里):
//   registry := cardtmpl.NewRegistry()
//   registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
//   registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
//   registry.Freeze()
package docsaccessrequest

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

// 路径常量:嵌入根 + 版本化子目录。二者拼接 = Register 的 root 参数。
const (
	// HandoffRoot 是 Assets 里契约资源的根路径,交给 Registry.Register 使用。
	HandoffRoot = "handoff/docs.access-request@0.2.0"
	// TemplateID 稳定 ID 常量,供 composition root 引用。
	TemplateID cardtmpl.ID = "docs.access-request"
	// TemplateVersion 本模板当前版本。
	TemplateVersion = "0.2.0"

	// StatePending 是 v2 交互档视图 "pending" 承载的唯一状态。
	// 本 PR 只注册 pending view;approved/rejected(result view)延后到 outcome PR。
	StatePending cardtmpl.State = "pending"

	// Variant 是 metadata.octo.variant 值,与 modules/notify.card.go:463 现值一致。
	Variant = "docs.access_requested"
)

// Template 是 pilot 的 L2a 实现。持有由 Registry 注入的 TemplateMeta。
type Template struct {
	meta cardtmpl.TemplateMeta
	// approveTitle / denyTitle 是按语言本地化的按钮文案;在 Build 阶段按 env.Lang 选择。
	// 硬编码到常量表(与 modules/notify 现有 approvalActionLabelsFor 一致)。
}

// New 构造一个未装配 Meta 的 Template 骨架。Registry.Register 调用 SetMeta 注入。
func New() *Template {
	return &Template{}
}

// SetMeta 满足 cardtmpl 包内 metaSetter 契约。Registry.Register 期一次性调用,之后不再变。
func (t *Template) SetMeta(m cardtmpl.TemplateMeta) {
	t.meta = m
}

// Meta 返回注册期固化的静态元数据,零成本调用。
func (t *Template) Meta() cardtmpl.TemplateMeta {
	return t.meta
}

// pendingFields 是 pending sample 反序列化后的数据契约。字段结构与
// pkg/cardtmpl/testdata/handoff/docs.access-request@0.2.0/contract/data.schema.json 对齐。
type pendingFields struct {
	RequestID          string `json:"requestId"`
	State              string `json:"state"`
	RequestReason      string `json:"requestReason"`
	RequestedAtDisplay string `json:"requestedAtDisplay"`
	MessageTimeDisplay string `json:"messageTimeDisplay"`
	Document           struct {
		Title      string `json:"title"`
		URL        string `json:"url"`
		SourceName string `json:"sourceName"`
	} `json:"document"`
	Requester struct {
		Name      string `json:"name"`
		AvatarURL string `json:"avatarUrl"`
	} `json:"requester"`
	Permission struct {
		Label     string `json:"label"`
		RoleLabel string `json:"roleLabel"`
	} `json:"permission"`
}

// Build 渲染指定状态下的卡片业务片段。
// 本 PR pilot 只实现 StatePending;其它状态由 Registry.Render 前置的 ViewFor 挡下。
func (t *Template) Build(
	ctx context.Context,
	state cardtmpl.State,
	fields json.RawMessage,
	env cardtmpl.BuildEnv,
) (cardtmpl.BuildResult, error) {
	_ = ctx // Build 不再依赖 ctx;i18n 走 env.Lang
	if state != StatePending {
		// Registry.Render 已经在前置 ViewFor 挡下未注册 state;
		// 这里作为深度防御,避免未来 result view 上线时 Template 悄悄 fallthrough。
		return cardtmpl.BuildResult{}, fmt.Errorf("docs.access-request: state %q not implemented in this PR", state)
	}

	var pf pendingFields
	if err := json.Unmarshal(fields, &pf); err != nil {
		return cardtmpl.BuildResult{}, fmt.Errorf("docs.access-request: unmarshal fields: %w", err)
	}

	labels := pendingLabels(env.Lang, pf)

	// docID 来自 requestId 的前缀? 不对 —— docID 是 deepLink 的路由标识,handoff schema
	// 的 document.url 是"文档 URL",docID 需要另建映射。当前 pilot 从 document.url 派生
	// 无解;正确做法是让上游 mapping 层把 docID 单独传入。以 requestId 拿作 doc_id
	// 会污染 callback data 语义。
	//
	// 决定:在 pending 阶段,docID 由 mapping 层通过 fields 里预留的 requestId 补齐 —— 但
	// handoff schema 未声明 doc_id 字段。折衷:当前 pilot 用 pf.RequestID 作 docID
	// 兼容既有 deepLink 拼接(/d/{docID}?sp={spaceID}),后续 mapping PR 引入 docId
	// 字段并 backfill schema 时切换。这是已知妥协,写死到 Variant 备注,回头统一治理。
	docID := pf.RequestID

	content := docsApprovalContentFromFields(pf, labels)
	actions := cardtmplApprovalActions(labels.approveTitle, labels.denyTitle)

	body, cardActions, deepLink, err := cardtmpl.BuildDocsAccessRequestBodyWithLang(
		env.Lang, env.WebLoginURL, docID, pf.RequestID, env.SpaceID, content, actions,
	)
	if err != nil {
		return cardtmpl.BuildResult{}, err
	}
	return cardtmpl.BuildResult{
		Body:     body,
		Actions:  cardActions,
		Variant:  Variant,
		DeepLink: deepLink,
		// Source 用 Meta.Source (Registry 从 manifest.sourceLabel 载入)
	}, nil
}

// FallbackText 返回纯文本 fallback。当前 pilot 采用简洁模板,与 modules/notify.buildDocsFallbackText
// 的 access_requested 分支等价。
func (t *Template) FallbackText(state cardtmpl.State, fields json.RawMessage, lang string) (string, error) {
	if state != StatePending {
		return "", fmt.Errorf("docs.access-request: fallback for state %q not implemented", state)
	}
	var pf pendingFields
	if err := json.Unmarshal(fields, &pf); err != nil {
		return "", fmt.Errorf("docs.access-request: unmarshal fields: %w", err)
	}
	labels := pendingLabels(lang, pf)
	actor := strings.TrimSpace(pf.Requester.Name)
	if actor == "" {
		return fmt.Sprintf(labels.fallbackAnon, pf.Document.Title), nil
	}
	return fmt.Sprintf(labels.fallbackNamed, actor, pf.Document.Title), nil
}
