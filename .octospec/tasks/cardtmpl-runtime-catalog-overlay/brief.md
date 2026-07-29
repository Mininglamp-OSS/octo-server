---
type: Task
title: "Task: cardtmpl-runtime-catalog-overlay"
description: Deliver the dark-launched RuntimeCatalog overlay together with the OpenClaw Model A and reasoning-control production closure under one E3 PR-B milestone.
tags: [card, cardtmpl, runtime-catalog, database, cache, wire-contract, trust-boundary, auth, acl, bot-api, cross-repo, error-response, i18n, rate-limit, test, testing, observability, rollback]
timestamp: 2026-07-28T21:57:37+08:00
# --- octospec extension fields ---
slug: cardtmpl-runtime-catalog-overlay
upstream: "roadmap E3 PR-B; PR-A merged via PR #674@68e8134d; parent spec cardtmpl-runtime-catalog"
issue: 672
source: self
---

# Task: cardtmpl-runtime-catalog-overlay

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the PR-B
> implementation contract extracted from the accepted E3 parent brief. AI may
> draft it from the current code; a human confirms it before `/octospec-go`.
>
> This brief was created during the OctoSpec **Plan** phase. Implementation
> status and acceptance evidence are updated below during Verify/Finish.

## Goal

在 PR-A 已提供的 strict compiler、immutable version claim/artifact/audit store 和
super-admin validate/publish 基础上，增加一个默认暗置的 `RuntimeCatalog`：

1. 对 frozen built-in `Registry` 与 MySQL dynamic artifact 提供统一的 exact/default
   resolution 和 render/metadata/action-context 读取；
2. 用 MySQL 权威 active pointer + revision CAS 实现 activate、显式 target rollback 和
   单向 emergency block；
3. 用 entry/byte 双上限 compiled cache + `singleflight` 支撑多副本 lazy load，同时保证
   active/block 真相永不由本地 cache 决定；
4. 把 Bot template send/edit、notify Registry render、`CardUpdater` 和 message action-context
   从具体 `*cardtmpl.Registry` 迁到只读 runtime catalog 接口，保持现有 static 行为和 wire
   字节兼容；
5. 以默认关闭的 control/new-send gates 部署，证明多副本、重启、DB outage、rollback 和
   hot-cache block 行为；
6. 在同一个 E3 PR-B delivery milestone 下完成 OpenClaw E1d Model A consumer 产品化和 E1e
   `reasoning_stop/reasoning_retry` 真实业务闭环，并用 successor 跑联合跨仓 E2E；之后再由 PR-C
   接入 catalog grants、B1/B2 和首个非生产 dynamic producer。

PR-B 完成后代表“server 已具备受控加载、切换、回滚和阻断 dynamic artifact 的运行底座”，
且 OpenClaw 的 Model A/stop/retry 已具备真实业务闭环；它仍**不代表任意 Bot/业务方已获准发现
或发送 dynamic catalog template，也不代表生产开关已自动打开**。

“同一个 PR-B”在本 brief 中指同一 delivery milestone、同一 go/no-go 和联合验收。E1d/E1e 的
代码位于 OpenClaw/plugin/runtime，不在本 octo-server worktree；必须按仓库形成 companion PR，
不能伪装成一个 GitHub PR 能原子修改多个仓库。

## Background

### Verified baseline at plan time (2026-07-28)

- Parent contract：
  [`.octospec/tasks/cardtmpl-runtime-catalog/brief.md`](../cardtmpl-runtime-catalog/brief.md)。
- PR-A：GitHub PR #674 / Issue #669，已于 2026-07-29 squash merge 到 `main`，
  merge commit 为 `68e8134d`。其 required checks、`code-review` 与 current-head
  approvals 在合并前均已通过。
- PR-A 已有：
  - `card_template_version_claim`：static/dynamic exact-key 永久占位；
  - `card_template_artifact`：immutable canonical bundle/hash，并预留单向 block 字段；
  - `card_template_audit`：append-only publish audit；
  - startup static inventory reconciliation；
  - super-admin validate/publish API；
  - `cardtmpl.CompileJSONArtifact` 及进程级 bounded compile gate。
- PR-A 尚无 `card_template_activation`、activate/rollback/block store/API、dynamic resolver、
  compiled cache 或 runtime consumer wiring；published artifact 始终 inactive。
- 现有 runtime consumers 直接绑定具体 `*cardtmpl.Registry`：
  - `modules/bot_api/card_template_catalog.go` 的 static `AdvertisedSend/EditCompatible`、
    `Lookup` 和 `Render`；
  - `modules/notify/card_via_registry.go` 的所有生产 Registry render；
  - `pkg/cardtmpl.NewCardUpdater` 及 notify finalizer/mutate；
  - `modules/message.resolveRegistryCardContext` 的 `ActionView` + `Lookup().Meta()`；
  - package-global `DefaultRegistry()`。
- built-in `Registry` 在 `main.go:installCardTmplRegistry` 注册、设 default、`Freeze()` 后注入；
  module factories 随 `register.GetModules` 构造。PR-B 不能依赖无契约的 blank-import/init 顺序
  在 Bot/notify/message 已构造后再安装 overlay。
- Bot `template-ref/v1` 仍要求 caller 显式传 `{id,version}`；raw/template XOR、Bot owner、
  Space、channel、message lifecycle、CAS 和 `card_seq` 约束均已冻结，不因 RuntimeCatalog 改写。
- OpenClaw handoff：
  [`.context/handoff/openclaw-bot-template-consumption.md`](../../../.context/handoff/openclaw-bot-template-consumption.md)。
  当前仅证明 `0.1.0` experimental send/edit lifecycle；尚未证明 selector/reasoning lane/config/package
  已合并发布。stop/retry 当前只 warning + metric + queue ACK，明确没有取消或重试副作用。
- E1d/E1e 的权威运行态在 OpenClaw/plugin/runtime：`reasoning_id → active run/AbortController`、原始
  request/context、retry scheduling 和多实例恢复不在 octo-server 代码库。本 brief 将其纳入联合
  交付，但不虚构本仓可直接实现这些对象。

### Why this is a separate PR

