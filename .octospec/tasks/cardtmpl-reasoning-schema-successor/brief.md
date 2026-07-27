---
type: Task
title: "Task: cardtmpl-reasoning-schema-successor"
description: Publish a bounded ai.reasoning-process successor, keep 0.1.0 immutable, and cut Bot discovery/send to the successor without stranding legacy Registry edits.
tags: [card, cardtmpl, ai-reasoning-process, json-template, bot-api, wire-contract, trust-boundary, space, isolation, auth, testing]
timestamp: 2026-07-25T12:47:51+08:00
# --- octospec extension fields ---
slug: cardtmpl-reasoning-schema-successor
upstream: "roadmap E1c; follows PR #657 and PR #659"
source: self
---

# Task: cardtmpl-reasoning-schema-successor

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

发布 `ai.reasoning-process` 的有界 successor（建议
`ai.reasoning-process@0.2.0`），在不修改已发布 `0.1.0`、不改变卡片视觉和
`template-ref/v1` wire 形状的前提下完成三件事：

1. 为所有非枚举自由字符串补 `maxLength`，为 `phases`、每个 phase 的 `actions`
   补 `maxItems`，并在进入 JSON 模板展开前强制“所有 phase 的 actions 合计不超过
   13”这一聚合约束；
2. 在 Registry 中同时注册冻结的 `0.1.0` 和 successor，并把该模板的 Registry
   default 切到 successor；
3. Bot capability 与新 send 只广告/接受 successor，同时保留对已存在
   `0.1.0` Registry 卡的同版本 edit 能力，避免 catalog 切换直接冻结在途卡。

本任务是 Model A 生产启用前的 schema/cutover 硬化，不负责打开生产开关。
`OCTO_BOT_CARD_ENABLED` 在生产仍保持 `false`；stop/retry 真实业务闭环仍由 roadmap
E1e 承接。

## Background

### 已交付基线

- PR #657 发布并注册了 `ai.reasoning-process@0.1.0`；该版本的五个 state、三个
  view、模板、actions、samples 和 goldens 已冻结。
- PR #659（squash `a8575cd2`）交付 `template-ref/v1` 的 capability、send 和 edit
  协议。Bot catalog 是显式授权边界，不等于 `Registry.List()`。
- `0.1.0` 已完成一次实验环境 Model A lifecycle 验证，但这不代表生产已启用，也不
  解除 schema hardening 与 stop/retry 业务闭环门槛。

### 为什么必须发新版本

`0.1.0` 的 `state`、`statusTone`、action `statusTone` 已由 enum 有界，
`statusGlyph.maxLength=2`；但以下 11 个自由字符串仍无 `maxLength`：

- `reasoningId`
- `title`
- `statusLabel`
- `timerText`
- `collapsedSummary`
- `progressText`
- `phases[].thought`
- `phases[].actions[].tool`
- `phases[].actions[].detail`
- `errorTitle`
- `errorMessage`

`phases` 与 `phases[].actions` 也只有 `minItems: 1`，没有 `maxItems`。现有 HTTP
body、JSON 展开、card node/depth 和最终 payload 上限仍会 fail-close，但病理输入可以
先通过 schema、消耗模板展开，再以笼统 render error 失败。调用方数据造成的确定性超限
应在 Build 前归类为 `ErrFieldsInvalid`，不能伪装成服务端 5xx。

平台 L1 冻结规则明确要求：同一 `id@version` 发布后，contract/samples/reports 不再
修改；需要收紧 schema 必须发布新版本，旧目录原样保留。因此禁止直接修补
`ai.reasoning-process@0.1.0/contract/data.schema.json`。

### producer 证据快照

cap 依据来自本机 OpenClaw Octo consumer checkout
`/Users/fangling/conductor/workspaces/openclaw-channel-octo/gaborone` 的
`src/reasoning-process.ts` / `src/card-render.ts`，快照 commit
`530bc2dcac4a09ce9fb488c1799e266b610d461c`。该分支尚不能视为已合并/发布；这里只把
它作为当前真实 producer 行为证据：

- `THOUGHT_MAX=280`，超长时 `slice(0, 280) + "…"`，实际输出上限为 281 个
  UTF-16 code units；
