---
type: Task
title: "Task: token-lifecycle-hardening"
description: Enforce a bounded user access-token lifetime, migrate legacy persistent sessions safely, and make security-event revocation complete
tags: [auth, security, redis, session, migration, observability]
timestamp: 2026-08-09T15:44:02+08:00
# --- octospec extension fields ---
slug: token-lifecycle-hardening
upstream: TBD
source: self
---

# Task: token-lifecycle-hardening

> 本 brief 只定义服务端修复边界和上线顺序，不包含生产环境现状结论。
> 生产 `cache.tokenExpire`、Redis 拓扑和历史 Token 数量仍需只读核查。

现有 TTL 配置入口继续作为唯一来源：配置文件使用 `cache.tokenExpire`，环境变量覆盖使用 `TS_CACHE_TOKENEXPIRE`，代码默认值为 `time.Hour * 24 * 30`（720h，即 30 天）。该配置由 Viper 在进程启动时读取，不做运行时热更新；本任务不增加第二个同义配置项。

当前依赖的 Viper v1.16 / cast v1.5.1 最终使用 Go `time.ParseDuration`，不支持 `30d` 这种 day 后缀。仓库示例中的 `tokenExpire: 30d` 会解析为零，再被 octo-lib 静默替换成默认 30 天，容易造成“覆盖已生效”的误判；实现时必须把示例改为 `720h`，并在 octo-server 启动层对 Viper 已完成 env 覆盖后的原始值做显式解析和校验。

## Goal

让所有用户 Access Token 同时满足以下约束：

1. 从签发开始具有服务端可验证的绝对最大寿命；任何资料更新、重复登录或扫码登录都不能把绝对到期时间向后延长。
2. Redis 中的 Token key 必须有有限 TTL；即使未来某次写操作意外丢失 TTL，Token payload 的绝对到期时间仍会让认证失败。
3. 当前设备退出、全部设备退出、密码变更/重置、账号禁用/注销等高风险事件能够按矩阵撤销对应 HTTP 会话，而不只是踢 WuKongIM 连接。
4. 历史永久 Token 通过可观测、可限速、可中断的迁移逐步收敛，避免一次性清空导致全量用户异常登出。
5. 不改变客户端当前携带 opaque Token 的请求协议；Access/Refresh Token 双 Token 协议另立跨端任务。

## Background

### 已验证事实

- 基线：`origin/main@8e2e66a84d1f`。
- 本地使用真实 MySQL、Redis、WuKongIM 链路完成：`POST /v1/user/login` → `PUT /v1/user/current` 修改昵称。
- 登录后 `TTL token:<token>` 为 `720h`；修改昵称后变为 `-1s`，即 Redis 永久 key。
- 直接原因是 `modules/user/api.go` 的昵称更新使用 `Cache.Set` 重写 Token payload；底层 `pkg/redis.Conn.Set` 使用 Redis expiration `0`，会移除原 TTL。
- `pkg/auth.CacheTokenParser` 当前只读取并解码 Redis value；payload v2 没有 `issued_at` / `expires_at`，因此 Redis TTL 丢失后没有第二道绝对过期校验。
- 当前 Redis 客户端配置只有单个 `db.redisAddr`，实现使用 go-redis `*redis.Client` 而非 `ClusterClient`；这只能证明代码按单端点工作，不能据此推断生产后端一定是 standalone，生产仍可能位于 Redis proxy 后。

### Token 写入路径盘点

下列路径会签发或重写用户 HTTP Token，必须统一收口，不能逐点修补：

