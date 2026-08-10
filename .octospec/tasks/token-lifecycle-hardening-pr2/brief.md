---
type: Task
title: "Task: token-lifecycle-hardening-pr2"
description: Activate bounded v3 user sessions, complete high-risk revocation, and migrate legacy tokens without cross-replica or Redis safety regressions
tags: [auth, security, redis, session, migration, observability]
timestamp: 2026-08-09T21:06:24+08:00
# --- octospec extension fields ---
slug: token-lifecycle-hardening-pr2
upstream: "follow-up to PR #723"
source: self
---

# Task: token-lifecycle-hardening-pr2

> 本 brief 是 `token-lifecycle-hardening` 的 PR 2 交付规格，只定义代码能力和受控启用流程，
> 不代表生产迁移已经执行，也不因 PR 合并就关闭漏洞。

## Goal

在不改变客户端 opaque Token 协议的前提下完成第二阶段修复：

1. 新签发或复用的用户 HTTP Token 使用 v3 payload，具有不可续期的绝对到期时间，并受
   per-UID session generation 约束。
2. 当前退出、设备退出、全部退出、密码变更/重置、账号禁用/注销等安全事件能够撤销对应
   HTTP 会话，而不再把 WuKongIM 下线等同于 bearer 撤销。
3. 历史 v1/v2 永久或超长 Token 通过单执行者、限速、可暂停续跑且只缩短寿命的迁移收敛，
   最终进入拒绝全部 v1/v2 的 enforce 模式。
4. 所有安全结论跨 API 副本成立；不依赖进程锁、本地缓存、单副本启动任务或 Redis 多 key
   原子性假设。
5. 认证热路径不做 Redis 写入；明确、压测并监控 v3 generation 校验带来的第二次 Redis 读取，
   不以牺牲撤销正确性为代价使用副本本地 generation 缓存。

漏洞关闭条件不是“PR 2 已合并”，而是生产完成 v3 writer 激活、历史迁移、`v1/v2=0`
验证、enforce 灰度和报告历史 Token 场景复测。

## Background

### PR 1 基线（已验证）

