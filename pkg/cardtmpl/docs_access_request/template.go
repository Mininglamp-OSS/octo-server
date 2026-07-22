// Package docsaccessrequest 实现 L2a pilot 卡片 docs.access-request@0.2.0。
// 与 pkg/cardtmpl (L0 基座) 的分层关系见 docs/platform-card-base.md §2。
//
// 注册流程 (在 composition root 里):
//
//	registry := cardtmpl.NewRegistry()
//	registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
//	registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersion)
//	registry.Freeze()
package docsaccessrequest

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

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

// Meta 返回注册期固化静态元数据的防御性深拷贝 (R3-2):调用方修改返值不会
// 污染 Registry 内部,冻结后契约不可变。非零成本 (克隆若干小 map),但生产
// 热路径 Registry.Render 内部直接读 entry.meta 不经过本方法,吞吐不受影响。
func (t *Template) Meta() cardtmpl.TemplateMeta {
	return t.meta.Clone()
}

// pendingFields 是 pending sample 反序列化后的数据契约。字段结构与
// pkg/cardtmpl/docs_access_request/handoff/docs.access-request@0.2.0/contract/data.schema.json 对齐。
type pendingFields struct {
	RequestID          string `json:"requestId"`
	State              string `json:"state"`
	RequestReason      string `json:"requestReason"`
	RequestedAtDisplay string `json:"requestedAtDisplay"`
	MessageTimeDisplay string `json:"messageTimeDisplay"`
	Document           struct {
		DocID      string `json:"docId"` // 服务端 mapping 层从 DocsCardFields.DocID 填入
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
// R2-7: 尊重 caller ctx (cancel/deadline);Template 内部虽为纯 CPU 计算,
// 一次早退以匹配 renderCore 传入的 ctx 语义。
func (t *Template) Build(
	ctx context.Context,
	state cardtmpl.State,
	fields json.RawMessage,
	env cardtmpl.BuildEnv,
) (cardtmpl.BuildResult, error) {
	if err := ctx.Err(); err != nil {
		return cardtmpl.BuildResult{}, err
	}
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

	// docID 优先取 document.docId (mapping 层从 DocsCardFields.DocID 填);
	// 缺失兜底到 requestId (与 pilot smoke test / handoff sample 语义一致)。
	docID := strings.TrimSpace(pf.Document.DocID)
	if docID == "" {
		docID = pf.RequestID
	}

	content := docsApprovalContentFromFields(pf, labels)
	actions := cardtmplApprovalActions(labels.approveTitle, labels.denyTitle)

	body, cardActions, deepLink, err := cardtmpl.BuildDocsAccessRequestBodyWithLang(
		env.Lang, env.WebLoginURL, docID, pf.RequestID, env.SpaceID, content, actions,
	)
	if err != nil {
		return cardtmpl.BuildResult{}, err
	}
	// pilot Template 给顶层 Action.OpenUrl 补 id "view_document",与
	// reports/pending.interaction.json 声明一致 (A15c 交互契约锁)。legacy
	// wrapper (BuildDocsAccessRequestCard) 不加 id,保持迁移前后字节等价基线
	// (F4);两条路径故意分叉,由本处的一行本地增改承担 "pilot 侧独有" 的差异。
	for _, a := range cardActions {
		if am, ok := a.(map[string]interface{}); ok && am["type"] == "Action.OpenUrl" {
			if _, has := am["id"]; !has {
				am["id"] = "view_document"
			}
			break // 只给第一个 (顶层查看详情) 加 id
		}
	}
	// Source 按 env.Lang 本地化 —— 覆盖 Meta.Source (Registry 从
	// manifest.sourceLabel 载入中文默认 "文档");F5: 英文卡片不能带中文来源。
	src := sourceForLang(env.Lang)
	return cardtmpl.BuildResult{
		Body:     body,
		Actions:  cardActions,
		Variant:  Variant,
		DeepLink: deepLink,
		Source:   &src,
	}, nil
}

// FallbackText 返回纯文本 fallback。R2-3: actor/title 用 sanitizeLine 消除内部换行/
// 控制字符,防止调用方通过嵌入 "\n" 伪造多行 DM 视觉 (同 modules/notify.sanitizeLine 语义)。
//
// R4-1 (#633 blocking): 必须保留申请原因(excerpt)与时间——与迁移前
// modules/notify.buildDocsFallbackText 的 access_requested 4 行(attribution/title/
// excerpt/time)信息量对齐。字段均由 mapDocsCardFieldsToJSON 传入(requestReason /
// requestedAtDisplay);先前只出 actor+title 一行会在降级路径静默丢字段。每个字段
// 各自 sanitizeLine 后再用 "\n" 连接,内部换行注入仍被消除,只有字段间是合法换行。
func (t *Template) FallbackText(state cardtmpl.State, fields json.RawMessage, lang string) (string, error) {
	if state != StatePending {
		return "", fmt.Errorf("docs.access-request: fallback for state %q not implemented", state)
	}
	var pf pendingFields
	if err := json.Unmarshal(fields, &pf); err != nil {
		return "", fmt.Errorf("docs.access-request: unmarshal fields: %w", err)
	}
	labels := pendingLabels(lang, pf)
	actor := sanitizeLine(pf.Requester.Name)
	title := sanitizeLine(pf.Document.Title)

	var b strings.Builder
	if actor == "" {
		b.WriteString(fmt.Sprintf(labels.fallbackAnon, title))
	} else {
		b.WriteString(fmt.Sprintf(labels.fallbackNamed, actor, title))
	}
	// 申请原因(与 legacy excerpt 行对齐:裸文本一行)
	if reason := sanitizeLine(pf.RequestReason); reason != "" {
		b.WriteString("\n")
		b.WriteString(reason)
	}
	// 时间(与 legacy "时间：<ts>" 行对齐);requestedAtDisplay 优先,回退 messageTimeDisplay
	ts := sanitizeLine(pf.RequestedAtDisplay)
	if ts == "" {
		ts = sanitizeLine(pf.MessageTimeDisplay)
	}
	if ts != "" {
		b.WriteString("\n")
		b.WriteString(labels.timeLabel + labels.kvSep + ts)
	}
	return b.String(), nil
}

// sanitizeLine 与 modules/notify.sanitizeLine 语义一致:控制字符 (含 \n \r \t) 替换成
// 空格,再 TrimSpace。防止调用方在 actor/title 里嵌换行伪造多行 DM。
func sanitizeLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}