PR-A 的安全边界是“可校验、可持久化，但永远 inactive”。PR-B 首次引入运行期 DB 读取、
active pointer、cache 和 emergency state transition，故障面从控制面扩展到消息 send/edit/action
热路径，必须独立评审、独立回滚并默认暗置。PR-C 再引入 producer grants 与发现面，避免把
“存在于 catalog”“成为 active”“被 producer 授权”三个动作合并成一个不可审计开关。

## Success definition for PR-B

> Server work-package status (2026-07-29): implementation and clean local
> integration/race/build/vet/lint verification are complete and tracked by
> Issue #672. PR-A merged through #674 and this branch has been rebased onto
> `main`; post-rebase CI and current-head re-approval remain pending. The
> OpenClaw E1d/E1e companions and joint release gate are not complete;
> production gates remain off.

- **Overlay 闭环**：static exact/default 零回归；dynamic exact 能从任意副本验 hash、编译并按
  server-authored metadata 渲染；双源或持久化损坏 fail-close。
- **状态闭环**：activate/rollback/block 使用 revision CAS，状态与成功 audit 同事务；block
  当前 active 时同事务 fallback 或 disabled。
- **消费闭环**：所有 production `DefaultRegistry()` 读取迁入统一 catalog 接口；static Bot、
  notify、CardUpdater 和 action-context 的现有行为、鉴权顺序及 wire 不变。
- **故障闭环**：cache 热/冷、两副本、重启、DB outage、compile failure、stale CAS 和 hot-cache
  block 均有自动化证据；不存在 silent fallback、未限内存或 goroutine/keyspace 泄漏。
- **暗置闭环**：默认配置不能 activate dynamic template，也不能让 dynamic template 进入业务
  new-send；PR-B merge/deploy 本身不会产生首张 dynamic 卡。
- **consumer 闭环**：OpenClaw 从实时 manifest 选择唯一兼容 successor、每消息固定 mode/version/
  card_seq，reasoning lane 生成有界且脱敏的 data；配置、package 和 release 证据完整。
- **control 闭环**：stop/retry 绑定 authoritative run/message/bot/Space，幂等、多实例和重启可恢复；
  只有真实 cancel/retry 成功后才写 `stopped` 或启动/展示新 reasoning。
- **联合闭环**：server + OpenClaw companion PRs 在同一版本矩阵完成 send→edit→completed/error→
  stop/retry、故障、回滚 E2E；任一 work package 未完成均不能把 E3 PR-B milestone 标 DONE。

## Load-bearing list

- **`trust-boundary`, `wire-contract`**：DB 中 canonical bundle 仍是不可信输入；每次 cache miss
  必须验 row identity、engine、SHA-256 并复用 PR-A strict compiler，最终仍走
  `renderCore` + `cardmsg.Validate`。不得直接反序列化成可执行 Template 或绕过 compile gate。
- **`auth`, `acl`, `bot-api`, `space`**：manager control 只认认证上下文中的 superAdmin；runtime
  request 的 principal/purpose/Space 必须由 server auth layer 构造。现有 Bot ownership、Space、
  sender、channel、feature gate 不得被 catalog existence/active pointer 替代。
- **`error-response`, `i18n`**：新增 manager/runtime 错误必须走 localized envelope；内部 DB/hash/
  compile cause 先记录脱敏日志，5xx code 设置 `Internal=true`。
- **`rate-limit`**：新增 manager GET/write routes 继续使用
  `AuthMiddleware → SharedUIDRateLimiter → superAdmin guard`；测试显式清 Redis UID bucket。
- **Static Registry freeze**：`Register/RegisterJSON/SetDefault/Freeze` 仍只属于 built-in
  composition phase；RuntimeCatalog 绝不 `Unfreeze` 或运行期修改其 entries/defaults。
- **Immutable identity**：`id@version` static/dynamic claim 永久唯一；artifact bytes/hash 不更新、
  不 hard-delete；same key 不以 source precedence 掩盖冲突。
- **Active semantics**：activation row 不存在、显式 active、显式 disabled 是三个状态；DB 查询
  失败不能被当作 row 不存在；历史 exact-version 永不重新解析 active。
- **Emergency block**：dynamic block 单向且 cache 不拥有 block 真相；已存 payload 仍可展示，
  后续 render/edit/action-context 必须拒绝并告警。
- **Bot compatibility**：`template-ref/v1` 封闭 wire、raw/template total XOR、static
  `AdvertisedSend/EditCompatible` allowlists、unlisted ref 在 target lookup 前拒绝、same-version edit
  provenance 校验全部保持。
- **Card update compatibility**：`CardUpdater.ReplaceView/Append` 的 authoritative Space、stored
  template identity、sender/lifecycle/CAS/card_seq 纪律保持；catalog 只替换模板解析依赖。
- **Action ingress compatibility**：template identity 只取 effective stored frame；ActionView、
  ActionContract 和 owner cross-check 必须来自同一个 exact resolved artifact，不能信 click body。
- **`test`, `testing`**：涉及 DB migration、cache concurrency、Bot/message/notify 热路径和多副本
  一致性，必须先补 RED fixtures，使用 bounded DB pool/cleanup，并跑 focused integration + race。
- **Observability/rollback**：指标 label 只能是有界 operation/source/result；日志与 audit 不写 token、
  bundle/schema/sample、card data、UID/Space 等高基数 label。kill switch 关闭后仍保留安全 rollback/
  block 与 historical reader。
- **Cross-repo Model A lifecycle**：manifest 必须动态选择唯一兼容版本；一条消息固定 Model A/B、
  `id@version` 与单调 card_seq；Model A 失败后不得 raw edit 覆盖 Registry-authored frame。
- **Reasoning control authority**：card_action 只证明用户意图已通过 server 校验，不证明业务成功。
  stop/retry 必须绑定 active run、message、bot、Space、operator 和原始请求，不能从展示文本重建 prompt。
- **Sensitive reasoning data**：`thought/detail` 只能是可展示、脱敏摘要，不记录 hidden chain-of-thought、
  system prompt、token、Cookie、Authorization、私有文档或完整工具输入输出。

## Architecture decisions

### D1 — Branch and dependency discipline

