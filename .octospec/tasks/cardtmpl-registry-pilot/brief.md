---
type: Task
title: "Task: cardtmpl-registry-pilot"
description: L0 卡片消息基座(octo-card@1.0)最小骨架 + docs.access-request@0.2.0 pilot 迁移;NotifyReq 外壳不动;handoff 4 类制品入 testdata 作跨仓契约锁;schema 级字段错改走 400(不再降级)
tags: [wire-contract, trust-boundary, escape, markdown, url-destination, auth, i18n, testing, error-response]
timestamp: 2026-07-21T00:00:00+08:00
# --- octospec extension fields ---
slug: cardtmpl-registry-pilot
upstream: ""
source: self
---

# Task: cardtmpl-registry-pilot

## Goal

按 `docs/platform-card-base.md`(octo-card@1.0)交付**最小可用 L0 基座**,并以 `docs.access-request@0.2.0` 作为首张 L2a 卡验证契约完整性。范围严格收敛:

- **本 PR 落地(L0 最小骨架)**:`TemplateMeta` + `Template` 接口(3 方法)、`Registry`(Register/SetDefault/Freeze/Lookup/List/RegisteredForTest)、**`Registry.Render` 唯一渲染入口**(schema 校验 + view 路由 + Build + metadata 注入 + `cardmsg.Validate` 全流程)、assets 载入(embed.FS 读 manifest/schema/reports)、conformance test 泛化框架、`pkg/cardtmpl` 自打的 metrics、docs/l2b-owners.md 空清单占位、pkg/cardtmpl/ext/ 目录规划占位。
- **本 PR 落地(L2a pilot 一张卡)**:`docs.access-request@0.2.0` 单视图 `pending`(profile `octo/v2`)完整走通 —— manifest/schema/reports/samples 入 testdata,pilot Template 实现 3 方法,注册期 Registry 自校验一致性,`modules/notify/card.go:336` 分支改走 `Registry.Render`;**outcome 视图(approved/rejected/`octo/v1`)不在本次范围**(理由见 Out of scope,呼应 handoff 里 outcome 由 finalizer 生成,与 ingress 通路解耦)。
- **本 PR 明确不做**:`CardUpdater` 更新接口、`/v1/message/card/templates` 公开端点、统一 callback envelope、L2b 通道、非 pilot 模板迁移、`${}` 引擎、`NotifyReq` 外壳变更。这些按 `docs/platform-card-base.md` §15 拆到后续 PR。

**唯一对外行为变更**:docs access-request 请求的 **schema 级字段错**(如 avatar 非 https、excerpt 超长)从"降级为文本 + `delivered:[uid]`"改为**"400 `ErrNotifyCardInvalid`,零投递"**(决策 C1)。base 字段错(`api.go:287-300`)与 render 级错(cardmsg.Validate/marshal 失败)行为不变。

## Background

现状(2026-07,已逐行核实,与上一版 brief 不同的关键事实):

