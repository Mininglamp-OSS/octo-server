---
type: Task
title: "Task: cardtmpl-runtime-catalog-grants-discovery"
description: Complete E3 PR-C with authoritative producer grants, visibility-aware B1/B2 discovery and export, dynamic Bot capability integration, trusted stored provenance, and one non-production interactive pilot.
tags: [card, cardtmpl, runtime-catalog, database, migration, grant, discovery, export, provenance, wire-contract, trust-boundary, auth, acl, space, bot-api, i18n, rate-limit, observability, testing, rollback]
timestamp: 2026-08-05T00:00:00+08:00
# --- octospec extension fields ---
slug: cardtmpl-runtime-catalog-grants-discovery
upstream: "roadmap E3 PR-C; parent cardtmpl-runtime-catalog; PR-A #674@68e8134d; PR-B #675@49a66475"
source: self
---

# Task: cardtmpl-runtime-catalog-grants-discovery

> This brief is the OctoSpec **Plan** contract for octo-server E3 PR-C. It is
> extracted from the accepted parent brief after rereading current
> `main@40627cc0`. The decisions below were accepted when implementation was
> authorized on 2026-08-05. The implementation must remain one reviewable PR-C
> milestone, built as the ordered test-first slices listed below. Implementers
> start from [`HANDOFF.md`](./HANDOFF.md), which owns the repository-relative
> file ledger, commit boundary, fixture policy and verification commands.

## Goal

在 #674/#675 已交付的 immutable artifact、RuntimeCatalog、activate/rollback/block
基础上，补齐“谁可以看、谁可以发、谁可以继续编辑”的授权闭环：

1. 用 MySQL 权威、可审计、CAS 防并发覆盖的 grant 管理
   `discover|send|edit`；
2. 为 B1/B2 提供 visibility/Space-aware 的模板发现与安全制品导出；
3. 把 Bot 的静态 allowlist 与 dynamic grant 安全合并，使 capability、new send、
   historical edit 读取同一授权真相；
4. 为 dynamic card 持久化不可由 raw caller 伪造的 producer/Space provenance，
   让 action-context 与 historical edit 不再猜 principal；
5. 在非生产用一个现有 L2a owner、现有 producer 和现有 RouteSpec 跑通
   publish→grant→activate→discover/export→send→Action.Submit→same-version
   edit→rollback/revoke。

PR-C 合并表示“dynamic catalog 已具备受控消费能力”，不表示生产 dynamic new-send
自动开启，也不表示 L2b `ext.*`、Bot 自助上传或 OpenClaw E1e stop/retry 已完成。

## Verified baseline at plan time

- 当前基线：`origin/main@40627cc0`。
- E3 PR-A #674（`68e8134d`）已交付 shared compiler、immutable claim/artifact/audit
  store 与 super-admin validate/publish。
- E3 PR-B #675（`49a66475`）已交付 RuntimeCatalog、MySQL active pointer、CAS
  activate/rollback/block、compiled cache、readiness 与默认关闭的
  `OCTO_CARD_RUNTIME_CATALOG_CONTROL_ENABLED` /
  `OCTO_CARD_RUNTIME_CATALOG_NEW_SEND_ENABLED`。
- `pkg/cardtmpl.CatalogAccess` 已有 `new_send|historical_edit|action_context` 与
  `bot|internal_producer|system` typed principal；production authorizer 当前在无下游
  grant store 时一律拒绝 dynamic business access。
- 数据库只有 version claim、artifact、audit、activation、capacity guard；没有 grant 表。
- `modules/bot_api/card_template_catalog.go` 在进程启动时从代码常量构造
  `AdvertisedSend/EditCompatible` 与固定 capability；它在进入 RuntimeCatalog 前会拒绝
  不在 static map 的 ref，因此 dynamic grant 尚不可能生效。
- Bot Registry outbound frame 已保留 server-authored top-level `template_ref`；raw Bot
  send/edit 不能写该字段。internal `carddispatch` 目前只持久化 card document/profile，
  不保存 producer ID，也不会在 envelope 中保留 `template_ref`。
- message action-context 当前对所有 Registry metadata 都以 `msg.FromUID` 猜成 bot
  principal；`CardUpdater.ReplaceView` 也以 `SenderUID` 猜成 internal producer。这对
  dynamic grant 不足，且正是 PR-C 必须先修的 provenance 缺口。
- B1/B2 route 尚不存在。static Registry 未保留完整 B2 export projection/catalog
  visibility；dynamic canonical bundle 可以提供导出源。
- server bot events opt-in long-poll #685（`a4d4c924`）和平台卡缺省
  `render_profile` #688（`60febd8e`）已合并。OpenClaw #194 当前 approve 待合并；
  它改善事件时延，不改变本 PR 的 grant/provenance 语义。

## Success definition

- **身份闭环**：dynamic new-send/edit/action-context 使用 authenticated 或 stored
  server-authored principal + authoritative Space；raw payload、query/body principal、
  template owner 或 sender 猜测均不能形成授权。
- **授权闭环**：grant/revoke 使用 strong MySQL read、revision CAS、同事务 audit；
  active/block/grant 在同一 primary-DB snapshot 内形成一次授权决定；revoke 提交后开始的
  新授权决策立即拒绝，local compiled cache 不缓存 grant 真相。
- **发现闭环**：B1/B2 的 public/private、Space grant、pagination、ETag 和
  anti-enumeration 有自动化证据；导出不泄露 audit/grants/DB IDs/template source/goldens。
- **Bot 闭环**：profile、new send、historical edit 与 action-context 对同一 Bot/Space
  使用同一 effective active + grant 真相；同一 template ID 最多广告一个 send version。
- **运行闭环**：两副本、cache 热/冷、DB outage、grant race、activate/rollback/revoke、
  restart 和真实 MySQL/Redis/WuKongIM pilot 均通过；生产 gates 默认仍关闭。

## Load-bearing invariants

1. `publish != activate != grant`：三者互不隐式产生下一步状态。
2. static `Registry` 继续冻结；dynamic artifact 不能覆盖任何 static exact key。
3. active pointer 只影响新消息；historical edit 固定 stored exact version。
4. compiled cache 只缓存 immutable artifact，不缓存 active/grant/block。
5. catalog 存在、public visibility 或 B1 可见均不等于 send/edit 授权。
6. dynamic action 只能从 effective stored frame 恢复 template/principal/Space，再进入
   既有 owner/RouteSpec/Space/operator/card-action 幂等链。
