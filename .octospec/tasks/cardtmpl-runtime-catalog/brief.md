---
type: Task
title: "Task: cardtmpl-runtime-catalog"
description: Add an audited, immutable, database-backed runtime catalog for JSON card templates, with super-admin validate/publish/activate/rollback controls and visibility-aware discovery, without weakening the frozen built-in Registry or producer authorization boundaries.
tags: [card, cardtmpl, json-template, catalog, control-plane, database, wire-contract, trust-boundary, auth, acl, space, rate-limit, observability, testing]
timestamp: 2026-07-27T23:36:03+08:00
# --- octospec extension fields ---
slug: cardtmpl-runtime-catalog
upstream: "roadmap E3; PR-A tracked by #669; follows E1 JSON engine (#654), E1b Bot consumption (#659), and E1c successor (#667)"
source: self
---

# Task: cardtmpl-runtime-catalog

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the design
> and acceptance contract for roadmap E3. AI may draft it from current code; a
> human confirms the decisions in the final section before implementation.
> D1–D9 were accepted on 2026-07-28 when implementation was authorized; delivery
> remains split into PR-A/PR-B/PR-C, and the current branch starts with PR-A only.
> PR-A implementation is tracked by Issue #669 and Draft PR #670.

## Implementation status (2026-07-28)

- **PR-A is implemented in Draft PR #670; CI and review are pending.** It includes the shared strict
  compiler, canonical artifact identity, immutable version claim/artifact/audit store, startup static
  inventory reconciliation, super-admin validate/publish APIs, localized errors, body/rate limits, and
  bounded control-plane metrics.
- Focused unit, coverage, race, build, vet, lint, and i18n checks pass locally. `pkg/cardtmpl` coverage is
  80.8% and `modules/card_template_catalog` coverage is 82.5%.
- The database-independent Bot catalog race lane passes. The local Bot profile integration lane is blocked
  before assertions by a stale shared test-database migration record
  (`20191106000001_event_legacy01.sql`); clean CI remains the authoritative integration result.
- PR-A artifacts are always inactive. Runtime overlay, activate/rollback/block, grants, B1/B2, and any
  production dynamic send remain PR-B/PR-C work and are not advertised as available.

## Goal

让已经通过平台治理的 **纯 JSON 卡片模板**无需重新构建、发布和部署 octo-server，
即可完成：

1. dry-run 校验；
2. 发布不可变的 `template_id@version` 制品；
3. 为新消息原子切换 active version；
4. 回滚到任一仍可用的历史版本；
5. 通过 B1/B2 发现或导出调用方有权看到的模板契约。

E3 交付的是生产级 **模板控制面 + 运行时 catalog**，不是“上传任意 JSON 并立即发卡”。
必须保持现有 L0 不变量：server-authoritative、`Registry.Render`/`renderCore` 统一验证、
L1 版本冻结、producer 授权、Space/owner 隔离、历史消息固定原 `id@version`。

本任务不直接打开任何生产 Bot/卡片开关。动态模板首次生产激活是独立 go/no-go，
必须在本任务全部验收完成后执行。

## Success definition

以下四件事必须同时成立，才算 E3 闭环：

- **制品闭环**：不可信 bundle 经过同一编译/校验管线，失败不会入库为 published；
- **运行闭环**：任意副本可按显式 `id@version` 载入并渲染，active 切换不会冻结历史消息；
- **治理闭环**：publish、activate、rollback、grant、emergency block 都有权限、CAS 和审计；
- **生产闭环**：多副本、重启、DB 故障、回滚、缓存冷启动和权限隔离均有自动化或演练证据。

## Background / current gap

- E1 已提供 `Registry.RegisterJSON` 和受控 `${}` 模板引擎；JSON 模板会经过 schema、
  aggregate constraints、模板 AST、sample self-check、interaction conformance 和
  `cardmsg.Validate`。
- 当前所有模板仍由 `//go:embed` 在 composition root 注册，随后 `Registry.Freeze()`；
  新模板或新版本仍要改 Go 仓库、发 server 镜像。
- `Registry.List()` 已存在，但 B1/B2 HTTP route 尚未实现；它当前只能列出二进制内置模板。
- L2b `ext.*` owner、private visibility、owner 授权和独立 callback route 仍未开放。
- Bot Registry catalog 是显式授权边界，不能把“存在于 runtime catalog”误当成
  “任意 Bot 可以 discover/send/edit”。

## Core model: publish != activate != authorize

三个动作必须分开，且任何一步都不能隐式完成下一步：

| 动作 | 含义 | 不产生的副作用 |
| --- | --- | --- |
| **publish** | 校验并持久化一个不可变 `id@version` | 不改变 active version，不向 producer 授权，不发消息 |
| **activate** | 修改某 template ID 的新消息默认版本 | 不覆盖旧制品，不迁移历史消息，不自动授权 producer |
| **grant** | 允许一个受信 principal discover/send/edit 某 template ID | 不改变 active version，不放宽 owner/callback/Space 校验 |

历史 edit 使用 stored、server-authored `id@version`；active pointer 只影响新消息/default
resolution。rollback 只是再次修改 active pointer，不删除任何制品。

## Architecture decision

### 1. Keep the built-in Registry frozen

禁止给现有 `Registry` 增加运行期 `Register`/`Unfreeze`：

