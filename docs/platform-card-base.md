# Octo 卡片消息平台基座契约（octo-card@1.0）

> **权威声明**：本文档是 octo-server 卡片消息基座(L0)的**跨仓契约权威**,与
> `pkg/cardtmpl/{template,registry,helpers,updater,metrics}.go`、`pkg/cardmsg/`、
> `internal/carddispatch/`、`internal/cardactiondispatch/` 的实现互为镜像 ——
> 两者如有出入,以代码为准并视为本文档的 bug。规范源头:
> `.octospec/tasks/cardtmpl-registry-pilot/brief.md`(基座落地 + docs.access-request pilot);
> 分层与生命周期规则见 §2。
>
> **读者**:业务后端(docs-backend / summary-backend / 未来 L2b 业务方)、octo-web、
> 平台组内部。跨仓联调请以本文档 + `pkg/cardtmpl/testdata/handoff/<id>@<ver>/`
> 制品为准。

> 本文定义 octo-server 卡片消息的平台级基座契约（L0），所有业务卡片（文档/智能总结/审批/Git/...）都在此基座上以版本化模板（L1）形式注册与运行。
>
> 首个验证样板：`docs.access-request@0.2.0`（web 侧起草）。
>
> 对齐参考：Slack Block Kit、飞书卡片 JSON 2.0；复用现有 octo 实现，不重复造轮子。

---

## 1. 设计原则

| 原则 | 说明 |
|---|---|
| **server-authoritative** | 卡片布局、文案、交互 ID 由服务端拥有；调用方只传结构化业务字段，不构造 AC JSON。 |
| **版本化** | 每张卡有独立 `templateId@version`；历史消息永远按当时模板版本渲染，新模板不影响在途消息。 |
| **契约机读** | 数据 schema、交互契约、样本都以 JSON 文件随代码提交，CI 自动校验；不依赖 Markdown 文档或 Go 注释作为跨仓契约。 |
| **双模式兼容** | 基座同时支持 ①Go 代码模板（强校验/性能好，pilot 优先）②JSON 模板+变量绑定（设计/运营可改，后续开放）。 |
| **显式降级** | 每张模板必须声明 fallback 文本；render 失败/低版本客户端/功能 gate 关闭时自动降级，通知"必达"。 |
| **白名单默认关** | 组件、action、profile 全部走白名单（复用 `pkg/cardmsg/whitelist.go`），未知元素默认拒绝。 |

---

## 2. 分层模型

```
┌──────────────────────────────────────────────────────────────────┐
│ L0  平台基座 (octo-card@1.0)                                     │
│     协议版本 · profile · 组件/动作白名单                          │
│     Template 接口 · Registry · Render 唯一入口                    │
│     统一 metadata 注入 · 回调 payload 结构 · 更新接口             │
│     (pkg/cardtmpl 平台层 + 现有 pkg/cardmsg / carddispatch)      │
├──────────────────────────────────────────────────────────────────┤
│ L1  单卡契约 (每业务卡一份,版本化)                                │
│     manifest.json · contract/data.schema.json                    │
│     reports/*.interaction.json · samples/ · goldens/             │
│     (纯 JSON 制品,与代码同仓,不含 Go 代码)                        │
├──────────────────────────────────────────────────────────────────┤
│ L2a 平台内置卡片 (octo 团队维护)                                  │
│     文档 · 智能总结 · 通用审批 · Git 通知等                       │
│     Template 接口实现 · 回调 finalizer · i18n 文案                │
│     (pkg/cardtmpl/<card>/ · modules/<domain>/)                   │
├──────────────────────────────────────────────────────────────────┤
│ L2b 业务自定义卡片 (业务侧维护,可选)                              │
│     业务方按 L0 接口自实现 Template + 自选 owner + 自建 callback  │
│     独立命名空间 · 独立 callback route · 独立能力清单             │
│     (pkg/cardtmpl/ext/<owner>/<card>/ 或独立 module)             │
└──────────────────────────────────────────────────────────────────┘
```

### 2.0 层职责一览

| 层 | 谁维护 | 稳定性承诺 | 代码归属 | 变更影响面 |
|---|---|---|---|---|
| L0 | 平台组(octo-server core) | 大版本(major)变更需通告所有 L2;小版本向后兼容 | `pkg/cardtmpl/{template,registry,helpers,updater,metrics}.go`、`pkg/cardmsg/*`、`internal/carddispatch/*`、`internal/cardactiondispatch/*` | 全平台;PR 强制两名 L0 维护者 review |
| L1 | 与对应 L2 卡同人维护 | 卡的 `version` 变即算新契约,老版本永不修改 | `pkg/cardtmpl/testdata/handoff/<card>@<ver>/` | 单卡;PR review 者需含平台组一人 |
| L2a | 平台组 + 业务组 | 卡自身版本管理;调用方 API 兼容期由业务组承诺 | `pkg/cardtmpl/<card>/*.go` | 单卡业务链路 |
| L2b | 业务方 | 卡自身版本管理;不受 L2a 排期约束 | `pkg/cardtmpl/ext/<owner>/<card>/*.go`(或业务独立 module) | 该业务 owner 命名空间内 |

### 2.1 三层变更权限