- `/v1/user/login`、username/email 登录以及已有用户的 GitHub/Gitee/OIDC 登录最终进入 `user.execLogin`。
- 扫码登录 `loginWithAuthCode` 自行复用或签发 Token。
- 设备验证二阶段 `loginCheckPhone` 自行签发 APP Token。
- 手机号/username/email 注册以及 GitHub/Gitee/OIDC 新建用户通过 `createUserWithRespAndTx` 自行签发 Token。
- 管理后台 `manager.login` 自行签发 Token。
- 昵称更新会重写当前 Token payload。
- `execLogin` / 扫码登录在复用 Web/PC Token 时使用 `SET XX` 加完整 `TokenExpire`，会把剩余 TTL 重新续满；这实现的是滑动续期，不是绝对最大寿命。
- `loginCheckPhone` 和 `createUserWithRespAndTx` 只写 Token key，没有写 `UIDToken` 反向索引；多个路径还会先写 Redis Token、再调用 WuKongIM，后续失败时没有统一补偿。

### Token 读取路径盘点

- 主 AuthMiddleware 使用 `pkg/auth.CacheTokenParser`；`modules/report` 虽沿用带 cache/prefix 参数的旧调用形式，但同一个 `WKHttp` 已注入自定义 parser，不构成旁路。
- `modules/group` 的公开邀请详情可选读取 header Token，`modules/message.sendMsg` 会读取 body 中的 Token，`/v1/auth/verify` 会验证请求体 Token；它们都直接 `Cache.Get + auth.Decode`，不会自动继承主 parser 后续新增的 TTL/绝对到期校验。
- `modules/qrcode` 在 AuthMiddleware 之后又直接读取并解码同一 header Token；当前不是认证旁路，但应消除重复解析或改走同一 validator，避免两套语义漂移。
- `updateSystemUserToken` 只给 System/FileHelper/Admin 调 WuKongIM `/user/token` 更新 IM transport Token，不写 octo-server 的 HTTP Token cache，不属于本任务的用户 Access Token writer。

### 当前撤销边界

- OIDC logout 已显式删除当前 HTTP Token，并用 compare-delete 清理恰好指向该 Token 的 `UIDToken` 索引；同时 `RevokeRefreshByUID` 会吊销该 UID 名下全部未吊销 IdP refresh token，`QuitUserDevice(uid, -1)` 会踢全部 WuKongIM 设备。三者均为 best-effort，失败仍返回现有 logout 响应并通过指标区分；该现状不能简写成“只吊销当前设备的 IdP RT”。
- `/v1/user/quit`、`/v1/user/pc/quit`、设备删除目前主要操作 WuKongIM/设备表，没有完整删除 HTTP Token。
- 多条修改/重置密码路径只更新密码，或最多删除 Web 反向索引指向的单个 Token；不能覆盖 APP、PC、历史孤儿 Token。
- 账号禁用/注销调用 `QuitUserDevice` 只会请求 WuKongIM `/user/device_quit`；它不会删除 octo-server Redis Token。
- 删除管理员时已有逻辑会清角色缓存，并尝试删除 APP/Web/PC 三个 `UIDToken` 当前指向的 Token；RoleResolver 正常读取 DB 时能及时阻断旧管理权限，但 resolver 故障会回退到 Token 中的角色快照，该枚举也覆盖不了历史孤儿 Token，因此不能把 RoleResolver 当作完整撤销的替代品。
- 默认 `uidtoken:<flag><uid>`（实际前缀可配置）每个设备类型只保存一个值，不是所有历史 Token 的权威清单；`loginCheckPhone`、注册签发及历史版本还可能产生无反向索引的 Token。
- OIDC sync worker 在确认某条 refresh token 为真实 `invalid_grant` 后，会标记该 RT 吊销并调用 `QuitUserDevice(uid, -1)`；当前仍只踢 IM，不会撤销该 UID 的 HTTP Token。

## Options evaluated

| 方案 | 优点 | 缺口 | 结论 |
| --- | --- | --- | --- |
| 昵称处改成 `SetAndExpire(TokenExpire)` | 改动最小 | 每次昵称更新把寿命续满；未覆盖其他写入、历史永久 Token 和撤销事件 | 不采用 |
| 昵称处使用 `KEEPTTL` | 能修复本次稳定复现 | 重复登录仍会续满；Redis TTL 再次丢失时无 payload 兜底；撤销仍不完整 | 只作为止血原语，不是完整方案 |
| 统一 Session Store + 有绝对过期的 payload + 历史迁移 + 会话索引 | 服务端可闭环，客户端协议不变；能覆盖签发、更新、撤销和迁移 | 改动面较大，必须分阶段兼容上线 | 推荐 |
| 立即改成短 Access Token + 旋转 Refresh Token | 长期安全模型最好 | 需要 Web/iOS/Android 同步升级、重放检测和混版兼容；不能作为本次服务端止血前提 | 单独跨端任务 |