- `TOOL_NAME_MAX=80`，同样因追加省略号，实际输出上限为 81；
- `MAX_RENDERED_PHASES=6`；
- `MAX_RENDERED_ACTIONS=12`，是所有保留 phase 的 action 合计，而不是每个 phase 12；
- 参数摘要最多 64 后追加省略号（输出至多 65），错误摘要最多 120 后追加省略号
  （输出至多 121）；二者以 `" · "` 合并时当前 `detail` 上限为 189；
- `errorMessage` 走同一 120+省略号清洗，上限为 121；
- `reasoningId = sessionKey + ":" + runId` 当前没有 producer 本地 cap。

冻结的 `0.1.0` 五态 fixture 还提供了一个必须优先满足的兼容证据：
`completed` sample 的三个 phase 分别有 `3 + 7 + 3 = 13` 个 actions。successor 又要求
samples/goldens 与 `0.1.0` 字节一致，因此 aggregate 不能设为 12，否则 successor 会在
注册期 self-check 直接失败。successor 的平台兼容上限据此取 13；当前 producer 的
`MAX_RENDERED_ACTIONS=12` 仍是更严格的 producer 侧上限，无需放宽。

JSON Schema `maxLength` 按 Unicode 字符/rune 计数；JavaScript 当前按 UTF-16 code
unit 截断。由于 code point 数不会大于 code unit 数，下表给
`thought/tool/errorMessage` 预留追加省略号后的值，可兼容当前 producer。producer
后续若改为 code-point-aware 截断，也必须继续满足 successor schema。

### 建议 cap 表（待 D2 人工确认）

| 路径 | successor 建议上限 | 依据/边界 |
| --- | ---: | --- |
| `reasoningId` | 512 | 平台建议值；producer 当前无 cap。超限不得盲目截断导致 ID 碰撞，应 fail closed 或改用稳定摘要 ID |
| `title` | 64 | 单行标题产品上限建议；当前 producer 为固定短文案 |
| `statusLabel` | 32 | badge 短文案产品上限建议 |
| `timerText` | 128 | 单行耗时/计数文案产品上限建议 |
| `collapsedSummary` | 160 | 收起态单行摘要产品上限建议 |
| `progressText` | 160 | 活动态单行进度文案产品上限建议 |
| `phases` | 6 items | 当前 producer `MAX_RENDERED_PHASES` |
| `phases[].thought` | 281 | 当前 producer 280 + `…` |
| `phases[].actions` | 12 items/phase | 嵌套数组必须自身有界；另有全卡 aggregate 13 |
| 所有 `phases[].actions` 合计 | 13 items | 冻结 `completed` fixture 为 13；当前 producer 的 12 仍天然满足；draft-07 不能仅靠普通 `maxItems` 表达 |
| `phases[].actions[].tool` | 81 | 当前 producer 80 + `…` |
| `phases[].actions[].detail` | 192 | 当前最大 189（65 + 3 + 121），向上取整留 3 字符余量 |
| `errorTitle` | 64 | 单行错误标题产品上限建议；当前 producer 为固定短文案 |
| `errorMessage` | 121 | 当前 producer 120 + `…` |

上表中 producer 已有依据的值可以直接锁定；`reasoningId` 与几类固定/单行文案是本
brief 的平台建议值，不伪装成既有产品契约。D2 未确认前不进入实现。

### 聚合 action 上限不能只写 schema `maxItems`

draft-07 可以表达 `phases.maxItems=6` 和每个 `actions.maxItems=12`，但不能直接表达
“所有 phase 的 actions 总数不超过 13”。只写两个 `maxItems` 会允许最多 72 个
action；该输入虽有界，仍可能通过 schema 后撞上 `cardmsg.MaxNodes=200`，把调用方错误
误归类成 render failure。

最小落地方式是给 `RegisterJSON` 增加 opt-in 的模板级、Build 前语义校验 seam：

1. 先完成现有 JSON decode + draft-07 schema 校验；
2. successor schema 以 namespaced 机读扩展声明聚合约束，建议形状如下；该扩展是
   aggregate limit 的单一真源，不能只把 `13` 写在 Go 常量或 Markdown：

   ```json
   "x-octo-constraints": {
     "aggregateArrayLimits": [
       {
         "parentArray": "phases",
         "childArray": "actions",
         "maxTotalItems": 13
       }
     ]
   }
   ```