- `go:embed` 内置模板继续在启动期 fail-close，并在 `Freeze()` 后不可变；
- 新增 `RuntimeCatalog`（名字可实现时微调）作为静态 Registry 的 overlay；
- 精确 `id@version` 在 static + dynamic 联集中必须唯一；动态制品不得覆盖同名内置版本；
- 该唯一性不能只检查“当前进程看见的 Registry”：MySQL 必须持久化 static/dynamic
  version claim。启用 RuntimeCatalog 的进程在进入 readiness 前，原子登记/核对本镜像的 static
  inventory；若某 exact key 已被 dynamic 占用则启动失败，禁止滚动发布后不同副本解析出不同内容；
- exact resolution 若仍检测到双源冲突，必须报 catalog integrity error，绝不能以“static 优先”
  静默选一个；
- active pointer 可以指向内置或动态版本，便于随时回滚到镜像内置的已知良好版本；
- overlay 对两种 source 统一暴露 server-authored `CatalogMeta`（source/engine/visibility/export hash）。
  static metadata 由 composition root 明确注入，未配置 visibility 时按 private fail-close，不能因为
  旧 manifest 没有该字段就默认公开；
- 现有调用方可逐步从 `*Registry` 迁到窄接口，不能一次大改所有卡片链路。

建议运行结构：

```text
                       +---------------- Built-in Registry (frozen)
caller -> RuntimeCatalog.Resolve/Render
                       +---------------- Dynamic Store (MySQL)
                                           |
                                           +-> bounded compiled-artifact cache
```

运行时消费者不能继续绑定具体 `*Registry`。应抽只读窄接口（名字可微调），至少覆盖 exact/default
render、metadata、`ActionView` 和 list/export；built-in `Registry` 与 `RuntimeCatalog` 都实现它。
`Register/RegisterJSON/SetDefault/Freeze` 仍只存在于 built-in Registry，不进入运行时接口。以下
load-bearing 消费点必须迁移，不能只改 Bot send：

- `modules/bot_api` template send/edit/capability；
- `pkg/cardtmpl.CardUpdater` 及 notify finalizer/mutate path；
- `modules/message.resolveRegistryCardContext` 的 `ActionView` + `ActionContract` 查询；
- B1/B2 discovery/export。

否则 dynamic interactive card 会出现“能发送，但 Action.Submit 或后续 edit 找不到模板”的半闭环。

授权不能继续靠每个 handler 手工“先查 grant、再调无身份 `Render`”。运行时接口必须区分
`new_send|historical_edit|action_context|discover|export` purpose，并接收由鉴权层构造的 typed principal
（kind/id/authoritative Space）；不得从 caller JSON、query 或任意字符串 context 取 principal。
底层无授权 exact resolver 仅供 package 内 compiler/control/readiness 使用，不对业务模块导出。
static compatibility path 仍由现有显式 producer policy 授权，dynamic path 则在同一次 runtime
resolution 中原子检查 grant/active/block，避免 TOCTOU 或漏查绕过。

### 2. MySQL is authoritative; caches hold immutable compiled artifacts only

- MySQL 是 version claim、artifact bytes、active pointer、grants、block 状态和 audit 的唯一真源；
- local cache 只缓存按 `engine_contract + content_sha256` 编译完成的不可变 Template，不拥有
  active/grant/block 真相；
- v1 的 new-send/default resolution 和 capability/discovery 权限判断读权威 DB 状态，
  不用仅靠 Redis pub/sub 或进程 TTL 猜 active pointer；
- dynamic exact-version render/edit 每次也必须从权威 DB 读取最小 artifact/block 元数据，再复用
  compiled cache；否则紧急 block 会被热 cache 绕过；
- active/grant/block 等授权读必须走具备 read-after-write 的主库/强一致连接，不得走可能延迟的
  MySQL replica；new-send 可用一次 join 查询合并 active + artifact + grant 检查；
- cache miss 用 `singleflight` 合并同一 `id@version` 的并发编译；缓存必须有 entry/byte 双上限；
- 任意副本在 publish commit 后都能从 MySQL lazy-load、验 hash、编译并服务显式版本，
  不依赖“恰好收到一次缓存失效消息”；
- Redis 事件可作为后续预热优化，但不得成为正确性或重启恢复的唯一机制。

这个选择有意接受 dynamic new-send/edit 多一次小型 catalog DB 查询，换取 v1 多副本一致性和
emergency block 可证明。
若未来性能数据证明需要 active/grant cache，必须引入单调 catalog revision、最大陈旧窗口和
跨副本回归，不能在本任务中提前做无法证明的最终一致缓存。

### 3. JSON-only, frozen engine contract

E3 v1 的发布 envelope 只接受 `engine = "octo-json-template/v1"`：

- 不接受 Go、JavaScript、WASM、脚本表达式、远程 include 或任意代码；
- 不从 URL、对象存储路径、Git ref 或本地绝对路径加载模板内容；
- bundle 中的 JSON Schema `$ref` 只能指向 bundle 内明确允许的资源；禁止网络/file `$ref`；
- 模板语法仍是 E1 已冻结的 ACT 子集；扩语法先发 server，再考虑发布依赖新 engine contract
  的制品；
- 混合 server 版本 rollout 期间，只允许激活所有副本都已支持的 engine contract。

## Bundle contract