## Recommended design

### 1. 收口 Session Store

在 user/auth 边界引入唯一的用户 Session Store；业务 handler 不再直接拼接
`TokenCachePrefix` / `UIDTokenCachePrefix` 写 Redis。至少提供：

- `BeginIssue` / `IssueNew`：签发随机 opaque Token，写 Token payload、兼容设备反向索引和 per-UID 会话索引。对本地密码登录，`BeginIssue` 必须在读取并验证密码/账号状态之前取得预期 generation，`IssueNew` 再以 CAS 提交；高风险事件推进 generation 后，事件前读取的旧凭据不能落成新会话。
- `UpdatePayloadKeepDeadline`：仅更新昵称/语言等快照，原子保留剩余 TTL 和原 `expires_at`。
- `ReuseExisting`：仅在 key 仍存在时更新 payload，不增加剩余 TTL；缺失时走 `IssueNew`，不能复活已登出 Token。
- `RevokeCurrent`：删除请求携带的 Token，并 compare-delete 匹配的设备反向索引。
- `RevokeByDevice`：按设备信息和会话索引删除所有匹配 Token。
- `RevokeAll`：先推进该 UID 的 session generation，使所有 v3 Token 立即因 generation 不匹配而失败，再尽力删除索引成员；索引删除是回收手段，不是完整撤销的唯一安全依据。
- `Validate`：统一完成读取、TTL 状态、payload 版本、绝对到期和 session generation 校验，供 AuthMiddleware 与所有显式 Token 入口复用。

Redis 原子更新优先使用 Lua `PTTL + SET PX`，兼容当前 go-redis 版本：

- `PTTL > 0`：以原剩余毫秒数重写 value。
- `PTTL == -2`：返回 missing，调用方不得重建相同 Token。
- `PTTL == -1`：记录 `persistent_detected`，在兼容迁移阶段只赋予一次固定 legacy grace TTL；不得继续保持永久。

多 key Lua 前必须核对生产 Redis 是否为 Cluster。若为 Cluster，key 设计必须使用同一 hash tag，或拆成单 key 原子操作并明确补偿；不能假设 standalone。
当前仓库的 `*redis.Client + 单 redisAddr` 没有原生 Cluster 拓扑发现能力；若生产是 native Redis Cluster 而不是兼容单端点 proxy，应先把客户端/slot 设计作为阻塞前置项，不能只调整 Lua key 名后宣称支持。

新签发和复用旧 Token 必须使用不同补偿语义：`IssueNew` 在索引或 IM 更新失败时删除本次新建的 credential；`ReuseExisting` 失败时不得把登录前已有效的旧 Token 当作“新 Token”删除。APP 替换旧 Token、Redis 部分成功、IM 成功/失败的每个组合都要有明确状态机和故障注入测试，避免孤儿 credential 或误踢已有会话。

#### 多副本与性能约束