**L0 变更规则**:
- 新增字段/方法(向后兼容)→ 小版本(`octo-card@1.1`),不影响存量;
- 破坏性变更(接口签名/metadata 结构/回调 payload 结构改动)→ 大版本(`octo-card@2.0`),存量 L2 必须显式升级,新老版本并存至少一个 release;
- L0 变更禁止绕过 `Registry.Render`,禁止在 wrapper 之外私自注入 metadata。

**L1 变更规则**:
- **同一 `<id>@<version>` 一经发布,contract/samples/reports 不再修改**;需要变更 → 发新版本 `<id>@<new-version>`,老版本目录原样保留;
- 客户端与在途消息永远按 `metadata.octo.template.version` 走对应契约,不受新版本影响;
- 一张 L2 卡可同时有多个 L1 版本注册在 Registry(灰度/回滚用),`SetDefault` 决定新消息用哪个。

**L2 变更规则**:
- L2a 与 L2b 共享 Template 接口,但**代码目录、owner 命名空间、callback route 完全隔离**(见 §2.2);
- L2a 的卡使用平台组约定的 owner 词表(`docs` / `summary` / `notify` / `action` 等,与 `cardactiondispatch.RouteSpec` 对齐);
- L2b 的卡必须使用平台组分配的独立 owner(命名规则见 §2.2),不允许复用 L2a owner。

### 2.2 业务自定义卡片(L2b)约束

业务方要接入自己的卡片,**不修改 L0/L1 代码,只在 L2b 层加东西**。约束如下:

1. **Owner 命名空间隔离**
   - L2b owner 必须以 `ext.` 前缀开头(例:`ext.acme.pipeline`),与 L2a 平坦 owner(`docs`/`summary`) 泾渭分明;
   - **owner 清单单独维护**在 `docs/l2b-owners.md`(本 PR 引入空清单),每次新增/回收走 PR + 平台组两名维护者审批;清单条目至少含:`owner`(`ext.xxx.yyy`)、`vendor`(法务主体)、`contact`(值班联系人)、`callback_domain_allowlist`(白名单)、`created_at`;
   - Registry 注册期读取该清单校验 owner 前缀 + 分配关系,清单外 owner → 启动 panic;
   - `TemplateID` 建议但不强制包含 owner 后缀(例:`acme.pipeline.release-approval`)以方便 grep。

2. **Callback route 隔离**
   - L2b 的 `TemplateActionContract.Owner` 必须与其 owner 前缀一致;`ActionType` 由业务方自定义;
   - callback URL 指向业务方自己的服务(不指向 octo-server 内部);`RouteSpec.URL` 走白名单校验;
   - 秘钥用 `<OWNER>_CARD_ACTION_SECRET` env(平台组分配 env 名);
   - 与 L2a 一样,启动期 Registry.Freeze 之后 `Resolve(sender_uid, owner, action_type)` 必须命中,否则 fail-close。

3. **能力与安全边界(与 L2a 完全一致,不放松)**
   - 组件白名单、URL https、markdown escape、input 白名单、data key 白名单 **一律走 L0** `pkg/cardmsg` 与 `Registry.Render`,业务方无法绕过;
   - 业务方不能自选 `WireProfile`(仍限 `octo/v1`/`octo/v2`);
   - `BuildResult.DeepLink` 必须是 https,业务方不能传入任意域名;由业务方在 Template 构造时注入自己允许的 `WebLoginURL` 白名单(平台组通过 PR 审核域名);
   - 不允许写 `metadata.octo.*` 未声明字段,如需扩展走 L0 PR 审查(其它 owner 也能看见)。

4. **发布通道**
   - L2b 卡片在 `/v1/message/card/templates` 端点默认可见,但可通过 `manifest.visibility=private` 隐藏(只给指定 owner 的能力发现返回);
   - `/v1/internal/notify` 现有 token 只能发 L2a 卡;L2b 卡必须由业务方独立申请 token(方案 B 阶段先跑 L2a,L2b 通道在后续 PR 开放,本次 pilot 不做);
   - 本文档 §9 端点表中"业务方能力发现"只对已获授权的 owner 开放对应清单。

5. **发布顺序("最后"含义)**
   - **交付顺序**:L2b 是最后一层——L0 (`octo-card@1.0`) 稳定 → L2a 平台卡验证契约 → L2b 通道开放。本次 pilot 只交付 L0 + L2a(`docs.access-request`),**L2b 通道不开放**;
   - **L2b 开放硬门槛**(写死,平台组可通过 RFC 评估微调,但不允许在无 RFC 情况下降低):
     ① L0 (`octo-card@1.x`) 至少一个 release 无 breaking change;
     ② L2a 至少 **3 张**卡片走完 Registry 全链路(注册 + Render + conformance test + 生产灰度),覆盖至少两种视图模式(纯展示 v1 / 交互 v2);
     ③ `docs/l2b-owners.md` 空清单已合入,且有至少一份未通过审核的 L2b 申请 PR 作为流程演练;
     ④ callback URL 白名单机制、独立 token 通道、per-owner 观测指标全部在 L0 落地。
   - **消息通道位置**:L2b 卡片在客户端 UI 上默认不做特殊突出显示(与 L2a 同渠道到达);渲染顺序、置顶、折叠策略由客户端自行决定,L0 不承诺。