- **回调路由真实模型**:`cardactiondispatch.NotifyCapability` 只有 `{SenderUID, Owner}`(`registry.go:79-82`);`CanNotify` **只服务 `ApprovalCard`**(`approval_card.go:20`、`api.go:410`);docs 通路由 `notifyCapabilityAllows` kind 分流(`api.go:399-416`),不经过 `CanNotify`。因此本 PR 引入**正交类型 `TemplateActionContract{Owner,ActionType}`**,与 `NotifyCapability` 无耦合,与 `cardactiondispatch.RouteSpec` 三元组 `{sender_uid, owner, action_type}` 对齐。
- **docs callback route 已 fail-close 校验**:`main.go:521-523` 启动时 `registry.Resolve(NotifyBotUID, "docs", "access_request.decision")` 缺则直接失败。本 PR 的 ActionContract 一致性 test 以此为锚点。
- **fallback policy 真实位置**:早期 guard(`card.go:287-300`)对基础字段错已经返回 `errNotifyCardInvalid`→`api.go:307-318` 映射为 400;**只有 builder 内部 `buildErr`**(`card.go:342-345`)被降级文本吞掉。决策 C1 只收敛这一段,复用现成 `errcode.ErrNotifyCardInvalid`→400 通道,**不新增 errcode**。
- **metadata 现状**:`buildMetadata`(`resource.go:262`)只写 `webUrl`+`variant`+`source`,**无** `protocol` 与 `template.{id,version}`;本 PR 由 `Registry.Render` 强制注入这两个新字段。
- **build metrics gap**:`internal/carddispatch/metrics.go` 只在 `Sender.Send` 后统计(`dispatch.go:35`),看不到 Registry.Lookup/Build 阶段。本 PR 在 `pkg/cardtmpl` 自打(见 A18)。
- **schema 校验库已核实**:`santhosh-tekuri/jsonschema/v6` v6.0.2,新增 transitive 仅 `regexp2`(`golang.org/x/text` 已是直接依赖 v0.32.0),详见 A17。
- **契约文档权威位置**:`docs/platform-card-base.md`(与本 PR 同批合入)。`.context/plan/platform-card-base.md` 是 stub,禁止分叉。

## Load-bearing list

字节/行为兼容边界(不变):

- **`NotifyReq` 外部形状**(`modules/notify/model.go:14-24`)`{Payload, Card, DocsCard, ApprovalCard}` 四选一——不动字段/tag/binding;`DocsCardFields`(model.go:73-87)不动;调用方零改动。
- **`cardactiondispatch.NotifyCapability`/`routeKey`/`RouteSpec`/`CanNotify`/`notifyCapabilityAllows`**——全部不动;本 PR 引入正交的 `TemplateActionContract`,不侵入现有 capability 模型。
- **docs callback route 契约** `{sender_uid=notification, owner=docs, action_type=access_request.decision}`(`main.go:521`、`docs_action.go:141-142`)——不动;pilot ActionContract 精确一致。
- **`cardtmpl.BuildDocsAccessRequestCard` 公开签名**(`docs_action.go:85`)——保留为 **legacy wrapper**(内部仍 marshal 完整 AC + 注入 metadata,供**未走 Registry.Render 的存量调用者/测试**兜底),不删除、不改签名;pilot Template.Build 调**导出**的 `cardtmpl.BuildDocsAccessRequestBodyWithLang(lang, ...) (body, actions, deepLink, err)`(只产业务片段),再由 pilot 自行组装 `BuildResult`(含按 lang 本地化的 `Source`)。
- **`cardmsg.ValidateInputs` + hidden `deny_reason` input id + action id 常量**(`docs_action.go:16-24`)——不动值;interaction report 引用这些常量。
- **`cardmsg.ProfileV1/V2` + `AcceptedProfiles`**(`profiles.go`)——不动;pilot pending view profile == `ProfileV2`。
- **`docsApprovalCardsEnabled()` gate**(`card.go:336`)——不动;pilot 只接管 gate 开启后的 `access_requested` 分支。
- **docs-notify fallback policy**(`card.go:342-345`)——**决策 C1 定向修改**:builder 内 schema 级字段错从"降级文本"改为"返回 `errNotifyCardInvalid`(→400)";render 级错沿用降级。api.go:307-318 映射链不动。
- **`escapeMarkdown`/`truncateRunes`/`requireHTTPS`/`labelsForLanguage`**(`resource.go:284-340`)——语义不动,**留在 `resource.go`**(未拆 helpers.go);`requireHTTPS` 改为委托新导出的单一真源 `cardtmpl.AbsoluteHTTPSURL`(render.go),现有 `_test.go` 回归。
- **`i18n.OutboundLanguage` 语义**——不动;通过 `BuildEnv.Lang` 由调用点提前解析后传入(A10)。
- **现有测试全绿**:`pkg/cardtmpl/*_test.go`、`modules/notify/{card_test,card_action_test}.go`、`internal/{carddispatch,cardactiondispatch}/*_test.go`。

