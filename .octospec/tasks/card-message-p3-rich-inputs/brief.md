---
type: Task
title: "Task: card-message-p3-rich-inputs"
description: 卡片消息 octo/v2 白名单扩容 Input.Number/Date/Time + 提交期值校验（AC 1.5 内，不升版本）
tags: ["error-response", "wire-contract", "trust-boundary", "test"]
timestamp: 2026-07-09T00:00:00+08:00
# --- octospec extension fields ---
slug: card-message-p3-rich-inputs
upstream: card-message-interaction P3-3
source: self
---

# Task: card-message-p3-rich-inputs

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

把卡片消息 octo/v2 交互档的输入元素白名单从 3 个（`Input.Text` / `Input.Toggle` /
`Input.ChoiceSet`）扩到 6 个，补齐 **`Input.Number` / `Input.Date` / `Input.Time`**。
三者都是 Adaptive Cards **1.0** 元素，完全落在已固定的 `card_version: "1.5"` 内——
**这是白名单增量，不是版本升级、不是信封/事件契约变更**（对照 P3-1 的 `expires_at`
新增信封字段才需 maintainer sign-off）。

改动落在两处纯服务端逻辑：

1. **发送期白名单**（`pkg/cardmsg/validate.go`）：新类型并入现有 octo/v2 Input.*
   分支，继承同一套纪律——id 必填且帧内唯一（`registerID`）、`label`/`errorMessage`
   markdown 链接目标走同一正向 URL allowlist、`inlineAction` 路由校验。octo/v1 携带
   新类型仍拒（越级）。
2. **提交期值校验**（`pkg/cardmsg/inputs.go` `ValidateInputs`）：延续「形状可信」信任边界
   （声明过 + 类型对），对三类新输入按声明帧校验上行值的**格式**。`min`/`max` 区间**不**
   服务端强制（下放 bot 业务校验，「待确认 #1」已定稿，见文末）：
   - `Input.Number`：值必须可解析为**有限数**（显式拒 `NaN`/`±Inf`）；`""`（未填）放行。
   - `Input.Date`：值必须是 `YYYY-MM-DD`；`""` 放行。
   - `Input.Time`：值必须是 `HH:MM`（24h）；`""` 放行。

同时用测试锁定校验器对 **1.5 内 renderer-only 风格属性**的容忍：`Input.Text`
`style:"password"`、`Input.ChoiceSet` `style:"filtered"|"expanded"`（当前校验器只白名单
已知键、忽略未知属性，预期无需改码——测试是为防回归）。`filtered` 可搜索下拉由此在 1.5
内可用、零服务端往返（客户端本地过滤静态 choices）。

**为什么锁 1.5、不做 AC 1.6 `Data.Query`**：`Data.Query` 是动态 typeahead，需要一条
「表单填写中实时查在线 bot」的同步查询通道，与 Octo 的 server-authoritative 静态帧 +
异步事件队列模型冲突，是比 modal 还重的独立大项——不在本任务范围（见 P3-ROADMAP.md
「Decision: keep the target at 1.5」）。

## Background

- 现状核实：`pkg/cardmsg/validate.go:262` 的 octo/v2 分支只接受 `Input.Text` /
  `Input.Toggle` / `Input.ChoiceSet`；`Input.Number`/`Date`/`Time` 落入 `default:`
  → `ErrCardUnknownElement`（发送期 400）。
- 提交期信任边界：`pkg/cardmsg/inputs.go` 顶部注释确立「声明过、类型对、有上限」为
  bot 拿到的「形状可信」值；`isRequired`/`regex` **有意不在服务端强制**（表单完整性是
  端上 UX + bot 业务校验的事）——本任务沿用该原则，仅做「声明+类型+区间」校验。
- 错误映射（决定无需新 errcode）：
  - 发送期 `cardmsg.Validate` 错误由 ingress 调用方映射（`modules/bot_api/send.go:106`、
    `modules/incomingwebhook/card.go:58`、`modules/robot/api.go:781`），沿用既有码。
  - 提交期 `card/action` 把所有 `cardmsg.ValidateInputs` 错误**统一折叠**进
    `errcode.ErrMessageCardActionInvalid`（`modules/message/api_card_action.go:207-210`，
    反枚举），新类型的值非法同样归入此码——**不新增 errcode、不改 i18n**。
- 契约镜像：`docs/card-protocol.md` §2（第 43 行枚举 octo/v2 输入集）+ §7（P2 契约冻结）
  是 `pkg/cardmsg/` 的**权威镜像**，白名单一变必须同步改文档以保镜像忠实。
