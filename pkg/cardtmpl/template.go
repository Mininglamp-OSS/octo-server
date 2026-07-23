// Package cardtmpl 提供 octo-server 卡片消息基座 (octo-card@1.0) 的模板注册与
// 渲染能力。所有业务卡片(L2a 平台内置 / L2b 业务自定义)以版本化 Template 形式
// 注册到 Registry,统一走 Registry.Render 产出经校验的 InteractiveCard payload。
//
// 权威契约: docs/platform-card-base.md
package cardtmpl

import (
	"context"
	"encoding/json"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Protocol 是本包实现的基座协议版本,写入 metadata.octo.protocol,
// 供客户端识别能力集合。破坏性变更需 bump 大版本 (§2.1)。
const Protocol = "octo-card@1.0"

// ID 是模板稳定标识,全小写点分隔 (例 "docs.access-request")。
type ID string

// ViewKey 是模板内视图名 (例 "pending" / "result")。
type ViewKey string

// State 是业务状态值 (例 "pending" / "approved" / "rejected")。
// 每个 State 在 manifest.views[*].states 里只能属于一个 view。
type State string

// TemplateActionContract 声明本卡 Submit action 归属的 callback 路由,
// 与 cardactiondispatch.RouteSpec 的 {owner, action_type} 对齐。
// 纯展示卡在 TemplateMeta.ActionContract 置 nil。
type TemplateActionContract struct {
	Owner      string
	ActionType string
}

// ViewSpec 视图静态元数据,由 manifest.views 反序列化得到。
type ViewSpec struct {
	// WireProfile 是渲染 profile,取值 cardmsg.ProfileV1 或 cardmsg.ProfileV2。
	WireProfile string
	// States 是该视图承载的业务状态集合,manifest 内跨 view 唯一。
	States []State
}

// Source 在 pkg/cardtmpl/resource.go 已定义,本文件复用。
// 结构:{Label, IconURL string},均可选;运行期由 producer 从 i18n 词表拿本地化文案后注入。

// TemplateMeta 是注册期一次性构造的静态元数据。内部持有 map/slice/*ptr,
// 对外暴露必须经 Clone() 深拷贝 (Template.Meta() / Registry.List()),
// 保证冻结后契约不可变、并发读安全。
type TemplateMeta struct {
	// ID / Version / Protocol 三字段唯一标识一版本模板。
	ID       ID
	Version  string
	Protocol string
	// RenderProfileCompatibility is the stable type-17 envelope value consumed
	// by clients; empty permanently selects the legacy renderer. The exact Forge
	// artifact remains provenance-only data in Manifest.
	RenderProfileCompatibility string
	// Views 承载 manifest 里所有已声明的视图 (含未激活的占位视图)。
	Views map[ViewKey]ViewSpec
	// ActionContract 非 nil 时,本卡的所有 Action.Submit.data 必须包含
	// {owner:Owner, action_type:ActionType}。校验时机:**注册期、且仅对有 sample 的
	// v2 view**(selfCheckSamples→assertActionContract,遍历顶层与内联 ActionSet)。
	// renderCore 运行期不再重复断言(pilot 由 TestActionContract_ThreeWayConsistency
	// 覆盖)。
	ActionContract *TemplateActionContract
	// Manifest 保留原始 manifest.json bytes,供 /v1/message/card/templates 端点透出。
	Manifest json.RawMessage
	// InputSchema 是调用方入参的 JSON Schema (draft-07);Registry.Render 前置校验。
	InputSchema *jsonschema.Schema
	// Source 是 metadata.octo.source 默认值;BuildResult.Source 非 nil 时覆盖。
	Source Source

	// stateIndex / interactions 由 Registry 加载 manifest 与 reports 后填充;
	// 查询走 ViewFor / Interaction,不允许直接读。
	stateIndex   map[State]ViewKey
	interactions map[ViewKey]InteractionReport
}

// ViewFor 返回 state 归属的视图。业务代码不允许自行维护映射,统一走本方法。
func (m TemplateMeta) ViewFor(state State) (ViewKey, bool) {
	v, ok := m.stateIndex[state]
	return v, ok
}

// Interaction 返回 view 的机读交互契约。非 v2 view 返回 ok=false。
func (m TemplateMeta) Interaction(view ViewKey) (InteractionReport, bool) {
	r, ok := m.interactions[view]
	return r, ok
}

// InteractionReport 对应 reports/<view>.interaction.json,注册期反序列化后固化。
type InteractionReport struct {
	Actions []DeclaredAction `json:"actions"`
	Inputs  []DeclaredInput  `json:"inputs"`
	Toggles []DeclaredToggle `json:"toggles,omitempty"`
}

// DeclaredAction 声明卡片顶层或内联的一个 Action。
type DeclaredAction struct {
	ID               string   `json:"id"`
	Type             string   `json:"type"`
	AssociatedInputs string   `json:"associatedInputs,omitempty"`
	DataKeys         []string `json:"dataKeys,omitempty"`
	InputIDs         []string `json:"inputIds,omitempty"`
}

// DeclaredInput 声明卡片内的一个 Input.* 元素。
// IsVisible=false 的 hidden input (如 deny_reason) 仍必须出现在 body 中,
// 是 cardmsg.ValidateInputs 的先决条件。
type DeclaredInput struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	IsRequired bool   `json:"isRequired,omitempty"`
	IsVisible  bool   `json:"isVisible"`
	MaxLength  int    `json:"maxLength,omitempty"`
}