- PR-B 使用独立 Conductor workspace/branch，最初基于 PR-A accepted head `fcb9c524` stacked 开始；
  未向 PR-A 分支追加提交，避免使 current-head approvals 失效。
- PR-A 已通过 #674 squash merge；PR-B 已 rebase/retarget 到 merge commit `68e8134d` 后的
  `origin/main`，并必须重新跑完整门禁及取得 current-head approvals 后方可合并。
- Issue #669 只覆盖 PR-A。PR-B 实施前新建独立 issue，并让 PR body 的 Linked Spec 指向本 brief。
- 同一 E3 PR-B milestone 至少包含三个可独立回滚的 work package：
  1. octo-server RuntimeCatalog PR；
  2. OpenClaw/plugin E1d Model A companion PR；
  3. OpenClaw/runtime E1e reasoning-control companion PR（可与 2 合并仅当确属同仓、同 owner、同回滚单元）。
- 三个 PR 共享 milestone、版本矩阵、联合 E2E 和 go/no-go；禁止为了“一个 PR”复制/搬运跨仓源码或
  让任一 PR 在依赖未合并时独自打开生产开关。

### D2 — Composition-root installation, not module-order luck

- `main.go` 先构造并 freeze built-in Registry，再显式构造唯一的 process-level RuntimeCatalog，
  然后才允许 `register.GetModules` 构造 Bot/notify/message APIs。
- catalog DB/store、metrics、built-in Registry 和 action-route validator 由 composition root 显式
  注入；`modules/card_template_catalog.API` 与业务消费者复用同一实例，不得各建一份 cache/catalog。
- 禁止依赖 blank-import 排列或 Go `init()` 顺序完成 RuntimeCatalog 安装。
- 保留 `DefaultRegistry()` 仅供 registration、static reconciliation 和兼容测试；新增只读
  `DefaultCatalog()`（名字可机械微调）。测试未安装 overlay 时，`DefaultCatalog()` 可安全退回当前
  frozen Registry adapter；生产 overlay 缺失必须在构造阶段 fail-close，而非流量到达后 nil panic。

### D3 — Narrow runtime interface and typed request context

接口名可按 Go 风格微调，但行为至少覆盖：

```go
type ResolvePurpose string // new_send | historical_edit | action_context

type Principal struct {
    Kind    PrincipalKind // bot | internal_producer | system
    ID      string
    SpaceID string
}

type Catalog interface {
    RenderExact(ctx context.Context, req ExactRenderRequest) (map[string]any, error)
    RenderDefault(ctx context.Context, req DefaultRenderRequest) (map[string]any, error)
    MetaExact(ctx context.Context, req ExactRequest) (ResolvedMeta, error)
    ActionContext(ctx context.Context, req ActionContextRequest) (ResolvedActionContext, error)
}
```

- runtime interface 不暴露 `Register/RegisterJSON/SetDefault/Freeze`。
- `ActionContext` 一次返回 view + cloned metadata/action contract，禁止 caller 用两次独立 lookup
  拼出可能跨状态的结果。
- `Principal` 只从 Bot auth、internal capability 或 stored server state 构造；不得从 bundle、body、
  query 或任意 string context 自报。
- 未授权 raw exact resolver 仅作为 `pkg/cardtmpl`/catalog control/readiness 内部方法，不导出给业务模块。
- PR-B 保留现有 static producer policy；dynamic business purposes 在 grant store 尚未由 PR-C 接入前
  一律返回 typed not-authorized/unavailable，不能靠 handler 手写 allow 或 catalog existence 放行。

### D4 — Exact and default resolution

**Exact `id@version`**：

1. built-in inventory 命中时只从 frozen memory 返回；启动 reconciliation 已证明该 exact key 的
   persistent claim 为 static，正常热路径不为 static render 增加 DB query；
2. 非 static key 从强一致 MySQL 读取 dynamic claim + artifact minimal metadata；source 不符、artifact
   缺失、identity/engine/hash 异常均为 catalog integrity failure；
3. 即使 compiled cache 命中，每次 dynamic exact 仍读取权威 `blocked_at` 与 current hash，避免
   hot cache 绕过 emergency block；
4. cache miss 才读取 canonical bundle、重新计算 SHA-256、strict decode/compile，并再次核对编译出的
   ID/version/owner/protocol/engine/hash 与 row；
5. historical edit/action-context 始终使用 stored explicit version，不读取 active pointer，也不跨版本
   偷换；blocked 是唯一拒绝继续执行的安全例外。

**Default/new send**：

- startup reconciliation 尚未成功时，default/dynamic resolution 必须 fail-close，且
  `/v1/ready` 返回 503；此时仅 explicit static exact 在未证明 integrity collision 的前提下继续可用。
  这是 readiness 语义，不是 rollout gate。证明 integrity failure 后所有 catalog resolution 均关闭。
- 查询 `card_template_activation`；row 不存在时，只有 built-in Registry 已声明 static default 才可
  走 legacy static default；dynamic-only ID 返回 unavailable/unknown。
- `status=disabled` 必须拒绝，不 fallback 到 built-in default。
- `status=active` 按 row 的 explicit version 进入 exact resolution；DB error、timeout、malformed row
  全部 fail-close，不能当作 no-row。
- dynamic target 还受 new-send gate 和未来 PR-C grant 约束；PR-B 阶段业务 dynamic new-send 永远
  不成功。

### D5 — Persistence additions

新增 migration：

1. `card_template_catalog_capacity_guard`
   - 预置且只允许 `guard_key=1` 的单行事务锁；
   - 所有可能改变 active target 数量的状态事务在 activation row 之后获取该锁；
   - 锁内以 `card_template_activation` 的权威 `COUNT(*)` 判定 128 hard cap，不维护可漂移的冗余计数。
2. `card_template_activation`
   - `template_id` primary key（ASCII、case-sensitive）；
   - nullable `active_version` + bounded `status=active|disabled`；
   - monotonic `revision BIGINT`；
   - `updated_by/updated_at/reason/change_ticket`；
   - active version 以 composite FK/等价事务约束指向同 template ID 的 permanent claim；
   - `active` 必须有 version，`disabled` 必须无 version；不提供 DELETE。