- 客户端渲染（web/Android/iOS 三端加 date/number/time 控件 + 内联校验 UX）是 P3-3 的
  真实大头成本，但**不在 octo-server 仓库范围**；本任务只交付服务端「线上已承载客户端
  所需」的那一半。

## Load-bearing list

- **error-response / wire-contract** — octo/v2 发送期白名单是对外校验契约；扩容会改变
  「哪些 AC 元素被接受」的可观测行为。必须保持：octo/v1 携带新类型仍 400（越级拒绝）、
  发送期错误码不变、提交期错误仍统一折叠为 `ErrMessageCardActionInvalid`。
- **trust-boundary** — `card/action` 上行 `inputs` 是不可信用户输入；新类型必须纳入
  `ValidateInputs` 的 fail-closed 声明校验（未声明键拒、类型不符拒），不得给出「已声明但
  不校验值」的天窗。`label`/`errorMessage`/`inlineAction`/`selectAction` 的 URL/动作面
  必须与现有 Input.* 同权威过 allowlist。
- **wire-contract（docs 镜像）** — `docs/card-protocol.md` §2 第 43 行 + §7 必须与
  `pkg/cardmsg` 白名单同步更新，否则镜像失真。此项需确认属 roadmap 已定调的「additive、
  无需 maintainer sign-off」范畴（对照 P3-1 明确需 sign-off）。
- **test** — 现有 `pkg/cardmsg/interactive_test.go` 的白名单门控（`TestValidateV2WhitelistGating`）
  与信任边界（`TestValidateInputsTrustBoundary` / `TestValidateInputsMultiSelect`）测试是
  回归基线，新类型须补对应用例，不得破坏既有断言。

## Out of scope

- **不升 `cardmsg.CardVersion`**（保持 `"1.5"`）；不引入 AC 1.6 任何元素/属性。
- **AC 1.6 `Data.Query` / `choices.data` 动态 typeahead**——独立大项，排在 Goal 4 之后。
- **服务端不强制 `isRequired` / `regex`**——保持现有信任边界原则（客户端 UX + bot 业务校验）。
- **客户端三端渲染器**（date/number/time 控件、内联校验 UX、`filtered` 下拉渲染）——不在本仓库。
- **不新增 errcode、不改 i18n locales、不新增/改端点、无 DB/迁移。**
- **不改 `expires_at` / 审批超时（P3-1）、WS 实时推送（P3-2）、可观测性（G1）** 等其他 P3 项。

## Acceptance

机器可校验：

- `go test ./pkg/cardmsg/...` 全绿；新增用例覆盖：
  - `Input.Number`/`Date`/`Time` 在 octo/v2 放行、在 octo/v1 拒（`ErrCardUnknownElement`）；
  - 三类新输入缺 `id` → `ErrCardBadShape`；帧内 id 重复 → 冲突拒；
  - 新类型的 `label`/`errorMessage` 携带 `javascript:` markdown 链接 → 拒（URL allowlist）；
  - `ValidateInputs`（只做格式/类型校验；min/max 区间**不**服务端强制 —— 即便声明了 min/max，越界的合法值也放行）：
    - `Input.Number` 非数字 / 非有限数（`NaN`/`±Inf`）→ `ErrCardInputInvalid`；合法有限数（含声明区间外）+ `""` → 放行；
    - `Input.Date` 非 `YYYY-MM-DD` → 拒；合法格式（含声明区间外）+ `""` → 放行；
    - `Input.Time` 非 `HH:MM` → 拒；合法格式（含声明区间外）+ `""` → 放行；
    - 未声明键仍 fail-closed 拒；
  - 风格容忍：`Input.Text style:"password"`、`Input.ChoiceSet style:"filtered"|"expanded"` 发送期放行。
- `docs/card-protocol.md` §2/§7 已列出 6 个输入元素，与 `pkg/cardmsg` 白名单一致。
- 既有 `pkg/cardmsg`、`modules/message` card_action 相关测试不回归。
- `go build ./...` 通过；`make i18n-extract-check` / `make i18n-lint` 无新增违规（预期：未触
  errcode，本项应无变化）。

## 待确认（human confirm）