3. `RegisterJSON` 只识别上述窄扩展；缺字段、非正数、目标字段不存在/不是数组等配置错误
   在注册期 panic。没有该扩展的既有模板行为不变；
4. successor 的 validator 只读取已解析的 `phases[].actions`，累计超过扩展值即返回字段
   非法；
5. 该错误统一包成 `ErrFieldsInvalid`，计入既有 `fields_invalid`，不得进入模板展开；
6. handler/Bot API 不写 `ai.reasoning-process` 特判；所有 Registry 消费路径获得同一约束。

具体 Go API 可在实现中保持最小，但不能把聚合检查只塞进 send handler，或只依赖
`cardmsg.Validate` 的后置节点预算。

### catalog cutover 与在途编辑

E1b 的 catalog 当前用同一 ref 集合同时承担“广告给新消息”“允许 send”“允许 edit”。
如果简单把 `0.1.0` 替换成 successor，所有已经发出的 `0.1.0` Model A 卡都会失去 edit
能力；如果同时广告两个版本，当前 OpenClaw selector 因“存在多个兼容条目”会 fail
closed 回 Model B。

本任务采用两个显式、代码评审的集合：

- **advertised/send refs**：只有 successor；用于 capability 输出和新 send 授权；
- **edit-compatible refs**：`0.1.0` + successor；只用于 edit 前置授权。

legacy edit 仍必须在目标查询前先过 edit allowlist，随后继续核对 stored 顶层
`template_ref`、`metadata.octo.template`、Bot owner、Space/lifecycle 和 CAS。它只允许
“已有 `0.1.0` 卡继续以 `0.1.0` 编辑”，不能创建新 `0.1.0` 卡，也不能跨版本改写。

capability 只返回一个 successor 条目，保持现有 consumer 选择无歧义。`0.1.0` 继续在
Registry 注册，但不再广告，也不再接受新 Bot send。

## Load-bearing list

- **L1 冻结与版本身份（`cardtmpl`, `wire-contract`）** — `0.1.0` 的 manifest、schema、
  reports、samples、templates、goldens 必须零字节修改；successor 使用新目录和显式版本。
- **schema/语义前置校验（`cardtmpl`, `trust-boundary`）** — 每个自由字符串与数组有
  明确上限；聚合 action 约束由 schema namespaced extension 机读声明，在 JSON 模板
  展开前执行并映射为 `ErrFieldsInvalid`。
- **JSON 模板资源预算（`cardtmpl`, `trust-boundary`）** — 合法最大 fixture（6 phases、
  13 actions 合计、字符串均到 cap）必须仍低于 `cardmsg` 节点/深度/payload 上限；不能
  用一个必然 render 失败的 schema 上限冒充可用契约。
- **Registry 多版本（`cardtmpl`, `wire-contract`）** — `0.1.0` 与 successor 同时注册，
  默认版本切 successor；历史消息继续按 stored version 运行。
- **Bot catalog 授权（`bot-api`, `auth`, `trust-boundary`）** — capability/send 与
  legacy edit 使用两个最小 allowlist；两者都不是 `Registry.List()` 全量授权。
- **edit 身份与防枚举（`bot-api`, `space`, `isolation`, `auth`）** — legacy ref 必须先
  过 edit allowlist，再查询目标；stored provenance、owner、Space/lifecycle、CAS/revision/
  CMD sync 规则不变。
- **capability wire（`bot-api`, `wire-contract`）** — `templating.templates` 只广告一个
  successor；view/state/wire profile/submit action 集合与 `0.1.0` 保持一致，数组顺序仍只
  是确定性输出，不承载推荐语义。
- **错误契约（`wire-contract`, `i18n`）** — schema、字符串/数组 cap、聚合 cap 违规均
  复用 `ErrBotAPICardInvalid` 的 localized 400，零 send/edit；不暴露 schema path、cap
  内部或 catalog 成员关系。真实 composition failure 仍走 generic internal error。
- **部署与回滚（`bot-api`, `wire-contract`）** — 本任务不启用生产 gate；新旧二进制对
  successor edit 的能力非对称，必须显式记录回滚边界。
- **测试（`testing`）** — cap 边界、聚合约束、多版本注册、catalog 分离、send/edit
  零副作用及 race 回归均需自动化覆盖。

## Out of scope