3. 扩充 `card_template_audit`
   - 足以记录 previous/target version、previous/resulting revision；
   - `activate|rollback|block` 成功状态与 audit 同事务；audit 失败则状态不变；
   - 不保存 bundle/card data/token。

`card_template_artifact` 已有 block columns，PR-B 只允许第一次从 NULL 写入；不得改写或清空 block，
不得更新 canonical bytes/hash。

### D6 — State transitions and CAS

**Activate**：

- request 明确 target version、`expected_revision`、reason、change ticket；首次无 activation row 时
  只接受 `expected_revision=0`，成功创建 revision 1；并发首次写只有一个成功。
- target claim 必须存在；dynamic target 必须 artifact 完整、unblocked、engine supported、hash/compile
  valid；static target 必须存在于本镜像 frozen Registry。
- compile/route validation 不在长事务内执行；进入事务后按固定 lock order 重新核对 immutable identity、
  block 和 current revision，再 CAS + audit。
- 首次 activate 或 `disabled→active` activate 在 activation row 后锁 capacity guard；active count 已为
  128 时返回 typed state conflict，不能先成功写入再让新副本在 startup fail-close。

**Rollback**：

- caller 必须明确 target version，不由 server 猜“上一版”；target 必须能由 append-only audit 证明曾
  成功处于 active，避免把 rollback endpoint 当成绕过 control gate 的第二个首次 activate 入口。
  验证与 activate 相同，但 operation/audit 为 `rollback`。
- rollback 可指向 static 或仍 unblocked 的 dynamic version，不删除/清 cache 新版。
- `disabled→active` rollback 与首次 activate 使用同一 capacity guard/count；不能借始终可用的安全
  rollback 路径写出第 129 个 active target。

**Block**：

- 只支持 dynamic artifact，单向不可恢复；static 风险通过 binary/config rollback 处理。
- request 带 `expected_revision`，可选 explicit fallback version。若 target 是 current active：
  - fallback 有效且为 built-in static 或曾成功 active 的版本时，同一事务 block target + active 切
    fallback + revision+1 + audit；
  - 无 fallback 时，同一事务 block target + `status=disabled` + revision+1 + audit；
  - 不得因“没有已知良好 fallback”拒绝 block。
- target 非 active 时只写 block + audit，active pointer/revision 不变，但仍校验 caller 看到的 current
  revision，防止基于陈旧控制面快照操作。
- stale revision、already-blocked、invalid fallback 使用确定性 typed conflict；任何失败不得留下
  “artifact 已 block 但 active 仍指向它”的半提交。
- current-active block 无 fallback 的 `active→disabled` 也锁同一 capacity guard，确保它与跨 template
  activate/rollback 的增量检查串行；带 fallback 的 active→active 不改变 target 数量。
- deadlock/lock-wait 只按 PR-A 同类规则 bounded retry 整个 transaction；CAS conflict 不重试。

### D7 — Control APIs

PR-B 增加以下 super-admin routes（具体 Go handler 名可微调，wire 语义冻结）：

| Method/path | Request / response |
| --- | --- |
| `GET /v1/manager/card-templates/{id}` | 返回 source-aware versions、block 状态、active status/version/revision；不返回 bundle |
| `GET /v1/manager/card-templates/{id}/audit` | bounded cursor pagination，返回脱敏 activate/rollback/block/publish audit |
| `PUT /v1/manager/card-templates/{id}/active` | `{version, expected_revision, reason, change_ticket}` |
| `POST /v1/manager/card-templates/{id}/rollback` | `{target_version, expected_revision, reason, change_ticket}` |
| `POST /v1/manager/card-templates/{id}@{version}/block` | `{expected_revision, fallback_version?, reason, change_ticket}` |

- 路由链固定为 Auth → shared UID limiter → handler superAdmin guard；actor 只取 login context。
- path/body ID/version 冲突或 unknown fields fail-close；reason/change ticket 沿用 PR-A bounded/trimmed
  规则。
- 继续使用 `httperr.ResponseErrorL`：wire status 保持 D14 固定 400，真实 409/503 放在
  `error.http_status`；除非 maintainer 明确批准，不切 `ResponseErrorLWithStatus`。
- 2 MiB 仍是**完整 HTTP control envelope**上限；不扩成“2 MiB bundle + wrapper”。PR-A compiler 的
  2 MiB canonical bundle 是独立内部 ceiling，不承诺 HTTP 能提交恰好满 2 MiB 的 bundle。
- manager read pagination 默认 50、hard max 100；响应不得含 canonical bundle、schema/sample、token
  或 callback secret。

### D8 — Kill-switch semantics

- `OCTO_CARD_RUNTIME_CATALOG_CONTROL_ENABLED`：默认/缺失/非法均为 false 并 WARN。false 时禁止
  forward `activate`；validate/publish 与 manager read 保持 PR-A/运维可用，rollback 和 block 作为
  安全操作保持可用且继续审计。这里的“可用”指不受 control gate 阻断；publish 仍须通过 runtime
  readiness，并只接受 reviewed runtime-owner allowlist。rollback target 必须是 audit 可证明的
  prior-active version，不能借 rollback 首次启用从未 active 的 artifact。
- `OCTO_CARD_RUNTIME_CATALOG_NEW_SEND_ENABLED`：默认/缺失/非法均为 false。false 时 dynamic
  default/new-send 返回 typed unavailable；不得 fallback 到另一个 static version。
- 两个 gate 都不能关闭 static exact/default compatibility path。
- historical dynamic exact reader/edit/action-context 不得被常规 rollout gate 关闭；首张 dynamic card
  一旦发送，它就是 binary compatibility floor。PR-B 尚无 grants，因此这一能力只由 internal tests
  验证，不对业务开放。
- 部署必须先让所有 serving replicas 升级并完成 static reconciliation，再允许 control gate；混合
  binary rollout 时两个 gate 都保持 false。

### D9 — Compiled artifact cache

- cache key 为 `engine_contract + content_sha256`；不以 caller ID、active revision 或 grant 作为 key。
- 默认最多 **64 entries / 32 MiB canonical bytes**；可配置但 hard max 为 **256 entries / 128 MiB**。
  byte accounting 使用 retained canonical bytes；entry cap 同时限制无法精确计量的 schema/AST heap。