- 同一进程的所有 Token reader/writer 必须复用 `config.Context` 内唯一的 Session Store 和唯一的有界 go-redis 连接池，不能由 group/message/user 等模块各自建池。PR 1 当前每副本池大小为 10，带有限 dial/read/write/pool timeout 和一次重试；上线前需按副本数核算 Redis `maxclients`，并通过 session latency/error 指标确认没有 pool wait 饱和。若需扩容连接池，应基于压测或生产证据单独调整，不能靠每个模块复制 client 绕过。
- 认证热路径的 payload 与 PTTL 读取必须由一个单-key Lua 原子完成；脚本缓存命中后的稳态为一次 Redis 往返，新 Redis 节点首次 `EVALSHA` 遇到 `NOSCRIPT` 时允许由客户端回退一次 `EVAL`。不得先 `GET` 再 `PTTL` 形成 TOCTOU，也不得在每个已认证请求上写 TTL、索引或 generation。资料更新、登录和撤销才允许写 Redis，避免把本次安全修复变成按请求写放大。
- 未确认生产 Cluster 拓扑前，PR 1 的 Lua 只能操作一个 key；Token key 与兼容 `UIDToken` 索引分步写，并以“新 credential 可补偿、旧 credential 不误删”的状态机处理部分失败。不同副本必须只通过 Redis 中的原子条件协调，不能依赖进程锁或本地缓存保证安全。
- migration observe/apply 不得随每个副本启动；显式运维进程默认限速、连接池独立且更小，可取消并只输出聚合。在线请求与迁移流量的 Redis 延迟、错误率必须分别可观测，避免扫描挤占认证连接。

### 2. Token payload v3 绝对到期

新增可向后解码的 v3 payload，至少包含：

- `uid`, `name`, `role`, `lang`
- `issued_at`
- `expires_at`
- `device_flag`，以及设备级撤销需要的稳定 `device_id`（无设备信息时显式为空）
- `session_generation`（不可预测的 per-UID generation 标识，供 `RevokeAll` O(1) 失效所有 v3 会话）

时间字段固定为 UTC Unix seconds，并通过可注入 clock 测试边界。对 v3，认证同时检查 Redis key 存在、`PTTL > 0`、`now < expires_at` 且 payload generation 等于当前 UID generation。Redis TTL 是回收机制，`expires_at` 和 generation 是安全兜底；任一不满足都拒绝。observe/apply 期间的 v1/v2 不具备这些字段，必须走下文单独的 legacy 兼容策略，不能伪造字段。

generation key 的生命周期至少覆盖该 UID 最晚到期的 v3 Token；validator 发现 generation 缺失必须 fail closed，不能把缺失解释为默认值而重新接受旧 Token。`RevokeAll` 即使索引缺失也必须创建新的 generation。

阶段一的推荐策略为：沿用现有 `Cache.TokenExpire`（YAML `cache.tokenExpire` / env `TS_CACHE_TOKENEXPIRE`），但在 `ConfigureWithViper` 吞掉解析错误之前，对 env 覆盖后的原始值显式校验：缺省才使用 720h；显式值必须是带单位的合法 Go duration、`> 0` 且不超过经安全/产品确认的硬上限。建议暂定最大 720h。生产配置若非法或超过上限，预检必须先处理，不能在发布时静默回退、截断或带错配置启动。启动日志和低基数指标应暴露最终生效的 TTL，便于确认配置覆盖结果。

所有直接读取 Token cache 后调用 `auth.Decode` 的验证入口也必须走同一个 validator，特别是公开邀请可选认证、`message.sendMsg` 的 body Token、`/auth/verify` 和二维码入口，避免主 AuthMiddleware 已收紧但辅助入口仍接受过期 payload。

### 3. 绝对寿命与重复登录

- 资料更新永远保留原 deadline。
- Web/PC 显式重复登录若继续复用旧 Token，只保留剩余寿命，不续满 30 天。
- 若产品要求“重新输入凭据后获得新的完整寿命”，必须签发新 Token；不能通过延长旧 Token 模拟。新 Token 对已有多端会话的影响需由会话索引和设备策略显式处理。
- APP 当前替换旧 Token 的行为可保留，但签发与删除必须由 Session Store 完成。

### 4. 新 Token 的会话索引

为每个 UID 维护可过期的会话索引，覆盖所有新签发路径，而不再把三个
`UIDToken:<flag><uid>` 当作完整清单。索引要求：

- 不写日志、指标或迁移输出中的 Token 明文。
- 成员随 Token 到期清理；读取时清除过期成员。
- 有明确的单用户会话上限，防止索引无界增长。
- 索引可使用按 `expires_at` 排序的集合，key 自身到期时间对齐最晚成员；任何读取/写入都先清理过期成员。具体 key/hash-tag 方案必须在生产拓扑确认后定稿。
- 新 Token key 成功但索引/IM 更新失败时执行针对新 credential 的补偿；复用旧 Token 走前述不同的补偿语义。
- `UIDToken` 在兼容期保留给旧逻辑；最终是否移除另行决定。