### 2.3 现有代码到分层的映射

| 现有代码 | 归属层 | 备注 |
|---|---|---|
| `pkg/cardmsg/*`(profile/validate/whitelist/inputs) | L0 | 不动,复用 |
| `internal/carddispatch/*`(Sender/Producer/Metrics) | L0 | 不动,复用 |
| `internal/cardactiondispatch/*`(Registry/Route/Finalizer) | L0 | 不动,复用;新增 owner 前缀校验(见 §2.2 约束 1) |
| `modules/cardtrust/*`(签名/信任) | L0 | 不动,复用 |
| `pkg/cardtmpl/{resource,approval_request,approval_result,docs_action}.go` | L2a | 保留;pilot 起逐张迁到 Template 接口 |
| `pkg/cardtmpl/testdata/handoff/<card>@<ver>/`(本 PR 引入) | L1 | JSON 制品目录 |
| `pkg/cardtmpl/{template,registry,helpers,updater,metrics}.go`(本 PR 新增) | L0 | 新增,平台组维护 |
| `modules/notify/*`(现有 docs/summary/approval 通道) | L2a 业务链路 | 方案 B 内壳走 Registry.Render |
| `pkg/cardtmpl/ext/<owner>/<card>/`(未来) | L2b | 目录规划占位,本 PR 不建 |

---

## 3. 协议版本与 Profile

### 3.1 协议版本

- 顶层 `AdaptiveCard` `version` 字段 = AC 规范版本（当前 `1.5`，复用 `cardmsg.CardVersion`）。
- 基座协议版本：本文档定义的 `octo-card@<major>.<minor>`，写入卡片元数据 `metadata.octo.protocol = "octo-card@1.0"`，便于客户端识别能力。

### 3.2 Profile（能力分档）

复用现有 `pkg/cardmsg/profiles.go`：

| profile | 能力 |
|---|---|
| `octo/v1` | 展示档：`TextBlock`/`ColumnSet`/`Container`/`Image`/`FactSet`/`ActionSet`/`Action.OpenUrl`/`Action.CopyToClipboard`；**无** `Action.Submit`、无 `Input.*`。 |
| `octo/v2` | 交互档：在 v1 基础上支持 `Action.Submit`、`Action.ToggleVisibility`、`Input.Text`/`Input.ChoiceSet`/`Input.Toggle`；需声明 input id 白名单。 |

每张模板在 `manifest.views[].wireProfile` 声明视图使用的 profile（对齐 handoff）；render 前由 `cardmsg.Validate(doc, profile)` 严格校验。

---

## 4. L1 单卡契约目录规范

每张卡一个目录，命名 `<template-id>@<version>`（例：`docs.access-request@0.2.0`）：

```
<template-id>@<version>/
├── manifest.json              # 卡元数据
├── contract/
│   └── data.schema.json       # 调用方入参 JSON Schema (draft-07)
├── templates/                 # JSON 模式用,代码模式下可选
│   ├── <view>.template.json
├── samples/                   # 入参样本,每个样本=一个典型状态
│   ├── <state>.json
├── goldens/                   # 编译后 AC JSON 基线(删除 template 元数据后字节相等)
│   └── <state>.card.json
└── reports/
    └── <state>.interaction.json   # 该视图交互契约
```

### 4.1 manifest.json

```jsonc
{
  "schemaVersion": 2,
  "id": "docs.access-request",
  "name": "文档访问申请",
  "version": "0.2.0",
  "contractVersion": "1.0.0",      // data.schema 版本
  "adaptiveCardVersion": "1.5",
  "renderProfile": "octo-chat@1.0.0",
  "protocol": "octo-card@1.0",
  "defaultLocale": "zh-CN",
  "owner": "docs",                 // 对应 cardactiondispatch owner,与 callback 路由一致
  "actionType": "access_request.decision",
  "views": {
    "pending": {
      "wireProfile": "octo/v2",
      "states": ["pending"],                          // 该视图承载的业务状态,一个 state 只能属于一个 view
      "template": "templates/pending.template.json",  // JSON 模式路径;代码模式可忽略
      "samples": ["samples/pending.json"]
    },
    "result": {
      "wireProfile": "octo/v1",
      "states": ["approved", "rejected"],
      "template": "templates/result.template.json",
      "samples": ["samples/approved.json", "samples/rejected.json"]
    }
  }
}
```

**state↔view 映射契约**:每个 `state` 值在整个 manifest 内**只能出现一次**(单向映射),由 `TemplateMeta.ViewFor(state)` 提供机读查询;Registry 加载 manifest 时校验唯一性,违例 → 注册期 panic。业务代码不允许自行维护 state→view 映射,一律走 `Meta().ViewFor(state)`。

```jsonc
// 简化示例(纯展示卡,单视图单状态)
"views": {
  "default": { "wireProfile": "octo/v1", "states": ["shown"], "samples": ["samples/default.json"] }
}
```

### 4.2 data.schema.json（调用方入参契约）