7. static 历史卡保持兼容；PR-C 不要求给历史帧回填 provenance。
8. unauthorized 与 nonexistent 对普通调用方不可形成枚举 oracle。
9. 首条 dynamic 卡一旦发送，保留 dynamic historical exact reader 成为 binary floor。
10. grant 是 template-ID 级授权：在 grant 有效期间，后续新版本只有经独立 publish +
    activate 后才继承该授权；grant 本身绝不选择版本。
11. 同一 principal/Space 的 exact-scope row 是权威覆盖，不能与 global row 做 permission
    union；exact tombstone 必须能屏蔽 global active grant。
12. dynamic active version 对同一 template ID 的 static new-send 形成 shadow；授权失败、
    blocked、integrity/DB failure 时不得静默降级到 static 同 ID 版本。

## Accepted implementation decisions

### D1 — One PR-C milestone, ordered internal slices

保持一个 octo-server PR-C，避免 migrations/API/runtime wiring 跨 PR 出现“grant 已写入但
消费边界尚未接线”的半状态。实现按下文 6 个 slices 顺序推进，每个 slice 先 RED 再 GREEN；
任一中间 commit 均不得打开生产 dynamic new-send。

### D2 — Principal and permission model

- v1 principal kind：`bot`、`internal_producer`、`space`；`system` 仅保留给内部
  control/readiness/test，不是 grantable business principal。
- 权限是固定列/枚举 `discover|send|edit`，不得接受任意字符串。
- `space` principal 只能 discover；`send|edit` 只授予可认证的 `bot` 或
  `internal_producer`。
- active grant 至少有一项权限；`send`/`edit` 必须同时包含 `discover`，避免 producer
  获得无法从 capability/discovery 观察的暗授权。
- grant 是 **template-ID 级**，不是 version 级：后续版本经 super-admin 独立 activate 后
  继承该 template ID 的有效 grant。这是 v1 的刻意选择；若不能接受未来版本继承，必须在
  实现前改主键模型，不允许实现者临时按 version 加隐藏条件。
- `bot` / `internal_producer` 的 `scope_space_id` 使用 non-null canonical empty-string
  表示 global，非空值表示 exact Space；非空 Space 必须存在且 active。
- `space` principal 的唯一合法形状是 `principal_id=<space_id>` 且
  `scope_space_id=''`，该 Space 必须存在且 active；禁止再叠一层 global/exact scope，避免
  `(principal space A, scope space B)` 这种无意义或跨租户组合。
- 对 `bot` / `internal_producer` 在 Space S 的授权解析顺序固定为：若 S 的 exact row 存在，
  只使用该 row；exact active 按其 permission 判定，exact revoked 直接拒绝；仅当 exact row
  不存在时才查询 global row。不得 union permissions，也不得让 global active 穿透 exact
  tombstone。`space` discover 只查上述 canonical `space` row。
- `new_send` 要求 `send`；`historical_edit` 与 `action_context` 都要求 `edit`。
  revoke send 不冻结已有卡；revoke edit 阻断后续 edit/action，但不删除最后成功 payload。
- static new-send/edit 继续服从 code-reviewed policy，不因 dynamic grant 自动扩权；private
  static B1/B2 discovery 可以使用 `space` discover grant。dynamic new-send/edit/action 才必须
  经过上述 business grant。
- grant 不替代 Bot ownership、internal producer registration、target Space/channel、
  feature gate、RouteSpec、message lifecycle 或 CAS。

### D3 — Stored server-authored catalog provenance

不修改 caller-facing `template_ref/v1` request shape。对 Registry 产出的有效 type-17
frame 增加 additive、server-only top-level marker：

```json
{
  "template_ref": {"id": "docs.access-request", "version": "<pilot-version>"},
  "catalog_provenance": {
    "version": 1,
    "principal_type": "internal_producer",
    "principal_id": "docs-notify",
    "space_id": "space_x"
  }
}
```

- Bot Registry path 从 `authBot()` 的 robot ID 与 target-authoritative Space 写入；
  internal path 由 `carddispatch.producerSender` 从已注册 `ProducerSpec.ID` 和已授权
  target Space 写入。调用方不能自报 principal。
- internal Registry send 同时保留 top-level `template_ref`；它必须与
  `metadata.octo.template` 完全一致。
- raw Bot/robot/user/incoming-webhook send/edit 对 `template_ref` 与
  `catalog_provenance` 保持或新增显式拒绝；不能依赖客户端“不会传”。
- Bot edit、`CardUpdater.ReplaceView`、Append 和 action ingress 从 effective Snapshot
  恢复并原样校验 provenance；不允许更新时改 principal/Space/template identity。
- action ingress 把已验证的 principal/Space 作为 `CardContext` 的 additive server-authored
  字段写入 durable event；finalizer/updater 只消费该字段或重新 Snapshot，不从 sender/owner
  反推 internal producer。
- 对 dynamic frame，缺失、畸形或与 stored sender/Space/registered producer 不一致的
  provenance 一律 fail-close。static 历史 Bot/notify 卡可继续走既有兼容路径。
- 这一步同时收口 roadmap G13：`ReplaceView` 在 dynamic authorization/render 前必须
  Snapshot 并验证 effective template identity/provenance，不能只信 `UpdateTarget`。

### D4 — Grant persistence and CAS

新增 `card_template_grant`：

- identity：`template_id + principal_type + principal_id + scope_space_id` unique；
- state：`status=active|revoked`、`can_discover/can_send/can_edit`、单调 `revision`；
- audit fields：updated_by/reason/change_ticket/updated_at；
- revoke 写 tombstone 并 bump revision，不物理删除，避免 revoke→recreate 的 ABA；
- create 要求 `expected_revision=0`，update/revoke 要求精确 current revision；并发只有一个成功；
- grant/revoke 状态与 append-only `card_template_audit` 在同一事务提交；audit 扩展记录
  principal/scope、before/after permissions/revision，不存 token 或卡片 data；
- grant target 至少存在一个 permanent version claim；不以 activation 存在作为前提；
- 没有跨 static/dynamic 的 template-level FK 时由同一事务 `SELECT ... FOR UPDATE`
  claim/guard 验证，不伪造脆弱 FK。

grant/activation/block 强一致读继续走 primary DB。v1 不引入 Redis 或本地 grant cache。

授权读取必须提供一个窄的、不可被调用方拆开的 store resolver：

- dynamic `new_send` 在一个 primary-DB single statement 或同一 consistent read transaction
  中读取 effective activation、exact artifact/block 与 exact/global grant rows，按 D2 规则
  产生一个 decision；不能先读 active、再在另一个 snapshot 读 grant。
- `historical_edit` / `action_context` 不读取 active pointer，但必须在同一 snapshot 中读取
  stored exact artifact/block 与 grant rows；stored provenance 是 principal/Space 输入。