跨仓契约文档承接(下游,非本 PR):

- `.octospec/tasks/card-message-internal-dispatch/docs-notify-contract.md` 更新为 PR-2,引用 `docs/platform-card-base.md` + testdata 路径 + `metadata.octo.template`。

## Out of scope

- **`${}` JSON 模板引擎**——不引入;handoff `templates/*.tmpl.json` 只作设计侧参考,不入库。
- **`NotifyReq` 外部形状变更**——不新增 `Card *CardEnvelope`;方案 B。
- **其余 Template 迁移**——summary / docs.shared/commented / generic.approval / **docs.access.outcome(approved/rejected 视图)** 延后。outcome 特别延后的理由:①由 `standard_action_finalizer` 生成,与 ingress 通路解耦,单独 PR 迁更干净;②pilot 只验 `octo/v2` 交互档;③handoff 的 outcome view 视觉与现实现差异较大,视觉对齐属独立工作项。
- **`CardUpdater` 更新接口**(`docs/platform-card-base.md` §8)——本 PR 不实现,现有 `pkg/cardrevision` + finalizer 通路继续用;等 outcome 迁移时一起做。
- **`GET /v1/message/card/templates` 公开端点**(§9)——本 PR 不实现;`Registry.List()` 内部可用但不 wire 到 HTTP。原因:公开端点涉及能力发现权限、per-owner 可见性、i18n 文案透出,单开 PR 更稳。
- **统一 callback envelope**(§7)——本 PR 不实现;现有 `POST /v1/message/card/action` 入参结构不动,由后续独立 PR 演进,pilot 交互链路走现网既有格式。
- **L2b 业务自定义卡片通道**——不开放。本 PR 仅引入 `docs/l2b-owners.md` 空清单占位 + `pkg/cardtmpl/ext/README.md` 目录规划占位;**不接 `ext.` owner 前缀校验入生产链路**(即 Registry 注册期只白名单 L2a owner,`ext.` 分支代码可存在但仅 test 覆盖,不 wire 生产)。L2b 开放条件见 `docs/platform-card-base.md` §2.2-5。
- **`docs-notify-contract.md` 文档更新**——独立 PR-2。
- **锁定 body 视觉结构对齐 handoff `goldens`**——不做,理由见 A11。
- **客户端消费 `metadata.octo.{protocol,template}`**——独立迭代,本 PR 只保证写入。
- **docs-backend 侧调用改动**——零改动;`metadata.octo.{protocol,template}` 未知子字段客户端自然忽略;唯一行为变更是决策 C1(schema 级 400)。
- **`pkg/cardrevision`/`standard_action_finalizer` 迁移**——延后到 outcome PR。
- **运行时 kill switch(新增 env)**——不引入;依赖启动 fail-close + 镜像 revert + 现有 `OCTO_CARD_MESSAGE_ENABLED` / `OCTO_DOCS_APPROVAL_CARD_ENABLED`。

## Acceptance

### L0 基座骨架(pkg/cardtmpl 内)

- **A1** 新增 `pkg/cardtmpl/template.go`(严格对齐 `docs/platform-card-base.md` §5):
  - `type ID string` / `type ViewKey string` / `type State string`;
  - `type TemplateActionContract struct { Owner, ActionType string }`(pattern 匹配 `cardactiondispatch/registry.go:31-32`);
  - `type Source struct { Label, IconURL string }`;
  - `type ViewSpec struct { WireProfile string; States []State }`;
  - `type TemplateMeta struct { ID; Version, Protocol string; Views map[ViewKey]ViewSpec; ActionContract *TemplateActionContract; Manifest json.RawMessage; InputSchema *jsonschema.Schema; Source Source; stateIndex map[State]ViewKey; interactions map[ViewKey]InteractionReport }` + `Meta.ViewFor(state) (ViewKey,bool)` + `Meta.Interaction(view) (InteractionReport,bool)`;
  - `type InteractionReport / DeclaredAction / DeclaredInput / DeclaredToggle`(字段与 §4.3/§5 完全一致);
  - `type BuildEnv struct { WebLoginURL, Lang, SpaceID string }`(**无** `Locals`);
  - `type BuildResult struct { Body, Actions []any; Variant, DeepLink string; Source *Source }`;
  - `type Template interface { Meta() TemplateMeta; Build(ctx, state State, fields json.RawMessage, env BuildEnv) (BuildResult, error); FallbackText(state State, fields json.RawMessage, lang string) (string, error) }` **仅 3 方法**。