- JSON Schema draft-07，`additionalProperties: false`。
- 字段分两类，必须在 schema 里显式归属：
  - **input 字段**：调用方传入（title/actor/requestReason/...）。
  - **server-filled 字段**：服务端填充（permission 文案、viewDetails 按钮、deepLink、source、variant、template 元数据、i18n 文案）—— 在 schema 里标 `readOnly: true` 或注释说明，schema 校验**只校验调用方传入部分**（由 Template 的 `Validate` 方法在内部对输入子集执行）。
- 状态机用 `allOf/if/then` 表达（如 handoff 里 approved/rejected 需 decision）。

### 4.3 reports/<state>.interaction.json（交互契约）

机读声明交互面，CI 一致性测试锚点：

```jsonc
{
  "actions": [
    {
      "id": "view_document",
      "type": "Action.OpenUrl",
      "dataKeys": []
    },
    {
      "id": "docs-access-approve",       // 与 Go 常量 DocsApproveActionID 一致
      "type": "Action.Submit",
      "associatedInputs": "none",
      "dataKeys": ["owner", "action_type", "doc_id", "request_id", "decision"]
    },
    {
      "id": "docs-access-deny",
      "type": "Action.Submit",
      "associatedInputs": "auto",
      "dataKeys": ["owner", "action_type", "doc_id", "request_id", "decision"],
      "inputIds": ["deny_reason"]
    }
  ],
  "inputs": [
    {
      "id": "deny_reason",
      "type": "Input.Text",
      "isRequired": true,
      "isVisible": false,
      "maxLength": 300
    }
  ],
  "toggles": [
    // Action.ToggleVisibility 目标
  ]
}
```

`id` 与 Go 常量 (`DocsApproveActionID`/`DocsDenyActionID`/`DocsDenyReasonInputID`) 必须字节一致；CI 断言：
1. 卡渲染产物中出现的 `Action.Submit.id` / `Input.id` == 声明集合(**完全相等**,任一漂移即失败);
2. 声明集合中每个 `Input.*`(无论 `isVisible` 值)都必须出现在产物 body 中——`isVisible:false` 的 hidden input(如 `deny_reason`)也不例外,它是 `cardmsg.ValidateInputs` 的先决条件;
3. `Action.Submit.data` 的 key 集合 == `dataKeys`(完全相等);
4. `Action.Submit` 的 `associatedInputs` 值与声明一致。

---

## 5. L0 Template 接口（Go 代码模式）

**设计原则**:接口小(3-4 个方法),元数据一次性拿走,`Build` 只交业务片段,基座强制注入 metadata,防止跨仓多方实现漏写。