- resolver 返回 bounded receipt（template exact、activation revision、grant revision、scope
  source），仅用于日志、metrics 和同请求校验；receipt 不是 bearer capability，不能跨请求缓存。
- 授权线性化点定义为 resolver 成功取得的 DB snapshot：snapshot 在 revoke/activate/block
  commit 之后开始的请求必须看到新状态并拒绝；更早已取得 snapshot 的 in-flight 请求允许完成。
  Bot profile 与 send 是两个独立决策，send 必须重新 resolve，不能信 profile receipt。

### D5 — B1/B2 read model

- B1：`GET /v1/message/card/templates`；cursor pagination，default 50/hard 100；只返回
  visible + unblocked exact versions、source/owner/protocol/contract/version/views、
  `action_contract`（owner+action_type 摘要，纯展示卡为 null——对应 platform-card-base §9
  承诺的 `actionType` 能力发现字段）和
  `active_for_new_send`，明确 `send_allowed` 不是 B1 的承诺。
- B2：`GET /v1/message/card/templates/{id}@{version}`；只返回
  manifest/schema/reports/samples 的安全 projection，不返回 templates、goldens、audit、
  grants、internal DB IDs 或 canonical storage bytes。
- 两者路由链：Auth → SharedUIDRateLimiter → **localized Space middleware/variant**；缺 Space
  时最多看到 public，private 必须命中经过 middleware 验证的当前 Space grant 或
  superAdmin。现有 `pkg/space.SpaceMiddleware` 的 raw `AbortWithStatusJSON` 不能直接复用到
  这两个新端点；PR-C 应增加保持同一 membership/cache 语义、但统一走 `httperr` 的窄 variant，
  不借机改变所有旧路由 wire status。
- public 只代表可发现；private 对普通调用方需 `space` discover grant。Bot 仍以
  `/v1/bot/card/profile` 为权威，不允许拿 B1/B2 绕过 bot grant。
- unauthorized/nonexistent/blocked 对普通 B2 返回同一个 localized not-found envelope；
  manager API 保留诊断视角。
- dynamic ETag 使用 immutable `content_sha256`；static 使用 deterministic export hash；
  支持 `If-None-Match`/304。private 响应至少 `Cache-Control: private, no-cache`；响应
  projection 有 2 MiB hard cap。
- B1 visibility/block/grant 必须在 pagination limit 与 `has_more` 计算前完成；cursor 只从最后
  一个已返回的 visible row 推进，不得让 hidden/blocked row 改变 cursor、count 或空页形状。
  cursor 至少绑定 source + template exact + Space/visibility context，跨 Space 重放 fail-close。
- static Registry 在 Freeze 前显式注入 `engine/visibility/export` metadata；未配置 visibility
  fail-close 为 private。注入点是 composition root 的 Go 侧注册参数（`main.go` static
  CatalogMeta），绝不回改已冻结的 L1 handoff manifest（platform-card-base §2.1 发布即冻结）。
  PR-C 不改变任何现有 static 卡的 visibility——全部保持 fail-close private；把某张 static 卡
  对普通调用方开放属后续 reviewed composition-root 变更，不是 PR-C 交付物；public 矩阵由
  测试 fixture 与 pilot dynamic bundle 覆盖。注册/compile 时构造 immutable safe export
  projection 并复制 raw manifest/schema/reports；请求期不读源码目录，也不从 compiled schema
  反向猜原文。
- `samples` 默认不导出；只有 manifest 显式列入 export allowlist、且确认是 synthetic
  fixture 的 sample 才进入 projection。static 卡受 L1 冻结约束无法回填 allowlist，现有
  static 卡的 B2 samples 恒为空；dynamic bundle 在自身 manifest 声明 allowlist（compiler
  strict 校验，未知键继续拒绝），pilot bundle 必须含至少一个 allowlisted synthetic sample
  证明导出通路。templates、goldens、未列入 sample 和 canonical bundle
  永不进入 B2。static/dynamic 共用同一 projection builder 与 2 MiB 序列化前 hard cap。
- wire status 决策：B1/B2 与 localized Space middleware variant 的全部错误响应走既有
  `httperr.ResponseErrorL`（pinned-400，D14 兼容），与 `modules/card_template_catalog`
  现有 responder 同一契约；不使用 `ResponseErrorLWithStatus`（未取得新端点偏离 D14 的
  maintainer 确认）。ETag 命中的 304 与缓存响应头不是 error envelope，不受此约束。

### D6 — Bot capability and runtime authorization

- static `AdvertisedSend/EditCompatible` 继续是 code-reviewed policy；构造期改为按 template ID
  检查 `AdvertisedSend` 最多一个 version，而非只检查 exact ref duplicate。
- Bot profile 从固定进程常量改为 request-scoped：`authBot → botActorUID →
  SharedUIDRateLimiter`，以 authenticated bot + authoritative Space 查询 active/unblocked
  dynamic send grant，再与 static policy 合并。
- Bot multi-Space 不做 union 广告。scope=space App Bot 使用认证上下文；显式
  `X-Space-ID` 必须走现有 authorization；User Bot 只有一个 active Space 时可使用该值，
  多 Space 且无有效 hint 时 dynamic 部分 fail-close，不选择一个会造成跨 Space 漂移的目录。
- 对 dynamic profile/send，显式但未授权的 `X-Space-ID` 直接拒绝 dynamic decision，不得像
  当前 legacy helper 一样 fallback 到 deterministic first Space；DB 错误也不得 fallback。
- Bot send 在 render 前为 DM/group/thread 解析 target-authoritative Space：group 从
  authoritative group row 取 Space，thread 从 authoritative parent-group row 取 Space；DM
  使用 scope=space App Bot context 或经过 authorization 的 `X-Space-ID`，并同时验证 bot 与
  对端用户都是该 active Space 的有效主体。若无法得到唯一 Space、对端不属于该 Space、
  multi-Space 无有效 hint 或 hint 非法，dynamic send 在 render/enqueue 前 fail-close。
  payload/query/body `space_id` 永远不是授权来源。
- dynamic explicit ref 必须同时是 effective active version + unblocked + 命中 send grant。
- dynamic historical edit 不跟随 active pointer，只校验 stored exact provenance + edit grant；
  active 切换不能把旧卡跨版本更新。
- new-send gate=false 时保留现有 static-only profile/send 行为，不为暗置功能引入 DB outage
  blast radius；gate=true 后 dynamic capability/send DB 失败必须 fail-close，不能静默广告 static
  同 ID 版本。
- profile 与 send 之间若 revision 改变，旧 explicit ref 安全失败，producer 刷新 profile 后重试；
  server 不偷换版本。