- **A2** 新增 `pkg/cardtmpl/registry.go`:
  - `Register(t Template, assets embed.FS, root string)`:载入 `<root>/manifest.json`(反序列化到 `TemplateMeta.Manifest` + `Views` + 计算 `stateIndex`,state 跨 view 重复 → **注册期 panic**)、`<root>/contract/data.schema.json`(`jsonschema.Compile`,失败 → panic)、`<root>/reports/<view>.interaction.json`(每个 v2 view 必读,缺失 → panic)、`<root>/samples/<sample>.json`(注册期 self-check 用);dup `{id,version}` → panic;
  - `SetDefault(id, version)` / `Freeze()`(Freeze 后 Register/SetDefault → panic) / `Lookup(id, version)` (未注册返 `ErrTemplateUnknown`) / `List() []TemplateMeta` / `RegisteredForTest() []Template`;
  - **owner 白名单校验**(§2.2-1):内部维护 L2a owner 集合(`docs`/`summary`/`notify`/`action`),`Meta.ActionContract.Owner` 不在集合内 → panic;`ext.` 前缀分支代码存在但**只跑测试,不 wire 生产**(见 Out of scope L2b 条目);
  - 并发安全:Freeze 后 Lookup 无锁并发。

- **A3** 新增 `pkg/cardtmpl/render.go`(实现 §5 `Registry.Render` 唯一入口):
  ```
  1. Lookup(id, version) → Template + Meta
  2. Meta.InputSchema.Validate(fields) → 失败返 ErrFieldsInvalid(typed)
  3. Meta.ViewFor(state) → view,未命中返 ErrStateUnknown
  4. Template.Build(ctx, state, fields, env) → BuildResult
  5. 校验 BuildResult.DeepLink 非空且绝对 https,否则返 error
  6. 组装 AC 顶层 { type, version=cardmsg.CardVersion, body, actions } +
     metadata: { webUrl=DeepLink, octo: { protocol="octo-card@1.0",
       template:{id,version}, variant, source } }
  7. cardmsg.Validate(doc, Meta.Views[view].WireProfile) → 失败视为 render_error 返 error
  8. json.Marshal 返回
  ```
  ErrFieldsInvalid / ErrStateUnknown / ErrTemplateUnknown 三类 typed error 由调用点区分处理(决策 C1)。

- **A4** 卡片渲染 helpers 与 metadata 注入落位(实际交付,取代早期"新增 helpers.go"设想):
  - `escapeMarkdown`/`truncateRunes`/`labelsForLanguage` 等**留在 `resource.go`**,未新增 `helpers.go`(避免无谓搬动+回归风险);
  - "必须为绝对 https URL"的判定提炼为**唯一导出真源** `cardtmpl.AbsoluteHTTPSURL`(`render.go`),`resource.go:requireHTTPS` 与 `renderCore` step5 均委托它,保证 preflight 与 Build 零缝隙(R3-1);
  - 不提供 `FinalizeCard` 导出函数:metadata 注入由 A3 `renderCore` 内部完成;`resource.go`/`docs_action.go` 直接构造完整 AC 的老函数保留各自 metadata 逻辑作为 legacy 分支,不被新链路调用。