会话索引只保证新 writer 产生的 Token 可枚举，不能让历史孤儿 Token 自动可撤销。兼容期 `RevokeAll` 还必须写 per-UID legacy deny marker；在全局 `v1/v2=0` 并进入 enforce 前不得自动过期，validator 对该 UID 的 v1/v2 一律拒绝。该 marker 不是 credential，可以在 enforce 后安全清理。否则密码重置或账号禁用发生在迁移窗口时，未被 `UIDToken` 索引覆盖的旧 Token 会在 marker 过期后重新有效。

### 5. 撤销矩阵

| 事件 | HTTP Token 目标 | WuKongIM | 备注 |
| --- | --- | --- | --- |
| 当前设备退出 | 当前 Token | 当前设备 | 先撤 HTTP Token；IM 失败不能让 bearer 继续有效 |
| OIDC logout（保持现状） | 当前 Token | 当前实现踢全部设备 | 同时吊销该 UID 全部未吊销 IdP RT；保持 RP-Initiated Logout 响应契约 |
| PC/Web 退出 | 对应 device flag 的全部会话 | 对应设备 | 不能只发 CMD |
| 全部设备退出 | UID 全部会话 | `device_flag=-1` | Redis 与 IM 分别计结果 |
| 用户主动修改密码 | 默认撤销全部会话 | 全部设备 | 客户端收到成功后重新登录；若要保留当前会话需产品明确批准 |
| 忘记密码/管理员重置密码 | 撤销全部会话 | 全部设备 | 无条件执行 |
| 账号禁用/最终注销 | 撤销全部会话 | 全部设备 | 状态落库与撤销失败要有可重试任务/告警 |
| 账号解禁 | 不恢复旧会话 | 无 | 必须重新登录 |
| 管理员降权/删除 | 角色缓存立即失效；必要时撤销管理会话 | 按现状 | 保留 RoleResolver 的实时降权防线 |
| OIDC sync 确认真实 `invalid_grant` | 默认撤销 UID 全部会话 | 全部设备 | 延续当前“踢全部 IM”的安全意图；是否收窄需产品/安全明确批准 |
| 昵称/语言变化 | 不撤销，仅更新 payload 且保留 deadline | 无 | 不得续期 |

设备删除在 v3 payload/索引具备 `device_id` 后才能精确撤销；在此之前宁可撤销该设备类型或全部会话，不得声称已完成设备级吊销。

高风险事件与登录签发必须覆盖并发竞态：事件开始前已经通过旧密码校验、但事件后才提交的 Token 不能存活。若不采用 generation/CAS，就必须给出等价的 per-UID 串行化方案及 Redis 故障时的 fail-closed 语义。涉及 DB 状态变更（密码、禁用、注销）时，撤销失败不能只写日志后丢失；需要持久化重试事件/告警，并明确“安全优先导致额外登出”可接受，“状态已变更但旧 bearer 无界有效”不可接受。

### 6. 历史 Token 迁移

提供显式运维工具/Job，不在每个副本启动时无锁全量 `SCAN`。扫描必须读取运行时配置的 `TokenCachePrefix` / `UIDTokenCachePrefix`，默认示例分别为 `token:*` / `uidtoken:*`，不能硬编码大小写或假设生产未改前缀：

1. `observe`：只读扫描配置前缀，仅输出总数、`missing / persistent / finite / over_max / decode_invalid / v1 / v2 / v3` 聚合，不输出 key、Token 或 payload。`UIDToken` 只用于一致性统计，不能据此推导完整会话数。
2. `apply`：游标可续跑、批量限速、分布式单执行者；对永久 key 设置一次 legacy grace TTL（建议 7 天），对超过最大寿命的有限 TTL 做上限收敛；重复执行幂等且绝不延长现有 TTL。
3. `enforce`：确认旧副本已全部退出、writer 已全部切 v3、`persistent=0` 且实际 `v1/v2=0` 后，认证拒绝所有 legacy payload；v3 的永久 key和过期/generation 不匹配 payload 从 v3 validator 上线起即拒绝。