```go
// pkg/cardtmpl/template.go
package cardtmpl

import (
    "context"
    "encoding/json"

    "github.com/santhosh-tekuri/jsonschema/v6"
)

// ID 是模板稳定标识,全小写+点分隔,例 "docs.access-request"。
type ID string

// ViewKey 是模板内视图名,例 "pending" / "result"。
type ViewKey string

// State 是业务状态值,例 "pending" / "approved" / "rejected";
// 在 manifest.views[*].states 里唯一归属某一 view。
type State string

// TemplateActionContract 声明本卡 Submit action 归属的 callback 路由,
// 与 cardactiondispatch.RouteSpec 三元组对齐 {sender_uid=NotifyBotUID, owner, action_type}。
// 纯展示卡(无 Submit)在 TemplateMeta.ActionContract 置 nil。
type TemplateActionContract struct {
    Owner      string
    ActionType string
}

// ViewSpec 视图静态元数据;由 manifest.views 反序列化。
type ViewSpec struct {
    WireProfile string  // octo/v1 | octo/v2
    States      []State // 该视图承载的业务状态,manifest 内跨视图去重
}

// Source 与现有 pkg/cardtmpl/resource.go 的 Source 结构一致(label + iconUrl)。
type Source struct {
    Label   string
    IconURL string
}

// TemplateMeta 静态元数据,注册期一次性构造完成后不可变;
// 通过 Template.Meta() 一次拿走,不再逐字段方法调用。
type TemplateMeta struct {
    ID             ID
    Version        string
    Protocol       string                        // 恒 "octo-card@1.0"
    Views          map[ViewKey]ViewSpec
    ActionContract *TemplateActionContract       // nil = 纯展示卡
    Manifest       json.RawMessage               // 原始 manifest.json,供 /v1/message/card/templates 端点透出
    InputSchema    *jsonschema.Schema
    Source         Source                        // metadata.octo.source 默认值,可被 BuildResult.Source 覆盖
    stateIndex     map[State]ViewKey             // 由 Views 反推;查询走 ViewFor()
    interactions   map[ViewKey]InteractionReport // 由 reports/*.interaction.json 载入
}

// ViewFor 是 state↔view 的唯一查询入口;业务代码不允许自行维护映射。
// 未注册 state → ok=false。
func (m TemplateMeta) ViewFor(state State) (ViewKey, bool) {
    v, ok := m.stateIndex[state]
    return v, ok
}

// Interaction 返回视图的机读交互契约;非 v2 视图 → ok=false。
func (m TemplateMeta) Interaction(view ViewKey) (InteractionReport, bool) {
    r, ok := m.interactions[view]
    return r, ok
}

// InteractionReport 对应 reports/<view>.interaction.json,注册期反序列化后固化在 Meta 里。
type InteractionReport struct {
    Actions []DeclaredAction
    Inputs  []DeclaredInput
    Toggles []DeclaredToggle
}

type DeclaredAction struct {
    ID               string
    Type             string // Action.Submit / Action.OpenUrl / Action.ToggleVisibility / Action.CopyToClipboard
    AssociatedInputs string // "none" / "auto"
    DataKeys         []string
    InputIDs         []string
}
type DeclaredInput struct {
    ID         string
    Type       string // Input.Text / Input.ChoiceSet / Input.Toggle
    IsRequired bool
    IsVisible  bool
    MaxLength  int
}
type DeclaredToggle struct {
    ByActionID    string
    TargetElement string
    IsVisible     bool
}

// BuildEnv 模板构建时的环境参数,由调用点统一解析并 typed 传入;
// 不允许模板内部再 i18n.OutboundLanguage(ctx) 或注入 map[string]any 上下文。
// 若某张模板需要额外 server-only 上下文(如 actor UID / 权限判定结果),
// 通过 constructor 依赖注入到 Template 实现里,不进 BuildEnv。
type BuildEnv struct {
    WebLoginURL string // 绝对 https,deep-link 拼接用
    Lang        string // i18n.OutboundLanguage(ctx) 由调用点解析后传入
    SpaceID     string
}

// BuildResult 是 Template.Build 的返回值,只承载业务片段;
// 基座 wrapper(Registry.Render)负责组装完整 AC 顶层文档并注入
// metadata.octo.{protocol,template,variant,source} + metadata.webUrl。
// 模板不 marshal AC 顶层,不写 metadata 字段。
type BuildResult struct {
    Body     []any   // AC body 元素(TextBlock/ColumnSet/Container/...)
    Actions  []any   // AC 顶层 actions(Action.Submit / Action.OpenUrl / ...)
    Variant  string  // metadata.octo.variant,如 "docs.access_requested"
    DeepLink string  // metadata.webUrl 与"查看详情"按钮共同来源,必填绝对 https
    Source   *Source // 覆盖 Meta.Source;nil = 不覆盖
}

// Template 是每张业务卡实现的接口,只有 3 个行为方法。
type Template interface {
    // Meta 返回注册期固化的静态元数据,零成本调用(不 IO / 不重计算)。
    Meta() TemplateMeta

    // Build 渲染指定业务状态下的卡片业务片段。
    // - fields: 调用方原始入参,基座已用 Meta().InputSchema 校验通过;
    // - state → view 的映射由 Meta().ViewFor(state) 提供;
    // - 产物只包含业务片段,不 marshal AC 顶层,不写 metadata。
    Build(ctx context.Context, state State, fields json.RawMessage, env BuildEnv) (BuildResult, error)

    // FallbackText 返回纯文本 fallback;渲染失败/低版本客户端/功能 gate 关闭时使用。
    FallbackText(state State, fields json.RawMessage, lang string) (string, error)
}

// ---- 基座 wrapper(强制 metadata 注入,pkg/cardtmpl 内部实现) ----

// Render 是"Template.Build → 完整 AC JSON" 的唯一路径:
//  1. Lookup(id, version) 命中 Template;
//  2. Meta().InputSchema.Validate(fields) → 失败返 ErrFieldsInvalid;
//  3. Meta().ViewFor(state) 决定 view / profile → 未注册 state 返 ErrStateUnknown;
//  4. Template.Build(ctx, state, fields, env) 拿 BuildResult;
//  5. 组装 AC 顶层 + 注入 metadata.octo.{protocol,template,variant,source} + webUrl;
//  6. cardmsg.Validate(doc, profile) → 失败视为 render_error。
// 业务代码只调 Render,不允许调 Template.Build 后自己拼装。
func (r *Registry) Render(ctx context.Context, id ID, version string,
    state State, fields json.RawMessage, env BuildEnv) (json.RawMessage, error)
```

### 关键约束(基座强制,非"模板自觉")

1. **metadata 由基座注入**:`metadata.octo.protocol` / `template.{id,version}` / `variant` / `source` 和 `metadata.webUrl` 全部由 `Registry.Render` 写入,模板返回 `BuildResult` 不接触这些字段。
2. **`cardmsg.Validate(doc, profile)` 由基座调用**:Template 无法绕过;view→profile 从 `Meta.Views[view].WireProfile` 取。
3. **`Action.Submit.data["owner"]` / `["action_type"]` 与 `ActionContract` 一致性**:conformance test 校验;pilot 注册时用其 sample 跑一次 `Render` + 断言,失败 → 注册期 panic(fail-close)。
4. **`DeepLink` 必填且绝对 https**:`BuildResult.DeepLink == ""` 或非 https → Render 返 error;deep-link 由模板私有拼接函数从 `env.WebLoginURL` 组装,不接受调用方任意域名。
5. **禁止未声明的 `metadata.octo.*` 扩展**:如需扩展走本文档 PR 审查。

---

## 6. Registry（注册中心）