Bot new-send/profile 对同一 template ID 的选择矩阵固定如下，profile 与 send 必须调用同一
resolver，不能各自实现：

| State | Advertise / send result |
| --- | --- |
| new-send gate=false | 只走现有 static policy，完全不访问 runtime DB |
| gate=true，ID 无 activation row | 使用 static advertised version（若有） |
| gate=true，active pointer 指向 static exact | 仅当该 exact 在 static policy 中时广告/允许；不查询 business grant |
| gate=true，dynamic active + unblocked + effective send grant | dynamic exact shadow/替换 static 同 ID |
| gate=true，dynamic active 但无 grant或 exact tombstone | 整个 ID 不广告；explicit dynamic/static new-send 均拒绝 |
| gate=true，dynamic active 但 blocked/integrity failure | 整个 ID fail-close，不回退 static |
| gate=true，runtime DB unavailable | profile 返回 typed localized unavailable；Registry new-send fail-close，不回退 static |

static historical edit 仍按 stored static exact + existing code policy 工作；dynamic activation shadow
只影响 new-send，不能破坏 static 历史卡兼容。

### D7 — First non-production pilot

pilot 固定使用：

- owner：`docs`（现有 L2a）；
- producer：`internal_producer/docs-notify`；
- action contract：现有 `docs/access_request.decision` RouteSpec；
- visibility：private；
- identity：never-before-claimed prerelease exact version，禁止复用任何 static key；
- Space：单一专用测试 Space，不使用生产 Space/用户数据。

pilot 在第一次 dynamic activate 前必须先建立可回滚基线：读取当前 activation；若当前 active
exact 不是 static known-good `docs.access-request@0.3.0`（包括无 row 或 disabled），先用正常
`activate` + current expected revision 将该 static exact 记录为 active baseline；CAS conflict 或
baseline activate 失败就停止 pilot。随后才能 activate prerelease dynamic exact。这样后续
`rollback` target 确实满足“previously active”；不能把 Registry default 误当成 activation
audit history。

docs finalizer 对 Registry action 必须使用 event stored `template_id@version` 做 same-version
result edit；static `0.3.0` 行为不变。pilot 不触碰 `ai.reasoning-process`，不与 E1e
stop/retry 或 OpenClaw rollout 绑定。

## Control APIs

在现有 super-admin manager group 下新增：

| Method/path | Semantics |
| --- | --- |
| `PUT /v1/manager/card-templates/{id}/grants/{principal_type}/{principal_id}` | create/update active grant；strict body 包含 scope_space_id、discover/send/edit、expected_revision、reason、change_ticket |
| `DELETE /v1/manager/card-templates/{id}/grants/{principal_type}/{principal_id}` | logical revoke；strict body 包含 scope_space_id、expected_revision、reason、change_ticket |
| `GET /v1/manager/card-templates` | 分页只读列表：全部 template 的 source/visibility/active/block 摘要与 bounded grant summary，含普通 B1 隐藏项；补齐父 brief v1 控制面缺失的最后一个 read API（PR-C 是 E3 收尾切片，不再顺延） |

两条写接口都要求 control gate=true、runtime ready、superAdmin、SharedUIDRateLimiter、
10s deadline 与 typed localized errors。只读列表与既有 manager GET detail/audit 同一
鉴权/限流/deadline 链，不要求 control gate=true，也不涉及 expected_revision。path/body identity 严格 canonical；body actor 字段拒绝。
manager API 必须按 D2 canonical shape 校验 scope，并在响应中返回实际命中的 revision；对
revoked exact row 的 re-activate 必须携带 tombstone 当前 revision，不能再用
`expected_revision=0` 伪装 create。

现有 manager detail 增加 bounded grant summary，audit page 可显示脱敏 grant/revoke identity；
普通 B1/B2 永不返回这些 control fields。

## Code impact map

- `pkg/cardtmpl`
  - 增 discover/export purpose、Space principal、typed provenance 与 discovery/export narrow interface；
  - static Registry 保留 deterministic export projection/hash；
  - RuntimeCatalog 接入 strong grant authorizer/list/export，不缓存 grant/active/block；
  - dynamic new-send 校验 effective active exact version，edit/action 校验 stored provenance。
- `modules/card_template_catalog`
  - migration、grant store/CAS/audit、principal resolver、manager grant APIs；
  - B1/B2 route、visibility filter、pagination、ETag、anti-enumeration；
  - metrics 和 runbook。
- `pkg/space`
  - 为 B1/B2 增 localized Space middleware variant；复用现有 membership/cache 检查，避免
    扩大旧路由 wire-contract 变更。
- `modules/bot_api`
  - request-scoped capability、target Space resolution、static+dynamic policy merge；
  - Bot Registry send/edit provenance；raw forge rejection；profile UID limiter。
- `modules/robot` / `modules/message` / `modules/incomingwebhook`
  - legacy robot raw card 显式拒绝 server-only marker；用户 type-17 绝对拒绝与 incoming-webhook
    server-built/drop-extra 边界增加回归证据，不为这些 raw ingress 新增 dynamic capability。
- `internal/carddispatch`
  - trusted sender authors producer provenance/template_ref from `ProducerSpec.ID`；
  - expose a read-only producer binding resolver for action-context validation，不能让业务 caller
    传任意 ProducerSpec。
- `pkg/cardtmpl/updater.go` / `modules/message/api_card_action.go`
  - Snapshot effective provenance before dynamic edit/action；same-version, same-principal,
    same-Space validation；将已验证 principal 写入 durable `CardContext`；static history compatibility。
- `modules/notify`
  - docs pilot passes template identity through carddispatch；finalizer uses stored event exact version；
  - existing static sends/results remain byte/behavior compatible except additive server metadata。
- `main.go` / docs
  - composition-root static CatalogMeta、grant principal dependencies、runbook/protocol/roadmap updates。

This section names architectural ownership only. The executable, per-slice file list and the distinction
between repository fixtures and non-production runtime state are maintained in
[`HANDOFF.md`](./HANDOFF.md); do not infer files or commit pilot data from this broad impact map.

## Test-first implementation slices

### Slice 1 — provenance RED→GREEN

- RED：raw forge、cross-bot、cross-producer、cross-Space、missing/malformed marker、template_ref vs
  metadata mismatch、ReplaceView without Snapshot。
- GREEN：typed marker at Bot/carddispatch boundaries；snapshot extraction/preservation；dynamic-only
  fail-close；static history compatibility。

### Slice 2 — grant schema/store/control RED→GREEN