1. **min/max 区间是否服务端强制** — **已定稿：不强制，下放 bot**（PR#556 review，maintainer 拍板）。
   理由:(a) AC 官方 schema 把 `Input.Number/Date/Time` 的 min/max 定义为「hint … may be ignored
   by some clients」——合规客户端可提交越界值,服务端强制会拒掉合法用户操作;(b) card/action 把
   所有 `ValidateInputs` 错误折叠成单一 `ErrMessageCardActionInvalid`(反枚举),越界拒绝让用户收到
   无从更正的笼统错;(c) 区别于 `Input.ChoiceSet` 的 choices(**构成性**约束,值不在其中即伪造)——
   min/max 是**建议性**约束,与 `isRequired`/`regex` 同类。故服务端只做格式/类型校验(有限数 /
   `YYYY-MM-DD` / `HH:MM`),区间交 bot 业务逻辑。
2. **`docs/card-protocol.md` 镜像修订**是否需 maintainer sign-off——roadmap 未把 P3-3 标为
   需 sign-off（区别于 P3-1），倾向按「additive、随代码同 PR 改文档」处理。

## Tier 1 追加（同 PR，PR#556 讨论后加入）

在同一 PR 内把 octo/v2 **展示元素**白名单补齐到 AC ≤1.5，新增 4 个展示元素（实测 adaptivecards.io
版本）：`ImageSet`(1.0) / `RichTextBlock`(1.2) / `Table`(1.5) / `ActionSet`(1.2)。纯展示类，octo/v1+v2
均放行；`ActionSet` 内的 `Action.Submit` 仍受 octo/v2 门控。每个元素覆盖：发送期校验（结构 +
URL allowlist + 递归节点/深度预算）、派发对称（`findSubmitInElements` 遍历 ActionSet.actions /
Table cells / ImageSet images / RichTextBlock inlines / TableRow 的 Submit，防死按钮）、plain 派生、
D12 清单 `elements` **及 `inputs`** 自动同步（displayElements/inputElements 单一权威 —— 元素粒度能力
探测两者都需，故 manifest 同时下发；PR#556 review 确认 `inputs` 在范围内，非 over-build）。

Acceptance（追加）：`go test -race ./pkg/cardmsg/` 覆盖四元素 v1/v2 放行、URL allowlist 拒
`javascript:`、结构错拒、Submit 派发对称、plain 派生；`TestDisplayElementsAuthority` 逐 fixture
守卫 displayElements↔校验器一致；修正既有 `TestValidateWhitelistRejections`（Table 实为 1.5 已支持，
替换为 Media/ToggleVisibility）。仍未支持（后续按需）：Media、ShowCard/ToggleVisibility/Execute、
模板绑定、AC 1.6。

Tier 1 review 加固（PR#556 review，head `7559c526`→`85baabdf` 多轮）：
- **flat-validated 子元素的 URL allowlist 绕过（P1，阻塞）—— 全类关闭。** octo 有若干「按位置约束、扁平
  校验」的子集合位置：`ColumnSet.columns[]`、`ImageSet.images[]`、`RichTextBlock.inlines[]`、`Table.rows[]`·
  `cells[]`、`FactSet.facts[]`。原逻辑对其中多数不钉子元素类型、也不递归其子树，于是伪类型
  （`{"type":"Container","items":[…]}`）或 typeless（`{"items":[…]}`）的「伪装容器」被当扁平叶子只校
  url/selectAction、其 `items` 子树永不走查 → 夹带 `javascript:` 绕过发送期 allowlist（违反「校验面 ≥ 渲染
  面」，PR#543 铁律）。三位 reviewer 分轮逐个揪出（ImageSet/RichTextBlock → typeless residual → Table）。
  **统一纪律 `checkConstrainedChild`**：每个约束子位置显式 type 必须匹配其契约类型（缺省放行，同 `column()`），
  且除该类型合法的那个子集合字段外不得携带任何 `childCollectionFields`。叶子（Image/TextRun/Fact）不许任何
  子集合；`TableRow` 只许 `cells`、`TableCell`/`Column` 只许 `items`。
- **派发面 == 校验面（P2）** —— `findSubmitInElements` 对 ImageSet/RichTextBlock/Table 子元素用**同一
  `childTypeMatches` 谓词**跳过校验期会拒的伪类型子元素，两面共用判定、结构上不可能漂移。
- `TestTier1MislabeledChildRejected`（伪类型 + typeless，覆盖 Table/Column/Fact）+
  `TestTier1DispatchSkipsMislabeledChild` 守卫；合规 Image/TextRun/Table 帧不受影响（`TestTier1ElementsAccepted` 仍绿）。
- **`TableRow.selectAction`（P2）补齐** —— 校验（`w.selectAction(row)`）+ 派发（`findSubmitInElements`
  读 `row.selectAction`）对称补上，使「每个节点的 selectAction 都过同一 allowlist」不留缺口（row 原为
  唯一漏网节点）。
