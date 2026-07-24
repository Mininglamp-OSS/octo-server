---
type: Task
title: "Task: cardtmpl-json-template-engine"
description: 给 cardtmpl 基座加一条 JSON 模板渲染通路(E1)—— 让卡片以纯 handoff 制品(.template.json + data.schema + reports + goldens)注册运行,不必手写 Go Build();以 ai.reasoning-process@0.1.0 的 goldens 为验收对照。
tags: [cardtmpl, wire-contract, trust-boundary, escape, testing]
timestamp: 2026-07-23T10:38:52Z
# --- octospec extension fields ---
slug: cardtmpl-json-template-engine
upstream: (self · roadmap E1)
source: self
---

# Task: cardtmpl-json-template-engine

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

给 `pkg/cardtmpl` 基座新增**第二条运行时渲染通路 —— JSON 模板通路(roadmap E1)**:一张卡
只要提供 handoff 制品(`manifest.json` + `contract/data.schema.json` +
`templates/<view>.template.json` + `reports/<view>.interaction.json` +
`goldens/<sample>.card.json` + `samples/`),就能注册进 Registry 并被
`Registry.Render` 渲染出 type-17 信封,**无须为它手写 Go `Template.Build()`**。

产物三块:
1. **Adaptive Card Templating 求值器(Go)** —— 支持这批 handoff 实际用到的
   ACT 子集:`${field}` / `${obj.path}` 绑定、`"✦  ${title}"` 字符串插值、
   `$data: "${phases}"` 迭代 + `$index`、`${if(cond, a, b)}` 三元、
   **类型化绑定**(整串 `"${traceExpanded}"` → 真布尔,`"${statusTone}"` → 真枚举串)。
2. **通用 `jsonTemplate` Template 实现** —— 满足现有 `Template` 接口(`Meta`/`Build`/
   `FallbackText`),`Build` 按 state 选 view 的 `.template.json`、用**已过 schema
   校验**的 data 展开、返回 body;不写死任何单卡布局。任意卡复用同一实现。
3. **Registry manifest JSON-view 装配** —— 解析 `views[].template` / `views[].samples`
   (当前被丢弃),注册期加载模板、跑 goldens 一致性 self-check(fail-close)。

验收硬对照:`ai.reasoning-process@0.1.0`(见
`.octospec/tasks/cardtmpl-json-template-engine/fixtures/`)—— 引擎对每个 sample
展开后必须与对应 `goldens/*.card.json` **canonical-JSON 逐字节相等**(metadata 注入前)。

## Background

- 契约 `docs/platform-card-base.md` §1 早写明"版本化模板(L1)双模式(Go / JSON)",
  但 §1 同时标注 **"JSON 引擎 = E1 未实现,当前仅 Go 在用"**。roadmap `E1` = 本任务。
- 现状(已核对代码):
  - `Template` 是 **Go 接口**,现有 5 张 L2a 卡(docs.access-request / docs.commented /
    docs.shared / summary.completed / summary.failed)全部**手写 Go `Build()`** 拼 body,
    共享 `assembleResourceCardBody`。
  - `Registry` 加载 `manifest.json`(schemaVersion 2 / views / wireProfile)+
    `contract/data.schema.json`(draft-07,`santhosh-tekuri/jsonschema/v6`,**可复用**)+
    `reports/<view>.interaction.json`(→ `InteractionReport` / `ActionContract` /
    `ActionView` 路由),但 **从不读 `.template.json`** —— 各卡 manifest 里的
    `template` 字段是给前端/SDK/golden 的参考制品,运行时靠 Go 复刻。
  - `renderCore` 管线:`InputSchema.Validate` → `tmpl.Build` → 注入 `metadata.octo`
    → `assertActionContract` → `cardmsg.Validate`(组件白名单 / URL https / 上限 /
    interaction 门)。JSON 通路必须**接进同一条管线**,不得旁路白名单与上限。
  - `manifestFile.Views` 结构体只有 `WireProfile` + `States`,**没有 `Template` /
    `Samples`** —— 第一处要补。