- migration Up/Down、binary collations/check constraints/global sentinel；
- create/update/revoke CAS、tombstone ABA、same-transaction audit、concurrent grant race；
- principal existence/Space binding/permission matrix；exact-over-global、exact tombstone、space
  canonical shape、future-version inheritance；manager auth/i18n/strict body/rate limit。

### Slice 3 — runtime authorization and Bot merge RED→GREEN

- effective-active send、inactive explicit ref rejection、historical edit/action edit grant；
- static+dynamic merge、template-ID single-version guard、per-Bot/per-Space isolation；
- single-snapshot active/block/grant race、revoke/rollback/capability race、gate-off static
  compatibility、DB outage fail-close；
- invalid Space hint、multi-Space ambiguity、DM peer non-membership、group/thread authoritative Space。

### Slice 4 — B1/B2 RED→GREEN

- static export retention/hash + dynamic projection；
- composition-root static CatalogMeta 注入（visibility/export/sample allowlist；不回改冻结 manifest）；
- manager 只读列表端点（含 B1 隐藏项与 bounded grant summary）；
- public/private/Space/superAdmin matrix、pagination/hard cap、ETag/304/cache headers；
- unauthorized vs nonexistent equality、blocked omission、visible-only cursor、cross-Space cursor replay；
- localized Space errors、synthetic sample allowlist、Bot cannot use B1/B2 as grant bypass。

### Slice 5 — interactive pilot RED→GREEN

- docs-notify dynamic private artifact、stored provenance、existing RouteSpec、same-version finalizer；
- establish static active baseline→publish→grant→activate→B1/B2→send→submit→edit→rollback→old
  dynamic edit→revoke→reject。

### Slice 6 — production verification and docs

- focused coverage/race/build/vet/lint/i18n/source guards；
- real MySQL migration/concurrency/two-replica tests；
- dedicated non-production Redis/WuKongIM E2E, restart and DB outage；
- runbook rollout/rollback/metrics/alerts and PR Linked Spec/COMPREHENSION；
- 文档收口（与代码同 PR）：platform-card-base §9 的 B1/B2 实际字段与授权语义、§10 新增
  dynamic unauthorized/blocked/DB-unavailable fail-close 行（不降级为 fallback 文本）；
  card-protocol §1 补 additive 顶层 `template_ref`/`catalog_provenance` 字段并注明客户端
  渲染门禁仍只看 `from_uid`（provenance 是服务端授权输入，不是客户端渲染契约）；runbook
  记录 `l2aOwnerAllowlist`（Registry 注册面）与 `approvedRuntimeOwners`（runtime
  publish/授权面）是两份刻意不同的清单——给未获批 runtime owner 建 grant 不会使其可发；
  platform-card-base §2.2-5 注明 L2b 硬门槛 ④ 不因 per-principal grant 落地而推进。

## Acceptance matrix

Verified 2026-08-06 against MySQL 8.0 + Redis + WuKongIM. Each box names the
test that would fail if the property broke; `[ ]` items say plainly what is
still missing and why. Test names are unique across the repo, so `go test -run`
finds them directly.

### Authorization and isolation

- [x] non-superAdmin cannot grant/revoke or inspect hidden grant details —
  `TestGrantWriteRequiresSuperAdminAndControlGate`,
  `TestControlEndpointsRejectNonSuperAdminBeforeCompile`,
  `TestManagerListRejectsCallersWhoAreNotSuperAdmin`,
  `TestRouteRequiresAuthenticationBeforeControlHandlers`; B1/B2 carry no grant
  state at all (`TestDiscoveryResponsesCarryNoControlPlaneFields`).
- [x] Bot A cannot discover/send/edit with Bot B's grant even on the same
  apiUrl/Space — `TestGrantsDoNotLeakBetweenBotsRealMySQL` (same template, same
  Space, only the Bot differs; also proves `bot:x` and `internal_producer:x`
  are different identities), plus
  `TestBotMessageEditRegistryTemplateRejectsForeignStoredProvenance`.
- [x] a Space discover grant never becomes send/edit authority —
  `TestValidateGrantPermissionsMatrix`, `TestValidateGrantIdentityCanonicalShapes`,
  and the `chk_card_template_grant_space` CHECK constraint, so the shape cannot
  be written even by a future caller that forgets the validator.
- [x] a server-authored catalog marker cannot be authored invalid, and cannot be
  dropped by an edit — `TestSendRefusesToAuthorAMarkerItsReadersWouldReject`,
  `TestCardMutatorRefusesToDropStoredCatalogMarkers`,
  `TestDocsActionFinalizerRoutesCancelledThroughTheRegistry`,
  `TestDocsAccessRequestV3RendersTheCancelledState`. Both halves were regressions
  this PR introduced, and neither was behind a runtime-catalog gate: the marker
  paths sit behind the pre-existing `OCTO_CARD_MESSAGE_ENABLED`, so the "both new
  gates are false" argument never covered them. Authoring ran *after*
  `cardmsg.Validate`, so the hook written to catch a missed path structurally
  could not see it, and an untrimmed Space produced a frame every reader
  refuses — permanently unclickable and uneditable. Erasure needed only a
  `cancelled` result, whose replacement frame came from a six-key allowlist
  carrying neither marker, leaving both identity guards in `updater.go` inert.
  Preservation is now enforced at `CardMutator.Mutate` rather than per call
  site, and `docs.access-request@0.3.0` declares `cancelled` and `unavailable`
  states so the replacement is a Registry render that legitimately carries the
  markers. Selecting the replacement route by state was the defect's shape, so
  the state list is gone rather than extended — `pending` reached the same
  branch and a later state would have inherited it silently; what remains is
  the no-updater fallback the branch was written for.
  `TestStandardActionFinalizerCannotSilentlyDowngradeAMarkedCard` pins the
  latent second caller: it is the default finalizer for every non-docs owner and
  routes every state through the same helper, harmless today only because
  `BuildApprovalRequestCard` writes no template metadata, so its cards are never
  marked.
- [x] every domain the Go validator enforces is also stated in the schema, so a
  write that skips the validator cannot land a row the validator would refuse —
  `TestGrantSchemaRejectsWritesTheValidatorWouldRefuseRealMySQL` (real rows, each
  case rejected by the database itself). Two gaps were open: `can_send`/`can_edit`
  outside `{0,1}` on an active row, and a blank `principal_id`. The first was
  fail-closed only by accident of the read — `GrantPermissions` scans into Go
  bools and the driver refuses the value, so the authorization load errored — and
  a future reader that scanned into an int and tested `!= 0` would have turned the
  same row into a grant. `chk_card_template_grant_bools` and
  `chk_card_template_grant_identity` state it where the value is written.
  (`can_discover` was already pinned by `chk_card_template_grant_shape` on both
  statuses; it is named in the new constraint for completeness, not coverage.)