- 使用 LRU/等价有界淘汰；immutable artifact 不需要 TTL。不得缓存 active、grant、block 或 DB error；
  compile/hash/integrity failure 不写正 cache，也不做无限期 negative cache。
- 同 key cache miss 使用 `singleflight`，闭包内双检 cache；共享 load/compile 使用 catalog-owned
  10s deadline 和现有 process compile semaphore。每个 waiter 可按自己的 context 提前返回；后台共享
  work 最迟在 catalog deadline 终止，不产生无界 goroutine。
- 每次 dynamic request 先读 authoritative minimal metadata/block，再查 cache；命中热 cache 也不能
  少这一步。

### D10 — Runtime consumer migration

- 新增 static Registry adapter，使 `*Registry` 与 RuntimeCatalog 均实现相同只读接口；registration API
  不进入接口。
- 将所有非测试 production `DefaultRegistry()` 读取迁到 `DefaultCatalog()`：
  - Bot catalog construction/send/edit；
  - `modules/notify/card_via_registry.go`；
  - `CardUpdater`、notify action finalizer 和 sibling mutate；
  - message action-context。
- `modules/card_template_catalog` 的 static reconciliation 仍显式使用 built-in Registry inventory，
  不通过 overlay 反向枚举自身。
- Bot static `AdvertisedSend/EditCompatible` 继续在任何 target snapshot/DB lookup 前判定；catalog
  render request 额外携带 server-authored bot ID、purpose 和 authoritative Space。PR-B 不合并 dynamic
  capability，也不修改 `template-ref/v1`。
- notify/internal producers 使用代码内固定 principal ID；PR-B 只允许它们继续消费既有 static policy，
  dynamic 仍 fail-close，等待 PR-C grant。
- `CardUpdater.ReplaceView` 使用调用链已证明的 explicit version；`Append` 从 stored effective frame
  读取 provenance。不得把 update 重新解析为 current active。
- action ingress 从 effective frame 的 `metadata.octo.template` 取 exact ref，并通过一次
  `ActionContext` 获取 view/contract；route owner 仍与 stored Action.Submit.data 交叉校验。

### D11 — Interactive activation precondition

- dynamic interactive artifact 的 ActionContract 必须完整；activate 前确认至少存在一个 configured
  `cardactiondispatch.RouteSpec` 匹配 `(owner, action_type)`，否则 fail-close。
- RouteSpec 实际还以 sender UID 分区。PR-B 只做 catalog-level “至少有可配置路由”检查；PR-C 在给
  具体 producer grant 时必须再次验证该 authoritative sender 的 exact route/owner/Space。PR-B 不把
  BotPull fallback 当作 dynamic interactive activation 证明。
- active artifacts 在进程安装/启动校验时复核 engine、owner policy 与 interactive route capability；
  不可服务的 active contract 使 catalog-enabled process fail-close，不允许不同副本各自解释。
- startup active target 查询使用 `LIMIT 129` 探测 **128** 的 hard cap；static reconciliation 与列表读取
  各有 30s budget，每个 target 有独立 10s validation budget，不再共享一个会随 target 数量触发的
  30s 总预算。重试时重新读取并验证全量 target，避免沿用已变化 activation state 的部分进度。
- write side 使用数据库单行 capacity guard 串行化跨 template 的 active-count 增减；第 129 个 activate、
  `disabled→active` rollback 均拒绝，两个 template 并发争最后一个名额只能一个成功。读侧的
  `LIMIT 129` 继续作为 migration 外写入/人工改库漂移的 fail-close backstop。

### D12 — Failure semantics and observability

Typed runtime errors至少区分：unknown/not-active、disabled、blocked、not-authorized、CAS conflict、
compile busy/timeout、DB unavailable、catalog integrity。业务 facade 可合并外部表现以防枚举，但日志/
metrics 必须能低基数区分内部类别。

- DB unavailable：dynamic/default/control fail-close；static exact 继续成功。default 查询失败不能回退
  static。
- hash/identity mismatch、missing artifact、双源冲突：internal integrity error + alert；不 panic 单个
  request，不返回 bundle/cause。
- blocked：new send/render/edit/action-context 拒绝，保留 last successful payload 展示。
- 新增/补齐：
  - `dmwork_card_catalog_resolve_total{source,result}`；
  - `dmwork_card_catalog_cache_total{result=hit|miss|shared|evict}`；
  - 复用 operation/compile/db 指标；
  - cache retained entries/bytes 使用 gauge。
  - `dmwork_card_catalog_active_targets` gauge；catalog pending/integrity 必须让 `/v1/ready` 503，body
    只暴露 bounded dependency status，不暴露内部 cause。
- metric labels 不含 template ID/version/hash、actor UID、principal ID、Space ID；这些只可进入遵循现有
  脱敏/采样纪律的结构化日志或 append-only audit。

### D13 — E1d/E1e cross-repo companion delivery

#### E1d — OpenClaw Model A consumer

- 每次新消息前读取 `/v1/bot/card/profile`，同时校验 top-level enabled、templating supported、
  `wire=template-ref/v1`、view/state/profile 和完整 `submit_actions`；版本从 manifest 动态选择，不硬编码
  `0.1.0/0.2.0`，多个兼容版本无明确 semver policy 时 fail closed 回 Model B。
- 一条消息创建时固定 `{mode, template_id, template_version, next_card_seq}`；Model A send 只发
  `template_ref+state+data`，edit 只发同版本 ref/data/card_seq/transient。不得中途切版本或回退 raw
  Model B edit。
- 接入 reasoning lane，生成用户可展示、脱敏的 `thought/actions`；落实 successor 的 string、phase、
  per-phase action、aggregate action cap。`reasoningId` 采用稳定且无碰撞的 bounded ID，不能直接截断。
- 配置 schema 至少支持 `off|shadow|experimental`；PR-B 联合验收前 production default 仍 off，
  `OCTO_BOT_CARD_ENABLED=false`。完成 unit/integration、package、version、release note 和可回滚制品。
- send 超时不盲重发；edit 仅对完全相同 body+seq 做有界重放；D14 按 `error.code`/
  `error.http_status` 识别 conflict，不能只看 transport 400。

