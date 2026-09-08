---
type: Task
title: "Task: loop-project-fleet-integration"
description: octo-server 作为 EVA 项目权威，向 geely-octo-fleet 提供项目成员/角色/epoch 的在线与内部鉴权接口，并以可靠 outbox 把项目生命周期与成员撤权事件推给 Fleet；project_id 与 Fleet workspace_id 是同一个 canonical UUID。
tags: [space, isolation, auth, acl, wire-contract, error-response, i18n, rate-limit, testing, commit, project, data-integrity, concurrency]
timestamp: 2026-09-07T14:48:27+00:00
# --- octospec extension fields ---
slug: loop-project-fleet-integration
upstream: >
  Eva_octo-server&loop_Integration V1.1（开发冻结稿，2026-09-07）；
  EVA Project × Fleet / daemon / CLI 详细开发方案 V1.0（2026-09-07）
source: user
---

# Task: loop-project-fleet-integration

> 一个任务 = 一个 `.octospec/tasks/<slug>/` 目录。本 brief 是这项工作的规格。
> 所有示例与测试只使用合成 UID / Space ID / Project ID。

## Goal

让 octo-server 成为 EVA「项目」的唯一权威，并把这个权威**可被 Fleet 消费**：

1. `octo-server.project_id == Fleet.workspace_id`，同一个 canonical UUID，无映射表。
2. Fleet 判断「当前请求人是否是本项目成员、角色为何」时，调 octo-server 在线 verify。
3. Fleet 判断「另一个目标用户（被指派人 / Runtime owner）是否仍是本项目成员」时，
   调 octo-server 的内部批量接口——Fleet 不保存成员副本，也不再查它自己的
   `workspace_member` 表。
4. Fleet 在每次受保护调用前复核 project epoch（服务端强制 ≤60s 缓存），
   epoch 来源是 octo-server。
5. 项目创建 / 归档 / 恢复 / 改名 / 成员撤权，由 octo-server 以**可靠 outbox**
   推送给 Fleet，Fleet 据此建 Workspace、停执行资源。

### 本仓在整个方案里的位置

上游两份文档定义四方协作，其中 `geely-octo-fleet` / `geely-octo-daemon` /
`geely-octo-cli` 是另外三个仓库的事，本 task **不涉及它们的任何代码**。
octo-server 在 V1.0 §9 中是「外部依赖」，要交付的就是上面 5 条。

需求可以分成三类，第三类才是风险所在：

| 类别 | 内容 |
| --- | --- |
| 被 Fleet 调用（入站 API） | verify 的 project 半边（已上线）、内部批量目标成员校验、membership epochs。两个新接口挂在**已有的** `/v1/internal` 层上，drive / docs 等消费方已经在用，范式沿用不新造 |
| 调用 Fleet（出站） | `POST /internal/project-workspace-events`，5 类事件 |
| 不是 API 的前置 | project_id 格式、archived 状态、lifecycle version、创建两阶段化、事件 outbox 可靠性、生产库 collation 转换 |

接口本身的查询逻辑基本已经存在（见下），实现量小。会返工的是第三类。

## Background

### 已经具备的（measured at `main@d5b091a`）

- **Project 数据模型完整**。`modules/project/sql/20260904000001_project_core.sql`
  建了 `octo_project` / `octo_project_member`，角色编码 `0=member / 1=admin / 2=owner`
  与上游文档 §5.1 的 wire 类型一致。
- **`member_epoch` 已是可用的成员纪元**。列在 `octo_project` 上；本包内唯一的写法是
  `member_epoch = member_epoch + 1`（`modules/project/db.go:205`），由源码 guard
  测试钉住单调性。五个 bump 点都在同一事务内：加成员（`service.go:861`）、
  改角色（`service.go:1198`）、开始移除（`service.go:1357`）、解散（`service.go:644`）、
  Space 级联关席位（`space_member_removal.go:311`）。
- **`POST /v1/auth/verify?include=context` 的 Project 半边已上线**。
  `modules/user/api_project_context.go` + `modules/user/api.go:4891`。语义已符合文档 §7.1：
  点查而非列表、上限 50、逐一覆盖请求 ID、非成员只回 `project_id + member`（不回 role /
  epoch / capabilities）。
- **Space 移除 → 项目席位关闭已有**。`modules/project/space_member_removal.go` 反向注册进
  modules/space 的成员移除清理工单，分页遍历、预算耗尽返回可重试错误。
- **项目侧成员移除已有 DB 持久化工单**。`20260906000001_project_group_binding.sql` 的
  `octo_project_member_removal_cleanup`：lease + 退避 + attempts + abandoned + cancelled，
  时间列一律应用侧 UTC 写入。
- **`/v1/internal` 已经是一个多消费方共享的内部接口层**，不是要为 Loop 新造的东西。
  当前已有 5 个模块在这个前缀下挂载：`modules/internal_resolve`（drive 侧解析）、
  `modules/notify` + `modules/bot_mention`（docs 侧）、`modules/bot_task`、
  `modules/user`。已经沉淀出的范式是：**每个消费方一套独立 token**、
  `X-Internal-Token` 常量时间比较、token 长度下限、body 上限、
  `StrictIPRateLimitMiddleware` 按 IP 限流（内部路由没有登录 uid）、
  各模块自己的 `internalAuthMiddleware`，以及 `main.go` 里
  `ValidateNotifyTokenExclusions` 的跨能力 token 互斥检查。
  Loop/Fleet 只是这一层的**新增消费方**，必须沿用同一形状。