- 实现基线为 `origin/main@d3daa912`；其中 PR 1 已由
  [#723](https://github.com/Mininglamp-OSS/octo-server/pull/723) squash merge 为 `b46de7b9`，
  后续主干提交也已纳入本分支。
- PR 1 已收口生产 Token writer，阻止新 Token 或被资料更新/重复登录触达的 Token 变为永久，
  并保持 Web/PC 复用 Token 的剩余 TTL，不再续满。
- `pkg/auth` 已具备 v3 编解码和严格校验骨架；但 `auth.Encode` 仍写 v2，运行时没有接入真实
  `SessionGenerationResolver`。当前若读到 v3 会因 generation validator 不可用而 fail closed。
- 当前认证读取是单 Token key Lua，原子返回 payload 与 PTTL，稳态一次 Redis 往返且不写 Redis。
  PR 2 为 v3 读取 UID 后还要查询 generation，通常增加第二次串行 Redis 往返。
- 当前每个 `config.Context` 只有一个共享的有界 `session` Redis pool；PR 2 必须复用该 pool，
  不能为 generation、索引或各业务模块再建连接池。
- PR 1 的 `_octo_issue_owner` 与 compare-delete 补偿解决了多副本下“旧签发请求误删已被另一请求
  接管的同一 Token”问题；PR 2 的 v3 writer 必须保留这一所有权语义。
- `tools/token-session-observe` 目前只读并只输出聚合，没有 apply/enforce 能力。

### 尚未闭环的风险

- 历史永久 v1/v2 仍被兼容 validator 接受；v1/v2 payload 没有 `expires_at` 或 generation。
- `uidtoken:<flag><uid>` 每个设备类型最多保存一个 Token，且历史路径可能没有该索引，不能作为
  UID 全部会话的权威清单。
- `/v1/user/quit`、`/v1/user/pc/quit`、设备删除、密码变更/重置、禁用和注销等路径仍存在只处理
  IM/设备记录或只删除一个兼容索引 Token 的情况。
- generation 是安全权威后，Redis generation 缺失、读取错误或 revocation 处理中都必须 fail
  closed；否则会重新接受已撤销的 bearer。
- 生产 Redis 是 standalone、sentinel、兼容单端点 proxy 还是 native Cluster 尚未核实。当前
  go-redis `*redis.Client + redisAddr` 不提供 native Cluster discovery，因此 PR 2 不得靠跨 key
  Lua 或 hash slot 假设声明兼容。

### 当前实现状态（2026-08-10，仅代码分支）

- 已实现默认 `expand` 的单调 rollout mode、持久 floor、v3 绝对到期、generation/issuance
  fence、按 UID + generation 隔离的有界会话索引、legacy deny、默认 dry-run 的可续跑 migration
  apply，以及独立小连接池的 `token-session-admin`。合并或部署不会自动切到 v3、apply 或 enforce。
- migration campaign/checkpoint 已按 campaign ID 隔离；batch/QPS 可在续跑时调整，固定 cutoff
  过期后已有 campaign 仍可续跑，但命中的剩余记录会按既定绝对截止时间立即删除；dry-run 记为
  `would_delete`，apply 记为 `deleted`，不再误报为 `shortened`。有限 legacy 策略不再硬编码，
  工具要求显式选择 `natural` 或 `cap`，并把选择纳入不可变 campaign 安全身份；cutoff 已过期
  或在 apply 运行中到期时，单 key Lua 都会在删除前要求显式确认立即删除影响。
- `bounded` / `enforce` floor 会机器校验 migration completion/checkpoint 和同一当前 floor 下最近
  两次完整 aggregate observe；首次推进 floor 时还会持久化批准的最小观测间隔，后续推进必须
  使用至少 `1h` 且不低于已持久化值的间隔，两份证据必须达到本次批准的新值。旧版 control
  若没有该字段仍可读取，并在下一次单调推进时由 CAS 自动补齐，无需删除或手改 Redis key。
  缺失、损坏、含读取歧义、间隔不足或计数不达标均 fail closed。上线命令、required-floor
  配对、Redis/连接预算和回滚边界见 runbook。
- 已把主动改密/重置、账号禁用/最终注销、管理员删除与 monotonic revocation intent 放入同一
  MySQL 事务；同步 Redis 应用失败后由带 owner lease 的多副本 worker 幂等重试。当前退出先撤
  HTTP bearer，再执行既有 IM 退出。新 intent 的 `next_attempt_at` 为同步 by-ID claim 保留 5 秒
  优先窗口；请求进程退出或失败后，共享 worker 再接管，不依赖副本本地锁或状态。
- 登录 writer 已覆盖普通、username、email、manager、微信、GitHub、Gitee、OIDC/external、扫码、
  设备验证和注册；密码或账号状态在 issuance fence 后重新读取。管理端同时重查 `status` 与
  `is_destroy`，禁用/注销账号不能在撤销后重新登录。
- 复核额外发现并修复了旧 revocation event 重放清空新会话索引的问题：索引按 generation 隔离，
  Lua 返回本事件实际撤销的上一 generation，重试只清理该旧索引，不影响事件后新登录的 cap。
- 本地 MySQL/Redis/WuKongIM 环境中，`pkg/auth`、`modules/user`、`modules/oidc` focused/integration、
  auth race、build、vet、golangci-lint 和 i18n 均通过。`go test ./...` 未全绿：未改动的
  `internal/msgextraseq` 持有共享 `test` 库直到 10 分钟超时，其他并发 package 随后报 MySQL 1205
  lock wait；PR 不得把该次全仓运行写成通过。
- `/v1/user/pc/quit`、设备删除和 OIDC sync `invalid_grant` 的精确撤销 scope 尚未获产品签字，当前
  实现没有擅自扩大。它们与生产 Redis 拓扑/QPS/连接预算、session cap 值、cutoff 和迁移策略同为
  激活前门禁；因此当前状态不是漏洞关闭。

## Security invariants

以下不变量优先于可用性优化和兼容捷径：

1. v3 的有效条件同时为：Token key 存在、`PTTL > 0`、`now < expires_at`、generation record
   存在且为 active、payload generation 与当前值一致。任一读取失败或不匹配都拒绝。
2. Redis TTL 负责回收，payload `expires_at` 与 generation 负责安全兜底；资料更新、重复登录、
   补偿、迁移和回滚都不能延长原绝对 deadline。
3. `RevokeAll` 先改变 generation；会话索引只用于枚举删除和容量治理。索引缺失或损坏不能让
   全部撤销失效。
4. 登录在验证密码/账号状态之前取得共享 issuance fence，并在返回 Token 前确认 fence 未变化。
   安全事件开始前读取的旧凭据不能在事件后提交为有效会话。
5. 新 credential 只有 Token、generation、会话索引和兼容索引达到可验证状态后才能返回。
   任一步失败必须按 issue owner 精确补偿；复用既有 credential 失败时不得误删其他副本已接管
   的会话。
6. legacy deny marker 只有在所有在线副本都已停止写 v2 后才能启用；否则新 v2 登录会被 marker
   立即拒绝。marker 在全局 enforce 前不得自动过期。
7. 多副本一致性只依赖 Redis 单 key 原子操作和共享状态；不得用 Go mutex、本地 generation
   cache 或“只有一个 pod 会执行”的假设作为安全边界。
8. Token、完整 Redis key、payload、generation、索引成员和 UID 不得进入日志、指标 label、
   migration checkpoint 或命令输出。

## Recommended design

### 1. 单调发布状态机

新增一个严格解析、启动时固定、默认 `expand` 的 session rollout mode。建议状态为：

| mode | writer | legacy reader | 安全能力 | 进入条件 |
| --- | --- | --- | --- | --- |
| `expand` | v2 | 兼容 v1/v2 | PR 2 代码已在位但不产生 v3 | PR 2 首次部署 |
| `v3-write` | v3 | 兼容 v1/v2；已有 deny marker 仍拒绝 | generation/index 生效；deny marker 创建与批量迁移禁用 | 所有旧制品已清零 |
| `revoke` | v3 | 兼容 v1/v2，并检查 per-UID deny marker | 完整撤销矩阵和 durable retry 生效 | 所有副本均为 `v3-write` 或更高 |
| `bounded` | v3 | 只接受有限且未超过批准上限/截止时间的 v1/v2 | 永久、超上限 legacy fail closed | apply 完成且 `persistent=0, over_max=0` |
| `enforce` | v3 | 拒绝全部 v1/v2 | 最终状态 | 完整扫描确认 `v1/v2=0` |

- mode 使用一个枚举配置，避免 `writer_v3=false + enforce=true` 等非法布尔组合；显式空值、未知值
  或低于持久 rollout floor 的配置必须启动失败。具体配置名在实现前定稿，并纳入启动日志和
  低基数 gauge。
- 滚动升级允许 `expand` 与 `v3-write` 短暂混跑，因为所有 reader 已能安全识别 v3；此时不得
  进入 `revoke`、运行 apply 或创建 legacy deny marker。
- 使用一个无 TTL、单 key 的 rollout control record 保存最小 writer version 和最小安全 mode；
  同一 record 还保存显式批准的最小 observe 间隔。该值硬下界为 `1h`，后续阶段可经审批增大但
  不得降低；证据必须满足本次写入的新值。旧版 record 缺失该字段时允许读取，下一次合法推进
  以运维传入值原子补齐，不要求生产 Redis 手工删除或改写；
  只能由显式运维命令 CAS 向前推进，不能由公开 HTTP API 修改。所有副本进入 `v3-write` 后才把
  writer floor 设为 3；所有副本进入 `revoke` 后才允许创建 deny marker/运行 apply；apply 完成
  后才把安全 floor 推进到 `bounded`。运行时配置低于 floor 必须启动失败，迁移工具在前置 floor
  缺失时拒绝执行。`v3-write` reader 即使尚未获准创建 marker，也必须尊重已存在的 marker，作为
  混版/误配置的防御。writer floor 激活后，control record 缺失或不可读必须 fail closed，不能
  被解释为尚未激活并恢复 v2 writer；部署配置仍须保持 v3 floor，control record 不能成为唯一
  回滚防线。
- 每个副本暴露 build SHA 与 rollout mode；进入下一阶段前必须同时核对 Deployment
  `desired/current/ready`、旧 ReplicaSet 为 0 和应用 mode 指标，不能只看一次健康检查。

### 2. 在首次激活前定稿 v3 payload

PR 1 从未启用 v3 writer，PR 2 是调整 v3 schema 的最后安全窗口。新 payload 至少包含：

- `uid`, `name`, `role`, `lang`
- UTC Unix seconds 的 `issued_at`, `expires_at`
- `device_flag` 与可获得时的稳定 `device_id`
- 不可预测的 `session_generation`
- issuance/revocation fence revision，用于阻止安全事件期间开始的登录在事件后提交

要求：

- `expires_at = issued_at + effective TokenExpire`，Redis TTL 以该绝对 deadline 计算并向下取整，
  不能分别用两个 `now` 形成 Redis TTL 晚于 payload deadline。
- 新 Token 使用可注入 clock 测试秒边界；generation 使用密码学安全随机值，不用可预测计数值
  充当 credential fence。
- v3 `UpdatePayloadKeepDeadline` 必须从当前 payload 复制 `issued_at/expires_at/device/generation/
  revision`，只允许更新显示快照；不得继续调用 `auth.Encode` 生成 v2。当前昵称更新丢弃 `ok`
  的路径也必须处理 missing/version-conflict，避免 DB 已更新却把安全更新错误呈现为成功或失败不明。
- v3 复用保留原 `issued_at`、`expires_at`、generation 与剩余 TTL。将仍有效的 v1/v2 复用 Token
  提升为 v3 时，以 `min(当前剩余 TTL, 配置上限)` 建立 deadline，绝不续期；提升失败时不得留下
  “v3 已写但索引缺失”的 credential。
- v3 一旦在线，任何 writer/update Lua 都必须拒绝 v3 降级为 v2；仓库级 reader/writer guard
  继续覆盖全仓。

### 3. Generation store 与登录 fence

generation store 复用现有 session Redis client。每个 UID 的逻辑记录至少包含 current generation、
issuance revision、active/revoking 状态和最后应用的撤销事件版本；具体序列化可实现时决定。

- generation 缺失时，新登录通过单 key `SET NX` 初始化；并发初始化只有一个值获胜，其他请求
  重新读取。已有 v3 的 generation 缺失时 validator 只拒绝，不能自动“修复”为 payload 值。
- generation key 的 TTL 必须覆盖该 UID 最晚到期 v3 Token。每次成功签发延长到至少本次
  `expires_at`，并用 generation 单 key 原子比较实现“只延长、不缩短”；较短会话的并发签发不能
  把仍有长会话依赖的 generation 提前过期。延长失败则 credential 不得返回。若选择永久
  generation key，必须说明删除/注销清理和容量预算，不能无评估地永久增长。
- `BeginIssue` 在读取并验证密码、账号状态或外部身份结果之前取得 `(generation, revision)`；
  `IssueNew/ReuseExisting` 在 Redis 写入前后确认 fence，变化时精确补偿本次新 credential 并返回
  可观测失败。事件后开始的新登录取得新 fence，可按产品策略正常登录。
- `RevokeAll` 原子切换到新的 generation 并推进 revision；即使 index 不存在也立即让全部 v3
  失效。相同 durable event 的重试必须幂等，不能再次旋转 generation 而误撤用户事件后新建的
  会话；乱序旧事件也不能覆盖较新的 generation。
- device-scope 撤销要建立共享 issuance barrier，阻止同 scope 的并发登录穿过索引删除窗口。
  若不能证明 device index 完整或 device identity 稳定，安全回退是扩大到 `RevokeAll`，不是返回
  “撤销成功”。

认证 v3 稳态明确允许且预计为两次串行 Redis 读取：一次单 Token key Lua，一次 generation
record GET/单 key Lua；两次均不写 Redis。不采用跨 Token/generation key Lua，不把 generation
缓存到副本内，也不为它新建连接池。

### 4. 有界 per-UID 会话索引

新增按 UID + generation 枚举 v3 会话的可过期索引，`UIDToken` 仅在兼容期继续服务旧行为。
generation 隔离使撤销事件只清理自己淘汰的旧索引；事件后新登录写入新索引，不会被旧事件重放
或首次撤销的并发尾部误删：

- 索引成员至少能定位 Token，并带 device flag/device ID 与绝对到期信息；score 使用
  `expires_at`，每次读写先清除过期成员，索引 key TTL 对齐当前最晚成员。并发新增只能延长 key
  deadline；只有在单 key 原子重算仍存成员最大 deadline 后才能缩短，不能由较短会话覆盖。
- 单用户会话数必须有硬上限。达到上限后的“拒绝新登录”或“原子淘汰最旧会话”由产品/安全
  签字；不得静默无界增长，也不得随机删除仍在使用的会话。
- 索引容量预留/清理使用索引自身的单 key Lua。Token key、generation key、索引和 UIDToken
  分步写，按状态机补偿；不得把多个不同 key 放进 Lua 后假设生产一定不分 slot。
- 新签发在索引失败时不能返回 credential。既有 Token 的复用/提升需要与新签发不同的补偿
  语义，故障注入必须覆盖 Token/index/UIDToken/generation 每一步及两个副本交错执行。
- `RevokeAll` 永远以 generation 为即时安全结果，再按 Lua 返回的上一 generation 精确清理旧索引；
  重放相同事件不得删除当前 generation 的索引。设备级撤销只有在索引完整性可证明时才精确删除；
  否则扩大撤销并产生低基数告警。
- Redis 中的索引仍属于 credential 数据，必须使用与 Token cache 相同的 ACL、备份和访问边界；
  工具、日志、指标和 API 均不得枚举其成员。

### 5. 撤销接口与 durable intent

Session Store 当前暴露统一的 `RevokeCurrent`、`RevokeAll`，业务 handler 不再直接拼接
Token/UIDToken key。`RevokeByDevice` 仍属设备精确 scope 的待签字能力，本 PR 不宣称已交付。
HTTP Token 撤销与 WuKongIM device quit 是两个独立结果，前者成功不能由后者替代。

对密码、账号状态、注销等 DB-backed 高风险事件：

- DB 状态变更与 monotonic revocation intent 在同一事务提交；intent 包含随机 event ID、每 UID
  单调版本、目标 scope/target generation 和重试状态，不包含 Token 或明文密码。
- handler 在返回成功前同步应用 intent；应用失败不得伪装成功，pending intent 由有界退避、
  可观测且支持多 worker 竞争的 worker 幂等重试。不能只记日志后丢失。
- 相同 intent 重试不再次撤销事件完成后新建的会话；旧版本 intent 不能覆盖新版本。worker
  claim/lease 和 release 必须比较 owner，进程崩溃后可接管。
- 需要跨 DB/Redis 的精确状态机与故障注入，至少证明：事件成功响应后旧 bearer 已失效；DB
  提交后 Redis 暂时失败时 intent 不丢；Redis 已推进而 DB 失败最多造成额外登出，不会恢复旧
  bearer；并发旧凭据登录不能穿过 issuance fence。
- APP/Web/PC 登录中的同密码 hash 算法升级不属于“用户改变凭据”，不得因 opportunistic rehash
  撤销全部会话；主动改密、忘记密码和管理员重置才进入撤销矩阵。

### 6. 撤销矩阵

| 事件 | HTTP Token 目标 | WuKongIM | 必要约束 |
| --- | --- | --- | --- |
| 当前退出 | 当前请求 Token | 当前设备或保持接口既有范围 | 先使 bearer 无效并 compare-delete 兼容索引 |
| `/v1/user/quit` | 待产品确认：当前 Token、Web/PC 还是全部 | 当前实现退出 Web+PC | 未确认前不得宣称“当前设备退出”已精确实现 |
| `/v1/user/pc/quit` | 对应 device flag 的全部 v3 会话 | 对应设备 | 并发登录受 device issuance barrier 约束 |
| 删除设备 | 相同稳定 `device_id` 的会话；无法证明时扩大到 flag/全部 | 对应设备 | 先撤 HTTP 会话，再删设备记录 |
| 全部退出 | UID 全部 v3 + legacy deny marker | 全部设备 | generation 是即时权威，索引仅回收 |
| 主动修改密码 | 建议撤销包含当前设备在内的全部会话 | 全部设备 | 产品签字；成功响应前完成同步撤销 |
| 忘记密码/管理员重置 | 全部会话 | 全部设备 | 无条件 generation rotate + legacy deny |
| 禁用/最终注销 | 全部会话 | 全部设备 | DB intent 可重试；解禁不恢复旧会话 |
| 管理员降权/删除 | 至少全部管理会话；保留实时 RoleResolver 防线 | 按现状 | resolver 故障回退不能重新赋予旧权限 |
| OIDC logout | 当前 HTTP Token；IdP RT 范围保持现状待签字 | 当前实现踢全部设备 | 保持 RP-Initiated Logout/best-effort wire contract |
| OIDC sync 确认真实 `invalid_grant` | 建议 UID 全部本地会话 | 全部设备 | 作为 durable event，不只踢 IM |
| 昵称/语言变化 | 不撤销 | 无 | 版本不降级，deadline 不变 |

`expand`/`v3-write` 的兼容撤销只能通过 APP/Web/PC 的 UIDToken 反查各处理最新一条已知会话；
只有进入 `revoke` floor 后，generation rotate 才保证“UID 全部会话”即时失效。因此生产不得把
`v3-write` 阶段的兼容撤销描述为全部会话，也应尽量缩短该过渡窗口。

所有认证失败继续使用现有通用 i18n/anti-enumeration envelope；具体的 expired、generation
mismatch、deny marker 或 Redis error 只进入低基数内部指标/日志，不泄露给客户端。

### 7. Legacy deny 与迁移 apply

`revoke` mode 起，对发生 `RevokeAll` 的 UID 写无 TTL legacy deny marker，所有 v1/v2 立即拒绝；
v3 只看 generation。marker 在 enforce 后可清理，但不得在迁移窗口自动过期。

legacy deny、generation 和 rollout control 都是认证安全状态，不能放在会静默驱逐这些 key 的
缓存策略下。上线前必须确认相关 Redis DB/namespace 为 non-evicting，或提供等价的 durable ledger
与 fail-closed 重建；仅依赖“无 TTL”不能防止 maxmemory eviction。deny marker 丢失不能让已撤销
legacy bearer 重新有效。

新增显式 migration apply 工具/Job，与 API 进程解耦：

- 默认 dry-run；apply 必须显式提供 campaign ID、不可变的绝对 `legacy_cutoff_at`、批准的处理策略、
  batch size 和 QPS。续跑时 campaign 参数不一致必须拒绝。
- 使用带 owner token 和 lease 的单执行者锁；续约失败立即停止新批次，release compare-delete。
  checkpoint 只保存 cursor/campaign/计数，不保存 Token、完整 key、payload 或 UID。
- SCAN 不是快照。一次读失败、锁丢失、取消或游标未归零都必须输出 `complete=false`；不能把部分
  扫描的零计数当作迁移完成。key 在 SCAN 后自然过期计入 missing，不应让整次扫描伪装成功或
  无限失败。
- 每个 Token 由单 key Lua 重新读取当前 payload/version/PTTL 后决定，保证 TOCTOU 下仍满足：
  missing 或已变 v3 则 no-op；只缩短、不延长、不创建 key；短于目标的 TTL 保持不变；重复执行
  不以“本次运行时间 + grace”续期。
- 已有 campaign 的绝对 cutoff 若在续跑前或本次限速扫描中已经过去，工具仍不得延长或替换该
  安全 deadline；
  命中的剩余目标记录必须删除，并在 dry-run/apply 中分别统计为 `would_delete` / `deleted`，使
  运维能在 apply 前评估批量重新登录影响。此时 apply 必须额外显式确认 elapsed cutoff；仅传
  `--apply` 时，每条记录的 Lua 都必须在 `DEL` 前 fail closed，本轮不得生成 completion evidence。
- 永久 v1/v2 使用固定 `legacy_cutoff_at`；有限 v1/v2 按批准策略选择自然等待，或统一收敛到
  `min(当前 deadline, legacy_cutoff_at)`。超过 `TokenExpire` 的有限值至少压到上限。
- v3 permanent/过期/generation 缺失本来就被 validator 拒绝；apply 不得把异常 v3“修复”为
  再次有效，只聚合告警并由单独清理动作删除。
- 迁移使用独立小连接池，不与在线 session pool 争抢连接；限速在 production 同拓扑压测后
  确定。当前一次性工具以聚合 JSON、Job exit status/elapsed 和 Redis 平台指标作为运维证据，
  尚未暴露独立 Prometheus latency/error endpoint；激活前必须把 Job 失败/超时接入告警，不能
  仅依赖人工查看终端。

进入 enforce 前至少完成两次 `complete=true` 的全前缀扫描，并同时满足
`persistent=0, over_max=0, v1/v2=0`；两次扫描间隔和 campaign 证据写入 runbook。不能仅按
grace 已经过期推断 v1/v2 已清零。

### 8. 多副本、Redis 拓扑与性能

- 所有新脚本默认单 key。生产若是 native Redis Cluster，先解决当前 `*redis.Client` 的拓扑
  不匹配；若是 proxy，则在同型号/版本/故障切换路径验证 `SCAN`、`EVALSHA`、`EVAL`、脚本缓存
  与 lease 语义。不能用本地 standalone Redis 结果替代。
- v3 每个认证请求预期两次串行 Redis 读取、零写入。基准测试必须断言命令数，生产容量评估按
  峰值认证 QPS 至少翻倍估算 Redis command rate，并纳入 p95/p99 latency、pool wait/timeout、
  Redis CPU/network 和 fail-closed 401 风险。
- 迁移窗口内 v1/v2 在解码 UID 后还要检查 legacy deny marker；该额外读取同样复用 session
  pool，并单独统计命令数/时延。进入 enforce 后直接拒绝 legacy，不再为其查询 marker。
- 继续使用 `OCTO_AUTH_SESSION_REDIS_POOL_SIZE` / `...POOL_TIMEOUT` 控制唯一 session pool。
  上线前按 `PoolSize ×（稳定副本数 + maxSurge）` 核算 `maxclients`；不得通过为 generation
  新建 pool 绕过预算。
- generation Redis 故障必须 fail closed，且触发独立告警；不得回退到“只检查 Token TTL”，
  不得在本地缓存 generation 延迟撤销。为避免故障被误判成用户 Token 失效，客户端 wire
  contract 是否仍固定 401 需沿用现状并在灰度中监控重新登录风暴。
- session cap、索引 key 数、generation key 数和 outbox backlog 都必须有容量模型与告警；指标
  只用 operation/outcome/reason/mode 等低基数 label。

## Load-bearing list

- `pkg/auth`：v3 schema、Session Store、TokenValidator、generation resolver、issue owner 补偿和
  仓库级 reader/writer guard。
- 所有签发/复用入口：普通/username/email、GitHub/Gitee/OIDC、扫码、设备验证、注册和 manager。
- 所有显式 Token reader：AuthMiddleware、group 可选认证、message body Token、`/auth/verify`、
  qrcode 二次读取等，必须继续走同一 validator。
- Redis：Token/UIDToken 兼容 key、generation、会话索引、legacy deny、rollout floor、migration
  lock/checkpoint 和脚本单 key 原子性。
- MySQL：高风险状态变更与 durable revocation intent/outbox 的同事务语义、worker claim/retry。
- 多设备/多副本：Web/PC 复用、APP 替换、并发登录/退出/改密、索引补偿和旧事件重试。
- WuKongIM：HTTP bearer revoke 与 IM device quit 的顺序、独立结果和重试。
- `wire-contract` / i18n：Token header、登录响应和通用未认证错误 envelope 不变。
- 性能与容量：认证 Redis command rate、唯一 session pool、migration 独立 pool、Redis
  `maxclients` 和 rollout `maxSurge`。
- 运维：mode 单调推进、旧副本清零、migration campaign、enforce 与安全复测。

## Out of scope

- 不设计短 Access Token + 旋转 Refresh Token、静默续期或 refresh replay family；这是 Web/
  iOS/Android 联合任务。
- 不实现每次 API 活动都写 Redis 的滑动空闲超时。
- 不改变客户端 Token header、登录响应字段、opaque Token 使用方式或现有错误 envelope。
- 不处理 OIDC Provider 自己的 access/refresh/id_token 生命周期，也不处理 Bot token、App Bot
  token、User API Key、Webhook secret、短信/扫码一次性凭据。
- 不以 PR 2 自动启动生产 SCAN/apply/enforce，不在未核实前宣称生产 Token 数量、Redis 拓扑、
  可接受 QPS 或影响用户数。
- 不顺手重构无关登录/设备代码；只改为满足会话签发、验证、撤销和迁移不变量所必需的路径。

## Observability and operations

- Counter：issue/reuse/update/revoke/migrate 按 `operation,outcome`；validation reject 按
  `reason`；outbox retry/dead-letter；device revoke fallback-to-all。
- Gauge：当前 rollout mode、v2/v3 writer 副本数、session index/member 数量级、outbox backlog、
  最近完整 observe 的 `persistent/over_max/v1/v2/v3` 和 `complete`。
- Histogram：Token read、generation read、index operation、整体 validation、outbox apply、迁移
  batch 时延；在线与 migration pool 分开。
- 启动日志只记录生效 mode、TokenExpire、pool size/timeout、build SHA 和 rollout floor，禁止
  credential/UID。审计日志只记录 event type、哈希化主体、scope、event version、结果和 trace ID。
- 告警至少覆盖 generation 读取错误/缺失、session pool timeout、认证拒绝率突增、v3 persistent、
  新增 v1/v2 writer、outbox 超龄、撤销失败、迁移 incomplete 和 enforce 后出现 legacy。

## Rollout

具体命令、配置配对、Kubernetes 旧副本清零证据、Redis 预检和 stop condition 见
`docs/token-session-rollout-runbook.md`；以下状态机仍是放行依据：

1. 在生产同拓扑预检配置、Redis Lua/SCAN/lease 兼容、non-evicting 安全状态、连接数和 v3/
   legacy 额外读取性能预算。
2. 以 `expand` 部署 PR 2，清零所有 PR 2 之前副本；确认 v3 仍未签发。
3. 滚动到 `v3-write`，确认全部副本 mode/build 一致；设置单调 v3 writer floor。此阶段不得 apply。
4. 滚动到 `revoke`；全部副本就绪后推进 revoke floor，再验证高风险事件、generation、legacy
   deny 和 outbox 指标，随后启动限速 apply。
5. apply 完成并两次完整 observe 证明 `persistent=0, over_max=0` 后进入 `bounded`；全部副本
   就绪后再推进 bounded floor。
6. 按批准策略等待/迁移到两次完整 observe 均为 `v1/v2=0`，再灰度 `enforce`。
7. 使用报告历史 Token、退出、改密/重置、禁用/注销、多设备、多副本竞态和 Redis 故障场景复测。

每一步有独立 go/no-go；自动化工具必须检查可验证前置条件，但不能伪造 Kubernetes 旧副本已
清零或生产安全审批已经完成。

## Rollback

- `expand` 尚未产生 v3 前可回滚到 PR 1 制品。
- 任一 v3 已签发或 writer floor 已设置后，只能回滚到能解析、校验 generation 且继续写 v3 的
  PR 2 兼容制品；PR 1 没有运行时 generation resolver，不能作为可用回滚版本。
- apply 可暂停/续跑，回滚不延长已经缩短的 TTL，不删除 legacy deny marker，也不清除 writer
  floor。已过期 Token 通过重新登录恢复，不能复活。
- enforce 灰度且 enforce floor 尚未建立时，异常最多退到 `bounded`，仍拒绝 permanent/over-max
  legacy、继续写 v3 并遵守 deny marker；退回即意味着漏洞关闭条件暂时不成立，必须记录安全
  例外。enforce floor 建立后不可再启动 bounded 实例，只能回滚到遵守 enforce floor 的兼容 v3
  制品。禁止退到 `expand`。
- generation/outbox 故障不得通过关闭 generation 校验或恢复 v2 writer 缓解；可暂停新登录/迁移、
  扩容 Redis、回滚到兼容 v3 制品并处理 backlog。

## Acceptance

- rollout mode 配置缺省为 `expand`；显式空/非法值启动失败。混版测试证明所有 PR 2 reader 可读
  v2/v3，writer floor 设置后任何 v2 mode 实例拒绝启动，apply 在 floor 缺失时拒绝运行。
- v3 新签发 Redis TTL `>0` 且不晚于 payload `expires_at`/配置上限；资料更新和 Web/PC/扫码复用
  前后 `issued_at/expires_at/generation/revision` 不变，TTL 不增加，v3→v2 被原子拒绝。
- 所有生产签发入口在 `v3-write` 后只产生 v3 并进入有界索引；源码守卫证明业务模块不能直接写
  Token/UIDToken/generation/索引 key。
- validator 对 v3 permanent、过期、generation missing/error/mismatch、revoking state 一律 fail
  closed；AuthMiddleware 和所有显式 Token 入口结果一致，保持现有 i18n/anti-enumeration 响应。
- 命令计数测试证明 v3 稳态认证为 Token 单 key Lua + generation 单 key read、零写入；每个
  `config.Context` 仍只有一个 session pool。代表生产峰值及 `maxSurge` 的同拓扑负载验证满足
  发布前签字的 p95/p99、pool timeout 和 Redis 余量 SLO。
- 两个独立 Redis client/Session Store 模拟两个副本，覆盖 issue/reuse/logout/password reset
  交错；安全事件前开始的登录不能在事件后产生有效 bearer，旧 durable event 重试不能撤销事件
  后的新会话，也不能从有界索引移除事件后会话而绕过 session cap。
- 故障注入逐步覆盖 generation/Token/index/UIDToken/DB commit/outbox/IM：新 credential 失败不
  留可用孤儿；复用失败不误删已接管会话；DB 事件不丢；重复 worker 幂等。
- session cap 在并发新签发下仍不超限；索引缺失时 `RevokeAll` 仍即时有效，设备撤销不能证明完整
  时自动扩大范围并告警。
- 多副本并发签发测试证明较短会话不能缩短 generation/index key 的 deadline；安全状态所在
  Redis 发生 eviction、control/deny key 缺失或读取错误时不会恢复 v2 writer 或重新接受已撤销
  legacy Token。
- 撤销矩阵中每个已签字事件都有 HTTP 集成测试；成功响应后旧 Token 返回统一未认证，IM 失败不
  恢复 bearer。密码 hash opportunistic rehash 不触发全量撤销。
- migration dry-run 零写入且无秘密输出；apply 单执行者、可取消续跑、固定 cutoff、重复执行不
  延期，过期 cutoff 的删除与预删除单独统计；覆盖 missing/persistent/finite-short/over-max/
  v1/v2/v3/concurrent-change/lock-loss。cutoff 在入口已过期或 apply 运行中到期时都必须额外
  显式确认；否则入口校验或单 key Lua 会在删除前拒绝，本轮不能生成 completion evidence。
- observe/apply 遇到读取错误、取消或未完成游标会输出 `complete=false`；只有两次完整扫描的
  `persistent=0, over_max=0, v1/v2=0` 且跨过本次批准的间隔才允许 enforce。批准间隔不得低于
  `1h`，并且只能相对 rollout control 中已持久化的值增大、不能降低。
- enforce 后 v1/v2、历史报告 Token、deny marker 命中的 legacy Token 全部失败；新 v1/v2 或
  permanent 异常触发告警。
- 运行 focused `pkg/auth`、`modules/user`、`modules/oidc`、migration tests，`go test -race
  ./pkg/auth/...`、`go build ./...`、`go vet ./...`、`go test ./...`、`golangci-lint run ./...`、
  `make i18n-extract-check` 和 `make i18n-lint`；若环境导致未完成，PR 必须明确列出未验证项，
  不得宣称全绿。

## Decisions required before activation / remaining scope

代码使用两个安全默认但仍需上线签字：单 UID cap 必须显式配置且超限时拒绝新登录；主动改密撤销
包含当前设备在内的全部会话。主动改密/重置与禁用的兼容撤销和 WuKongIM 全设备退出在默认
`expand` 部署即生效，不等待 floor 推进，因此相应产品签字是合并/部署门禁，不是后续 activation
门禁。以下环境参数和未接线路径不能由代码自行猜测：

- legacy grace 与绝对 `legacy_cutoff_at`：原建议 7 天，需生产 observe 后签字。
- 两次完整 observe 的最小间隔；工具要求每次 floor 推进时显式给出，硬下界为 `1h`，后续只可
  经审批增大、不得降低。实际值需结合 Token QPS、Redis 压力和风险窗口签字。
- finite v1/v2：自然等待最长 TTL，还是全部压到 cutoff；后者重新登录影响更大。
- 单 UID 会话硬上限及超限策略；Web/PC 显式重登录继续复用剩余寿命，还是签发新 Token。
- `/v1/user/quit`、`/v1/user/pc/quit`、设备删除的精确产品 scope；缺少稳定 device ID 时允许扩大
  到 device flag 还是全部会话。
- 主动改密是否保留当前设备；本 brief 建议包含当前设备在内全部撤销。
- 管理员降权/删除仅撤管理会话还是全部会话。
- OIDC logout 是否继续“当前 HTTP Token + 全部 IdP RT + 全部 IM 设备”，以及 sync worker 的真实
  `invalid_grant` 是否撤销 UID 全部本地会话；本 brief 默认保持/扩大安全语义，不擅自收窄。
- 生产 Redis 拓扑、proxy/Cluster 命令兼容、迁移 QPS、session pool/`maxclients` 预算，以及 v3
  两次读取可接受的 auth p95/p99 SLO。
- durable intent 的表归属、重试期限/dead-letter/值班告警和数据保留；未确认前不能以日志代替。