- 触发场景:业务方交付了 `ai.reasoning-process` —— 一张 **bot 主动产出、流式更新的
  推理进度卡**(纯 JSON 制品、零 Go 代码)。state 流转
  reasoning→answering→completed/stopped/error(同一 message_id,靠 `ReplaceView`
  整-view 重渲染)。**产品决策(2026-07-23):推理卡不含任何操作行为(去掉 `停止`/
  `重试` 等 `Action.Submit`),只保留 `Action.ToggleVisibility`(展开/收起推理明细)。**
  → 该卡因此是 **octo/v1 纯展示 + 本地折叠**卡(ToggleVisibility 是 v1 本地动作、无
  服务端回调),**不是 octo/v2**。它就是奔这条 JSON 引擎来的(布局之丰富 —— accent
  header / `$data` 阶段循环 / 折叠面板 —— 而非交互,才是上引擎的理由),也是本任务的
  黄金验收 fixture。**排序:引擎先行,再翻译落地这张卡**——引擎以**当前交付的 handoff
  原样**(含 Submit、octo/v2)当 conformance oracle(覆盖面更全);"去 Submit、降
  octo/v1" 的产品决策属下游"翻译推理卡"任务,非引擎前置(见 Risks D5)。
- **下发原语(已核对)**:去掉 Submit 后**无任何服务端回调**——不需要 owner /
  action_type / RouteSpec / `reasoning_id` 路由 / `/v1/bot/events` 回流。下发退化为
  memory `card_message_v2_web_gate` 里**已验证的"波 A"**:bot 发 v1 卡
  (`bot_api/send.go` → `cardmsg.Validate`)+ 流式 `edit`/`ReplaceView` 刷进度
  (`bot_api` 持 `carddispatch.CardMutator`),服务端 ✅ + web ✅ 早已打通。**与 C3 /
  L2b / 动态 owner 彻底脱钩。**

## Load-bearing list

<!-- 触及的既有契约/行为。tag 与 .octospec/rules/_index.yaml inject_when.touches 对齐。 -->

- **wire-contract** — `Registry` manifest 加载 / `views` 结构 / schemaVersion 2:
  新增 `views[].template` + `views[].samples` 解析;JSON-view 缺 `template` →
  注册期 panic(与现有 v2-view 缺 reports 同为 fail-close)。
- **wire-contract** — `Template` 接口不变:新增的是一个**实现**(`jsonTemplate`),
  不是改接口签名;现有 5 张 Go 卡零影响,全部既有测试须继续绿。
- **wire-contract / escape / trust-boundary** — `renderCore` + `cardmsg.Validate`:
  JSON 通路展开出的 card 必须走**同一** `cardmsg.Validate`(组件白名单 / 上限 /
  interaction 门 / URL 正向白名单)。**信任分界**:`.template.json` 是 L0 审查过的
  可信制品;`${}` 代入的 **data 值是调用方不可信输入**,须与 Go 路径同规格做
  markdown escape + rune 上限,`${detail}` / `${title}` 等不得成为注入面。
- **wire-contract (C1)** — data.schema 校验:data 不过 `contract/data.schema.json`
  → `ErrFieldsInvalid`(调用方翻 400,零投递),与现有 Go 卡 C1 一致;引擎不得在
  schema 失败后仍做部分渲染。
- **wire-contract** — `InteractionReport` / `ActionView`:推理卡**无 `Action.Submit`**
  (仅本地 `Action.ToggleVisibility`),故**不涉及 owner/action_type/回调路由**。引擎仍须
  self-check:模板里 `Action.ToggleVisibility` 的 `targetElements`(`trace_panel`/
  `collapsed_panel`)在展开后的 card 里真实存在(防模板/report 漂移)。若将来注册**带
  Submit 的 v2 JSON 卡**,才需接 `ActionContract` 派生 —— 本任务按 v1 toggle-only 实现,
  但引擎的 report 加载/校验路径应对 Submit 视图保持兼容(不硬编码"无 Submit")。
- **wire-contract** — `metadata.octo.{protocol,template,variant,source}` 注入仍是
  `Registry.Render` 的职责(goldens **不含 metadata**,正好印证注入点在引擎之外)。
- **wire-contract** — L1 冻结纪律(§2.1):`<id>@<ver>` 发布即冻结;JSON 制品同规矩,
  引擎不得在运行期改写模板。

## Out of scope