### 缺失的

- 没有到 Fleet 的**出站** HTTP 客户端，也没有任何跨服务事件 outbox。
  （本仓已有的 Fleet 相关代码只有 `modules/botfather` 的 runtime onboarding：
  它把接入信息下发给 daemon，方向是「被拉取」，octo-server 从不主动调 Fleet。
  新增出站链路时接入地址应沿用该模块既有的服务地址推导方式，不要新造一套配置项。）
- 没有 `GET /v1/internal/membership/epochs`。
- 没有 `POST /v1/internal/project-memberships/_verify`。
- 没有归档 / 恢复：`status` 只有 `1=正常 / 0=已解散`，解散是终态
  （`modules/project/model.go:10-15`）。
- 没有生命周期版本号，事件乱序判定没有依据。
- 创建直接 `status=1`，没有 `provisioning` 中间态。

### 五个必须先冻结的合同问题

**C1 — `project_id` 格式冲突（P0，硬冲突，阻塞创建链路联调）**

文档要 canonical UUID（36 位带横线，如 `550e8400-e29b-41d4-a716-446655440000`）。
代码用 `util.GenerUUID()`，实现是 `strings.Replace(NewV4().String(), "-", "", -1)`
（octo-lib `pkg/util/string.go:13-16`），产出 **32 位无横线 hex**；
`octo_project.project_id` 是 `VARCHAR(40)`，宽度够但值不是那个形状。

上游 V1.0 §6.1 的 migration 215 明确把事件表的 `project_id` 建成 `UUID` 列；
`workspace.id` 的实际列类型**本仓无法核实**（没有 Fleet 仓库访问权），需要 Fleet 侧确认。
如果它同样是 Postgres `uuid`：Postgres 接受无横线输入，但**回读一律带横线**，
于是 V1.1 §6.2.1 第 7 步「octo-server 校验 `workspace_id == project_id`」的
字符串比较必然失败。即便它是文本列，两侧各自生成/校验 UUID 的写法迟早会分叉，
格式仍要显式冻结。

**已决（2026-09-07）：采用 (a)，36 位带横线 canonical UUID。**
落地在 O1：换掉 `createProjectOnce` 的生成函数并加格式测试。仍需运维确认生产是否
已有项目数据——`OCTO_PROJECT_CREATE_ENABLED` 默认 false，若从未开启则迁移成本为零，
否则要决定存量 32-hex 项目是回填还是不接入 Loop。

（未采用的 (b) 是「双方按去横线后比较」，会把格式差异永久摊到 Fleet 存储、
daemon 工作目录名、CLI 参数和日志检索上。）

**C2 — verify 的失败信号语义不一致（合同澄清，不改代码）**

文档 §7.1 写「`context_included` 缺失或为 false 代表上下文查询失败」。
本仓实现是故意反着来的：失败时 `context_included` 仍为 **true**、列表清空、
另置 `context_error=true`（`modules/user/api.go:4894-4911`）。理由写在
`authVerifyTokenResp.ContextError` 的字段注释里——清掉该 flag 会让消费方回退到
pre-v2 路径去信任客户端自报的 `X-Space-Id`，等于在故障期间把每个网关降级成信任调用方。
已有测试 `TestAuthVerifyToken_IncludeContext_DBError_FailSecure` 钉住这个行为。

结论：语义不改，合同里写清 **Fleet 必须读 `context_error`**。只看
`context_included` 不会不安全（空列表 → 拒绝，方向是对的），但会把故障和
「真的不是成员」混为一谈，重试和告警都没法做。

**C3 — 归档 / 恢复在本仓不存在**

`project.archived` / `project.restored` 没有对应状态。现在只有「解散」，是终态，
而且通过 `active_name` 生成列**释放项目名**（`uk_octo_project_space_active_name`）。
所以「恢复」可能撞上已经占了同名的新项目——不是加个 status 就完事的。

**已决（2026-09-07）：要有归档能力，即 (b)。**
新增归档状态 + `archived_at`；归档期间项目名**继续占用**（否则恢复会撞上已占同名的
新项目），解散仍是终态、仍释放名字。两者是不同状态，不能合并。落地在 O1。

**C4 — `project_version` 没有来源**

文档要求 archive / restore / metadata 按 `project_version` 防状态回退。
现表没有这个列；`member_epoch` 只覆盖成员维度，不覆盖资料和生命周期，不能兼任。

**跟随 C3 已决**：新增生命周期版本列，每次生命周期/资料写入 +1，
`member_revoked` 按文档 §6.2 不参与版本比较。递增与比较语义文档已写死，
不需要额外拍板。落地在 O1。

**C5 — 内部批量目标成员 verify 的正式合同未冻结（上游 P0-1）**