- [x] exact Space grant overrides global; exact tombstone blocks global without
  permission union — `TestResolveGrantRowsPrecedence` (reducer),
  `TestStoreLoadAuthorizationExactTombstoneShadowsGlobalRealMySQL` (real rows and
  collations), `TestLoadAuthorizationsAppliesExactOverGlobalPrecedenceRealMySQL`
  (the batch reducer, which is a second implementation).
- [x] `space` principal rejects non-canonical principal/scope combinations —
  `TestValidateGrantIdentityCanonicalShapes`, `TestGrantPathIdentityCanonicalValidation`.
- [x] an unresolvable card-origin Space refuses the action on both branches,
  whether or not the frame names a Space —
  `TestUnknownOriginSpaceIsRefusedWhateverTheFrameClaims`. Assigning an unknown
  origin used to leave the principal's Space empty, which resolves against the
  global grant row alone — so an active global grant plus an exact tombstone for
  the card's real Space would have been allowed, defeating invariant 11.
- [x] B1's listing predicate and B2's authorizer apply the same approved-owner
  policy — `TestDiscoveryHidesRowsWhoseOwnerLeftTheAllowlistRealMySQL`. Both now
  derive from `approvedOwnerPredicate`, so narrowing the allowlist during an
  incident cannot leave B1 listing rows B2 answers not-found for.
- [x] cross-Space profile/send/edit/action attempts fail before
  render/enqueue/mutation — send `TestBotSendCatalogPrincipalUsesTheTargetGroupSpace`,
  edit `TestGroupCardStaysEditableAndKeepsItsAuthorizedSpace` (the cross-Space
  arm), updater `TestCardUpdaterReplaceViewRejectsCrossSpaceStoredProvenance`,
  action `TestResolveRegistryCardContextRejectsInconsistentProvenance/cross-space_provenance`.
- [x] dynamic DM requires both bot and peer in the selected active Space;
  invalid/ambiguous hints never fallback —
  `TestDMSendRequiresThePeerToBeInTheAuthorizedSpace`,
  `TestResolveBotGrantSpaceIDRefusesEveryAmbiguity`,
  `TestDMPeerCheckIsSkippedWhenTheCatalogIsDark`.
- [x] a markerless frame naming a template the frozen Registry does not know is
  refused before authorization — `TestMarkerlessFrameNamingAnUnknownTemplateIsRefused`.
  Raw ingress rejects the two server-only *top-level* keys, but the template
  identity also appears in `card.metadata.octo`, inside the body a caller
  controls. Without this the sender-derived fallback principal is the sending
  Bot's own grant identity and `Allows(action_context)` reads `edit`, so a
  fabricated frame let an `edit` grant stand in for the `send` grant it never
  had. Markerless compatibility is now scoped to the pre-PR-C population.
- [x] raw callers cannot forge `template_ref` or `catalog_provenance` through
  any type-17 ingress/edit — `TestSendMessageRawCardRejectsForgedCatalogProvenance`,
  `TestBotRawEditRejectsCatalogProvenanceMarkers`,
  `TestRawContentEditCannotForgeRegistryProvenance`,
  `TestSendMessageRegistryModeRejectsCallerProvenanceKey`, plus the robot and
  incoming-webhook absolute-rejection suites.
- [x] unauthorized and nonexistent B2 responses are wire-equivalent apart from
  request correlation — `TestGetTemplateAnswersOneNotFoundForEveryInvisibleReason`
  (byte-compares the bodies, including a malformed ref),
  `TestLoadDiscoverableGivesOneIndistinguishableMissRealMySQL`.

### State and consistency

- [x] publish, activate, grant remain independent; none creates another
  implicitly — `TestPublishUsesAuthenticatedActorAndKeepsArtifactInactive`, and
  the D7 loop in `TestPilotDynamicCatalogEndToEndRealMySQL` proves each step
  separately.
- [x] grant/revoke CAS has one winner; revoke tombstone prevents ABA; audit and
  state commit together — `TestStoreGrantConcurrentCASHasOneWinnerRealMySQL`,
  `TestStoreGrantLifecycleRealMySQL`.
- [x] capability and send use the same strong active/grant truth; stale explicit
  ref never switches version — `TestBotTemplateManifestAndSendAgree`,
  `TestBotTemplateSendRejectsStaleDynamicRef`,
  `TestAdvertisedSendRefsBatchesAndAgreesWithThePerIDPath` (the batched manifest
  reaches the same conclusion as the per-ID resolver).
- [x] Bot historical edit consults the runtime `edit` grant, not only the static
  allowlist — `TestBotEditResolvesADynamicRefThroughTheEditGrant` (granted,
  revoked, send-only and blocked, with the query pinned to the stored exact
  version), `TestBotEditKeepsStaticRefsAnsweringWithoutTheRuntime`. Two
  reviewers flagged this independently: `requireEditableRef` consulted only the
  boot-time `EditCompatible` map, which a runtime-published version can never
  be in, so `send` reached the dynamic authorizer and `edit` never did. The
  brief claimed a closure the code did not have until this was wired.
- [x] the ref gate and the render authorize against the *same* Space, so a
  per-Space grant works on a group or thread target —
  `TestGroupSendAuthorizesTheRenderWithTheSameSpaceAsTheGate`,
  `TestGroupEditAuthorizesTheRenderWithTheSameSpaceAsTheGate`. The round above
  ticked the edit line on the strength of the resolver alone, and both
  reviewers pointed out the witness stood at the layer where the bug was not:
  `renderPayload` rebuilt its `CatalogAccess` from `env.SpaceID`, which send.go
  populates for DMs only, so `cardtmpl.Render` re-resolved the grant in the
  global scope and refused every exact-Space grant the gate had just accepted.
  The render now receives the principal the gate decided on. Both tests assert
  the recorded `Access` directly *and* fail with `ErrRuntimeCatalogNotAuthorized`
  when the fix is reverted; `stubAuthorizationSource` gained a `grantSpaceID`
  because until then it ignored `query.Principal` and this whole class of
  divergence was unobservable in the package.
- [x] a Space that could not be established never reverts a shadowed template ID
  to its static version — `TestBotTemplateSendRefFollowsTheShadowMatrix`
  ("an unavailable Space does not revert a shadowed ID to static", plus the two
  rows that bound it: static still answers when nothing shadows the ID, and a
  Space-less target is still authorized by a global grant). `botCatalogPrincipal`
  carries three Space states rather than a bool, because "this target has no
  Space" and "we could not read which Space this target is in" were the same
  `false` and led to opposite correct answers.