```go
// pkg/cardtmpl/registry.go
package cardtmpl

type Registry struct { ... }

// Register 注册模板,composition root 显式调用,不依赖 init() 副作用。
// 传入 manifest / schema / interaction reports 的 embed.FS 由 Registry 载入并
// 构造 TemplateMeta;dup {id,version} → panic;schema 编译失败 → panic;
// state 在多个 view 里重复 → panic(fail-close)。
func (r *Registry) Register(t Template, assets embed.FS, root string)

// SetDefault 显式设置某 templateId 的默认版本;Lookup 空版本时用。
func (r *Registry) SetDefault(id ID, version string)

// Freeze 冻结注册表,之后再 Register/SetDefault → panic。
func (r *Registry) Freeze()

// Lookup 查找模板;未注册/无默认版本 → 类型化 error ErrTemplateUnknown。
// 命中只返回 Template(不返回 Meta,由调用方按需 t.Meta())。
func (r *Registry) Lookup(id ID, version string) (Template, error)

// Render 是"Template.Build → 完整 AC JSON"的唯一路径(见 §5)。
// 业务代码只调 Render,不允许直接调 Template.Build 后自己拼装。
func (r *Registry) Render(ctx context.Context, id ID, version string,
    state State, fields json.RawMessage, env BuildEnv) (json.RawMessage, error)

// List 导出已注册模板元数据,供 GET /v1/message/card/templates 使用。
func (r *Registry) List() []TemplateMeta

// RegisteredForTest 给 conformance test 用,返回与生产完全相同的注册集合。
func (r *Registry) RegisteredForTest() []Template
```

**启动 wiring(composition root)**:
- 在 `internal/cardtmplwiring/`(或 `main.go` 同层 wiring 文件)显式 `Register` 所有模板,与 `cardactiondispatch.NewRegistry`、`carddispatch.NewRegistry` 并列;
- 注册后立即 `Freeze()`;
- 注册期 wrapper 用每张模板的 samples 跑一次 `Render`,断言产物 `Action.Submit.data.{owner,action_type}` 与 `Meta.ActionContract` 一致(§5 约束 3),失败 → 启动 panic(fail-close);
- 若某回调路由要求存在的模板未注册(如 `main.go:521` 已 assert 的 `docs/access_request.decision`),启动 fail-close。

---

## 7. 统一交互回调契约

复用 `internal/cardactiondispatch`，统一回调 payload 顶层结构（对齐 Slack/飞书交互 payload 结构）：

```jsonc
{
  "protocol": "octo-card@1.0",
  "type": "card.action",                // card.action | card.revision
  "action": {
    "id": "docs-access-deny",           // 对应 reports 里的 action id
    "data": {                           // Action.Submit.data 的内容
      "owner": "docs",
      "action_type": "access_request.decision",
      "doc_id": "d_xxx",
      "request_id": "REQ-024",
      "decision": "deny"
    },
    "inputs": { "deny_reason": "..." }  // associatedInputs=auto 时收集声明的 input 值
  },
  "card": {
    "template_id": "docs.access-request",
    "template_version": "0.2.0",
    "view": "pending",
    "message_id": 12345,
    "channel_id": "uid_xxx",
    "channel_type": 1
  },
  "actor": { "uid": "uid_xxx" },
  "trigger_id": "...",                  // 短期令牌,用于 modal/延迟响应
  "response_url": "..."                 // 异步更新卡片用,5min 有效
}
```

### 业务方回调处理
- 业务方实现 `cardactiondispatch.Finalizer`（复用现有 finalizer 机制），按 `(owner, action_type)` 绑定；
- Finalizer 调用 `Registry.Lookup(templateId, version)` → 校验业务状态 → 调用 `Build(resultState, resultFields, env)` → 通过 `CardUpdater` 更新原消息（切换视图/替换 body），对齐 Slack response_url / 飞书流式更新体验。

---

## 8. 统一更新接口（视图切换/终态）

```go
// pkg/cardtmpl/updater.go (在现有 pkg/cardrevision 与 carddispatch 之上封装)
type CardUpdater interface {
    // ReplaceView 切换到同模板另一视图(如 pending → result),
    // 服务端权威重渲染,替换原消息 body,保持 message_id 不变。
    ReplaceView(ctx context.Context, target carddispatch.Target,
        templateID ID, version string, newState string, fields json.RawMessage) error

    // Append 追加内容到卡片底部(如进度帧),不替换原 body;v1/v2 均支持。
    Append(ctx context.Context, target carddispatch.Target, element json.RawMessage) error
}
```

底层复用现有 `pkg/cardrevision` 修订账本 + `carddispatch` 发送层，不新造通路。

---

## 9. 对外能力端点（契约开放给业务方/前端）

复用/新增以下 HTTP 端点（pilot 阶段可只做内部用）：

| 端点 | 用途 |
|---|---|
| `POST /v1/internal/notify` | **现有**发送入口；方案 B 阶段外壳不动，内部走 Registry。未来加 envelope 模式后可选显式 `template_id`。 |
| `POST /v1/message/card/action` | **现有**交互回调入口(挂在 `/v1/message` group,经 `modules/cardtrust` 校验后路由到 `cardactiondispatch`);payload 按第 7 节标准化(客户端上行结构与已有实现一致,基座只统一 owner/action_type/inputs 语义,不改端点)。 |
| `GET /v1/message/card/templates` | 新增:列出所有已注册模板(id/version/views/protocol/owner/actionType);给业务方/前端做能力发现。 |
| `GET /v1/message/card/templates/{id}@{version}` | 新增:返回 manifest + data.schema + interaction reports + samples,供跨仓生成 SDK/联调。 |