路径、角色编码、错误码、批量上限、限流、SLA 都没定，Fleet 侧的 F9 单卡在这里。
本仓实现成本低：`pkg/project.MembershipsInSpace`（`pkg/project/membership.go:305`）
已经是「一个 uid × 多个 project」的批量查询，只需要换成「一个 project × 多个 uid」
的同形查询，谓词（`status=1 AND removing=0` + Space 过滤）可以原样复用。

### 一个和需求同等重要的部署前置

`20260904000001_project_core.sql` 的头注释记了一条**已测量、未修复**的生产库问题：
`user` / `space` / `space_member` 在生产是 `utf8mb4_0900_ai_ci`（历史 mysqldump 导入
产物，导出时省略了等于库默认值的 COLLATE），而本模块建表是 `utf8mb4_general_ci`。

后果：模块一旦开启，`GET /v1/projects/:project_id/members`（LEFT JOIN user）每次
MySQL 1267 报 500，五个对账扫描里有三个（JOIN 了 Space 侧表的那三个）同样失败。
CI 结构上抓不到——`ci/run-e2e-shard.sh` 建库时显式指定了 general_ci。

目前 `OCTO_PROJECT_CREATE_ENABLED` 默认 false（`modules/project/config.go:24`）挡住了它。
**EVA 项目功能上线前必须先做全库 collation 转换窗口**，且不能只转这三张表
（它们还 JOIN robot / group_member / group / app_bot / opanalytics 维表）。
这条要进发布顺序，排在 V1.0 §12 第 7 步「测试 Space 创建 Project」之前。

## Load-bearing list

每条后面的 `touches:` 用 `.octospec/rules/_index.yaml` 的词表，供 Implement
阶段解析注入。本节末尾有汇总表。

- **`POST /v1/auth/verify?include=context` 的线上契约。** 网关每个请求都调它。
  Project 半边的现有语义（点查、上限 50、非成员不带 role/epoch/capabilities、
  失败时 `context_included` 保持 true + `context_error`）全部是授权事实，不能顺手改。
  `touches: wire-contract, auth, acl`
- **`member_epoch` 的单调性与 bump 时机。** Fleet 的 60 秒收敛完全建立在
  「成员/角色一变，epoch 就变」上。任何新写路径都必须在同一事务内 `+1`，
  不能出现绝对赋值——包内 guard 测试靠 grep 形状生效。
  `touches: auth, wire-contract, data-integrity`
- **`removing = 1` 视为非成员。** 这是本仓所有授权谓词的共识
  （`pkg/project/membership.go`）。新接口必须沿用同一谓词，否则 Fleet 会在
  席位关闭窗口内继续放行。
  `touches: auth, acl, isolation`
- **I1 不变量（活跃项目成员 ⊆ 同 Space 活跃成员）。** 同步半在请求事务内，
  异步半靠 Space 移除清理工单。新增的事件发送不能改变这个次序：
  必须**先关席位、后发事件**（文档 §6.4 明确要求）。
  `touches: space, isolation, acl, data-integrity`
- **Space 隔离。** 新的内部接口带 `space_id`，project 必须落在该 Space 内，
  否则「不在这个 Space」和「不是成员」必须是同一个不可区分的答案——
  现有 `MembershipsInSpace` 把 Space 过滤做进谓词就是为了这个。
  `touches: space, isolation`
- **既有的项目成员移除工单** `octo_project_member_removal_cleanup`：
  它驱动的是**进程内** step 注册表（`cascade_registry.go`）。文档 §6 明确禁止
  跨服务撤权复用进程内成员事件，所以 Fleet 事件必须是**另一条** outbox，
  不能挂成这条工单的一个 step（失败会互相拖累重跑）。
  `touches: data-integrity, concurrency`
- **`/v1/internal` 这一层的既有消费方（drive / docs / bot_task）。** 新增的两个
  内部接口和它们共用同一个路由前缀，任何改动到共享中间件、限流键空间或
  token 校验的地方，都会同时影响它们。所以：
  - **新增独立 token，绝不复用任何既有内部 token。** 一个 token 只能对应一种能力，
    否则 drive 的 token 泄漏就等于拿到项目成员权限。新 env 必须加进
    `main.go` 的 `ValidateNotifyTokenExclusions` 互斥检查并有测试。
  - 限流 tag 必须是新的、与既有 tag 互不相同的字符串，否则会和别的消费方共用桶。
  - 认证形状照既有范式：常量时间比较、token 长度下限、body 上限、
    `StrictIPRateLimitMiddleware`；未配置时 503、错误 token 401。
  - **不能挂 `SharedUIDRateLimiter`** ——内部路由没有登录 uid，挂上去会静默 fail-open。

  `touches: auth, rate-limit, isolation`
- **迁移文件的两条硬约束。** 时间列一律 Go 侧 UTC 写入、禁 `CURRENT_TIMESTAMP`
  （本模块两份迁移都写了原因：已经有一个指标因为会话时区读出 -28799 秒）；
  文件里不得出现单引号（migration test 朴素分句，引号奇偶性会让它误判）。
  `touches: data-integrity, testing`
- **新增的用户可见错误响应。** 两个内部接口和事件失败路径要新增错误码，走
  `pkg/errcode` + `httperr.ResponseErrorL`；新 handler 文件要进模块的
  `NoLegacyResponseError` guard 清单，并补 zh-CN 翻译。
  `touches: error-response, i18n, wire-contract`