控制面接收结构化 JSON bundle，不接收 zip/tar，避免 path traversal、zip bomb、重复 entry
和解压后尺寸不确定性：

```jsonc
{
  "catalog": {
    "engine": "octo-json-template/v1",
    "visibility": "private"
  },
  "manifest": { "...": "manifest.json content" },
  "schema": { "...": "contract/data.schema.json content" },
  "templates": {
    "active": { "...": "templates/active.template.json content" }
  },
  "reports": {
    "active": { "...": "reports/active.interaction.json content" }
  },
  "samples": {
    "reasoning": { "...": "samples/reasoning.json content" }
  },
  "goldens": {
    "reasoning": { "...": "goldens/reasoning.card.json content" }
  }
}
```

`catalog.engine` / `catalog.visibility` 是运行时发布治理元数据，不写回现有 handoff
`manifest.json`。这样 `0.1.0` 等已冻结 manifest 无需原地改版，static fixture 也可套同一
envelope 进入 compiler 做 parity test。owner、protocol、version、views 等模板契约仍以
manifest 为唯一真源，禁止在 envelope 重复一份可漂移字段。

static JSON template 使用 composition-root `CatalogMeta` 补齐同样的 engine/visibility，并对其可导出
manifest/schema/reports/samples 计算 canonical export hash；不修改 embed bytes。B2 的 ETag 对 dynamic
使用 `content_sha256`，对 static 使用该 export hash。

v1 proposed hard limits（实现前由 D3 最终确认）：

| 项目 | 上限 | 理由 |
| --- | ---: | --- |
| HTTP request body | 2 MiB | 当前最大 handoff 约 129 KiB，保留充足余量，同时限制解析前内存 |
| canonical bundle bytes | 2 MiB | 入库、hash、审计和 cache 可控 |
| logical documents | 128 | 防病理 map/file fan-out |
| single document | 512 KiB | 与现有 card payload 量级一致 |
| views / states | 16 / 64 | 平台卡状态机有界，避免 capability/validation 爆炸 |
| samples / goldens | 64 / 64 | 每个 state 至少一份 sample，仍保持 CI/发布耗时可控 |

规则：

- server 对完整 catalog descriptor + documents 做版本化、确定性的 canonical JSON（建议固定
  RFC 8785/JCS 兼容规则）再计算 SHA-256；hash 不信任调用方传值。canonicalization 规则属于
  engine contract，不能在同一 contract 下随 server 版本变化；
- decoder 必须拒绝 duplicate object key、trailing JSON token、非法 UTF-8 和非有限/越界数字，
  不能沿用 `encoding/json` 的 last-key-wins 作为签名/hash 语义；
- document key 只能使用有界安全 token；manifest 明示或按 view/key 约定推导的
  schema/template/report/sample/golden 必须全部存在且只能指向 bundle 内文档，unknown/unreferenced
  document fail-close；
- 同一 `id@version + same hash` publish 是幂等成功；同一 `id@version + different hash`
  永久冲突，不允许“管理员覆盖”；
- catalog descriptor 必须显式包含 engine、visibility；manifest 必须显式包含 id、owner、
  protocol、version、views；
- `visibility` v1 仅允许 `public|private`；unknown value fail-close；public 仅代表可发现，
  不自动产生 send/edit 权限；
- root schema 必须 `type=object`、`additionalProperties=false`；自由 string/array 必须由
  `maxLength`/`maxItems` 或枚举等价有界，不能只依赖最终 payload cap；
- 每个 manifest state 至少一份 sample；每份 sample 必须 schema-valid、可展开、可通过
  `cardmsg.Validate`；golden 若存在必须 canonical 相等；
- v2 view 必须有 interaction report；实际 action/input/report、owner/action_type 全量一致；
- aggregate constraints、node/depth/expanded-node/payload budgets 与 E1/E1c 共用实现；
- samples/goldens 必须是可导出的合成数据，不得包含 token、真实用户数据或生产业务秘密；
- 错误响应只暴露安全的 validation category/document key，不回显 bundle、sample、schema
  内容或完整 caller data。

## Validation/compiler refactor

当前 loaders 绑定 `embed.FS` 且用 panic 表达启动失败。E3 不能通过 `recover` 把任意 panic
当普通校验错误；应先抽一个共享、显式返回 error 的 compiler：

```go
type CompiledArtifact struct {
    Template Template
    Meta     TemplateMeta
    Hash     string
    Bundle   CanonicalBundle
}

func CompileJSONArtifact(ctx context.Context, bundle Bundle, limits CompileLimits) (*CompiledArtifact, error)
```

- static `RegisterJSON` 调 compiler，遇 error 继续 panic，保留 boot fail-close；
- runtime validate/publish 调同一 compiler，返回 typed validation error；
- compiler 不访问 DB、网络或进程环境；有 context deadline 和显式资源预算；
- validate/publish/runtime lazy-load 的所有 compile 入口共享进程级 bounded semaphore；队列满或
  deadline 到期时快速失败，避免 `SharedUIDRateLimiter` 的 burst 或冷 cache 同时展开大量 bundle；
- static embed fixture 与同内容 runtime bundle 必须生成 canonical 相同的 Meta/render 结果；
- `renderCore` 仍是 metadata 注入、profile、URL、白名单和最终 `cardmsg.Validate` 的唯一真源。

## Persistence model