- [x] every producer of that Space state is witnessed too, not just the pure
  decision that consumes it — `TestDMSendRequiresThePeerToBeInTheAuthorizedSpace`
  ("a Bot with no membership never becomes global-scoped on a DM"),
  `TestAGroupInADisabledSpaceHasNoReadableGrantScope`,
  `TestEditRefusesAFrameFromAnotherSpaceEvenWhenTheEnvelopeCorroboratesIt`. The
  round that added the row above ticked it on `decideSendRef` alone, and the
  reviewer's objection was that the witness stood one layer above the defect:
  the matrix was green while the *resolver* producing the state was wrong. Three
  producer-side defects were behind that line — membership absence read as "no
  Space" (which for platform App Bots, who never get a `space_member` row, made
  an `X-Space-ID` header the difference between seeing an exact revoke tombstone
  and not seeing it), a group resolver that alone among this file's four Space
  resolvers ignored `space.status = 1`, and an edit whose Space check could be
  satisfied by the frame's own `space_id` while its grant was read in the
  request's Space. Each has a test that fails when its fix is reverted.
  **Not closed by these**: the DM scope is still derived from the sender's
  request context rather than from the target, which is the asymmetry all three
  came out of. It is recorded in `HANDOFF.md` as a hard prerequisite for
  flipping the new-send gate, not as a merge item — it changes nothing while the
  gate is false.
- [x] one authorization decision never combines activation/block/grant values
  from different DB snapshots — `TestStoreLoadAuthorizationReadsPointerArtifactAndGrantInOneTransaction`,
  `TestRuntimeCatalogDynamicNewSendUsesExactlyOneSnapshot`,
  `TestStoreLoadAuthorizationSnapshotIsTheLinearizationPointRealMySQL`,
  `TestRuntimeCatalogRejectsIncoherentAuthorizationSnapshot`. B2 violated this
  until the third review pass — it read the discovery predicate and the
  authorizer separately — and now resolves once.
- [x] revoke send blocks subsequent new-send; revoke edit blocks subsequent
  edit/action; compiled cache cannot bypass —
  `TestRuntimeCatalogHotCacheCannotOutliveARevoke` (a hot entry answers the
  revoke from a fresh snapshot without reloading the bundle, and an edit-only
  grant does not authorize a send), `TestStoreLoadAuthorizationSnapshotIsTheLinearizationPointRealMySQL`,
  and the revoke arm of the D7 pilot loop.
- [x] rollback changes new-send only; old dynamic card remains same-version
  editable while edit grant exists — `TestRuntimeCatalogPinnedReadsNeverFollowActivationPointer`,
  and the rollback arm of the D7 pilot loop.
- [x] active dynamic same ID shadows static new-send for allowed, denied,
  blocked and DB-failure cases exactly as the D6 matrix specifies —
  `TestBotTemplateSendRefFollowsTheShadowMatrix` (all eleven rows),
  `TestDecideSendRefRefusesADisabledPointerFromTheBatch`.

### Discovery/export

- [x] B1 pagination is deterministic and bounded; blocked/unauthorized rows do
  not leak — `TestListDiscoverableFiltersBeforePagingRealMySQL` walks the whole
  listing at three page sizes over interleaved public/private/granted/blocked/
  tombstoned rows; `TestDiscoveryPageLimitBounds`,
  `TestListTemplatesPaginationIsBoundedAndResumable`.
- [x] hidden rows do not advance cursor or alter `has_more`; cursor cannot be
  replayed across Space contexts — `TestStaticPageCursorSkipsHiddenRowsWithoutCountingThem`,
  `TestListDiscoverableReportsMoreWithoutLeakingTheNextRowRealMySQL`,
  `TestDiscoveryCursorIsBoundToItsSpace`, `TestDiscoveryCursorRejectsWhatItCannotAccountFor`.
- [x] B2 projection contains only manifest/schema/reports/samples and is capped
  — `TestSafeExportOmitsTemplatesGoldensAndBundle`,
  `TestSafeExportRejectsUnknownExportKeys`,
  `TestSafeExportRefusesToBuildAnOversizedProjection` (the 2 MiB cap fails the
  build, not the response, so an oversized artifact cannot reach a client as a
  truncated body).
- [x] only explicitly allowlisted synthetic samples are exported; request
  handling never reads source files — `TestSafeExportShipsNoSamplesUnlessTheManifestOptsIn`,
  `TestSafeExportRejectsAnAllowlistNamingAMissingSample`,
  `TestStaticRegistryExportIsPrivateWithNoSamples`. The projection is built at
  registration/compile time, so the request path has no file access to make.
- [x] static/dynamic ETag is deterministic; `If-None-Match` returns 304 without
  body — `TestWriteExportServesETagAndA304WithoutABody`,
  `TestWriteExportFallsBackToTheExportHashForStaticTemplates`,
  `TestSafeExportHashIsDeterministicAndCoversVisibility`,
  `TestMatchesETagHandlesListsWildcardAndWeakValidators`.
- [x] private response is not shared-cacheable; no audit/grant/token/real sample
  data leaks — `TestWriteExportServesETagAndA304WithoutABody` asserts
  `Cache-Control: private, no-cache`; `TestDiscoveryResponsesCarryNoControlPlaneFields`.
- [x] all B1/B2 and Space-membership failures use the localized envelope; no raw
  non-OK JSON response remains — `TestCatalogNoLegacyErrorResponses` (source
  guard) plus `make i18n-lint`; the Space half is
  `TestLocalizedSpaceMiddlewareRejectsANonMemberOnTheEnvelope`,
  `...ReportsALookupFailureAsUnavailable`,
  `...RequiresALoginBeforeCheckingMembership`, and
  `...ResolvesTheSameSpaceAsTheLegacyOne` keeps the twin from becoming a second
  definition of the membership rule.

### Runtime and operations

- [x] gate-off deployment preserves current static Bot/profile/send/edit/action
  paths during DB outage — `TestRuntimeCatalogStaticExactNeedsNoSnapshot`,
  `TestBotCatalogPrincipalSkipsSpaceLookupWhenTheCatalogIsDark`,
  `TestDMPeerCheckIsSkippedWhenTheCatalogIsDark`,
  `TestRuntimeCatalogGatesFailClosedOnMissingAndInvalidValues`,
  `TestEditSpaceCheckSeparatesUnknownFromEmpty` (a dark gate is a configuration
  state, never an outage).
- [x] gate-on dynamic DB failure is observable and fail-closed without static
  same-ID fallback — `TestRuntimeCatalogDefaultDBFailureDoesNotFallbackStatic`,
  `TestBotTemplateManifestFailsClosedOnListError`,
  `TestAdvertisedSendRefsFailsClosedWhenTheBatchReadFails`,
  `TestStaticPageFailsClosedWhenTheGrantProbeFails`.