无法从 v1/v2 payload 推导真实签发时间，因此不能伪造 `issued_at`。legacy grace 只用于平滑退出，不能被刷新、资料更新或重复迁移延长。

只给永久 legacy key 设置 7 天 grace 时，原本具有正常有限 TTL 的 v1/v2 最长仍可能存活一个 `TokenExpire` 窗口，因此不能仅观察 7 天就 enforce。若业务要求 7 天后统一拒绝 legacy，`apply` 必须显式把**所有** v1/v2 TTL 收敛到 `min(current_ttl, 7d)`，并在执行前评估重新登录影响；否则应等待 observe 证明 v1/v2 自然清零。

### 7. 混版兼容与发布顺序

v3 是新的 cache value 格式，当前旧二进制不能解码。必须采用 expand/activate 两阶段发布；Release A/B 是运行模式，不强行等同于 PR 1/PR 2：

1. Release A（expand）：所有 parser/显式 Token 入口统一走 validator；validator 能解码 v3，并对任何读到的 v3 强制校验 `PTTL`、`expires_at` 和 generation。完整 v3 writer/index/generation 能力随制品上线但由开关保持关闭，线上 writer 继续写 v2；TTL 保留、原始配置校验、指标和 migration observe 生效，legacy 暂按兼容模式读取。
2. 淘汰全部 Release A 之前的副本，确认回滚制品能解码/校验 v3，并能在回滚时继续以 v3 模式写入。
3. Release B：writer 切 v3，启用新会话索引、generation/CAS 和完整撤销；随后执行 legacy apply。
4. 确认 `persistent=0`，并按选定策略等待 v1/v2 实际清零（自然等待最长有限 TTL，或事先批准后统一压到 grace），同时观察错误率和重新登录率，再进入 enforce。

旧副本仍存活时不得执行 apply/enforce：它仍可能通过昵称更新重新制造永久 key。Release B 不能回滚到不识别 v3 的旧制品，只能回滚到 Release A。

### 8. 建议交付拆分

- PR 1（立即止血）：原始 TTL 配置校验及错误示例修正、Session Store 的 v2 writer、昵称/重复登录保留 deadline、所有签发路径收口、统一 validator（含 v3 前向解码和 v3 时间校验）、指标及 migration observe。该 PR 不改变客户端协议，也不开始批量过期历史 Token。
- PR 2（expand 能力）：实现受开关保护的 v3 writer、generation/CAS、per-UID/device 会话索引、撤销矩阵及 migration apply/enforce 能力；先以 Release A 模式部署并清零旧副本，再通过配置进入 Release B，不能在代码首次出现时同时切 writer。
- 运维变更：旧副本清零确认、生产只读盘点、限速 apply、grace 观察和 enforce；每一步有独立 go/no-go 与回滚检查。
- 跨端后续：若 30 天仍不满足目标，再设计短 Access Token + 单次旋转 Refresh Token；不阻塞前两项服务端整改。

## Load-bearing list

- `auth`：opaque Token cache 格式、AuthMiddleware parser、直接 Token 验证入口和反枚举错误语义。
- `wire-contract`：客户端仍使用现有 `token` header；过期/撤销继续呈现统一的未认证响应，不泄露具体原因。
- Redis：Token payload、TTL、`UIDToken` 兼容索引、新 per-UID 会话索引、Lua 原子性和 Cluster key-slot 约束。
- 所有签发入口：普通/username/email 登录、GitHub/Gitee/OIDC、扫码、设备验证、注册和 manager 登录。
- 多设备行为：APP 替换、Web/PC 复用、扫码登录复用以及并发 logout/login 的“不复活”保证。
- 高风险账号事件：当前/全部退出、密码修改/重置、账号禁用/注销、管理员降权。
- WuKongIM：HTTP Token 撤销与 IM device quit 是两个独立结果，不能互相替代。
- 生产迁移：历史永久 Token、旧 v1/v2 cache value、混版副本、灰度、回滚和限速。
- 可观测性：不得记录 bearer/token key/value；指标 label 不含 uid/token。