建议新增独立 module `modules/card_template_catalog/`（最终目录名可按 maintainer 习惯微调），
包含以下表：

所有 identity 字段使用明确长度、大小写敏感 collation 和 canonical version 格式；不依赖数据库
默认的大小写折叠。所有状态写与对应成功 audit 必须在同一事务提交，audit 写失败则状态不变。

### `card_template_version_claim`

跨 static/dynamic 的 exact-key 占位表：

- `(template_id, version)` primary key，`source=static|dynamic`；
- dynamic publish 必须先原子取得 dynamic claim；任一历史 static claim 永久禁止被 dynamic 复用；
- RuntimeCatalog 启动时将 `Registry.List()` inventory 登记为 static claim；撞到 dynamic claim 时
  readiness fail-close，并输出不含 bundle 的 integrity 告警；
- claim 不提供 DELETE/改 source API。即使某旧镜像暂时不再注册该 static version，也不能把
  exact key 回收给另一份内容。

### `card_template_artifact`

不可变内容与身份：

- `(template_id, version)` primary/unique key，并 FK/等价事务约束到 dynamic version claim；
- owner、visibility、engine_contract、protocol、contract_version；
- canonical_bundle（MEDIUMBLOB/等价有界字段）和 `content_sha256 CHAR(64)`；
- created_by、created_at；
- operational block 是单向状态：首次 `blocked_at/by/reason` 写入后不可 unblock/改写；v1 如需恢复，
  发布新 version。canonical bytes/hash 永不更新；
- 对已 blocked artifact 重放 same-hash publish 只返回“已存在且 blocked”，不得借幂等 publish 清除状态；
- 不提供 DELETE/UPDATE bundle API。

### `card_template_activation`

- `template_id` primary key；
- nullable `active_version` + 显式 `status=active|disabled`；active source 通过 version claim join 派生，
  不冗余存两份可漂移字段；active version 以 FK/等价事务约束指向同 template ID 的 claim；
- 单调 `revision BIGINT`；
- updated_by、updated_at、reason/change_ticket；
- v1 只做 global active pointer，不做 per-Space 不同 active version；Space 只参与 grant/visibility；
- activation row **不存在**时可沿用 built-in Registry 的 static default；row 显式 disabled 时必须
  返回 unavailable，不能悄悄 fallback 到 static default；dynamic template 不存在 activation row
  时不可 new-send；
- activate/rollback 使用 `WHERE revision = expected_revision` CAS，冲突返回语义 409。

### `card_template_grant`

- `(template_id, principal_type, principal_id, scope_space_id)` unique；所有 key 列 non-null，global
  scope 使用唯一 canonical sentinel，避免 MySQL nullable unique key 允许多条 `NULL`；
- v1 principal type 至少支持 `bot`、`internal_producer`、`space`；
- 权限只允许有界集合 `discover|send|edit`；不得存任意字符串权限。`send` 只允许当前 active
  version，`edit` 只允许 target 中 server-authored provenance 指向的 same version；
- `space` principal 只授予 discover；send/edit 只能授予具备可认证 producer identity 的
  `bot|internal_producer`，不能把“Space 成员”偷换成发卡主体；
- active 切换不得自动撤销旧版本 edit；但撤销 principal 的 `edit` 权限应立即阻止后续 edit。
  此外 sender/owner/Space/lifecycle/CAS 仍需全部通过；
- grant 写入/撤销必须审计，且不能替代既有 Bot ownership 或 callback RouteSpec。

### `card_template_audit`

append-only 记录：validate（可只打日志/指标）、publish、activate、rollback、grant、revoke、block。
至少包含 actor UID、operation、template ID/version、hash、old/new revision、reason/change ticket、
result、timestamp；不保存 token、完整 bundle 或业务 sample 内容。

## Authorization and visibility

### Control plane

v1 控制面只开放给现有 `superAdmin`：

- route group：`/v1/manager/card-templates`；
- `AuthMiddleware` → `SharedUIDRateLimiter` → handler 内 `CheckLoginRoleIsSuperAdmin()`；
- 这是显式的全局 superAdmin 控制面，不以请求当前 Space 作为授权范围，因此不套普通业务
  Space middleware；涉及的 target Space 必须按 body/path scope 单独校验存在性并写审计；
- body cap 必须在 `BindJSON` 前安装；
- 不接受 Bot token、internal notify token、匿名请求或“manifest.owner 自声明即授权”；
- actor UID 从服务端认证上下文取，不接受 body 中的 created_by/approved_by；
- publish/activate 还要通过服务端 owner policy：v1 只允许已批准的 L2a owner；`ext.*` 和未知
  owner 在 D1/D2 gate 打开前 fail-close，display-only template 也不例外；
- 所有错误走 `httperr.ResponseErrorL` + `pkg/errcode` localized envelope；若要使用真实 HTTP
  status，须另行获得 maintainer 对新端点偏离 D14 的明确确认。

这一步实现“无需 server 发版”，但不是最终 L2b 自助发布。owner delegated publisher、双人审批、
external owner 与独立 callback secret 必须在 D2/L2b gate 满足后再开放，不能为了 E3 降低门槛。

### Read side (B1/B2)