- [ ] two replicas with cold/hot cache converge after grant/activate/rollback/
  revoke and restart. **Partly covered**:
  `TestRuntimeCatalogMultiReplicaActivationRollbackBlockAndRestart` covers
  activate, rollback, block and restart across two stores on one database.
  Grant/revoke convergence is *not* covered and cannot be from a single
  container — the gap is the cache-invalidation window between replicas, which
  is the only place a revoked grant can still be honoured. Pre-merge operational
  prerequisite; see HANDOFF.md › Pre-merge operational prerequisites.
- [ ] dedicated non-production MySQL, Redis and WuKongIM pilot completes before
  OctoSpec finish/PR ready-for-review. **Not done.** The D7 loop in
  `pilot_mysql_integration_test.go` runs green here, but its exact-version
  preflight only interrogates real claims when `OCTO_PILOT_CATALOG_DSN` points
  at the dedicated deployment's catalog database; unset, the test says so out
  loud. Pre-merge operational prerequisite.
- [x] changed core packages meet at least 80% coverage; focused race suites pass
  — `modules/card_template_catalog` 80.6%, `pkg/cardtmpl` 81.2%, `pkg/cardmsg`
  85.1%; `pkg/space`'s new `localizedSpaceMiddleware` is 91.4% (the package
  total is dominated by pre-existing untested Redis cache helpers this PR does
  not touch). `go test -race` passes for `pkg/cardtmpl`, `pkg/cardmsg`,
  `pkg/space`, `internal/carddispatch`, `internal/cardactiondispatch`.
- [x] `go build ./...`, focused/full relevant tests, `go vet`, `golangci-lint`,
  `make i18n-extract-check`, `make i18n-lint`, source guards and
  `git diff --check` pass — all green on 2026-08-06; `golangci-lint run` reports
  0 issues across the six changed packages.

## Rollout / rollback

1. Deploy PR-C binary with control/new-send gates both false; run migrations and verify readiness/static traffic.
2. Non-production only: control=true, new-send=false; publish pilot inactive, create grants, verify B1/B2 and audit.
3. Read current activation. Unless active exact is already static known-good
   `docs.access-request@0.3.0`, explicitly activate it with the current expected revision to establish
   auditable rollback history; record the resulting revision and stop on any CAS conflict.
4. Activate pilot while new-send remains false; prove capability/send still blocked.
5. Set new-send=true only in the dedicated test environment and only after all replicas run PR-C.
6. Run the full interactive pilot and two-replica/restart/DB-failure matrix.
7. Rollback active pointer to the previously active static known-good, verify new static send, then revoke pilot
   send/edit grants. A rollback target with no prior activation audit is a test/setup failure, not a fallback case.
8. Return new-send=false, then control=false. Do not delete claim/artifact/grant tombstone/audit rows.
9. PR-C merge or non-production pilot does not authorize production. First production dynamic activation needs a
   separate change ticket, backup/restore proof, RPO/RTO, version matrix and go/no-go.
10. After any dynamic card is sent, rollback must use a binary retaining dynamic historical exact read/edit;
   never roll directly to pre-E3 code.

## Out of scope

- production dynamic activation or enabling production new-send/control gates;
- OpenClaw E1e stop/retry, reasoning successor controls, or #194 implementation;
- L2b `ext.*`, delegated owner publishing, Marketplace, visual editor, Bot self-service publish/activate;
- per-Space active version; v1 active pointer remains global;
- user/UID send grants; Space is discover-only;
- dynamic callback URL/secret/RouteSpec/finalizer creation;
- cross-version historical edit, artifact hard delete/GC, unblock, active/grant TTL cache or Redis truth;
- exporting templates/goldens/canonical bundle or production business samples through B2;
- E2 generic internal notify envelope or C3 generic approval migration.

## Decision confirmation

Accepted with the explicit instruction to start E3 PR-C implementation on 2026-08-05:

- [x] Accept D1: one PR-C milestone with six ordered test-first slices.
- [x] Accept D2/D4: template-ID grants, exact-over-global scope, typed permissions, tombstone + revision
  CAS and one-snapshot authorization linearization.
- [x] Accept D3: additive top-level `catalog_provenance`, raw ingress rejection, dynamic-only strict requirement.
- [x] Accept D5/D6: localized B1/B2, safe export/cursor rules, request-scoped Bot Space resolution and
  dynamic-over-static shadow matrix.
- [x] Accept D7: `docs-notify` + existing docs RouteSpec, including explicit static activation baseline,
  as the non-production interactive pilot.

## Plan refinement log

### 2026-08-05 round 2 — repo-doc cross-check

对照父 brief（`.octospec/tasks/cardtmpl-runtime-catalog/brief.md`）、
`docs/platform-card-base.md`、`docs/card-protocol.md`、`docs/l2b-owners.md` 与
`main@40627cc0` 代码复核后落入本 brief 的五项细化。均为收敛/补缺，不改变已确认的
D1–D7 语义：

1. B1 字段补 `action_contract`（platform-card-base §9 的 `actionType` 能力发现承诺；
   跨仓 SDK 生成依赖它对齐 callback 路由三元组）。
2. static visibility/export 注入点定为 composition root Go 侧（`main.go` static
   CatalogMeta），不回改冻结 L1 manifest；PR-C 不改变现有 static 卡 visibility（保持
   private fail-close）；static B2 samples 恒空，dynamic 由 bundle manifest allowlist
   承载，pilot bundle 必须证明导出通路。背景：全部 10 个生产 handoff manifest 均无
   visibility/allowlist 字段，若无此步 B1/B2 对普通调用方为空壳。
3. 补父 brief 控制面表中缺失的 `GET /v1/manager/card-templates` 只读列表端点
   （grant 分散到多 principal 后，运维需要可枚举的权威清单入口）。
4. B1/B2 wire status 定为 `httperr.ResponseErrorL`（pinned-400），与模块既有 9 个
   responder 同契约；不引入 `ResponseErrorLWithStatus`。
5. Slice 6 文档收口清单显式化（platform-card-base §9/§10、card-protocol §1/§4、
   runbook 双 owner 清单差异、L2b 门槛 ④ 不推进声明）。

另记两条既定实现口径（前轮已确认，此处固化）：robot legacy ingress 对
`template_ref`/`catalog_provenance` 用显式 reject（新增独立函数，不复用也不改变
`__obo_*` silent-strip 的既有语义）；pilot exact version 仍以非生产 DB preflight 为准，
`bundle.json` 目录名在 preflight 后才落盘。