<!-- 本任务刻意不碰的东西。 -->

- **bot 流式下发通路(独立后续任务,非本任务)** —— 推理卡去掉 Submit 后是 **octo/v1
  纯展示 + 本地折叠**卡,下发**无任何服务端回调**:退化为 memory `card_message_v2_web_gate`
  里**已验证的"波 A"**——bot 发 v1 卡 + 流式 `edit`/`ReplaceView` 刷进度。下发侧仅剩
  轻量 wiring(bot 侧渲染已注册卡的路径 + 主动 `ReplaceView` 暴露给 bot_api),
  **无 owner / action_type / RouteSpec / `/v1/bot/events` 回调**。本任务只做"注册 +
  渲染出正确 card",不接生产下发。
- **进度更新用 `ReplaceView` 而非 `Append`** —— reasoning→answering→result 是同一
  message_id 的整-view 重渲染(`ReplaceView`,#641 已有语义),**不走** Append 进度帧,
  正好绕开 G10(Append 无生产调用者、`card_seq` 源未定)。接流式是上条的后续任务。
- **多语言模板选择** —— 本 fixture 文案(`显示 / 隐藏推理` 等)**内嵌在 `.template.json`
  字面量**里,`defaultLocale: zh-CN` 单语言。多 locale 模板/字符串表机制(与 G4 卡片
  i18n 折叠相关)留后续,本任务只支持单 defaultLocale。
- **迁移现有 Go 卡到 JSON** —— 5 张 Go 卡保持 Go 实现;本引擎是**增量并存**的第二通路,
  不动 legacy。
- **能力发现端点 B1/B2** —— 不 wire HTTP;`Registry.List()` 已在,端点另行。
- **完整 ACT 规范** —— 只实现这批 handoff 实测用到的子集(见 Goal),不追 Microsoft
  Adaptive Card Templating 全集(`$when` 复杂谓词、`$root`、`TemplateHost` 函数库等);
  遇到子集外语法 → 注册期报错,不静默产出错卡。
- **翻译/落地推理卡本身** —— 把 `ai.reasoning-process` 作为**产品卡**注册 + 应用"去
  Submit、降 octo/v1"决策 + 波 A 下发,是**引擎之后的下游任务**,不在本任务。本任务
  只把它的 handoff 当引擎的 conformance fixture(原样,验字节)。

## Acceptance

<!-- 尽量机读:测试 / 断言 / 可复现门禁。 -->

- **goldens 一致性(核心验收)**:新增 conformance 测试,把 fixture
  `ai.reasoning-process@0.1.0` 注册进临时 Registry;对每个 `samples/<s>.json`,
  按其 `state` 解析 view、展开对应 `templates/<view>.template.json`,结果与
  `goldens/<s>.card.json` **canonical-JSON 逐字节相等**(metadata 注入前的裸 card)。
  5 个 sample(reasoning/answering/completed/stopped/error)全过。
- **state→view 映射有据**:引擎从 `manifest.views[].states` 解析 state→view。
  fixture 现未声明 `states` —— **实现期由本任务补齐 fixture manifest 的 `states`**
  (reasoning/answering→active,completed/stopped→result,error→error;照
  docs.access-request@0.3.0 的 views 写法),引擎侧不得靠 sample 文件名猜 view。
- **模板 AST 注册期缓存**:进度卡一轮推理会**高频重渲染**(每帧多一段 phase)。
  引擎须在**注册期**把 `.template.json` 解析成可复用 AST(与 InputSchema 编译同期),
  运行期每帧只重新 bind data,不得每次重新 parse JSON;有基准/断言防止运行期 parse。
- **注册期 fail-close**:JSON-mode view 缺 `template` 文件、模板含白名单外组件、
  `ToggleVisibility` 目标 id 在模板里不存在、reports 与模板 action 漂移 → **注册期
  panic / error**,不推迟到运行期。
- **C1 零投递**:data 不过 `data.schema.json` → `ErrFieldsInvalid`;有独立单测覆盖
  (缺必填 `reasoningId`、`traceExpanded`/`traceCollapsed` 违反 oneOf 互斥、error
  态缺 `errorTitle`/`errorMessage`)。
- **走同一 L0 关卡**:展开后的 card 过 `cardmsg.Validate`;构造一个"模板代入超长
  `${detail}` / 含 markdown 元字符"的用例,断言输出被 escape + 截断到与 Go 路径
  同一 rune 预算,且仍过 `Validate`(不是靠模板作者自觉)。
- **`${}` 求值器单测**:字段绑定、嵌套路径、`$data`+`$index` 迭代、
  `${if($index==0,'None','Large')}` 三元、类型化布尔绑定(`isVisible:"${traceExpanded}"`
  → 真 `bool` 而非字符串)、字符串插值、缺字段行为(定义为 error 或空,二选一并测)。
- **交互契约(v1 toggle-only)**:推理卡无 `Action.Submit` → 无 owner/action_type/回调
  断言。仅断言 `Action.ToggleVisibility` 目标(`trace_panel`/`collapsed_panel`)
  self-check 命中展开后 card 的 element id;manifest views `wireProfile` 为 `octo/v1`,
  展开产物**不含任何 Submit/Input**(若含 → 注册期 fail-close)。
- **零回归**:`go test ./pkg/cardtmpl/... ./modules/notify/...` 全绿;现有 5 张 Go 卡
  的字节等价基线不变;`Registry.Freeze`/`Lookup`/`Render` 并发语义不变。
- **门禁**:`gofmt` / `go vet` / 相关 `golangci-lint` 干净;若触及错误响应门面则
  `make i18n-lint`(本任务预计不碰 errcode)。

## Risks / decisions(确认阶段)

**已定(实现期照做,非开放问题)**

- **D1 · fixture 补 `states`** —— fixture manifest 缺 `views[].states`,由本任务补齐
  (reasoning/answering→active,completed/stopped→result,error→error),不回制品作者。
- **D2 · 无回调,与 C3/owner 彻底脱钩** —— 推理卡去掉 `停止`/`重试` 等 Submit
  (产品决策 2026-07-23),仅留本地 `Action.ToggleVisibility`。故**无 owner /
  action_type / RouteSpec / 回调**,与 C3(动态 owner)、L2b 通道无任何关系。views
  全为 `octo/v1`。
- **D5 · 引擎与卡片解耦:fixture 原样当 oracle,v1 变体属下游** —— 排序确定为
  **引擎先行,再翻译/落地推理卡**。故本引擎任务的 conformance **以当前交付的 handoff
  (v2、含 `reasoning_stop`/`reasoning_retry` Submit + toggle + `$data` + `${if}` + 样式)
  原样为字节 oracle** —— 它覆盖的 ACT 面更全,正好把引擎验到底(引擎必须支持 Submit
  模板,为将来 v2 JSON 卡留路)。**"去 Submit、降 octo/v1" 是"翻译推理卡"那步的产品
  决策**(见下游任务),**不是引擎的前置**,不必为建引擎而重出 fixture。引擎对 v1/v2、
  Submit/toggle 一律无关心,只做"任意合法 handoff → 其 goldens"。
- **D3 · 求值器自研受控子集** —— `go.mod` 无 ACT 库、无现成 Go 实现;自研面小可审、
  无第三方信任面,而非引未审计依赖。注册期解析成 AST 缓存(见 Acceptance 性能条)。
- **D4 · ACT 子集冻结 + L0 演进** —— 以这 3 个模板实测语法冻结子集;子集外语法注册期
  报错;扩子集走 L0 PR + 卡片发新版本(§2.1 冻结纪律)。
- **D6 · 转义模型:引擎字面代入,`cardmsg.Validate` 兜底(spike 已证实)** —— goldens
  由 Forge **字面代入**(`run_sql`/`funnel_definition.sql` 的下划线未转义);故引擎
  **不做 markdown 转义**(否则破坏字节等价)。安全防线是 `cardmsg.Validate` —— 其正向
  URL allowlist **覆盖 TextBlock markdown 链接**(`cardmsg.go:101`)+ 元素白名单 +
  节点/深度上限,注入的坏链接/HTML/非白名单元素被拒整卡(调用方绕不过的边界,符合
  trust-boundary)。仅装饰性 `*`/`_` 透传(agent 产出、低危)。jsontmpl 仍保留注入式
  `EscapeFunc`(可测、未来某卡可 opt-in 转义),但推理卡及默认走 identity + Validate 兜底。

**需你拍板 / 知悉**

1. **排序确认:引擎先行,再翻译推理卡** —— 本任务 = 通用 JSON 引擎,拿当前 handoff
   原样当 conformance oracle(v2 也无妨,覆盖面更全)。**翻译/落地推理卡另起下游任务**,
   届时才应用"去 Submit、降 octo/v1"产品决策 + 注册 + 波 A 下发。请确认这个切分。
2. **下发是独立后续任务(已大幅缩小)** —— 无回调后,下发 = bot 发 v1 卡 + 流式 edit,
   走 memory 里已验证的"波 A",仅剩轻量 wiring。请确认下发另起任务这个切分。
3. **客户端渲染:逐元素 feature-detect,非阻塞门** —— 客户端**已按 AC 渲染,且 v2 交互
   都活**(2026-07-23 用户截图:`docs.access-request` 的 `Action.Submit` 允许/拒绝 +
   头像/角标/FactSet 在客户端完整渲染)。推理卡是 **v1(展示 + 本地 toggle,无 Submit)**,
   比 docs 卡还轻,渲染面完全被覆盖;唯一要 feature-detect 的是 `Action.ToggleVisibility`
   + 容器 `style`/`bleed`(标准 AC 1.5),由 `GET /v1/bot/card/profile` 的
   `elements`/`actions` 清单回答(`DisplayActions()` 已含 ToggleVisibility)。**不存在
   "v2 全局渲染门"。** 若某客户端不认 toggle,按 profile 降级(默认展开、藏折叠按钮),
   而非整卡不可交付。
4. **多语言** —— fixture 文案内嵌模板字面量、单 `defaultLocale`。多 locale(与 G4 卡片
   i18n 折叠同源)留后续,本任务只支持单 defaultLocale(见 Out of scope)。

## 实现记录(2026-07-23)

引擎已落地并全绿(`go test ./pkg/cardtmpl/... -race`,gofmt/vet 干净):

- `pkg/cardtmpl/jsontmpl/`:ACT 求值器(`expr.go`)+ 展开器(`expand.go`),cover 86.1%。
  子集 = 单标识符绑定 / `$index` / `if($index==N,a,b)` / 字符串插值 / 类型化整串绑定 /
  `$data` 迭代;子集外语法 → error。
- `pkg/cardtmpl/json_template.go`:通用 `jsonTemplate`(实现 `Template`+`metaSetter`,注册期
  AST 缓存)+ `RegisterJSON(assets, root)`。`FallbackText` 复用 `cardmsg.BuildPlain`。
- `render.go` D7:`DeepLink` 可选(空 → 跳过 https 校验 + 省略 `metadata.webUrl`);既有卡
  一律带 DeepLink,行为不变(docs/summary conformance 全绿佐证)。
- 验证:5 张推理 golden canonical 字节等价(`jsontmpl/conformance_test.go`);极简 v1 fixture
  端到端过 renderCore + `cardmsg.Validate`;C1(schema 失败→ErrFieldsInvalid);D6 兜底
  (注入 `javascript:` markdown 链接 → Validate 拒 → ErrRenderFailed);fail-close(缺 template
  路径 → 注册期 panic)。

**诊断发现(交下游"翻译推理卡"任务)**:as-delivered 推理 handoff 现**无法原样注册**——
其 reports 按 **state 命名**(`reasoning/answering/….interaction.json`),而 Registry 约定按
**view 命名**(`active.interaction.json`)。去 Submit 降 v1 后 v1 视图**根本不需要 reports**,
此错配自然消解(与 D5 一致)。

**延后(非阻断,honest)**:
- **ToggleVisibility 目标 id 存在性 self-check** —— brief Acceptance 列了但本次未做。
  理由:悬空 toggle 目标是"按钮点了没反应"的低危展示问题(非安全),且 `cardmsg.Validate`
  已挡结构性问题;极简 v1 fixture 未用 toggle。留作 hardening follow-up(需遍历展开后 card
  收集 id vs toggle targetElements)。
- 推理卡作为**产品卡**注册 + 波 A 下发 = 下游"翻译推理卡"任务(见 Out of scope)。