## Out of scope

- 本任务不设计新的客户端 Refresh Token 协议、Access Token 静默刷新或 Refresh Token 重放族检测；这需要 Web/iOS/Android 联合 brief。
- 本任务不实现按“每次 API 活动”滑动的空闲超时；绝对到期先落地，避免每请求写 Redis。
- 不改变 OIDC Provider 自己的 access/refresh/id_token 生命周期；这里只处理 octo-server 用户会话。OIDC logout 现有 IdP RT 吊销继续保留。
- 不处理 Bot token、App Bot token、User API Key、Webhook secret、短信/扫码一次性凭据等其他 credential 类型。
- 不修改客户端 Token header、登录响应字段或错误 envelope。
- 未完成生产只读核查前，不在文档中宣称生产 TTL、永久 Token 数量或影响用户数。

## Observability and operations

- Counter：签发/更新/撤销按 `operation,outcome`；认证拒绝按低基数 `reason`；legacy permanent 检测与修复数量。
- Gauge：迁移最近一次扫描的 `persistent / over_max / legacy` 剩余量；不得以 token/uid 为 label。
- Histogram：Redis Session Store 操作时延、迁移批次时延；不记录 key。
- Alert：enforce 后仍检测到 permanent Token、新签发出现无 TTL、撤销连续失败、认证错误率/重新登录率突增。
- 审计日志只记录事件类型、匿名/哈希化主体标识、device flag、结果和 trace id；禁止 Token 明文及完整 Redis key。
- 运维 Runbook 必须包含 dry-run、批大小/速率、暂停/续跑、验证查询、混版检查、回滚制品和用户沟通门槛。

## Rollback

- Release B 只允许回滚到能解码并校验 v3、且保留 v3 writer 能力的 Release A 制品；禁止回滚到当前 main 版本。
- 回滚代码不回滚已收紧的 TTL，不把 key 恢复为永久；已过期 Token 通过重新登录恢复，不能“复活”。
- legacy apply 可暂停/续跑，但回滚不延长已经设置的 TTL；需要改变 grace 时只能对尚未处理的 key 使用新策略，不能按重试时间给已处理 key 续期。
- enforce 如引发异常，可退回“接受具有有限 TTL 且未命中 legacy deny marker 的 v1/v2”兼容模式；永久 key和 `expires_at` 已过期/generation 不匹配的 v3 在任何回滚模式下仍拒绝。Release B 激活后即使回滚到 Release A 制品也必须保持 v3 writer 开关开启，不能重新制造 v2；若 v3 签发本身故障，应暂停新签发并修复，不能用恢复 legacy writer 绕过。
- Redis/IM/DB 任一撤销链路失败必须保留可重试记录和告警；不得仅返回成功后丢失失败事件。

## Acceptance