- 修改或删除 `ai.reasoning-process@0.1.0` 的任何已发布制品。
- 改卡片布局、文案、五个 state、三个 view、wire/render profile、Submit/Toggle action
  ID/data keys 或 `template-ref/v1` 请求形状。
- OpenClaw consumer 实现、配置 schema、package/release 或默认启用；本任务只使用其
  当前源码作为 cap 证据。producer 对 `reasoningId` 超限的稳定摘要策略由 E1d 对齐，
  不在 octo-server PR 中跨仓修改。
- `reasoning_stop` / `reasoning_retry` 的取消、重试、active-run 注册表或成功状态写回
  （roadmap E1e）。
- 打开生产 `OCTO_BOT_CARD_ENABLED`、生产流量灰度或把既有实验 E2E 宣称为生产验收。
- 新 route、数据库表/迁移、rate limiter、错误码、i18n 文案、动态数据库 catalog、
  per-bot/per-Space 模板 ACL 或远程模板加载。
- 通用跨版本 message migration；edit 仍只能保持 stored `id@version` 不变。

## Acceptance

### successor 制品与冻结纪律

- 新增建议目录
  `pkg/cardtmpl/ai_reasoning_process/handoff/ai.reasoning-process@0.2.0/`；manifest
  `id` 不变，`version=0.2.0`。建议 `contractVersion=1.1.0`，最终值由 D1 确认。
- `0.2.0` 的 templates、reports、samples、goldens 与 `0.1.0` canonical/字节内容保持
  一致；允许差异仅限新 manifest 版本元数据和 data schema cap。
- `git diff origin/main --
  pkg/cardtmpl/ai_reasoning_process/handoff/ai.reasoning-process@0.1.0` 为空；不得通过
  “顺手格式化”改变冻结目录。
- Registry 同时能 Lookup 两个版本；default 显式为 successor；五个 state 在两个版本
  均保持原 view/profile 映射。

### schema 与聚合边界

- 11 个自由字符串均有上表确认后的 `maxLength`；enum 字符串继续由 enum 有界，
  `statusGlyph.maxLength=2` 不变；所有对象继续 `additionalProperties:false`。
- successor schema 含确认后的 `x-octo-constraints.aggregateArrayLimits`；注册期从该扩展
  读取 aggregate 13，malformed/unknown target fail closed。运行期不得另抄一个可能漂移
  的 magic number。
- 每个字段用 table-driven test 证明：恰好 cap 个 Unicode 字符通过，cap+1 返回
  `ErrFieldsInvalid`；至少包含 ASCII、CJK 和 astral emoji 用例，避免误判 rune/byte/
  UTF-16 语义。
- `phases=6` 通过、`7` 拒绝；单 phase `actions=12` 通过、`13` 拒绝。
- actions 合计 12 在单 phase 与跨 6 phases 两种分布都通过；合计 13 在跨 6 phases
  分布时也通过；合计 14 即使每个 phase 均未超过 12，也必须在 Build/Expand 前以
  `ErrFieldsInvalid` 拒绝。
- 合法最坏 fixture（6 phases、13 actions 合计、所有自由字符串恰到 cap）在 active、
  result、error view 均能完成 Registry render 并通过 `cardmsg.Validate`，不超过 node、
  depth 或 payload budget。
- 聚合 validator 的失败计入既有 `fields_invalid`，不计 `render_error`，且日志/响应不
  输出完整 caller data。

### conformance 与多版本注册

- successor 的 reasoning、answering、completed、stopped、error 五份 sample 均通过
  schema、注册期 self-check 与 canonical golden conformance。
- active/error 的 `submit_actions` 仍分别为 `reasoning_stop` / `reasoning_retry`，result
  为空；`inlineAction` 等完整 report-drift 遍历面不回退。
- 现有 `0.1.0` conformance 与 render tests 继续通过，证明新 validator 不被错误应用到
  legacy 版本或其他 JSON/Go templates。

### capability、send 与 edit cutover

- `GET /v1/bot/card/profile` 的 templating catalog 只含一个
  `ai.reasoning-process@0.2.0`；views/states/profiles/actions 与冻结版本一致；
  `enabled:false` 时仍返回同一完整 capability。
- successor 的 Registry send 成功并保留 server-authored `template_ref@0.2.0`；新 send
  请求 `0.1.0` 或其他未广告 ref 返回现有 card-invalid，dispatch 为零。