#### E1e — Real stop/retry semantics

- 建立 `reasoning_id → active run` 权威注册表，至少绑定 run/AbortController handle、message ID、bot ID、
  Space ID、origin request reference、owner instance、status、created/expiry；caller/card 展示数据不得成为
  authority。
- registry 必须支持多实例路由与 bounded TTL，定义进程重启后的恢复策略：可恢复 run 重新挂载；不可恢复
  run 明确失败/过期，不能把“找不到内存 AbortController”当取消成功。
- stop：校验 stored card event 的 message/bot/Space/reasoning ID 和 operator 权限，使用 event/reasoning
  identity 幂等；只有底层 cancel 已确认后才 edit 为 `stopped`。already-completed/already-stopped/not-found
  有确定且不泄漏的语义。
- retry：只从 server/plugin 持久化的原始 user request/context 重建，不从卡片 `thought/detail` 反推 prompt；
  有幂等 key、per-run/per-user 频率限制、并发 single-writer。明确 v1 创建**新 reasoning ID/new run**，
  新 run 创建成功后才发送/更新 reasoning 卡；失败保持原 error frame并返回可观察结果。
- queue ACK 只在业务动作进入确定终态（success、safe idempotent replay 或明确不可重试 rejection）后执行；
  transient infra failure 不得 ACK 丢失。业务成功、event ACK 和 card edit 分别可观测，不能合并成一个
  “handled”指标。
- stop/retry 的最终用户反馈、重试次数、超时、DLQ/补偿和告警必须有界；日志不记录 prompt、隐藏推理或
  tool secrets。

#### Joint contract

- octo-server 继续把 action identity/data 绑定到 effective stored frame，并按 event ID 提供 at-least-once；
  OpenClaw 用 event ID + reasoning operation key 实现幂等，不把 `client_token` 当业务幂等键。
- successor 跨仓 E2E 必须覆盖：reasoning→answering→completed、error→retry→new run、active→stop→
  stopped、重复/迟到/跨 Space action、owner instance crash、DB/Redis/HTTP 短暂故障、server catalog
  rollback 和 consumer package rollback。
- E1d/E1e companion 未合并发布或联合 E2E 未通过时，server RuntimeCatalog PR 即使绿灯也不能把
  E3 PR-B milestone 标 DONE，更不能打开 production Bot card gate。

## TDD implementation checkpoints

1. **RED — state/store**：先写 first-activation concurrent CAS、stale revision、rollback、active block
   with fallback、active block without fallback、non-active block、audit failure rollback fixtures。
2. **RED — resolver/cache**：写 static no-DB exact、dynamic cold/hot load、hash mismatch、hot-cache block、
   singleflight、entry/byte eviction、caller cancellation 和 DB outage fixtures。
3. **GREEN — narrow interface/static adapter**：先迁 static callers，证明现有 Bot/notify/update/action
   tests字节与鉴权顺序零回归。
4. **GREEN — dynamic overlay**：接 store + compiler + cache；无 grant 的所有 business dynamic purpose
   必须仍失败。
5. **GREEN — control state machine/API**：实现 manager detail/audit + activate/rollback/block、gates、i18n、
   metrics。
6. **RED→GREEN — multi-replica**：两个 RuntimeCatalog 实例共享 DB、各自独立 cache，覆盖 activate、
   rollback、block、restart 和 DB failure。
7. **RED→GREEN — E1d companion**：先锁 manifest selector、unknown action、random version、caps、
   per-message mode/version/seq、reasoning mapping、retry discipline，再接真实 profile/send/edit。
8. **RED→GREEN — E1e companion**：先锁跨 Space/owner mismatch、duplicate/late event、completed-vs-stop
   race、retry single-writer、instance loss/restart，再实现 active-run registry 与 control handlers。
9. **Joint E2E**：固定 server/plugin/runtime/client version matrix，跑正常、stop、retry、故障和回滚；
   任何 companion 只能在联合证据完成后标 DONE。
10. **Verify**：focused → race → integration → build/vet/i18n/diff；不得以 mock-only 单副本测试代替 DB
   transaction/multi-replica 证据。

## Out of scope

- PR-C 的 `card_template_grant`、grant/revoke API、principal/Space permission matrix。
- B1/B2 普通用户 discovery/export、visibility filtering、ETag 和 anti-enumeration。
- Bot dynamic capability 合并、按 template ID 单版本广告 guard、dynamic producer grant。
- PR-C grant 接入前必须先设计并持久化可信 producer provenance，使 action-context 能从 server-authored
  state 区分 `bot` 与 `internal_producer`。现有消息只保存 sender/template identity，禁止按 sender UID、
  template ID 或 owner 硬编码推断 principal kind。
- 首个非生产 publish→activate→send→Action.Submit→edit→rollback pilot，以及任何生产 go-live。
- `ext.*` L2b owner、自助/委托发布、动态 callback URL/secret/finalizer/RouteSpec 创建。
- per-Space active version；v1 仍只有 global pointer。
- hard delete/GC claim/artifact、修改 immutable bytes、unblock 已 blocked version。
- Redis pub/sub correctness、active/grant TTL cache、read-replica eventual consistency。
- JavaScript/WASM/Go dynamic artifact、新 engine contract 或修改 `${}` ACT 语法。
- 修改 `template-ref/v1` 或放宽 raw/template XOR；E1d/E1e 必须在既有 wire/event contract 上闭环。
- 用 PR-B merge 自动打开 `OCTO_BOT_CARD_ENABLED`、control 或 new-send 开关。

## Acceptance

### A. Composition and static compatibility

- [x] RuntimeCatalog 在 module factories/handlers 构造前由 composition root 唯一安装；测试证明交换
  blank-import/module 注册顺序不会让 Bot/notify 退回另一份 catalog。
- [x] built-in Registry 仍在 registration 后 `Freeze()`；不存在 runtime Register/Unfreeze 路径。
- [x] 所有非测试 production `DefaultRegistry()` runtime reads 已迁移；registration/reconciliation 除外。
- [x] static Registry adapter 与 RuntimeCatalog 对所有 built-in `id@version` 的 Meta、ActionView、
  render payload/profile/render_profile canonical 相等。