### 预期注入的 rule

| rule | 经由 | 是否适用 |
| --- | --- | --- |
| `space-isolation` (92) | `space` / `isolation` / `auth` / `acl` | 适用。新接口按 `space_id + project_id` 作答，跨 Space 与不存在必须同答案 |
| `error-handling` (85) | `error-response` / `i18n` / `wire-contract` | 适用。新错误码 + 本地化信封 + guard 清单 |
| `rate-limit` (80) | `rate-limit` | 适用，但**方向与默认相反**：内部路由用 `StrictIPRateLimitMiddleware`，不能挂 `SharedUIDRateLimiter`（没有登录 uid） |
| `trust-boundary` (90) | `wire-contract` | 预计**不适用**——该 rule 管的是 inbound adapter 把攻击者内容带进消息载荷时的转义与 adapter 间一致性，本任务不新增 adapter、不渲染外部内容。但有一条要记：`project.metadata_updated` 的 `name` / `description` 是用户可控文本，跨服务边界后由 Fleet 展示；octo-server 不得假设 Fleet 会替它转义，Fleet 侧也必须按不可信文本处理。Implement 阶段读全文后据实记 `applied: false` + 该注记，不要静默丢弃 |
| `testing` (70) | `testing` | 适用 |
| `commit-style` (60) | `commit` | 适用。英文 Conventional Commits |

## epoch 0 哨兵冲突（#852 评审 P1，已定）

### 缺陷

`member_epoch` 的 DDL 默认值是 0，而 `createProjectOnce` **刻意不 bump 它**
（`service.go` 的注释：创建是名册产生而非变化，所以 0 是初值）。实测确认：

~~~text
新建项目 → member_epoch=0  status=1  active_members=1
~~~

同时上游文档 §7.3 把 **0 定义为「Project 不存在或不可见」**。两者冲突，且是双向的：

- **陈旧授权**：消费方按 `(uid, P, 0)` 缓存肯定结论 → 项目解散 → epochs 因
  `status = 1` 过滤把它当不存在、返回 0 → **与缓存值相等 → epoch 检查通过 →
  授权永久保留，epoch 再也不会变**。这正是读序守卫要防的那一类失效。
- **可用性**：新建项目如实返回 0，而合同定义 0 = 不存在，遵守合同的消费方会拒绝
  一个刚建好的项目，直到有人加或删一次成员。

这不只是 octo-server 的实现问题——文档合同本身就把一个可达的真实值当成了哨兵。

### 决策：在源头修，不改 wire 合同

**新建项目由 `bumpMemberEpochTx` 把 epoch 抬到 1；存量 `= 0` 的行由迁移 +1；
对账扫描持续修复重新落回 0 的活跃项目。**

> 机制上有一个必须写清楚的细节：第一版实现是在 insert 列表里**播种**
> `member_epoch = 1`，被既有守卫 `TestIsOfficialHasNoWriter` 拒绝——该列只允许以
> `member_epoch + 1` 的形式写入。改成走共享的自增反而是更好的解法：创建时写了
> owner 席位，那本来就是一次名册写入。此处曾按被否决的形状描述了一轮。

考虑过并否决的三个替代方案，理由都是「要重新谈合同」：

| 方案 | 否决理由 |
| --- | --- |
| 线上用 `null` 表示不存在 | 文档 §7.3 明确「不扩展为对象」；且 `null` 在 Go 里 unmarshal 进 `int64` 就是 0，对端也是 Go，这个坑会原样复现 |
| 用 `-1` 之类的负数哨兵 | 仍是改合同，且哨兵仍然住在值域里——下一个把某处初始化成 -1 的人会重蹈覆辙 |
| 从 map 里省略不存在的项目 | 消费方分不清「键缺失」和「请求被丢了一项」，这正是当初决定「逐一覆盖每个请求 ID」的原因 |

选定方案是**唯一不需要对端改一行代码**的：合同一字不动，只是让 0 不再是活跃项目
会停留的值，于是文档里那句「返回 0 = 不存在」第一次真正成立。

### 但迁移只在「某个瞬间」成立

建项目路径只覆盖新二进制，迁移每个部署只跑一次且记账在 `gorp_migrations`。于是有
两个窗口会把不变量重新打破，而且都不是假想的：

- **滚动发布**：第一个新 pod 跑完回填后，旧镜像的 pod 仍在按列默认值 0 插入；
- **回滚**：迁移的 Down 是 no-op，注释原本写「回滚二进制即可」——对**读**为真，
  对**写**为假。回滚会无限期恢复写 0 的建项目路径，而账本行让前滚也不会重跑回填。

而且没有任何东西能发现：`scanEpochSanity` 连 `status` 都没 select，
`status = 1 AND member_epoch = 0` 这个形状全仓没有任何查询在问。

所以不变量改为**持续强制**：对账扫描把该形状记为异常并用同样的 `+1` 语句自愈；
语句自身的 WHERE 就是它的 CAS，所以多 pod 并发不需要租约。端点那句 fail-closed
的推理实际依赖的是这个扫描，而不是迁移。上线与回滚说明见
`docs/project-member-epoch-rollout.md`。