---

## 10. 降级策略

降级触发条件与行为（统一）：

| 条件 | 行为 |
|---|---|
| `OCTO_CARD_MESSAGE_ENABLED=false`（总开关） | 全量走模板 `FallbackText()` 发纯文本 DM。 |
| 模板 gate（如 `OCTO_DOCS_APPROVAL_CARD_ENABLED=false`） | 该模板回退到对应 v1 视图（如 resource card），不发交互卡。 |
| 客户端 profile < 模板要求（`octo/v2` 卡发向 v1 客户端） | 发送 v1 展示视图；若模板无 v1 视图则发 fallback 文本。 |
| **输入校验失败**（schema 不通过、必填缺失） | **400 `ErrNotifyCardInvalid`，零投递，不降级**（对齐 brief 决策 C1，禁止把错请求伪装成成功文本）。 |
| 服务端 render 错（cardmsg.Validate 失败/marshal 失败） | 打 `cardtmpl_build_total{result=render_error}` metrics，降级为 fallback 文本，保证必达。 |
| Registry 未命中（composition bug，理论不应发生） | 500 internal error，打 error log；不降级为错文本。 |

---

## 11. 安全边界（复用现有规则）

基座强制以下安全规则，任何模板不得绕过：
- URL 白名单：所有 image/icon/link/deep-link 必须绝对 https（复用 `requireHTTPS`）；deep-link 域名来自 `External.WebLoginURL`，禁止业务方传入任意域名。
- Markdown/XSS 转义：所有用户可控文本走 `escapeMarkdown`；`TextRun` 除外（不渲染 markdown）。
- 长度上限：title ≤200 rune、excerpt ≤300 rune、fact value ≤500 rune、action title ≤80 rune、copy text ≤`cardmsg.MaxCopyTextBytes`（复用 `pkg/cardtmpl/resource.go` 常量）。
- Input 白名单：`Action.Submit.inputIds` 中每个 id 必须在模板 `Interaction.Inputs` 中声明，否则 `cardmsg.ValidateInputs` 拒绝（已存在）。
- Action data 白名单：`Action.Submit.data` key ⊆ 声明 `dataKeys`（conformance test 校验）。
- 防越权：callback 路由按 `(owner, action_type)` 匹配 RouteSpec，与 `Template.ActionContract()` 三方一致（CI 校验）；业务 finalizer 内部再做业务级权限校验（如"是否文档 owner"）。

---

## 12. i18n

- 模板通过 `BuildEnv.Lang` 拿收件人语言，不自行调 `OutboundLanguage(ctx)`；
- 服务端文案（按钮/标签/状态词）由模板按语言 hardcode 或加载语言包（复用 `labelsForLanguage` 模式，逐步从 Go 代码迁到 locales）；
- 业务字段（title/actor/excerpt）是调用方预格式化字符串，**模板不替调用方做语言选择**；
- fallback 文本同样按 lang 生成。

---

## 13. 观测指标（在 pkg/cardtmpl 自己打，不复用 carddispatch.Metrics）

| 指标 | labels | 说明 |
|---|---|---|
| `cardtmpl_build_total` | template_id, version, view, result=ok\|fields_invalid\|render_error | Build 结果；fields_invalid 应在 400 返回前就拦截，本指标反映防线穿透情况。 |
| `cardtmpl_callback_total` | template_id, version, action_id, result=ok\|rejected\|error | 交互回调结果。 |
| `cardtmpl_update_total` | template_id, version, result=ok\|error | 卡片更新（视图切换/追加）结果。 |

label `template_id` 基数 = 注册表大小（硬编码），无基数爆炸。

---

## 14. 与现有代码映射

| 基座组件 | 现有代码位置 | 改造动作 |
|---|---|---|
| AdaptiveCard 构建/校验/白名单 | `pkg/cardmsg/*` | 不动，复用 |
| 派发/bot 身份/空间鉴权 | `internal/carddispatch/*` | 不动；Sender.Send 接受的 Card.Profile 从 Registry 拿 |
| 回调路由/finalizer | `internal/cardactiondispatch/*` | 回调 payload 顶层结构标准化（第 7 节），RouteSpec 不动 |
| 信任边界/签名校验 | `modules/cardtrust/*` | 不动 |
| 业务 builder | `pkg/cardtmpl/{resource,approval_request,approval_result,docs_action}.go` | 保留为 L2 实现；抽出 Template 接口、Registry、helpers |
| notify 入口/能力准入 | `modules/notify/{api,card,approval_card}.go` | 方案 B：外壳不动，内部 `deliver*` 改为 `Lookup→Build`；docs 通路 card.go 分支改造，保持 `notifyCapabilityAllows` 准入 |
| 修订账本 | `pkg/cardrevision/*` | 不动；ReplaceView 复用 |
| 现有 docs/summary/approval 卡片 | `pkg/cardtmpl/*` | pilot 迁 docs.access-request；其余分 PR 迁 |
| 现有 errcode/i18n | `pkg/errcode/notify.go` | 不动，复用 `ErrNotifyCardInvalid` |