- **A5** 新增 `pkg/cardtmpl/metrics.go`(§13):Prometheus counter `cardtmpl_build_total{template_id, version, view, result}`,`result` 枚举 `ok|fields_invalid|state_unknown|template_unknown|render_error`。由 `Registry.Render` 内部打点。

- **A6** 新增 `docs/l2b-owners.md` 空清单(格式见 `docs/platform-card-base.md` §2.2-1)、`pkg/cardtmpl/ext/README.md` 目录规划占位(L2b 未开放,不放代码)。

### L1 单卡契约 + L2a pilot Template

- **A7** 提交 handoff 4 类制品到 **pilot 子包**:`pkg/cardtmpl/docs_access_request/handoff/docs.access-request@0.2.0/` (每卡自持 embed;Go 的 `//go:embed` 拒绝 `.`/`..` 和跨父目录,所以每张 L2a 卡在自己子包内 embed 自己的 handoff):
  - `manifest.json`(补 `owner:"docs"` / `actionType:"access_request.decision"` / `views.pending.states:["pending"]` / `protocol:"octo-card@1.0"` / `sourceLabel:"文档"` 五个字段,handoff 原样本缺,**在提交时 backfill**;**R1-#2 修复后 result view 从 manifest 移除**,pending 是唯一注册视图;approved/rejected 待 outcome PR 以 `0.3.0` 新版本发布);
  - `contract/data.schema.json`(**R2-#4 修复后** avatarUrl pattern 补 host,所有 string 字段补 maxLength;`state` enum 收敛到 `["pending"]`);
  - `samples/pending.json`(handoff 原样;`approved.json` / `rejected.json` 已删,配合 result view 移除);
  - `reports/pending.interaction.json`(**R1 修复后** action id/dataKeys/inputIds 与 Go 常量 `DocsApproveActionID`/`DocsDenyActionID`/`DocsDenyReasonInputID` + `baseData` 严格一致,而非 handoff 原样);
  - **不提交** `templates/*.tmpl.json`(不引入 `${}` 引擎)、`goldens/*.card.json`(理由 A11)、`README.md`。
  Registry 加载时校验 `pending.states=["pending"]` 独占;v2 view 缺 report → **注册期 panic**(R2-#3 fail-close)。

- **A8** 新增 pilot Template `pkg/cardtmpl/docs_access_request/template.go`:
  - `Meta()` 返回构造好的 `TemplateMeta`(注册期由 Registry 组装完成后传入,Template 只持引用);
  - `Build(ctx, state=StatePending, fields, env)` 内部:
    - Unmarshal `fields` 到私有 struct(schema 校验已在 Registry.Render 前置完成,此处只做类型转换);
    - 调新增私有函数 `buildDocsAccessRequestBodyWithLang(env.Lang, docID, requestID, spaceID, content, actions, env.WebLoginURL) (BuildResult, error)`——把现有 `cardtmpl.BuildDocsAccessRequestCard` 逻辑抽出,**只**产生 `body[]` 与 `actions[]` 与 `DeepLink`,不 marshal 顶层、不写 metadata;
    - `Variant := "docs.access_requested"`(与 `card.go:463` 现值一致);
    - 返回 `BuildResult{Body, Actions, Variant, DeepLink, Source: &src}`,其中 `src = sourceForLang(env.Lang)` 按语言本地化(zh-CN"文档"/en"Docs"),覆盖 Meta.Source 从 manifest.sourceLabel 载入的中文默认值(F5);
  - `FallbackText(state, fields, lang)` 复用 `modules/notify/card.go:buildDocsFallbackText` 逻辑或抽到 helper;
  - `ID()`/`Version()`/`Protocol()`/`ActionContract()` 语义等价从 `Meta()` 派生,不额外方法。

### 生产链路迁移(pilot 唯一动的路径)

- **A9** `modules/notify/card.go:336-338` 的 `access_requested && docsApprovalCardsEnabled()` 分支改为:
  ```
  fields := mapDocsCardFieldsToJSON(card)  // 新增映射函数,DocsCardFields → schema JSON
  doc, err := cardtmplRegistry.Render(ctx, "docs.access-request", "",
      cardtmpl.StatePending, fields,
      cardtmpl.BuildEnv{WebLoginURL: n.webLoginURL, Lang: lang, SpaceID: req.SpaceID})
  ```
  **不再直接调 `cardtmpl.BuildDocsAccessRequestCard`**(legacy wrapper 保留但脱离新链路)。gate 关时行为不变(走 `buildDocsCard` v1)。

- **A10** 抽 `BuildDocsAccessRequestBodyWithLang(lang string, ...)` **导出**实现于 `pkg/cardtmpl/docs_action.go`(pilot Template 与 legacy wrapper 共用),`i18n.OutboundLanguage(ctx)` 调用从原 `BuildDocsAccessRequestCard` 顶层上移(wrapper 保留 ctx→lang 解析后转调;新链路由 modules/notify 调用点解析后经 `BuildEnv.Lang` 传入)。保留公开 `BuildDocsAccessRequestCard(ctx, ...)` 作**legacy 薄 wrapper**:内部拼装完整 AC + 注入 `metadata.octo.{webUrl,variant,source}`(**不写 protocol/template**,legacy 分支不承担新 metadata 责任),给未走 Registry 的存量测试/调用者兜底。中英文各加一条 pilot Template Build 等价回归(同输入不同 lang → 文案切换,body/actions 结构一致)。

- **A11** **迁移前后自输出字节等价基线**(取代锁 handoff golden;实际实现见 `modules/notify/card_via_registry_baseline_test.go`):
  - **不落 `.golden` 文件**:同一测试内先跑 legacy `buildDocsAccessRequestCard` 拿 pre-migration 输出作基线,再跑 `Registry.Render` 拿 post-migration 输出,两侧用递归 sorted-key **canonical JSON** 序列化后 `bytes.Equal` 断言;
  - 比较前从新链路输出剥除**三个**明确"仅新链路才有"的白名单差异:`metadata.octo.protocol`、`metadata.octo.template`(§5 强制注入)、`actions[Action.OpenUrl].id=="view_document"`(pilot 按 interaction 契约加,legacy 无 id);
  - 任何**其它**新增/顺序/别名漂移都会使字节不等而失败。基线来自 octo-server 自身,不锁 handoff 视觉。

- **A12** `docsApprovalCardsEnabled()==false` 时行为不变,`card_test.go` 相关测试全绿。

### 错误行为(决策 C1)

- **A13** pilot Build 前置的 schema 校验失败 / Registry.Render 返回 `ErrFieldsInvalid` / `ErrStateUnknown`,在 `card.go` 分支转换为 `errNotifyCardInvalid` → api.go:307-318 映射为 **400 `errcode.ErrNotifyCardInvalid`,零投递,不降级**。**不新增 errcode**。新增 test:
  - 传 `actor_avatar_url` 非 https → 400,`delivered:[]`,无文本兜底;
  - 模拟 render 级错(mock `cardmsg.Validate` 返回 error) → 沿用降级为文本,`delivered:[uid]`,`cardtmpl_build_total{result=render_error}` +1;
- **A14** `Registry.Lookup` 未命中(理论不该发生):作为**内部错误**→ 500 `internal error`(api.go:317-318),不伪装成成功文本、不当客户端 400。

### Conformance test(泛化,§4.3 全部四条断言)

- **A15** 新增 `pkg/cardtmpl/conformance_test.go`,遍历 `RegisteredForTest()`,对每张 Template 每个**已注册 v2 view**断言(pilot 只跑 pending view):
  - **A15a** samples 通过 `Meta.InputSchema.Validate`;
  - **A15b** `Registry.Render(state)` 无错;产物通过 `cardmsg.Validate(doc, wireProfile)`;
  - **A15c 交互契约完整锁**(修上一版遗漏):
    ① 产物 `Action.Submit.id` + `Input.*.id` 集合 **完全等于** `reports/<view>.interaction.json` 的 `actions[].id` + `inputs[].id` 并集(任一漂移即失败);
    ② 声明集合中每个 `Input.*`(**无论 `isVisible` 值**)必须出现在产物 body 中——`isVisible:false` 的 `deny_reason` hidden input 也不例外;
    ③ 每个 `Action.Submit` 的 `data` key 集合 **完全等于** report 声明的 `dataKeys`;
    ④ 每个 `Action.Submit` 的 `associatedInputs` 值与 report 声明一致(`"auto"`/`"none"`);
  - **A15d** `metadata.octo.template.id==string(Meta.ID) && .version==Meta.Version`;
  - **A15e** `metadata.octo.protocol == "octo-card@1.0"`;
  - **A15f** `metadata.webUrl` 为绝对 https。

- **A16 ActionContract 三方一致性 test**:对 pilot 断言:
  ① `Meta.ActionContract == {Owner:"docs", ActionType:"access_request.decision"}`;
  ② `Registry.Render` 产物里每个 `Action.Submit.data.owner`/`.action_type` == ActionContract;
  ③ 构造含 `{sender_uid=notification, owner=docs, action_type=access_request.decision}` 的 `cardactiondispatch.RouteSpec`,`registry.Resolve(NotifyBotUID, "docs", "access_request.decision").Kind == ResolutionCallback`(与 `main.go:521` 同源)。

### 工程门禁

- **A17** 新增依赖 `github.com/santhosh-tekuri/jsonschema/v6`(v6.0.2,Apache-2.0;draft 4/6/7/2019-09/2020-12,覆盖 handoff draft-07 + `additionalProperties:false` + `allOf/if-then`;层级化错误输出便于字段级信息记录)。新增 transitive 仅 `github.com/dlclark/regexp2`;`golang.org/x/text` 已是直接依赖(go.mod v0.32.0)。`go mod tidy` 后 vendor 干净。PR 描述交代淘汰选项(v5 / gojsonschema / qri-io / 手撸)及 v6 在 `boon` 分支(import path 认 `/v6`,不影响使用)。
- **A18** `go test ./pkg/cardtmpl/... ./modules/notify/... ./internal/carddispatch/... ./internal/cardactiondispatch/... -race -cover` 全绿;coverage 不低于变动前基线。
- **A19** `golangci-lint run ./pkg/cardtmpl/... ./modules/notify/...` 无新警告;`gofmt`/`goimports` 干净。
- **A20** `make i18n-extract && make i18n-extract-check && make i18n-lint` 通过(本 PR 不新增 errcode,预期无 delta)。

### 观测与回滚

- **A21** `cardtmpl_build_total{template_id, version, view, result}` 由 `Registry.Render` 打;不复用 `carddispatch.Metrics`。label 基数 = 注册表大小(Template.ID 硬编码),天花板 <20,无爆炸风险。
- **A22** 启动 fail-close:composition root 注册 pilot Template 后 `Freeze()`;`Registry.Lookup("docs.access-request","")` 与 schema `Compile` 任一失败 → 启动 panic(与 `main.go:521-523` 现有 docs route fail-close 一致)。init 期 schema 语法错无 env 挽救,回滚 = 镜像 revert;本 PR 拆成 "L0 骨架 + pilot 迁移 + 契约文档" 一个可 revert 单元。

### Rollout 前置(说明,非本 PR 交付物)

- **A23** 本 PR 合并后 PR-2(`docs-notify-contract.md` 更新)才引用 testdata 与 `metadata.octo.{protocol,template}` 契约字段。
- **A24** docs-backend/octo-web 无需跟版;唯一对外行为变更是 C1(schema 级 400),PR 描述向 docs-backend 明示并确认其 retry/filtered 状态机可承受。