- `GET /v1/message/card/templates`：Auth + UID limiter + Space middleware；分页、有上限；
- `GET /v1/message/card/templates/{id}@{version}`：同一鉴权链；
- public：返回给有正常卡片能力的已认证调用方；
- private：仅 superAdmin 或命中当前 Space/principal grant 的调用方可见；
- B1 对普通调用方只列 visible + unblocked 的 exact versions，并明确 `active_for_new_send`；
  “能发现”不等于“能发送”。blocked 制品和控制面状态只在 manager API 可见；
- unauthorized 与 nonexistent 对普通调用方使用同一 not-found 语义，避免 template 枚举 oracle；
- B2 返回 manifest/schema/reports/samples，不返回控制面 audit、grant、内部 DB id；
- B2 response 带 source-specific canonical export hash ETag（dynamic 为 `content_sha256`）并实现
  `If-None-Match`；private 响应使用
  `Cache-Control: private`，不得进入 public shared cache；
- Bot 模板发现仍以 `/v1/bot/card/profile` 的 bot-scoped catalog 为权威，不让 Bot 直接调用
  B1/B2 绕过 grant。

## Runtime resolution and producer authorization

### Exact version

`Resolve(id, version)` 通过 version claim 确定唯一 source；static/dynamic 任一路径都返回同一
Template/Meta 抽象并走 `renderCore`。不得用 lookup 顺序掩盖双源冲突。dynamic 路径在命中 compiled
cache 前仍要读取 DB block 元数据。static exact key 则使用已在 readiness 阶段核对过的 frozen
in-memory inventory，运行中不为每次 render 查询 DB。历史消息必须始终显式使用 stored version，
不重新解析 active。blocked 是唯一安全例外，会拒绝继续 render/edit，但已存储的最后成功卡片
仍可正常展示。

### Default/new send

只有新消息/default resolution 才读取 `card_template_activation`。activation row 不存在与显式
disabled 是两个不同状态，后者不可 fallback。新 send 必须同时满足：

1. effective active resolution（activation row，或仅 static 的 legacy default）指向该版本；
2. exact source 可解析；若为 dynamic，则 artifact 存在、hash 正确、未 blocked、engine 支持；
3. static template 命中既有显式 producer policy，或 dynamic template 命中 principal 的 `send` grant；
4. 既有 producer-specific ownership、Space、channel、feature gate 全部通过。

catalog 存在本身绝不是发送授权。现有 Bot E1b 路径迁移时：

- 当前代码内 `AdvertisedSend/EditCompatible` 继续约束 static templates；dynamic templates 使用
  DB grant，最终 capability 是两者安全合并，不把 `Registry.List()` 全量授权给 Bot。对已有
  activation row 的 template ID，new-send 只广告 effective active version：dynamic active 不得
  同时残留 static `AdvertisedSend`，rollback 到 static 后再按 static policy 恢复；
- `/v1/bot/card/profile` 仍走 bot-token auth，不套用户 Space middleware；runtime 必须从既有 Bot
  identity/ownership 取 authoritative bot ID + Space，不能信 query/body 自报 principal。由于它从
  部署常量变成 per-Bot DB read，route 应升级为 `authBot → botActorUID → SharedUIDRateLimiter`；
- `templating.templates` 的 dynamic 部分只从该 Bot 的 `send` grant + active unblocked version 生成；
  historical stored version 的 edit 还要求 `edit` grant，保持 same-version，不能用 active pointer
  跨版本改写；
- profile/capability 与 send 都读同一权威 revision。active 在二者之间变化时，旧显式 ref 安全失败，
  producer 刷新 capability 后重试，不允许 server 偷换版本。
- RuntimeCatalog 的 not-active/not-granted/blocked/internal typed errors 必须映射到既有 Bot API
  card-invalid/catalog-unavailable error facade，不向 producer 暴露 grant、owner 或 artifact 存在性。

### Interactive templates

interactive artifact publish 可以通过静态契约校验，但 activate 前必须确认其
`(owner, action_type)` 对应的 `cardactiondispatch.RouteSpec` 已存在且 owner 策略允许。
E3 不动态创建 callback URL、secret 或 finalizer。route 缺失时 activation fail-close；catalog-enabled
进程启动时也要复核所有 active artifacts 的 engine/owner policy，以及 interactive artifact 的
RouteSpec，避免未来 binary 删除能力或 route 后仍 ready。
Action ingress 必须从 effective stored frame 的 server-authored `id@version` 调 RuntimeCatalog
`ActionView/Meta`，再执行现有 owner/route/Space 校验；不得信点击请求自报 template identity。

## Control APIs

建议 v1 API（字段可在实现 brief refinement 时机械微调，语义不得合并）：

| Method/path | 语义 |
| --- | --- |
| `GET /v1/manager/card-templates` | 分页列出全部 source/visibility/active/block 摘要，含普通 B1 隐藏项 |
| `GET /v1/manager/card-templates/{id}` | 返回 versions、source、block、active revision 和 grants，供 CAS 操作前读取 |
| `GET /v1/manager/card-templates/{id}/audit` | 分页读取该 ID 的脱敏 append-only 操作记录 |
| `POST /v1/manager/card-templates/validate` | dry-run compile/conformance，返回 server-computed hash 和安全摘要，不持久化 |
| `POST /v1/manager/card-templates/publish` | 重跑完整校验并插入 immutable artifact；exact key + server hash 天然幂等 |
| `PUT /v1/manager/card-templates/{id}/active` | CAS 激活指定 published/unblocked version |
| `POST /v1/manager/card-templates/{id}/rollback` | CAS 切回明确 target version，单独 audit operation |
| `POST /v1/manager/card-templates/{id}@{version}/block` | 单向阻断 dynamic artifact；若当前 active，同事务切到明确 fallback，或将该 ID 置 disabled |
| `PUT /v1/manager/card-templates/{id}/grants/{principal_type}/{principal_id}` | 创建/收敛 discover/send/edit grant；可选 Space scope |
| `DELETE /v1/manager/card-templates/{id}/grants/{principal_type}/{principal_id}` | 撤销 grant，不删除 artifact/历史消息 |