---

## 15. Pilot 落地步骤（docs.access-request@0.2.0）

按 brief 走，基座能力随 pilot 一起交付：

1. **PR-0（基座骨架，无业务迁移）**
   - 新增 `pkg/cardtmpl/template.go`（Template 接口+类型）、`registry.go`、`helpers.go`（从 resource.go 抽出共用工具）、`updater.go`（封装 CardUpdater）、`metrics.go`；
   - 新增 `pkg/cardtmpl/testdata/handoff/` 目录结构说明（仅 README，不放业务卡）；
   - 新增 `/v1/message/card/templates` 只读端点(pilot 注册前返回空);
   - conformance test 骨架（A7），空 registry 下通过。

2. **PR-1（pilot docs.access-request@0.2.0）**
   - 把 handoff 4 类制品（manifest/contract/samples/reports）落到 `pkg/cardtmpl/testdata/handoff/docs.access-request@0.2.0/`（**不提 templates/*.tmpl.json 和 goldens**，按 brief A6）；
   - 新增 `pkg/cardtmpl/docs_access_request/template.go` 实现 Template 接口(3 方法:Meta/Build/FallbackText);Build 内部调抽出的 `buildDocsAccessRequestBodyWithLang(lang,...)` 返回 `BuildResult{Body,Actions,Variant,DeepLink}`,不 marshal AC 顶层、不注入 metadata(由 `Registry.Render` 强制注入);现有 `BuildDocsAccessRequestCard` 保留为 legacy 薄 wrapper(内部仍 marshal 完整 AC,给非 Registry 调用者兜底,后续 PR 迁移完可删);
   - composition root 注册 + Freeze；
   - 修改 `modules/notify/card.go` access_requested 分支为 Lookup→Build（A9），schema/kind 错返 400，render 错仍降级（决策 C1）；
   - 加入迁移前/后字节等价基线（A11）；
   - ActionContract 三方一致性 test（A8）；conformance test 全绿。

3. **PR-2（文档）**
   - 更新 `.octospec/tasks/card-message-internal-dispatch/docs-notify-contract.md`，引用基座契约文档和 testdata 路径；
   - 在 `.context/docs/` 或 `docs/` 放本基座契约文档的对外版本（给 docs-backend/octo-web 联调）；
   - 把本文件搬到正式 docs 路径。

4. **后续 PR（非 pilot）**
   - 迁 `docs.shared/commented`、`summary.completed/failed`、`generic.approval`、`docs.access.outcome` 到 Registry；
   - 开放 JSON 模板模式（`templates/*.template.json` + 表达式引擎）；
   - 开放 envelope 模式（调用方显式传 `template_id`），替换 `NotifyReq` 四选一；
   - 契约导出端点的 i18n 文案返回。

---

## 16. 不做什么（Out of scope, 对齐 brief）

- 不引入 `${}` 表达式引擎（PR-1 阶段）；
- 不改 `NotifyReq` 外部形状（方案 B）；
- 不迁非 pilot 模板；
- 不做客户端渲染分支（只写入 `metadata.octo.template`，客户端侧独立迭代）；
- 不新增 kill switch env（依赖启动 fail-close + 镜像 revert + 现有卡片总开关）；
- 不提交 handoff 的 `templates/*.tmpl.json` 和 `goldens/*.card.json`；
- **不开放 L2b 业务自定义通道**:本次只交付 L0 + L2a(`docs.access-request`);L2b 开放条件见 §2.2 约束 5,本 PR 仅引入空 `docs/l2b-owners.md` 清单占位与 `pkg/cardtmpl/ext/` 目录规划,不放代码、不接 owner 前缀校验入生产链路(校验逻辑本 PR 落但只跑 L2a owner 白名单,`ext.` 分支 dead code 有 test 但不 wire)。

---

## 附录 A：与 Slack/飞书能力映射

| 能力 | Slack Block Kit | 飞书卡片 2.0 | octo-card@1.0 |
|---|---|---|---|
| 版本声明 | block types 自然兼容 | 顶层 `schema: "2.0"` | `metadata.octo.protocol` + `AdaptiveCard.version` + template `version` |
| 模板模式 | Block Kit JSON 直出 | template_id + template_variable | L1 目录 + 双模式(Go/JSON) |
| 组件白名单 | 官方 block types | 官方 `tag` | `pkg/cardmsg/whitelist.go` |
| 交互元素 ID | `block_id` + `action_id` | `element_id` 全局唯一 | `DeclaredAction.ID` / `DeclaredInput.ID`，机读 interaction report |
| 回调 | block_actions 事件 | action 回调 | `POST /v1/message/card/action`,payload 第 7 节 |
| response_url 更新 | response_url(5min) | 延迟更新/流式更新 | `CardUpdater.ReplaceView/Append` + response_url 预留 |
| 降级 | 未知 block 忽略 + text fallback | schema 不兼容提示升级 | Profile 分档 + FallbackText（第 10 节） |
| 能力分档 | 全量交互 | 全量交互 | octo/v1 展示 / octo/v2 交互 |