- 真实 Redis 集成测试证明：新登录 Token TTL `> 0` 且 `<= configured max`；改昵称前后 TTL 不增加，绝不会从有限值变为 `-1`。
- 配置测试证明合法的 `cache.tokenExpire` 和 `TS_CACHE_TOKENEXPIRE`（例如 `48h`）能覆盖默认 720h，且 env 优先；`30d`、无单位数字、其他 malformed 值、零值、负值或超过硬上限配置均在启动期 fail loudly，不能静默回退默认值。
- payload v3 被人工放入永久 Redis key 时，无论 `expires_at` 是否仍有效都因 `PTTL=-1` 被拒；有限 TTL key 中 `expires_at <= now` 或 generation 不匹配的 v3 仍被所有认证/verify/辅助入口拒绝。
- Web/PC/扫码复用旧 Token 时以 `PTTL + SET PX` 保留原 deadline；并发 logout 后不得复活 Token。
- 所有生产 Token writer 通过 Session Store；源码守卫禁止业务代码直接对 `TokenCachePrefix` / `UIDTokenCachePrefix` 执行 `Set` / `SetAndExpire`。
- 普通、username/email、GitHub/Gitee/OIDC、扫码、设备验证、注册、manager 登录均有测试证明签发有限 TTL，并正确进入会话索引。
- 故障注入覆盖 Token/index/UIDToken/IM 每一步失败：新签发失败无可用孤儿 credential，复用失败不误删登录前已有效的旧 Token。
- 当前退出、全部退出、密码修改、各类密码重置、禁用和注销按撤销矩阵测试；旧 Token 随后访问返回统一未认证响应。
- 并发测试证明旧密码校验与密码修改/重置竞态时，安全事件前开始、事件后才提交的会话不能存活；v3 索引缺失时 `RevokeAll` 仍通过 generation 立即失效 Token，legacy 孤儿则由 deny marker 阻断。
- 两个独立 Redis client/Session Store 实例模拟不同副本时，并发复用与 logout 不得复活 bearer；补偿撤销只能 compare-delete 自己的兼容索引，不能删除其他副本刚写入的新 Token 索引。
- OIDC logout 继续撤当前 HTTP Token、吊销该 UID 全部未吊销 IdP RT、踢全部 IM 设备，并保持现有 best-effort/RP-Initiated Logout 响应行为。
- legacy 迁移工具 dry-run 不写 Redis、不输出秘密；apply 可中断续跑、幂等且不延长 TTL；测试覆盖 `TTL=-2/-1/>max/normal`。
- 混版测试证明 Release A 能读取 v2/v3，Release B 写 v3；发布检查能阻止旧副本存活时进入 enforce。
- 迁移完成后按配置的 Token 前缀扫描：`persistent=0`、`over_max=0`、`v1/v2=0`；enforce 期间新增这些类型会触发告警。
- Redis 不可用或 payload/TTL 不合法时认证 fail closed，且保持现有 i18n/anti-enumeration wire contract。
- 认证 validator 稳态每次请求执行一次单-key Redis 脚本且不写 Redis（允许新节点首次 `NOSCRIPT` 回退）；每副本只创建一个 Session Store 连接池，池大小、timeout、retry 和操作时延均有测试或指标证据，迁移扫描不与在线池混用。
- 相关测试至少覆盖 `pkg/auth`、`modules/user`、`modules/oidc`；运行 focused tests、`go test ./...`、`make i18n-extract-check`、`make i18n-lint` 和 lint。

## Decisions required before implementation

- Access Token 服务端硬上限：复用现有 `cache.tokenExpire` / `TS_CACHE_TOKENEXPIRE`，建议最大 30 天；若要更短，需要同步确认客户端重新登录体验。
- legacy grace：建议 7 天；生产 permanent 数量和活跃率核查后再定。
- legacy finite 策略：等待最长有限 TTL 自然清零，还是把全部 v1/v2 一次性压到 grace；后者会扩大重新登录影响，必须单独 go/no-go。
- 用户主动修改密码后：建议撤销包含当前设备在内的全部会话。
- 设备级会话上限及 Web/PC 多登策略。
- 生产 Redis 拓扑（standalone/sentinel/cluster）与可接受的迁移 QPS。
- OIDC logout 当前“当前 HTTP + 全部 IdP RT + 全部 IM 设备”的既有语义是否原样保留；本 brief 默认保留，不擅自收窄。
- `/v1/user/quit`、`/v1/user/pc/quit` 与设备删除分别对应“当前 Token、设备类型还是全部会话”；在接口语义确认前按更安全的扩大撤销处理，不能声称已实现精确设备撤销。
- OIDC sync 的真实 `invalid_grant` 是否继续按当前安全意图撤销该 UID 全部本地会话；本 brief 默认撤销全部。
- Refresh Token/空闲超时是否另开跨端任务；不得用无限滑动 TTL 替代正式决定。