写接口要求 `reason`；activate/rollback/block 额外要求 `expected_revision`，建议要求
`change_ticket`。v1 不增加通用 Idempotency-Key store：publish 由 immutable exact key + hash 天然
幂等，pointer 写由 revision CAS 防并发覆盖。以下重试/冲突语义必须测试：

- same id@version + same server hash → publish success/idempotent；
- same id@version + different hash → immutable conflict；
- stale expected revision → conflict，不能 last-write-wins。

block 是安全操作，不能因为“没有已知良好 rollback target”而被拒绝；此时必须原子 block +
disable active，让新 send 明确不可用。v1 不提供 unblock；误封只能发布新 version 并重新激活。

## Failure semantics

- validation error：400，零 claim/artifact/active/grant 状态变化；允许追加不含 bundle/data 的
  失败审计摘要；
- immutable/CAS conflict：语义 409，经 D14 error envelope 返回；
- unauthorized/not visible：generic forbidden/not-found，不暴露 owner、hash、version 是否存在；
- DB/cache/compile internal failure：5xx internal code，服务端日志记录 cause；响应不回显 bundle；
- dynamic DB unavailable：dynamic new send、activation、B1/B2 fail closed；不能静默换成另一个版本；
- historical dynamic edit 失败时保留最后成功帧并返回可重试错误，不能 raw overwrite；
- 显式 static exact-version render 不依赖 dynamic tables；需要判断 activation/disabled 的 default
  resolution 在 DB 不可用时仍须 fail closed，不能把“查询失败”当成“activation row 不存在”；
- blocked version 不允许 new send/render/edit；已存储的旧卡片 payload 仍可展示。所有后续 edit
  拒绝并告警，这是安全 kill switch，不能被 local cache 绕过。

## Multi-replica consistency

必须用自动化测试证明以下序列：

1. replica A/B 启动时登记相同 static inventory，并复核 active engine/owner/RouteSpec；claim 冲突或
   active contract 不可服务的 replica 不 ready；
2. replica A publish；replica B 无本地 cache；
3. A activate revision N；
4. B 从强一致 DB 读取 active/grant/block，lazy-load artifact、验 hash、compile 后成功 new send/render；
5. 已有旧卡仍按旧 version + edit grant edit；
6. A rollback 到旧 version revision N+1；
7. B 的新 send 立即按 DB 权威指针选择旧版，动态新版 cache 即使仍在也不能被默认选择；
8. A block 当前 active；有 fallback 时原子切换，无 fallback 时原子 disabled；B 的热 cache 不能继续 render；
9. 进程重启、cache 全空后可从 DB 恢复同一行为。

v1 不以“所有副本收到 pub/sub ACK”作为 correctness 条件。若后续引入 active cache，必须另起
L0 brief，定义 revision fence、最大陈旧窗口和 capability→send 不错配证明。

## Observability and audit

新增低基数指标（确切名字实现时统一）：

- `dmwork_card_catalog_operation_total{operation,result}`；
- `dmwork_card_catalog_resolve_total{source=static|dynamic,result}`；
- `dmwork_card_catalog_compile_seconds{result}`；
- `dmwork_card_catalog_cache_total{result=hit|miss|evict}`；
- `dmwork_card_catalog_db_total{operation,result}`。

日志/审计可带 template id/version、hash、revision、actor UID、principal ID、Space ID；不得带
token、完整 bundle、完整 schema/sample、卡片 data 或业务敏感文本。至少对以下情况告警：

- active artifact 无法 compile/hash mismatch；
- activation/rollback/block 失败；
- dynamic DB 持续不可用；
- CAS conflict 异常升高；
- blocked version 仍收到 send/edit；
- static/dynamic claim 冲突或启动 inventory reconciliation 失败；
- cache 编译耗时或内存超过预算。

## Implementation slices

本任务不应以一个超大 PR 落地，建议三个可独立评审、在首次 dynamic send 前可回滚的切片；
首次 dynamic send 后受下文 binary compatibility floor 约束：

### PR-A — compiler + immutable store + validate/publish (no runtime activation)

- 抽共享 error-returning compiler，static RegisterJSON 行为/输出零回归；
- schema bytes 只经一个 loader/parse 结果进入 JSON Schema compile 与 `x-octo-constraints`
  解析，禁止 `contract/data.schema.json` 路径和读取逻辑双写；
- Go-authored `Registry.Register` 若发现仅 JSON compiler 能执行的 `x-octo-constraints`，必须
  注册期 panic/fail-close，禁止 silent no-op；
- 用 table-driven fixtures 覆盖全部约束误配置分支：unknown key、空白/未 trim parent/child、
  non-positive limit、duplicate/empty list、parent/child missing 或类型错误、invalid sub-schema；