代价是 #852 要碰 `modules/project`（失去与 #850 零文件重叠的性质）。接受：为保住
一个文件重叠的优点而发布一个已知有缺陷的授权合同，不划算。

### 附带修正

「0 匹配不上任何真实快照」这句话此前写在四处（`api.go` ×2、`api_test.go` 的用例
说明、PR 描述）。它当时是**错的**；现在因为上面的不变量才成立，所以措辞要改成
指向那个不变量，而不是断言一个巧合。

第二轮 review 又指出，改写后的措辞（端点注释、迁移注释、journal）仍然是**无条件**
的「0 变得不可达」。它们现在改为说明由三个写入方中的哪一个强制什么，以及残留窗口
的方向是可用性方向而不是陈旧授权方向。

### 另一处同类缺陷：只有项目席位不等于授权

`ProjectMemberships` 原本只读 `octo_project_member`。Space→项目的级联是异步的，
清理步骤直接 `deactivateMemberTx` 而没有同步的 `removing = 1` 阶段，重试到上限后
行被保留但不再被认领。于是一个已被移出 Space 的用户，其项目席位在一个**失败时无界**
的窗口里仍读作有效成员——而席位没被动过，epoch 就不会变，对端的过期检查反而会
**同意**这个陈旧结论。

仓内其它所有用这个谓词的调用方都在 Space 闸门之后（项目路由的中间件、群准入显式
合取 `spacepkg.ActiveMembers`）。这个函数是第一个前面没有闸门的调用方，而且它的
消费方**拿不到**缺的那一半：它问的是第三方（受派人 / 专家团成员 / Runtime owner），
它没有对方的 token，仓内也没有任何接口能代第三方回答「uid X 在 Space S 吗」。

所以合取放在服务端，走 `space.ActiveMembers`（避免谓词漂移），并且**排在席位查询
之后**——这次读只会缩小答案，放最后才是 fail-closed 的顺序。

**没有关掉的那一半**：只修了**新鲜**查询。在移出 Space 之前就已缓存的授权仍靠
epoch 一致存活，因为 Space 移除不会 bump `member_epoch`。要关掉它需要「Space 移除
提交时 bump epoch」（O5/O7 事件切片）或者合同里一个不依赖 epoch 一致的硬 TTL。

## 安全与信息约束

- **本 brief 及后续所有交付文档、代码注释、提交信息里不写敏感信息**：不写任何
  token / api_key 的值、内网地址、主机名、端口、真实 UID / Space ID / Project ID。
  需要举例时一律用合成标识符。
- 内部接口的鉴权凭据只从部署配置读取，不落日志、不进错误响应、不进指标标签。
- 事件 outbox 的 `last_error` 只保存低基数失败摘要，不保存 payload 内容或凭据
  （沿用 `octo_project_member_removal_cleanup` 已有的同名列约定）。
- 新增接口的错误响应不得泄露「项目存在但你不是成员」与「项目不存在」的区别——
  这是 `answerProjectMembership` 和 `MembershipsInSpace` 已经在守的性质。

## Out of scope

- `geely-octo-fleet` / `geely-octo-daemon` / `geely-octo-cli` 的任何代码。
  本仓只提供接口与事件，Fleet 的 `WorkspaceAccessContext`、Task 授权快照、
  Task Credential renew、旧 Fleet Project feature flag 都不在这里。
- EVA Web / octo-web 的改造。
- Fleet 旧 Project 模型的任何处理——本仓从不知道它的存在。
- 成员列表展示接口：EVA Web 直接调已有的
  `GET /v1/projects/:project_id/members`，它不进 Fleet 鉴权链，本任务不动它。
- Project service principal / Automation 授权主体：上游 P0-2 已决为
  「实际 Runtime owner 作为 Task actor」，那是 Fleet 侧的实现，本仓不感知。
- 生产库 collation 转换本身（是发布前置工单，不是本任务的代码改动），
  但本任务的验收里要求它被显式记录为上线阻塞项。

## 与 PR #850 的边界

`Mininglamp-OSS/octo-server#850`（P2 PR-5，`.octospec/tasks/project-p2-subsystem-integration/`）
与本任务同 base（`d5b091a`）、同目标模块，做的是同一件事的另一个版本。它在
2026-09-07 08:07 创建时不知道本分支存在，本节是事后划定的边界。

### 冲突的实质

不是文件冲突（那六个文件都是「两边各插一行」）。是 **workspace 身份模型互斥**：

| | #850 | 本任务（依据 V1.1 §1 / §2.1） |
| --- | --- | --- |
| fleet 侧标识 | 16 字节 crypto/rand + 前缀，**刻意不可由 project_id 推导**，永不下发 | `project_id == workspace_id`，同一个 canonical UUID，无映射表 |
| 谁建 | octo-server push 「ensure container」 | Fleet 收到 `project.created` 自建 |
| 对端认证 | 每 target 一个 HMAC + conformance vectors | Bearer service token |

### 结论：身份用 V1.1，机器用 #850