// DeclaredToggle 描述一个 Action.ToggleVisibility 的目标声明。
type DeclaredToggle struct {
	ByActionID    string `json:"byActionId"`
	TargetElement string `json:"targetElement"`
	IsVisible     bool   `json:"isVisible"`
}

// BuildEnv 是 Template.Build 的环境入参,由调用点统一解析并 typed 传入。
// 禁止在 Template 内部再解析 ctx (i18n.OutboundLanguage) 或引入 map[string]any 上下文。
// 若某模板需额外 server-only 上下文 (如 actor UID),走 constructor 依赖注入。
type BuildEnv struct {
	WebLoginURL string // 绝对 https,deep-link 拼接用
	Lang        string // i18n.OutboundLanguage(ctx) 由调用点解析后传入
	SpaceID     string
}

// BuildResult 是 Template.Build 的返回值,只承载业务片段。
// 基座 wrapper (Registry.Render) 负责组装完整 AC 顶层 + payload envelope
// (type=17 / card / card_version / profile) + 注入 metadata。
// 模板不 marshal AC 顶层、不写 metadata 字段。
type BuildResult struct {
	Body     []any   // AC body 元素
	Actions  []any   // AC 顶层 actions
	Variant  string  // metadata.octo.variant
	DeepLink string  // metadata.webUrl (必填绝对 https)
	Source   *Source // 覆盖 Meta.Source,nil 表示不覆盖
}

// Template 是每张业务卡实现的接口。3 个行为方法,元数据一次拿走。
type Template interface {
	// Meta 返回注册期固化静态元数据的**防御性深拷贝** (m.Clone())。调用方可安全
	// 读取甚至修改返值而不污染 Registry 内部 —— 冻结后契约必须不可变,直接返回
	// 内部含 map/slice/*ptr 的浅拷贝等于把内部状态交给外部改坏 (还有并发 race)。
	// 因此本方法非零成本 (克隆若干小 map),但 Render 热路径内部直接读 entry.meta
	// 不经过 Meta(),吞吐不受影响 (R3-2)。
	Meta() TemplateMeta

	// Build 渲染指定业务状态下的卡片业务片段。
	// - fields 已经过 Meta().InputSchema 校验;
	// - state 到 view 的映射由 Meta().ViewFor(state) 提供;
	// - 产物只包含业务片段,不 marshal AC 顶层,不写 metadata。
	Build(ctx context.Context, state State, fields json.RawMessage, env BuildEnv) (BuildResult, error)

	// FallbackText 返回纯文本 fallback,渲染失败/低版本客户端/gate 关闭时使用。
	FallbackText(state State, fields json.RawMessage, lang string) (string, error)
}

// Clone 深拷贝 TemplateMeta,让外部持有者 (Template.Meta() 返值 / List() 返值 /
// 未来 HTTP 端点) 修改不会影响 Registry 内部 (R3-2:冻结后契约不可变;struct 里
// 含 map/slice/*ptr 共享,直接返值等于让外部改内部状态,还有并发 race)。
// InputSchema 是编译好的 jsonschema.Schema (santhosh-tekuri 库持有),对外只暴露
// Validate、内部不可变,故共享指针安全,不深拷贝。
func (m TemplateMeta) Clone() TemplateMeta {
	out := TemplateMeta{
		ID:                         m.ID,
		Version:                    m.Version,
		Protocol:                   m.Protocol,
		RenderProfileCompatibility: m.RenderProfileCompatibility,
		Source:                     m.Source,
		InputSchema:                m.InputSchema,
	}
	if m.Manifest != nil {
		out.Manifest = append(json.RawMessage(nil), m.Manifest...)
	}
	if m.ActionContract != nil {
		c := *m.ActionContract
		out.ActionContract = &c
	}
	if m.Views != nil {
		out.Views = make(map[ViewKey]ViewSpec, len(m.Views))
		for k, v := range m.Views {
			states := append([]State(nil), v.States...)
			out.Views[k] = ViewSpec{WireProfile: v.WireProfile, States: states}
		}
	}
	if m.stateIndex != nil {
		out.stateIndex = make(map[State]ViewKey, len(m.stateIndex))
		for k, v := range m.stateIndex {
			out.stateIndex[k] = v
		}
	}
	if m.interactions != nil {
		out.interactions = make(map[ViewKey]InteractionReport, len(m.interactions))
		for k, v := range m.interactions {
			out.interactions[k] = cloneInteractionReport(v)
		}
	}
	return out
}

func cloneInteractionReport(r InteractionReport) InteractionReport {
	out := InteractionReport{}
	if r.Actions != nil {
		out.Actions = make([]DeclaredAction, len(r.Actions))
		for i, a := range r.Actions {
			out.Actions[i] = a
			out.Actions[i].DataKeys = append([]string(nil), a.DataKeys...)
			out.Actions[i].InputIDs = append([]string(nil), a.InputIDs...)
		}
	}
	if r.Inputs != nil {
		out.Inputs = append([]DeclaredInput(nil), r.Inputs...)
	}
	if r.Toggles != nil {
		out.Toggles = append([]DeclaredToggle(nil), r.Toggles...)
	}
	return out
}