- module/migrations、version claim + static inventory reconciliation、store/audit；
- superAdmin validate/publish endpoints、body cap、rate limit、i18n errors；
- artifact 只能 inactive，生产 render 路径不读动态表。

### PR-B — runtime overlay + activate/rollback/block

- RuntimeCatalog exact/default resolution；
- 抽 runtime Catalog 窄接口并迁移 Bot send/edit、CardUpdater、message action-context 等 load-bearing
  消费点；built-in registration API 保持隔离；
- 双源 exact-key 冲突在 runtime resolution 继续 fail-close；
- bounded compiled cache + singleflight；
- CAS active pointer、rollback、emergency block；
- static/dynamic parity、多副本、restart/DB failure tests；
- control/new-send kill switches，历史 exact-version resolution 不随意关闭。

### PR-C — grants + B1/B2 + one non-production pilot

- principal/Space discover/send/edit grants；
- visibility-aware B1/B2、pagination/ETag/anti-enumeration；
- Bot static policy + dynamic grants 的安全合并；一个现有 producer 通过显式 grant 消费 dynamic
  active version；
- `AdvertisedSend` 必须按 template ID 强制最多一个 version，不能只按完整 `id@version` 去重；
  duplicate ID 在 capability 构造期 fail-close 并有回归测试；
- 非生产 publish→activate→send→Action.Submit(existing RouteSpec)→same-version edit→rollback E2E；
- 生产首次激活仍由单独 rollout 工单批准。

任何切片都不能把未实现的下一阶段伪装成已支持：PR-A 合并后仍不能动态发卡；PR-B 合并后
未授权 principal 仍不能发现/发送；PR-C E2E 通过也不等于开放 L2b。

## Acceptance

### Compiler / trust boundary

- static embed 与同内容 runtime bundle 的 Meta、interaction、sample、render canonical 相等；
- malformed/duplicate-key/trailing-token JSON、schema/template/report/sample/golden 全部 typed error，
  不 panic 服务进程；
- static `RegisterJSON` 与 runtime compiler 共用一次 schema load/parse；Go-authored Register 遇到
  `x-octo-constraints` 必须拒绝，不能静默忽略；
- aggregate constraint 的 unknown/empty/untrimmed/non-positive/duplicate/missing/wrong-type/invalid
  sub-schema 误配置分支全部由 table-driven RED→GREEN fixtures 锁定；
- remote/file `$ref`、unknown engine/directive、unbounded schema、超 body/doc/count/node/depth/
  expansion budgets 均 fail closed；
- Action.Submit、Input、inlineAction/Table 等完整遍历面与现有 conformance 一致；
- fuzz/property tests 覆盖深嵌套、重复 key、astral Unicode、巨大数字、恶意 URL/markdown。

### Immutability / state transitions

- same hash publish 幂等；different hash same id@version 永久冲突；
- static/dynamic version claim 全局唯一；未来镜像若引入已被 dynamic 占用的 exact key，readiness
  必须失败，不能产生按副本漂移的解析结果；
- publish 不改变 active/grants；grant 不改变 active；activate 不创建 grant；
- publish/activate/rollback/grant/revoke/block 的成功 audit 与状态在同一事务；revision CAS 并发写
  只有一个成功；
- 不存在 artifact/claim hard-delete 路径；未 blocked 历史版本可显式 lookup/render；
- block 是单向的；active version 要么与 fallback 切换同事务成功，要么与 disabled 同事务成功，
  不能因缺少 fallback 留下一个仍可发送的已知危险版本。

### Authorization / isolation

- control endpoints 非 superAdmin 全拒，且不信 body actor/owner；
- B1/B2 public/private、Space/principal grant 矩阵有 table-driven tests；
- unauthorized 与 nonexistent 不形成枚举 oracle；
- Bot A 的 grant/cache/capability 不能被同 apiUrl 的 Bot B 复用；
- revoke edit 立即阻断后续 edit；仅 active 切换不影响仍有 edit grant 的历史 same-version edit；
- grant 不能绕过 Bot owner、Space、callback RouteSpec、card feature gate。

### Runtime / resilience

- 已完成启动期 claim reconciliation 的进程，其显式 static exact-version 路径在 dynamic DB outage
  时仍成功；dynamic/default-resolution 路径 fail closed 且可观测；
- 两副本冷/热 cache、publish/activate/rollback、restart recovery 测试通过；
- dynamic exact render/edit 在热 cache 下仍读取权威 block 状态；强一致授权查询不可误接只读副本；
- exact historical edit 不因 active 切换改变版本；raw/template mode XOR 继续成立；
- dynamic interactive action 只能用 stored provenance 解析 ActionView/ActionContract 并进入已有
  RouteSpec；伪造 template ref、缺 route、blocked version 全部在 enqueue/callback 前拒绝；
- cache 有 entry/byte 双上限、singleflight、race tests，无无界 goroutine/keyspace；
- activation 后 capability/discovery 与 new send 读取同一权威状态，不出现“广告新版、另一副本只认旧版”。

### HTTP / operations