**身份模型**：#850 的不透明 ID 是在用标识符当访问控制，它自己的描述也称之为
capability-URL protection 并写明了局限。它防的链条是「Space 成员读到 project_id →
当 workspace_id 用 → fleet 按 Space 成员放行 → `UpsertMember{Role:"member"}` 永久落行」，
而 V1.1 §3.2/§3.3 砍掉的正是第 3、4 步。两者是同一缺陷的**缓解**与**修复**，修授权优于藏 ID：
藏 ID 的代价是一张永久映射表，缺陷修好后它仍在，且成为新的失败面（映射行丢失即无法定位
workspace，且无法重算）。

**工程实现**：#850 明显更成熟，本任务应当让位——出站围栏是机制而非约定（独立包 +
handler 禁止 import + 源码守卫）；HMAC + 已发布的 conformance vectors 严格强于 Bearer，
且规范串写错会 fail closed 并自报，而宽松的 bearer 校验 fail open 且静默；`attempts`
在 claim 时判定预算比本任务在完成时判定更稳；扫描索引以 `target` 打头避免
`SKIP LOCKED` 跨 target 互锁（本任务没有意识到这个问题）。

**push 与 pull 不冲突**——早先把它们当对立选项是看错了。撤权是安全边界、有 60 秒收敛
要求，必须 push；容器回收是资源清理，pull 合适。

### 落地顺序

1. **#850 按原样合并，不 drop 任何东西。** 它默认 inert，且 fleet target 在 fleet 侧
   收窄（V1.1 §3.2/§3.3）落地前本来就不许开启，所以 fleet 的 `container_id` 语义在
   那之前不可观测。为一个依赖他仓未发布工作的设计去返工一个七轮 review 的 PR 的
   load-bearing 属性，代价与风险都不划算；drive 那一半与本争议无关，更不应被阻塞。
2. **#850 只需补一段记录**（非代码）：fleet target 的 container id 是**临时形态**，在
   fleet 侧收窄落地后改为 `project_id`，届时不可推导守卫**收窄为仅 drive 适用**。
   写下来是为了这个决定不会丢失。
3. **本分支作废 O4 整套发件箱**（表 / worker / 客户端）。生命周期事件改为搭载 #850 的
   `octo_project_provisioning` 出发件箱与 `internal/projectprovision` 出站通道。
   唯一需要带过去的是**载荷在入队时冻结**：对端按 event_id 对 payload 打指纹判冲突，
   重投时字节必须一致；#850 的 ensure 是固定形状所以原本不需要这条。
4. **本分支的 O2/O3 立即可评审**：两个入站接口不碰 #850 的任何文件，而且**恰恰是它们
   让 fleet 侧的收窄成为可能**——没有它们，fleet 无法从 `workspace_member` 切到在线
   verify，#850 的不透明 ID 就永远摘不掉。
5. 两条分支同 base，冲突文件为 `main.go`、`modules/project/` 的
   `api.go` / `config.go` / `db.go` / `metrics.go` / `service.go`，形态都是「两边各插一行」。
   **由本分支承担 rebase**，因为它更小、投入的 review 更少。

### 前置条件（必须写死）

共享 UUID 只有在 fleet 侧收窄落地后才安全。fleet target 的开关卡在 fleet 发布
`source=octo_server_project` 路径之后——即 #850 自己那条「Do not enable a target until
its P-2 has landed」，再加上 P-3。


## 拆单建议

> **本分支（`claude/internal-membership-endpoints`）只包含 O2 与 O3。**
> 表中其余标记为已实现的条目在另一条分支上，未包含在本 PR 中。这里列出完整拆单是
> 为了让评审看到这两个接口在整体里的位置——尤其是它们与 PR #850 的关系，见上一节。

| 单 | 内容 | 依赖 | 状态 |
| --- | --- | --- | --- |
| O1a | project_id 切 36 位带横线 UUID + `lifecycle_version` 列与递增纪律 | C1/C4 已决 | ✅ 已实现 |
| O1b | 归档状态 + `archived_at` + 写路径拒绝 + archive/restore 接口 | 与 #850 无关 | ⏸ **契约已冻结（§8），代码有意不写**（按指示）。**注意本行早先写的「半接线」已失效**：那批列/模型/响应字段在已废弃的本地分支上，当前谱系里**根本不存在**，没有东西需要回退 |
| O2 | `GET /v1/internal/membership/epochs` | 无（不依赖 O1） | ✅ 已实现 |
| O3 | `POST /v1/internal/project-memberships/_verify` | C5（临时路径） | ✅ 已实现 |
| ~~O4~~ | ~~搭载 #850 发件箱~~ | — | ✅ **已实现，但用独立表**：`octo_project_provisioning` 是 `UNIQUE (project_id, target)` 的**映射表**，一个项目每个目标只有一行，装不下 append-only 事件流。下面「落地顺序」第 3 条写于核对该约束之前。需要带过去的「载荷入队即冻结」已带过去（`TestPayloadIsFrozenAtEnqueue`） |
| O5 | 五类事件接入各写路径，均在业务事务内入队 | #850 合并后；archived/restored 另需 O1b | ✅ 已实现（archived/restored 仅定义常量与载荷，无生产者——O1b 未实现） |
| O6 | 创建链路两阶段化：provisioning → 调 Fleet → 校验 workspace_id → active | #850 合并后 + fleet 侧收窄落地 | ✅ **闸门已实现**，且**不依赖 fleet 收窄**：见下 |
| O7 | Space 移除级联：席位关闭事务内补写 member_revoked | #850 合并后 | ✅ 已实现 |
| O8 | 合同文档 + Mock + SLA/告警 + collation 转换预案 | 全部 | 部分：契约文档 `docs/project-lifecycle-contract.md` 已冻结；Mock / SLA / 告警 / collation 预案未开始 |