- [x] 现有 Bot `0.2.0` advertise/new-send、`0.1.0/0.2.0` historical edit、notify cards、
  CardUpdater 和 message action tests 零行为回归；unlisted Bot ref 仍早于 target lookup 拒绝。

### B. Persistence and transitions

- [x] migration 创建 activation table、FK/check/index，并扩充 audit；up/down migration 在 clean DB 通过。
- [x] absent row + `expected_revision=0` 并发 activate 只有一个 revision 1 成功；其余 deterministic conflict。
- [x] stale activate/rollback/block 不 last-write-wins；CAS conflict 不被 transient retry 吞掉。
- [x] activate/rollback target 必须存在、可服务、unblocked；dynamic target hash/compile/owner/route 校验通过。
- [x] rollback 只接受 audit 可证明的 prior-active target；从未 active 的 artifact 不能通过 rollback 绕过
  `CONTROL_ENABLED=false`。
- [x] active block + valid fallback 原子切换；无 fallback 原子 disabled；任何注入的 audit/commit failure
  都保持原状态。
- [x] non-active block 不改变 active revision；blocked columns 只写一次，无 unblock/update/hard delete。
- [x] state transition 与成功 audit 同事务，audit 可还原 actor、operation、target、old/new revision、reason、
  ticket，但不含敏感 payload。

### C. Resolution, cache, and failure behavior

- [x] exact static render 在 runtime DB outage 时成功且无 per-request catalog DB query。
- [x] default resolution DB error fail-close；no-row 才可使用 built-in default；explicit disabled 不 fallback。
- [x] startup pending/integrity 时 `/v1/ready` 返回 503；pending 时 static exact 可用但 default/dynamic
  fail-close，避免 replica 在 active-pointer truth 未建立时继续接收卡片流量。
- [x] dynamic cold load 从 DB 验 identity/engine/hash 后走 PR-A compiler；compile result 与同 bundle static
  registration canonical 相等。
- [x] dynamic hot cache 每次仍读取 block/hash metadata；block 后另一实例的 hot cache 首次请求即拒绝。
- [x] cache 同时满足 entry/byte cap、LRU eviction、singleflight shared compile、race safety；error 不污染正
  cache，无无界 goroutine/keyspace。
- [x] hash mismatch、missing artifact、source conflict 返回 typed integrity error并记录低基数指标；响应不泄漏
  bundle、owner/grant/existence 细节。
- [x] PR-B 中任意 Bot/internal producer dynamic new-send/edit/action-context 均因无 grant fail-close；package
  internal tests可验证 resolver，但不存在业务绕过入口。

### D. Control API, auth, and gates

- [x] manager routes 使用 Auth → SharedUIDRateLimiter → superAdmin；匿名、普通用户、Bot token 全拒，actor
  只取 login context。
- [x] strict JSON、unknown field、path/body identity mismatch、reason/ticket/revision bounds 均有 table tests。
- [x] `CONTROL_ENABLED=false` 时 forward activate 拒绝，但 validate/publish/read/rollback/block 语义按 D8
  保持；`NEW_SEND_ENABLED=false` 时 dynamic new-send 拒绝且不 fallback。
- [x] env 缺失/非法默认 false 并有 bounded WARN；开关不影响 static exact/default。
- [x] control-plane conflict/control-disabled/blocked/unavailable/error 均走 registered localized code；runtime
  disabled 保持 typed `cardtmpl.ErrRuntimeCatalogDisabled`，由 consumer facade 映射；5xx Internal + server log
  cause；source guard 无 legacy/raw error response。
- [x] manager detail/audit pagination hard cap 生效，响应不返回 canonical bundle/schema/sample/token/secret。

### E. Multi-replica and restart

- [x] replica A/B 使用同一 DB、不同 RuntimeCatalog/cache：A activate 后 B cold-load 同 version；A rollback 后
  B 下一次 default 立即读取旧 version，不被旧 hot cache 影响。
- [x] A block current dynamic version：B 即使 cache 热也拒绝 target；fallback/disabled 与 DB revision 一致。
- [x] process restart + empty cache 能恢复同一 active/disabled/block 行为；static claim/active integrity 不可服务
  时 fail-close。
- [x] mixed-version rollout 测试/运维断言证明两个 gates 默认 false；不以 Redis event/ACK 作为一致性条件。

### F. Observability and verification

- [x] operation/resolve/cache/compile/db metrics label 集合有界；无 ID/version/hash/UID/Space 高基数 label。
- [x] active-target hard cap 128 同时由写侧 capacity guard/count 与 startup limit+1 检测保护；第 129 个
  activate、disabled rollback 和并发争最后名额有真实 MySQL 回归；per-target 10s deadline、gauge 与
  startup readiness dependency status 生效。
- [x] activation/rollback/block failure、DB outage、integrity、hot-cache blocked request 有脱敏日志/指标。
- [ ] focused tests：`pkg/cardtmpl/...`、`modules/card_template_catalog/...`、`modules/bot_api/...`、
  `modules/message/...`、`modules/notify/...` 通过；变更包覆盖率不低于 80%。
  - 2026-07-29：上述 focused suites 全绿；核心 `pkg/cardtmpl`/catalog 覆盖率为
    80.6%/80.8%。但三个历史 consumer 大包的 whole-package baseline 仅
    Bot 48.3%、notify 68.7%、message 21.6%，因此该条按字面仍未勾选；不得用
    targeted-path tests/race 冒充 whole-package 80%。
- [x] targeted `-race` 覆盖 RuntimeCatalog/cache、Bot template send/edit、CardUpdater、message action-context。
- [x] DB integration lane 在 clean MySQL/Redis/WuKongIM 环境通过；若本地共享 DB 有已知 migration 污染，
  必须记录真实阻断并以 clean CI 证据补齐，不能伪称本地通过。
- [x] `go build ./...`、定向/全仓 `go vet`、`make i18n-extract-check`、`make i18n-lint`、相关 source
  guards 与 `git diff --check` 全绿。

### G. E1d OpenClaw Model A companion

- [ ] selector 对 enabled/templating/wire/views/states/profiles/submit_actions 全量校验；随机 manifest version
  测试证明无 `0.1.0/0.2.0` 硬编码，多兼容版本无 policy 时 fail closed。