- manager routes 为 Auth → SharedUIDRateLimiter → superAdmin guard；B1/B2 含 Auth/Space/UID limiter；
- dynamic Bot profile 为 authBot → botActorUID → SharedUIDRateLimiter，测试重置持久 Redis UID bucket；
- 所有错误使用 localized envelope；5xx `Internal=true` 且先 log cause；
- body cap 在 JSON decode 前生效；分页和导出响应有硬上限；
- compile 并发/队列有硬上限；多副本 publish 重试保持 same-hash success、different-hash conflict，
  pointer 写保持 CAS；
- metrics label 无 bundle hash、UID、Space 或任意高基数业务值；
- `make i18n-extract-check`、`make i18n-lint`、source guards、focused/race/integration tests、
  `go build ./...`、`go vet` 和 `git diff --check` 通过。

## Rollout / rollback

1. PR-A 部署时 runtime activation 完全不可用；只在测试环境 validate/publish inactive bundle。
2. PR-B 以 `OCTO_CARD_RUNTIME_CATALOG_CONTROL_ENABLED=false` 和
   `OCTO_CARD_RUNTIME_CATALOG_NEW_SEND_ENABLED=false` 部署；验证 static 路径零回归。
3. 所有 serving replica 均升级到支持 RuntimeCatalog 且完成 static claim reconciliation 后，才允许
   激活 dynamic version；混合新旧 binary 时 control/new-send 必须保持关闭。
4. 在测试环境发布一个 bundle，分别从两个 server replica 冷 cache render，重启后重验。
5. PR-C 建 grant 并执行 publish→activate→new send→Action.Submit(existing RouteSpec)→same-version
   edit→rollback→旧版 new send；
   同时验证 private B1/B2 隔离。
6. 首次生产 rollout 只允许既有 L2a owner、已有 RouteSpec、已知 producer；先 publish inactive，
   再 shadow/canonical diff，最后小流量 activate。
7. 普通回滚：CAS 将 active pointer 切回上一已知良好版本；不要删新版 artifact/cache。
8. 安全事件：同事务 block + fallback；无 fallback 时 block + disabled。告警所有仍引用 blocked
   version 的请求，已有消息保留最后成功 payload。
9. 禁止用“关闭 runtime catalog 读取”作为常规回滚，因为历史 dynamic 消息仍需要 exact-version
   render/edit。kill switch 应先关闭 control/new send，保留安全的历史读取；只有安全事件才 block version。
10. **兼容性底线：**首条 dynamic 卡片一旦成功发送，E3 exact-version reader 就成为 server binary
    的最低兼容版本。不得直接回滚到 pre-E3 binary；如需回滚实现，只能部署仍保留 dynamic historical
    read/edit 的修复版。首次生产激活前必须完成 catalog 表备份/恢复演练并确认 RPO/RTO。

## Out of scope

- 任意 Go/plugin/script/WASM 上传或运行；远程模板 URL/include；服务端主动 fetch；
- 可视化模板编辑器、draft 协作 UI、Marketplace；
- 匿名、普通用户、Bot 自助 publish/activate；
- 动态创建 callback route、callback secret、finalizer 或 owner namespace；
- 在 D1/D2 gate 未满足前开放 `ext.*` L2b producer；
- 自动迁移历史消息到新版本、跨版本 edit、hard delete/GC 已发布版本；
- v1 unblock 已 blocked version；误封后只能发布新 version；
- per-Space 不同 active version（v1 只有 global active；Space 仅做 visibility/grant）；
- 修改 `template-ref/v1` wire；修改 E1d/E1e stop/retry 业务语义；
- E2 internal notify envelope（可在 E3 RuntimeCatalog 就绪后独立接入）；
- active/grant 的最终一致本地缓存或 Redis-only source of truth。

## Confirmed implementation decisions (2026-07-28)

- **D1 — v1 publisher：**建议仅 `superAdmin`；owner delegated publishing 等 D2/L2b，不在 E3 v1。
- **D2 — activation scope：**建议 v1 只有 global active pointer；Space 只用于 grant/visibility，
  不做同一 ID 每 Space 不同 active version。
- **D3 — bundle limits：**确认 2 MiB body/bundle、128 documents、512 KiB/document、
  16 views、64 states/samples/goldens。
- **D4 — consistency：**建议 v1 active/grant 读 MySQL 权威状态，local cache 只缓存 immutable
  compiled artifact；接受每次 dynamic new send/edit 一次 catalog DB check，后续按指标优化。
- **D5 — built-in overlap：**允许 active pointer 指向 static 版本，但 dynamic publish 禁止覆盖
  任一 static exact `id@version`；用持久化 version claim + startup readiness reconciliation 防止
  “dynamic 先发布、未来 binary 后引入同 key”的反向冲突。
- **D6 — interactive activation：**必须已有 RouteSpec；E3 不动态创建 route/secret/finalizer。
- **D7 — first pilot：**建议非生产使用一个现有 L2a owner + 已有 producer/route；生产首张动态卡
  另起 rollout 工单，不直接拿 E3 merge 当 go-live。
- **D8 — four-eyes：**v1 要求 reason/change ticket + append-only audit；生产 SOP 要求双人复核，
  v1 暂不在应用内硬编码“发布人与激活人必须不同”，避免在缺少审批对象/应急旁路契约时造半套流程。
- **D9 — emergency block：**建议 block 单向不可恢复；当前 active 无安全 fallback 时原子置
  disabled，不因缺少 rollback target 拒绝安全操作。