**O6 为什么不再被 fleet 收窄阻塞**：本行原先把两件事绑在一起——「两阶段闸门」和
「`workspace_id == project_id` 逐字节相等」。前者只需要「有没有拿到确认」，与 id 形态无关，
已实现（`octo_project.activated_at`，未确认时两个入站接口一律按不存在作答）。后者仍然阻塞，
而且**顺序不能反**：在 fleet 收窄落地前把容器 id 换成 `project_id`，等于亲手打开
P-3 那条提权链路。当前校验的是「目标回显的 id == 我方要求创建的 id」，与契约那条是同一个校验，
只是两个 id 还不是同一个值；fleet 收窄落地后换 id，**本处代码不变**，校验自然字面成立。

**为什么用 `activated_at` 列而不是第三个 status 值**：与 O1b 下面那段论证同源——
约二十处谓词读 `status = 1` 且含义各不相同，第三个值会一次性改变全部含义，漏改一处
就是「成员静默变非成员 → I2 判违规 → 群被拆」。列的漏改代价只是「对端早看到一小会儿」，
和今天的窗口一样。

O2 / O3 已完成且零冲突，可立即独立评审。

**修正一条早先的设计注记**：O2/O3 曾记录「归档只要不是 status 1 就自动读作 epoch 0」。
O1b 改用独立列 `archived_at`（status 保持不变，见下）后该结论不再成立——
`ProjectEpochsInSpace` 与 `ProjectMemberships` 需要显式追加 `archived_at IS NULL`，
否则归档项目对 fleet 仍报活跃 epoch，归档事件一旦丢失就没有兜底。**这一条尚未实现。**

**O1b 为什么用独立列而不是第三个 status 值**：约二十处谓词读 `status = 1`，
而它们表达的含义各不相同（席位存在 / 名字被占 / 占配额 / 可见 / 可写）。
第三个 status 值会一次性改变全部含义。两种方案漏改一处的后果不对称：
status 路线漏一处 → 归档项目的成员静默变成非成员 → I2 对账判违规 → 群清理拆掉
group_member → 恢复恢复不回来，静默且破坏性；独立列路线漏一处 → 归档项目接受了
一次本该拒绝的写入，可见且可撤销。因此 status 保持原义，`archived_at` 只在必须
不同的地方被查询：写路径，以及上面那两个对端谓词。

**O1b 的五个语义问题及取定**。这些原本是阻塞项，独立列方案让其中四个自动落定
（`status` 不变 ⇒ 所有既有谓词行为不变），只剩两个需要判断：

| 问题 | 取定 | 来源 |
| --- | --- | --- |
| 归档项目成员还能不能读？ | 能。中间件照常放行，写路径逐个拒绝 | 判断：不能读就没人能发起恢复 |
| 占不占 Space 项目配额？ | 占 | 自动——`status` 不变，两个计数查询原样命中 |
| 项目群归档期间还能不能用？ | 不受影响，席位仍在 | 自动——I2 与群准入谓词读 `status`，未改动 |
| 谁能归档 / 恢复？ | owner，与 disband 对齐 | 判断：归档停掉全部执行，量级接近 disband；放宽比收紧容易 |
| 列表里隐藏还是标记？ | 显示并带 `archived` 标记 | 判断：隐藏了就无从恢复 |

`active_name` **不需要改动**，这是独立列方案的直接结果：`status` 仍为 1，生成列照常
持有名字，归档期间重名被拒、恢复不会撞上同名新项目。早先记录的「`active_name` 生成列
要改」是 status 方案下的结论，已不适用。

当前实现停在半接线：迁移、模型字段、响应字段已加，但**没有任何强制**——没有写路径拒绝、
没有 archive/restore 接口、`pkg/project` 的两个对端谓词也还没加 `archived_at IS NULL`。
读到这个列的人会合理地以为它起作用了，实际没有。收尾或回退，二选一。

## Acceptance

契约与格式：

- [ ] C1 已冻结并落到代码：新建项目的 `project_id` 形状有测试钉住，
      且与 Fleet 回显的 `workspace_id` 逐字节相等（用 Fleet Mock 验证）。
- [ ] C2 已写进给 Fleet 的接口合同：明确 `context_error` 是失败信号，
      `context_included` 不是；现有 fail-secure 测试保持通过。
- [ ] C3 / C4 已决策并落到 schema：生命周期状态与 `project_version` 均可查，
      `project_version` 每次生命周期/资料写入 +1 且只允许 +1（源码 guard）。

入站接口：