- [ ] successor exact-limit/limit+1 fixtures 覆盖全部 string、phases、per-phase actions 和 aggregate actions；
  producer action 总数保持不高于 12，`reasoningId` bounded 且不以截断制造碰撞。
- [ ] reasoning lane 映射只输出可展示脱敏摘要；hidden chain-of-thought、prompt、credential、私有 tool
  payload 不进入 card data/log。
- [ ] 每消息固定 mode/id/version/card_seq；Model A/B send/edit total XOR；active transient、terminal
  non-transient；Model A 失败不 raw overwrite。
- [ ] off/shadow/experimental 配置 schema、package、release artifact、version 和 rollback note 完整；production
  default仍 off，未在未授权环境打开 Bot gate。
- [ ] 对真实 server profile 跑 reasoning→answering→completed 与 error；相同 edit 幂等重放、stale seq、能力
  关闭和 server 5xx 行为符合 handoff。

### H. E1e reasoning-control companion

- [ ] active-run registry 绑定 reasoning/run/message/bot/Space/origin request/owner instance/status/TTL；多实例
  定向、owner loss、restart/expiry 有自动化测试和可观测结果。
- [ ] stop 拒绝 forged/cross-Space/cross-bot/mismatched message event；duplicate/late/completed race 幂等；底层
  cancel 成功后才写 stopped，失败不伪造成功文案或状态。
- [ ] retry 不从 card display data 重建 prompt；使用持久化 origin request、幂等 key、频率限制和 single-writer；
  创建新 reasoning ID/new run 成功后才发送/更新新 reasoning，失败保留原 error frame。
- [ ] transient infra failure 不 ACK 丢事件；success/safe replay/permanent rejection 才 ACK。event ACK、business
  outcome、card edit 分开记录 bounded metrics/logs。
- [ ] stop/retry 多副本、重启、timeout、重复事件、乱序事件、queue redelivery、server edit failure 和补偿路径
  通过 race/integration tests。

### I. Joint release gate

- [ ] octo-server、OpenClaw plugin/runtime、client 的 commit/package/version matrix 固化在 E2E 记录中；不能用
  “latest”代替可复现版本。
- [ ] successor 完成 send→edit→completed、error→retry→new run、reasoning→stop→stopped；同时覆盖跨 Space
  拒绝、实例 crash、DB/Redis/HTTP 抖动、catalog rollback 和 consumer rollback。
- [ ] 三个 work packages 的 CI/review/release evidence 均完成后才将 E3 PR-B milestone 标 DONE；任何一个缺失
  时 production `OCTO_BOT_CARD_ENABLED`、runtime control/new-send gates 保持关闭。

## Rollout and rollback

1. PR-B 首次部署两个 runtime gates 均为 false，只验证 static traffic、manager read、metrics 和 startup
   reconciliation；不得 activate dynamic version。
2. 所有 serving replicas 升级且 clean DB multi-replica/restart evidence 完整后，才可在非生产短期开
   control gate执行 activate/rollback/block 演练；new-send 仍 false。无 grants 阶段只能使用无人消费的
   template ID，不能把某个生产 notify/Bot 正在使用的 ID 切到 dynamic 后再解释为“演练无影响”。
3. OpenClaw E1d package 先以 shadow/experimental 发布，使用 built-in successor 验证 Model A；E1e active-run
   registry 和 stop/retry 未发布前不允许生产展示可操作按钮。
4. E1d/E1e companion 完成后固定跨仓版本矩阵跑联合 E2E；只有联合 go/no-go 批准才可单独开启 Bot card
   灰度。该动作不自动开启 dynamic catalog new-send。
5. PR-C 接入 grants 与 catalog pilot 前，production dynamic new-send 保持不可达。
6. 常规回滚先关闭 forward control/new-send，CAS rollback 到 known-good static/dynamic target；不得删
   artifact/cache。
7. OpenClaw rollback 按消息固定 mode/version：已有 Model A 消息保留最后成功帧并停止 edit，不得用旧
   Model B raw edit 覆盖；active-run rollback 必须先 drain/转移或明确终止在途 run。
8. 安全事件使用 block + fallback/disabled；block 不受 forward control gate 阻断。
9. 首张 dynamic 卡真正发送后，不得回滚到缺少 dynamic historical exact reader 的 pre-PR-B binary；只能
   部署仍保留该 reader 的修复版本。该兼容性底线在 PR-C pilot/生产工单再次确认。

## Human confirmation before `/octospec-go`

- [x] 接受 PR-B 先 stacked、待 PR-A merge 后 rebase main 的分支策略；#674 合并及本次 rebase 已完成。
- [x] 接受 PR-B 无 grants 时所有 business dynamic purpose fail-close，首次 producer/pilot 留 PR-C。
- [x] 接受 forward control gate 只禁 activate，rollback/block 始终保留为安全操作。
- [x] 接受 cache 默认 64 entries / 32 MiB，hard max 256 / 128 MiB。
- [x] 接受 dynamic interactive activation 的 PR-B coarse RouteSpec 检查，以及 PR-C grant 时按 exact sender
  再校验。
- [x] 接受“同一个 E3 PR-B”是一个联合 delivery milestone；octo-server、OpenClaw E1d、OpenClaw/runtime
  E1e 按仓库与回滚边界形成 companion PR，三者共享联合 E2E/go-no-go，不能压成单一 GitHub diff。
- [x] 接受 E1e retry v1 创建新 reasoning ID/new run，且只有真实 cancel/retry 成功才改变卡片业务状态。
- [x] 接受 startup 未 ready 时 default/dynamic fail-close，并由 `/v1/ready` 503 将 replica 摘流；不以
  未验证的 static default 绕过 active pointer truth。
- [x] 接受 publish 的 runtime-readiness + reviewed owner allowlist 前置；typed-object
  `patternProperties` fail-close 依赖已合并的 PR-A compiler hardening，并非本 PR-B 新增。untyped
  combinator 的结构性完整扫描留在 dynamic producer/grant 打开前收口。
- [x] 接受 action-context principal kind 不能从现有 sender/template 猜测；可信 producer provenance
  设计与持久化是 PR-C grant 上线前置。