- successor 卡只能以 `0.2.0` 继续 edit；试图用 `0.1.0` ref 跨版本改写失败，且 revision/
  message-extra/CMD side effect 均为零。
- 一个已存、provenance 完整的 `0.1.0` Registry 卡仍可用 `0.1.0` ref edit；该 ref 不
  出现在 capability，也不能用于新 send。
- 不在 edit-compatible allowlist 的 ref 在目标 lookup 前 fail closed；现有防消息存在性/
  owner oracle 顺序不回退。
- raw Model B send/edit、Registry/raw total XOR、Bot ownership、Space/lifecycle、CAS、
  transient/revision 与 authoritative `plain` 行为全部保持兼容。

### 验证命令

- `go test ./pkg/cardtmpl/... ./modules/bot_api/... -count=1`
- `go test ./pkg/cardmsg/... ./internal/carddispatch/... ./internal/cardactiondispatch/... -count=1`
- `go test -race ./pkg/cardtmpl/... ./modules/bot_api/... -count=1`
- `go test -race ./internal/carddispatch/... ./internal/cardactiondispatch/... -count=1`
- `go build ./...`
- `go vet ./pkg/cardtmpl/... ./modules/bot_api/... ./internal/carddispatch/... ./internal/cardactiondispatch/...`
- `make i18n-extract-check`
- `make i18n-lint`
- `git diff --check`

需要 MySQL/Redis/WuKongIM 的测试按仓库 test environment 执行；若本地依赖不可用，必须
明确记录未执行项并以 CI 结果补齐，不能写成已验证。

## Rollout / rollback

1. 先以 `OCTO_BOT_CARD_ENABLED=false` 部署包含两个 Registry 版本和新 catalog policy
   的二进制，等待所有副本 rollout 完成；部署期间不允许生产 Model A 新 send。
2. capability 即使 gate 关闭也会展示 successor，这是既有 discovery 语义；consumer
   仍必须先检查顶层 `enabled`。
3. rollout 后可在隔离实验环境用 successor 重跑 reasoning → answering → completed/error
   baseline；该验证不打开生产 gate。
4. 回滚到 E1c 之前的二进制后，已发 successor 卡不能继续 edit，因为旧二进制未注册
   `0.2.0`。生产 gate 本任务始终关闭，因此不应产生生产 successor 在途卡；实验环境回滚
   时接受“最后成功帧冻结并告警”，恢复编辑应 roll forward。
5. 若部署前确认某环境已把 `0.1.0` Model A 当成受支持生产能力，或无法接受上述 rollback
   边界，则本 brief 的 rollout 假设不成立：必须暂停 cutover，改为两阶段兼容部署/独立
   active-version 开关后再实施，不能直接合并并启用。

## Decisions for human confirmation

- **D1 — successor identity：**建议模板版本 `0.2.0`、`contractVersion=1.1.0`；
  `0.1.0` 永久冻结并继续注册。
- **D2 — exact caps：**确认“建议 cap 表”的全部数值，尤其是目前没有上游产品约束的
  `reasoningId=512` 和短文案上限。未确认前不实现。
- **D3 — aggregate actions：**总 action 上限为 13（兼容冻结 `completed` fixture；当前
  producer 仍最多输出 12）；由 schema 的
  `x-octo-constraints.aggregateArrayLimits` 作为单一真源，Registry/JSON-template 在
  pre-Build 执行，拒绝 handler 特判、Go/JSON 双写和 post-render budget 兜底方案。
- **D4 — catalog policy：**capability + new send 只允许 successor；edit-compatible 集合
  同时允许 `0.1.0` 与 successor，以保留 legacy 在途编辑但禁止新建 legacy 卡。
- **D5 — one advertised version：**不同时广告两个兼容版本；当前 consumer 在多版本时
  fail closed，catalog 必须保持唯一选择。
- **D6 — immutable edit identity：**不新增跨版本 migration；每条消息从首帧到终态固定
  stored `id@version`。
- **D7 — existing public errors：**所有 cap/aggregate 违规继续使用现有 localized
  card-invalid 400，零副作用；不新增错误码或暴露 schema 内部。
- **D8 — production gate：**E1c 合并/部署不等于生产启用。生产 gate、E1d consumer
  release、E1e stop/retry 与跨仓生产级 E2E 仍是后续独立 go/no-go 条件。