- [ ] `POST /v1/internal/project-memberships/_verify`：`uids` 去重、上限 50、
      逐一覆盖请求 UID；成员返回 role，非成员**不返回** role；
      超时 / 5xx / 缺项 / 重复项 / 解析失败一律 fail-closed。
- [ ] `GET /v1/internal/membership/epochs`：返回 `{"projects":{"<id>":<int>}}`；
      项目不存在或不可见返回 `0`；不返回状态字段。
      **「不可见」包含父 Space 被封禁或解散**——这两种情况都不动项目行、也不动 epoch，
      所以必须由谓词把它们折进 `0`，否则 epoch 这条失效通道会说「什么都没变」而
      `_verify` 的答案已经翻了（封禁后陈旧授权存活、解封后陈旧拒绝存活）。
- [ ] 两个内部接口：**新增独立 token**、常量时间比较、token 长度下限、
      按 IP 严格限流（新限流 tag，不与既有消费方共桶）、body 上限；
      新 env 已加入 `main.go` 的 `ValidateNotifyTokenExclusions`。
- [x] ~~未配置时 503、错误 token 401~~ → **改为两者都 401（已定，不阻塞合并）。**
      理由是反枚举：区分开来等于让未认证调用方探测部署状态。
      代价只有一项，且已补偿：对端拿到 401 时分不清「凭据错，别重试」和「服务端还没
      配好，该重试」。补偿是 token 未就绪时点名 env 的启动 ERROR 日志
      （`modules/internal_membership/api.go` 的 `New`），加上
      `octo_internal_membership_configured` 这个 gauge——后者随时可查，不像日志会被
      轮转掉（本集群只留几分钟），所以「是不是没配」这个问题在运维侧有确定答案。
      范围也有限：路由无条件挂载，未配置同样答 401 而非 404，所以「端点存在」本来就
      能探到，401 只藏住「token 配没配」，对攻击方价值很低。
      仍需同步改 O8 接口文档（对端按 503/401 实现会白等一次重试语义），
      但那是文档跟进，不是合并前置条件；实现细节见 `context.yaml` deviations。
- [ ] ~~测试证明它与 drive / docs / bot_task 的既有 token 互斥~~ →
      **drive / docs 已覆盖；bot_task 这一项本片交付不了，改为另立。**
      bot_task 的 per-source bearer token 存在 `OCTO_BOT_TASK_SOURCES` 这个 JSON
      registry 里而不是单个 env，只在 registry 内部去重，`main.go` 的固定 env 注册表
      和 `ValidateNotifyTokenExclusions` 都看不见它。要覆盖必须让 bot_task 暴露它
      配置的 token 值——那是对该模块的改动，不属于本片。本片改为把固定单 env 那一类
      **穷尽**覆盖（由 `TestFixedInternalTokenRegistryIsCompleteBySweep` 全树扫描保证，
      并因此补进了 `TS_WEBHOOK_SECRET_KEY` / `OCTO_MAIL_GATEWAY_SECRET` /
      `TS_GRPC_AUTH_TOKEN` 三个漏网的），bot_task 那一维单独排期。
- [ ] `/v1/internal` 既有消费方（drive / docs / bot_task）的现有测试全绿，
      共享中间件与限流键空间无回归。
- [ ] 两个内部接口都不可被终端用户 token 调用（有负向测试）。
- [ ] 谓词与既有授权链一致：`removing=1` 判非成员、跨 Space 与不存在同答案。

出站事件：

- [ ] 事件在**业务事务内**入队，crash 不丢；worker 有 lease、退避、
      attempts 上限与 abandoned 终态。
- [ ] 同 `event_id` 重投返回首次结果；本仓侧对 Fleet 的 409
      `IDEMPOTENCY_CONFLICT` / `WORKSPACE_ID_CONFLICT` 有明确的终态处理与告警，
      不无限重试。
- [ ] `project.created` 走两阶段：Fleet 未确认前项目停在 provisioning，
      EVA 读不到「可用」；Fleet 超时/失败时以同一 `event_id` 重试。
- [ ] 成员撤权：`member_epoch` 在同一事务 +1，且事件 payload 携带该 epoch。
- [ ] Space 移除：**先关席位、后入队** member_revoked，每个受影响项目一条；
      顺序有测试钉住。
- [ ] 事件积压、投递失败、abandoned 有指标与告警；SLA（Space 移除 → 项目席位关闭
      → Fleet 收到事件）有实测数据记录。

回归与上线：

- [ ] 现有 project 模块测试全绿，`member_epoch` 单调性 guard、
      I1 / I2 对账扫描、removal 工单相关测试均未退化。
- [ ] `make i18n-extract-check` + `make i18n-lint` 通过；新错误码走
      `pkg/errcode` + `httperr.ResponseErrorL`，`active.zh-CN.toml` 已补翻译；
      新 handler 文件已加入模块的 `NoLegacyResponseError` guard 清单。
- [ ] 生产库 collation 转换被显式记录为上线阻塞项，并排在
      「Space 创建 Project 联调」之前。
- [ ] 全部新增代码、测试、文档与提交信息中无 token 值、无内网地址/端口、
      无真实 UID / Space ID / Project ID；凭据不进日志、错误响应与指标标签。
