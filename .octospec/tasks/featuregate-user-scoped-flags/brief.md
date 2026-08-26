---
type: Task
title: "Task: featuregate-user-scoped-flags"
description: 复活 PR #280 的 pkg/featuregate + modules/featuregate 框架（不含 incomingwebhook 接入），扩展 user 维度的 whitelist/percent，并在 modules/featuregate 下新增需登录的只读端点 GET /v1/featuregate/flags 下发用户级灰度位；appconfig 的免鉴权契约与字段集不动。
tags: ["auth", "error-response", "i18n", "rate-limit", "wire-contract", "testing"]
timestamp: 2026-08-25T00:00:00+08:00
slug: featuregate-user-scoped-flags
upstream: Mininglamp-OSS/octo-server#280 （CLOSED，未合并）
source: self
---

# Task: featuregate-user-scoped-flags

## 已定关键决策（本轮拍板，实施按此执行，不要再退回默认方案）

1. **新端点归属**：放 `modules/featuregate` 自己，路由 `GET /v1/featuregate/flags`。
   **不**放 `modules/user`，**不**另开消费方模块。理由见 Background 对应段落。
2. **与 appconfig 的关系**：**归属唯一**（存量 appconfig 位一个不动、新功能只进
   featuregate），确需与部署位联动时由**服务端**做 AND，客户端只读一个最终布尔——
   AND 绝不放在客户端。理由与实现约束见 Load-bearing 对应条目。
3. **刷新契约与失败语义**：冷启动 + 登录成功 + 前台化（节流 ≥5 分钟）；单个 key 遭遇
   存储故障时进响应的 **`unavailable` 数组**（既不下发 `false`，也不是从 `flags` 里
   悄悄省略），客户端对这些 key 保留上次值。
   **2026-08-25 修订**：初稿用「从 `flags` 里省略」来表达这层含义，评审阶段改为显式
   字段——语义等价，但「缺席携带含义」任何 schema 语言都描述不了，codegen 客户端会把
   缺席读成 `false`，恰是这套设计要防的失败。见 Load-bearing 与 Acceptance 对应条目。
4. **对外 JSON key 与 `feature_key` 解耦**：注册表项同时声明运维面的 `feature_key` 与
   对外的 `client_key`，两者独立演进——运维改 gate 名不得破坏客户端。见 Load-bearing
   对应条目。

## 需要外部对齐（不阻塞服务端开工，但上线前必须完成）

- **客户端团队**：实现上面第 3 条的拉取时机，以及"key 出现在 `unavailable` 里 = 保留
  上次值、无历史值 = `false`"的本地语义。这半边服务端单方面做不到。
- **运营**：生效延迟是**分钟级到小时级，不是秒级**。紧急关停必须走
  `OCTO_FEATUREGATE_<KEY>_KILL=1` + 服务端拒绝相关 API，**不能靠隐藏客户端入口止血**——展示位
  不是安全边界。

PR #280 的历史关闭原因**不作为开工前置**：当前已经出现明确的用户级定向灰度需求，是否
实施应由当前需求与当前方案本身决定，而不是由历史 PR 的关闭动作决定。PR #280 仅作为可复用
代码和已知评审问题的参考；本任务不盲目 cherry-pick，也不要求复刻当时的全部设计。

## Goal

给「按用户灰度」提供一条真正能落地的路径：

1. **复活框架本体** `pkg/featuregate` + `modules/featuregate`——以 PR #280 的
   `03f4bbe9` 为实现参考，复用其中的纯函数 `Evaluate`、DB 真源、Redis 读缓存、env kill
   switch `OCTO_FEATUREGATE_<KEY>_KILL` 和 `/v1/manager/featuregate` 管理端点；最终实现以本 brief、
   当前仓库规则和现行代码为准，不照搬已不符合当前契约的细节。**只复活框架，不复活 PR
   #280 第二个 commit（`eb5cc933`）的 incomingwebhook 接入**——理由见 Out of scope。
2. **扩展到 user 维度并修掉白名单语义**：`pkg/featuregate.Dims` 已预留 `UID` 字段，但
   `Evaluate` 当前只消费 `GroupNo`（whitelist 只比 `scope_type=group`，percent 固定
   `Bucket(rule.Key, dims.GroupNo)`）。三处改动：①新增 `ScopeTypeUser`；②把 percent 的
   分桶维度做成规则上的一个字段（`bucket_by`），否则一条规则无法既按群放量又按用户放量；
   ③**把白名单从 whitelist 模式专属，升格为贯穿 mode 的豁免名单**（percent 模式下白名单
   优先命中）——原实现的 `ModePercent` 分支完全不读 `scopes`，导致标准放量路径
   `whitelist → percent` 会把内测人员整批甩掉，详见 Load-bearing。
3. **新增一个需登录的只读端点** `GET /v1/featuregate/flags`（挂在 `modules/featuregate`
   自己下），把"哪些灰度位对当前用户开放"下发给客户端。
   **`GET /v1/common/appconfig` 保持不变**——它故意免鉴权（未登录也要拉
   `local_login_off` 等开关），请求里没有 uid，结构上背不了按用户的判定；本任务不改它的
   鉴权语义，也不往它身上加任何字段。

## Background

**当前灰度现状（本次会话已核实）**：`system_setting` 表 + `SystemSettings` 内存快照
（`modules/common/system_settings.go`）是唯一的通用灰度机制，60s 多实例收敛；
`GET /v1/common/appconfig`（`modules/common/api.go:363` `appConfig`）是唯一下发通道。
所有位都是**部署级全局 on/off**，没有百分比放量、没有按用户/群/空间定向的能力。唯一的
例外是 `modules/bot_mention` 自己写的 env-only allowlist（`config.go:172` `featureGate`），
那是一次性方案，不通用、不走 DB/管理台，本任务不复用它、也不受它影响。

**框架代码在哪（这条之前记错过，已更正）**：曾经有过完整实现，对应 PR #280 的两个
commit（`03f4bbe9` 框架本体、`eb5cc933` incomingwebhook 接入）。分支
`webhook-feature-flag-rollout` 已从 `origin` 删除，但 GitHub 保留了
`refs/pull/280/head` 指向同一 commit，`git fetch origin refs/pull/280/head` 或
`gh pr diff 280` 可直接取到完整代码，**不依赖任何本地 worktree**。（本地
Conductor 工作区 `farmerville-v1` 也仍 checkout 着这个分支，是第二个来源。）
`origin/main` 与当前分支都没有这两个包、没有 `feature_gate`/`feature_gate_scope` 表。

PR #280 于 2026-06-11 关闭且未合并，但其关闭原因不影响本任务立项。历史代码与 review 只用于
识别可复用实现和已知缺口；当前需求已经独立证明 `system_setting` 无法承载用户级 whitelist /
percent，因此本任务按当前契约重新评审和落地。

**appconfig 为什么背不了用户维度的判定**：路由挂在 `commonNoAuth` 组
（`modules/common/api.go:83-93`），没有 `AuthMiddleware`，请求里没有 uid。要在这条路径上
做用户定向，必须先解决"免鉴权接口里怎么安全识别用户"这个更大的问题，代价和风险都不在
本任务范围内（见 Out of scope 的方案 A）。本任务选更干净的路：新开一个挂
`AuthMiddleware` 的端点，`c.GetLoginUID()` 现成可用。

**为什么要 featuregate 而不是继续用 system_setting——理由是"定向维度"，不是"秒级"**
（这条纠正了立项时的一个错误论证）：featuregate 相对 system_setting 的两个卖点是
①支持 whitelist/percent 定向，②Redis DEL 失效带来的秒级多实例收敛。**②对本任务的
展示位基本不成立**：秒级收敛只在"每次请求都在服务端重新评估"的场景有意义（原设计的
`AllowCreate`/`AllowPush` 正是如此）；而展示位是下发给客户端后由客户端持有，端到端生效
延迟由**客户端拉取节奏**决定，不由服务端收敛速度决定。

已核实的现有客户端行为可作参照：iOS `WKAppConfig.m:610` 拉 `common/appconfig`，带
`requestSuccess` 去重（一次成功后不再拉，除非显式强制刷新），由 `WKApp.m`/登录/注册页
触发；Android `WKCommonService.java:40` 声明 `@GET("common/appconfig")`，由
`WKUIKitApplication`/`TabActivity`/登录相关 Activity 调用。**两端都没有发现周期性轮询。**
因此若新端点照搬这个节奏，"关掉开关秒级全量回滚"是一个假承诺。

结论：**立项理由只保留①**。定向维度是 system_setting 结构上做不到的，这才是本任务成立
的原因；②在展示位场景下不作为收益宣称，运营预期必须按"客户端下次拉取时生效"来管理。

**为什么新端点放 `modules/featuregate` 自己，而不是 `modules/user`**：仓库里有一个逐字
对应的先例——`modules/common` 同时装着 `SystemSettings`（框架）和
`GET /v1/common/appconfig`（它面向客户端的下发面），两者同模块。新端点就是 featuregate
的客户端下发面，同构。

需要区分两类消费者：**执行型消费者**（如 incomingwebhook 调 `AllowCreate` 做拦截）确实
历来是各业务模块自己 `NewService(ctx)` 直接调、不经 module 注册；但本端点不是执行型
消费者，它是**框架自己的对外暴露出口**，归属框架。

更硬的理由是「可下发 key 白名单」注册表的归属：这份表登记的是 docs、drive 之类**各业务**
的 key，与 `modules/user` 没有任何语义关系；它的本质是"gate 框架对客户端暴露了哪些位"，
属于框架自身。放在 `modules/featuregate` 内，将来某业务要加一个客户端可见的灰度位，只改
一个模块、一处评审、一个守卫测试即可盖住；放 `modules/user` 则会让一个与用户无关的注册表
长在用户模块里，并让 `modules/user` 反向依赖 featuregate、继续变胖。

路由取 `GET /v1/featuregate/flags`：与 `/v1/common/appconfig`、`/v1/sticker/user` 同形
（模块名做第一段），路径归属与模块归属一致，便于从路由定位代码。不用 `/v1/user/...`
前缀——它不属于 user 模块。

## Load-bearing list

- **`pkg/featuregate.Evaluate` 是零 IO 纯函数**：新增 `ScopeTypeUser` 与可配置分桶维度
  必须保持这个不变量（零 IO、表驱动可测），不能在这一层引入 DB/Redis 依赖。
- **`Dims.UID` 参与 whitelist 命中的语义**：`ScopeTypeUser` 命中条件是
  `dims.UID != "" && s.Type == ScopeTypeUser && s.ID == dims.UID`。空 UID（理论上不会
  发生，消费方都在 `AuthMiddleware` 之后）不应命中任何用户白名单条目——与现有
  `GroupNo == ""` 不匹配 group 白名单的处理方式对称。
- **白名单升格为贯穿 mode 的豁免名单（语义变更，本任务修）**。原实现的 `ModePercent`
  分支**完全不读 `scopes`**，白名单在 percent 模式下是死的。这会在**最标准的放量路径**上
  出事：`off → whitelist（内测）→ percent 5%/20%/50% → on`，切到 percent 的瞬间白名单
  数据还在但不再被读，内测 N 人里只剩恰好 `Bucket(key, uid) < P` 的那部分——N=20、P=5
  时期望只留 1 人。三个后果按隐蔽度排：①内测账号掉功能且**不可复现**（同事正常），排查
  方向天然被引向账号/缓存问题；②**数据孤儿**——展示位不承担鉴权，API 与数据都还在，只是
  入口没了，内测期建过文档的人看得见数据进不去；③最隐蔽的一条：**percent 阶段调
  `addScope` 是静默失败**——返回 200、表里有行、缓存也失效了，读路径却根本不看。

  **修法**：percent 分支在分桶前先做一次白名单判定（复用 whitelist 分支的谓词），命中即
  放行并报 `ReasonWhitelistHit`（保留可观测性，能区分"白名单进来的"与"分桶进来的"）。
  精确语义：

  | mode | 白名单 |
  |---|---|
  | `whitelist` | 生效（唯一判据） |
  | `percent` | 生效，**优先于分桶**（永久豁免 + 其余人群按比例） |
  | `off` | **无条件失效** |
  | `on` | 无意义（本就全放） |

  `off` 那行是硬边界：`off` 是回滚/止血语义的一部分，**白名单不得穿透 `off`**，否则
  kill 的确定性就没了。改完后运维要记的规则反而更简单——"白名单永远优先，除非 off"，
  而不是"白名单只在 whitelist 模式下有效"。

  **为什么不新增第五个 mode（`whitelist_percent`）**：那样运维照样可能选 `percent` 而不是
  新 mode，上面第③条静默失败原封不动；且 mode 会随维度增长成笛卡尔积。改语义现在最便宜
  ——这套东西从未上线，没有存量规则会被影响，零迁移。

  ⚠️ **实现陷阱（必须配集成测试，纯函数单测盖不住）**：Service 层原本只在 whitelist
  模式下加载 scopes——
  `if rule.Mode == fg.ModeWhitelist { scopes, err = s.loadScopes(ctx, key) }`
  ——改语义必须同步加上 `|| rule.Mode == fg.ModePercent`。**漏改这一行的后果与当前缺陷
  一模一样**（`Evaluate` 收到空 scopes、白名单静默失效），只是从设计缺陷降级为实现遗漏，
  更难发现。这一行在 Service 层，`pkg/featuregate` 的表驱动测试照不到，必须有一条走
  Service 的集成测试钉住。
  （`AllowCreate`/`AllowDisplay` 的 percent 路径因此多一次 scopes 加载，有 Redis 缓存，
  成本可忽略；`AllowPush` 不受影响，它只判 `mode != off`。）
- **`feature_gate` 表要加一列 `bucket_by`**：`Rule.BucketBy` 必须持久化，而原表
  （`feature_key`/`mode`/`percent`/`description`/时间戳）没有这一列。因为这张表从未上过
  线，直接写进复活后的初始 `CREATE TABLE` 即可，不需要 ALTER。列默认值 `'group'`，使
  未显式指定的规则语义等同于原设计的"固定按 GroupNo 分桶"。
  （**注意**：这是"忠实于原设计"的默认值选择，**不是**生产兼容性约束——这套东西从未
  部署过，不存在需要保持行为不变的既有部署。不要按"byte-identical rollout 红线"的强度
  去守它。）
- **`feature_gate_scope` 表结构不变**：只扩 `scope_type` 的合法取值（`group`/`space`/
  `user`），不迁移、不加列——`scope_type` 本就是自由字符串，`scope_id` 存 uid 与存
  group_no/space_id 同构。
- **切换 `bucket_by` 是人群重新洗牌，不是渐进操作**：把一条已在放量的规则从
  `group` 改成 `user`（或反向），分桶的加盐输入整体改变，**原先命中的对象会掉出去**。
  原设计"percent 调高只进不出"的单调性保证只在同一分桶维度内成立，不覆盖切维度这个
  操作。管理端至少要在文档/响应上明示；把它当成普通字段随手改会造成用户侧功能闪退。
- **`bucket_by` 默认 `'group'` 与展示端点的 UID-only 上下文直接冲突，必须双向堵住**。
  展示端点只有 UID，没有 GroupNo/SpaceID。而列默认值是 `'group'`（为忠实原设计而定）。
  两者一撞会产生**静默错配**：运维给一个客户端可见的 key 建 `mode=percent, percent=50`
  却没设 `bucket_by` → 服务端拿 `Dims{UID:"u123", GroupNo:""}` 评估 →
  `Bucket(key, "") = crc32(key+":")%100` 是**该 key 的一个固定常数** → 实际结果不是 50%，
  而是**全体 `true` 或全体 `false`**（取决于那个哈希落在哪）。管理台显示"50%"，真实放量
  是 0 或 100，**全程无任何报错**。

  按仓库既有的"写侧校验 + 读侧兜底"形状双向堵：
  - **写侧**：管理端 `update` 时，若 `feature_key` 在"可下发给客户端的 key 白名单"内，
    拒绝 `bucket_by=group`（以及 `whitelist` 模式下只有 `scope_type=group` 条目的配置），
    返回 `err.server.featuregate.request_invalid`；
  - **读侧**：`AllowDisplay` 发现规则要求的分桶维度在传入 `Dims` 中为空时，**fail-closed
    并打 Warn 日志**（携带 key 与缺失维度），不要按空串照算。

  只做写侧不够——直接改库能绕过；只做读侧不够——运维会以为配置生效了。
- **端点不做 space 维度，flags 是账号级**。`Dims` 虽有 `SpaceID`，但本端点不接收也不推断
  空间上下文：一个用户可属多个空间，"当前空间"在这条请求里没有确定答案。因此注册为
  客户端可见的 key **不得依赖 space 维度**（写侧校验同上一条一并拦）。明写这一条是为了
  让 space-isolation 评审有据可依——本端点不返回任何跨空间数据，不构成隔离面。
- **`AllowCreate`（fail-closed）/`AllowPush`（fail-open）的既有非对称语义不可动**：这是
  原设计"create 误拒 < 数据裸奔；push 误推 < 中断存量推送"的刻意决策，本任务只加维度，
  不改 fail 策略，也不把 fail 策略做成 per-rule 旋钮（原设计明确拒绝过，以免被配反）。
- **展示位判定是第三种语义，固定 fail-closed，并明确记录其代价**：它既不是
  `AllowCreate` 的写时闸门，也不是 `AllowPush` 的推送总开关，服务层需要新增独立方法
  （如 `AllowDisplay(ctx, key, dims) bool`，复用 `loadRule`/`loadScopes` 缓存路径），
  不要塞进前两者里混用语义。
  **选 fail-closed 的理由**：本框架展示位的第一批用途是 dark launch（后端服务尚未部署、
  功能尚未验收），此时把入口露出去，用户点进去撞上不存在的服务或 403，比短暂看不到入口
  更糟。
  **必须同时记录的代价**：对"已上线、只是分批放量"的功能，fail-closed 意味着
  DB/Redis 抖动期间**所有人**都丢掉入口（可用性代价）；而 fail-open 的代价只是少数人
  提前看到——因为展示位不承担鉴权，服务端仍独立拦截。本任务仍选 fail-closed（dark
  launch 是主场景，且与原设计"fail 策略按端固定"的哲学一致），但实施者与运维都应知道
  这个取舍不是无代价的。将来若某类 key 确实需要 fail-open，做法是**另开一个方法**（像
  `AllowPush` 那样），不是给规则加一个 fail 模式字段。
- **端点不接受客户端指定 key，只返回代码内预注册的白名单**：`feature_gate` 表的
  `feature_key` 是自由字符串，没有区分"哪些 key 允许暴露给客户端"。若按客户端传入的 key
  查库，等于让任意登录用户可枚举/探测内部 key 是否存在及其状态（信息泄露面，类比
  `system_setting` 的 `settingDef` 白名单、`bot_setting` 键注册表存在的理由）。本任务
  必须在代码里固定一份"可下发给客户端的 key 列表"，端点按这份列表批量评估后整体返回，
  **请求不带任何 key 参数**。
- **注册表项同时声明 `feature_key` 与 `client_key`，两者解耦**：

  | 字段 | 面向 | 出现在 |
  |---|---|---|
  | `feature_key` | 运维 | `feature_gate` 表主键、管理台、**`OCTO_FEATUREGATE_<KEY>_KILL` 环境变量名由它推导** |
  | `client_key` | 客户端 | `GET /v1/featuregate/flags` 响应 JSON 的字段名 |

  **为什么解耦**：`feature_key` 是运维面标识，重命名（改归类、改前缀）本该是无风险操作；
  若它同时是 wire 契约，运维改个名就静默破坏三端客户端。注意 `killSwitchOn` 是
  `strings.ToUpper(key)` 从 `feature_key` 推导环境变量名的（**不折叠连字符**——连字符已被 `validFeatureKey` 禁掉，不折叠才使 key→env 名成为单射）
  ——这进一步说明 `feature_key` 属于运维面词汇表，不该被客户端契约锁死。

  **反向约束**：`client_key` 一旦发布即**冻结**，与 appconfig 字段同级别的 wire 契约
  （additive-only）；要改就是破坏性变更，须与客户端发版协调。命名沿用 appconfig 的
  snake_case 风格；因为「归属唯一」规则，它不会与 appconfig 已有字段重名。

  ⚠️ **两个 key 都必须在注册表内唯一，且在构造期校验、重复即 panic**。尤其 `client_key`
  重复是**静默故障**：响应是 `map[string]bool`，两项声明同一个 `client_key` 时后写的直接
  覆盖先写的，map 不会报错，线上表现为"某个 flag 的值莫名其妙跟着另一个功能走"。
  仓库已有同型做法可参照——`mustLookupSharedCode` 在 init 阶段解析并对未注册项
  大声 panic（见 CLAUDE.md 的 i18n 小节）。
- **客户端字段只能是布尔；"未命中/未注册"与"评估出错"的区分只能落在日志与指标**：
  与 appconfig 现有位口径一致（`docs_on`、`tracking_enabled` 都是纯布尔），不暴露
  mode/percent/scope 等内部细节，防止客户端反推灰度策略。排障所需的区分度通过服务端
  日志（携带 key 与失败原因）实现，**不得为此在响应体里加 `reason`/`source` 之类字段**，
  否则布尔约束当场破掉。
- **响应体必须是动态 map，禁用 `omitempty`**。
  `flags` 里"值为 `false`"必须能被表达出来，但仓库里最顺手的先例
  `appConfigResp` 是**固定字段 struct**（`DocsOn bool \`json:"docs_on"\``）。照抄那个形状
  会连环出两个错：
  - 固定字段 struct 无法承载动态注册的 key；
  - 加 `omitempty` → **`false` 会被一起吞掉** → "规则不存在的确定性关"
    与"存储故障"在线上变成同一个样子 → 客户端保留旧值 → **灰度永远关不掉**，正是上一条
    失败语义花大篇幅要防的那件事。

  因此钉死：响应体形如 `{"flags": {"<key>": true, ...}}`，值类型 `map[string]bool`
  （动态 key），**不得用固定字段 struct，不得对该 map 或其元素使用 `omitempty`**。
  用一条序列化测试直接断言"值为 `false` 的 key 必须出现在 JSON 里"。
- **与 appconfig 的关系：归属唯一 + 服务端 AND（已拍板）**。
  **主规则是归属唯一，不是组合**：
  - 存量的十来个 appconfig 展示位（`docs_on`/`drive_on`/`dmloop_on`/`tracking_enabled`/
    `sticker_custom_enabled`/`message_reaction.*`…）**一个不动**，也不为它们建 featuregate
    key；
  - **新功能要用户级定向的，只建 featuregate key，不往 appconfig 加位**；
  - 存量功能将来要升级成用户级定向，是一次显式迁移，单独设计，不在本任务。

  **为什么不允许同一功能出现在两处**：老客户端只读 appconfig。若 `docs` 既有
  `docs_on` 又有 featuregate key，老客户端会在灰度批次之外照常显示——灰度当场失效。
  这不是风格问题，是功能性缺陷。

  **"部署没就绪"不需要 appconfig 那一层来表达**：featuregate 自己的 `mode=off` 就是
  没就绪，部署好了改 `whitelist`/`percent` 即可。

  **可选挂钩**：确有需要与某个部署位联动时，注册表项可**可选地**声明一个"部署前置位"
  （指向某个 `SystemSettings` getter），由**服务端**做 AND；未声明即无约束。
  **AND 永远在服务端完成，客户端永远只看到一个最终布尔**——把组合逻辑放客户端，就是
  三端各实现一次然后漂移，`SystemBotUIDs` 那段注释记载的正是这个事故。

  ⚠️ **实现陷阱**：读 `SystemSettings` 必须在**构造期**调一次
  `common.EnsureSystemSettings(ctx)` 拿到实例并持有，**绝不可在每请求路径上调**——该函数
  每次调用都取进程级 mutex，而 `SystemSettings` 的设计前提是读侧走 `atomic.Pointer`、
  永不取锁。同一个坑 `bot-setting-store` 已经踩过并写进它的 load-bearing。

  📌 **口径更正**：不要把现状描述成"appconfig = 部署能力、featuregate = 人群"。现有
  appconfig 位其实是**混的**——`docs_on`/`drive_on`/`dmloop_on`/`tracking_enabled` 确实是
  部署能力（都依赖外部服务上线），但 `sticker_custom_enabled` 与 `message_reaction.*` 本
  就是纯灰度开关。上述分工只能作为**今后的**约定，不能用来追溯解释存量。
- **客户端刷新契约与失败语义（已拍板）**。这是新接口，拉取节奏是本任务要**定义**的
  契约，不是去猜的既成事实。

  **拉取时机**：冷启动 + 登录成功 + **App 前台化**，前台化带节流（最小间隔 ≥5 分钟）。
  加"前台化"是关键——移动端 App 常驻后台，冷启动可能几天一次，只按 appconfig 现状
  （冷启动/登录页）会让回滚对活跃用户几天不生效。5 分钟节流下最坏每小时 12 次，对共享
  限流桶影响可忽略。

  **失败语义（服务端与客户端两半，缺一不可）**：
  - 服务端：单个 key 遭遇**存储故障**（DB/Redis）时把它列进响应的 **`unavailable`
    数组**，不下发 `false`、也不从 `flags` 里省略；
  - 客户端：整请求失败 → **保留上次快照**（不得回落成全 `false`）；响应缺某 key →
    **保留该 key 上次值**；无历史值（新装/清数据）→ `false`。

  **为什么不下发 false**：`flags` 里只有布尔（这是本 brief 自己定的约束），客户端因此
  **无法区分"真的关"与"服务端抖了一下"**。若故障时下发 `false`，Redis 抖 3 秒的后果就是
  **全体用户功能消失**；改成显式列出后，影响面缩小到"无本地缓存的新装用户暂时看不到"，
  而且**依然是 fail-closed**（无历史值时默认 `false`）。这条同时保住了布尔约束——它没有
  引入 `reason`/`source` 之类字段。

  **必须与"规则不存在"区分开**：查询成功但表里没有该 key 的规则，是**确定性的关**，
  正常进 `flags` 且值为 `false`；只有**存储故障**才进 `unavailable`。两者混淆会让
  "未配置"变成"保留旧值"，
  灰度就永远关不掉。

  **运营预期**：生效延迟 = 用户下次拉取时间，**分钟级到小时级，不是秒级**。
  由此推出一条运维硬规定：**展示位不是止血手段**。真要紧急关停，路径是
  `OCTO_FEATUREGATE_<KEY>_KILL=1` + 服务端拒绝相关 API（立即生效），客户端隐藏入口只是随后收敛的
  UI 表现。若将来运营确需秒级 UI 收敛，可复用 WuKongIM 长连接推一条重拉信号（列在
  Out of scope），即本决策是可演进的，不是死路。
- **响应不可被共享缓存**：结果因人而异，但 URL 对所有用户逐字节相同，区分调用者的只有
  `token` 头（本仓 `AuthMiddleware` 读的就是它，不是 Authorization）——任何按 URL 缓存的共享代理都会把用户 A 的判定回给用户 B。必须下发
  `Cache-Control: private, no-store`（`private` 禁共享缓存，`no-store` 连私有副本也不留）。
  同型教训见 `bot-setting-store` 给 `/v1/bot/card/profile` 加 `config` 后的处理。
- **管理端点鉴权：校验沿用，但拒绝路径必须换成现行写法**。`list`/`update`/`addScope`/
  `delScope` 继续用 `c.CheckLoginRoleIsSuperAdmin()` 做校验，`scope_type=user` 走同一个
  `validScopeType`，不新开权限模型——但**拒绝时不得照抄原实现**。
  原 `03f4bbe9:modules/featuregate/api_manager.go` 四个 handler 均写作
  `if err := c.CheckLoginRoleIsSuperAdmin(); err != nil { c.ResponseError(err); return }`，
  其中 `c.ResponseError` 正是 CLAUDE.md 禁止的 legacy 裸响应（这也是原模块能不带守卫
  测试交付的原因）。既然本任务要补 `TestFeatureGateAPINoLegacyResponseError`，照抄会
  当场触发守卫。
  现行正确写法在仓库里已有同型样板：`modules/common/api.go:580` 的 `addAppVersion` 做
  同一件事（superAdmin 校验 + 拒绝），用
  `httperr.ResponseErrorL(c, errcode.ErrSharedForbidden, nil, nil)`，并按反枚举约定回
  通用 403、不透出"需要更高角色"这一具体原因。管理端四个 handler 照此办理。
  **不要为此新增 `err.server.featuregate.forbidden` 之类的专用码**——鉴权失败统一收敛到
  一个共享码是既有的反枚举约定，`pkg/errcode/featuregate.go` 仍只保留原有四个业务码。
- **写后失效路径不变**：`update`/`addScope`/`delScope` 成功后仍调 `svc.Invalidate(key)`，
  不因新增维度而改变缓存失效触发点。
- **i18n / 错误码**：新端点走 `httperr.ResponseErrorL` / `ResponseErrorLWithStatus` +
  注册码，不得用 raw `c.ResponseError`。`pkg/errcode/featuregate.go` 的四个既有码
  （`request_invalid`/`not_found`/`query_failed`/`operation_failed`）复活时原样保留；
  用户侧只读端点若需新增码，一并归到 `pkg/errcode/featuregate.go`。
  端点已归属 `modules/featuregate`，因此不存在"码散落到别的模块"的问题——但需注意
  **该端点在正常路径上几乎不该产生错误码**：单 key 故障进 `unavailable`、规则不存在走 `false`，
  两者都是 200；只有鉴权失败与请求本身不合法才走错误信封。
- **复活的 `modules/featuregate` 缺一个 `NoLegacyResponseError` 守卫测试**：原分支只有
  `api_i18n.go`，没有对应的 `api_i18n_test.go` 守卫，而仓库约定是每个已迁移模块都要有
  （`modules/` 下已有 20 个模块带此守卫）。复活时按既有约定补上，不要原样照搬这个缺口。
- **限流**：新端点挂 `appwkhttp.SharedUIDRateLimiter(r, ctx)`，且必须在 `AuthMiddleware`
  **之后**挂（CLAUDE.md 明确的既有陷阱：挂早了读不到 uid，静默 fail-open）。
  **注意这是一个进程级共享桶**：每个 uid 跨所有已挂载路由共用一份配额（生产现为
  5 rps / burst 300）。因此若客户端把这个端点做成轮询，吃掉的是该用户其他接口的配额，
  不是免费的——这也是上一条"刷新契约"要规定拉取节奏的实际约束之一。

## Out of scope

- **`appconfig` 的鉴权语义或字段集**——本任务明确不碰，免鉴权全局位继续走
  `system_setting`，不因本任务而叠加任何用户维度字段。
- **软鉴权（方案 A：appconfig 内部可选解析 token）**——已讨论并否决，不在本任务重新
  评估。未来若确需未登录场景的按用户灰度，是独立任务且需单独安全评审。
- **PR #280 第二个 commit（`eb5cc933`）的 incomingwebhook 接入**——**明确排除**。
  incoming webhook 已在生产运行数月且从未有过这个门；现在给一个存量功能加 fail-closed
  的 create 门 + 种子迁移，是一套与"用户级展示灰度"毫无关系的独立风险面（种子迁移写歪
  或漏执行，直接后果是全部 create 变 403）。捆在一起只会放大爆炸半径。本任务只复活框架
  本体，`incomingwebhook` 一行不动。若之后确要给它加门，另立任务。
- **按 App 版本的灰度维度**——`Dims` 目前只有 `SpaceID`/`GroupNo`/`UID`；加
  `ClientVersion` 需要客户端在请求里带版本信息，涉及跨端传参契约设计，留给后续任务。
  本任务只做 user 维度。
- **服务端主动推送灰度变更**（例如复用 IM 事件通道推一条 `flags_updated` 让客户端立即
  重拉）——这是让"秒级回滚"对展示位真正成立的唯一途径，但引入新的推送依赖与投递可靠性
  问题，不在本任务。若运营明确要求秒级，另立任务。
- **客户端消费本端点后的 UI 行为**（哪个入口读哪个 key、灰度位命名）——本任务只交付
  服务端能力与一份可扩展的"可下发 key 白名单"机制；具体业务 key 的注册属于各业务功能
  自己的后续任务。
- **`feature_gate_scope` 的批量管理端点**（如"一次性拉一批测试账号进白名单"）——本任务
  只做单条 `addScope`/`delScope`，批量留给后续任务。

## Acceptance

**pkg/featuregate（用户维度）**

- `ScopeTypeUser` 白名单：`Rule{Mode: ModeWhitelist}` + `Scope{Type: ScopeTypeUser,
  ID: "u1"}` 下，`Dims{UID: "u1"}` → `Allow=true`；`Dims{UID: "u2"}` →
  `Allow=false, Reason=ReasonWhitelistMiss`；`Dims{UID: ""}` → `Allow=false`。
- percent 按用户分桶：`Rule{Mode: ModePercent, Percent: 50, BucketBy: "user"}` 下，
  同一 uid 多次评估结果恒定；两个不同 key 对同一 uid 的分桶相互独立（crc32 按 key 加盐
  这条既有不变量在 user 维度同样成立）。
- 同一维度内的单调性保持：`Percent` 调高只会纳入更多对象，已命中的不掉出。
- `BucketBy` 为空（列默认值 `'group'`）时行为等同于原设计的按 `GroupNo` 分桶——一条
  回归测试钉住这个默认值语义。
- **白名单贯穿 mode**：`Rule{Mode: ModePercent, Percent: 0}` + 一条命中的
  `Scope{Type: ScopeTypeUser, ID: "u1"}` → `Dims{UID:"u1"}` 得 `Allow=true` 且
  `Reason=ReasonWhitelistHit`（`Percent: 0` 保证放行只可能来自白名单，排除分桶巧合）；
  同一规则下不在白名单的 `u2` → `Allow=false`。
- **`off` 不可被白名单穿透**：`Rule{Mode: ModeOff}` + 命中的白名单条目 →
  `Allow=false, Reason=ReasonOff`。这条是止血语义的硬边界，必须单独有测试。

**modules/featuregate 框架复活**

- `feature_gate`（含新增 `bucket_by` 列）/ `feature_gate_scope` 两表按迁移建出。
- `/v1/manager/featuregate` 的 list/update/addScope/delScope 四端点以 `03f4bbe9` 的功能
  语义为基线（superadmin-only，写后 `Invalidate`），错误响应、中间件和守卫遵循当前仓库
  规则，不要求逐字复刻原实现。
- `addScope` 接受 `scope_type=user`；非法 `scope_type`（非 group/space/user）→ 400
  `err.server.featuregate.request_invalid`。
- **Service 层在 percent 模式下也加载 scopes**：一条走 Service（非纯函数）的集成测试——
  规则 `mode=percent, percent=0, bucket_by=user` + 白名单含 `u1` → `AllowDisplay` 对 `u1`
  返回 `true`。这条专门钉住"`loadScopes` 的加载条件漏加 `|| ModePercent`"这个实现遗漏，
  `pkg/featuregate` 的表驱动测试照不到它。
- **写侧拒绝对客户端可见的 key 配不可满足的维度**：对已注册为客户端可见的 `feature_key`，
  `update` 传 `bucket_by=group`（或 `whitelist` 模式下仅含 `scope_type=group`/`space` 条目）
  → 400 `err.server.featuregate.request_invalid`。
  **限定在会读这些输入的 mode（`whitelist`/`percent`）**：`off`/`on` 既不读白名单也不读
  `bucket_by`，对它们施加同一校验会让「关掉一个 gate」比「打开它」更难——而 `off` 正是本
  框架的回滚杠杆。放行不留隐患：每次切进 `whitelist`/`percent` 都会重新校验请求里的
  `bucket_by`。**这条限定是刻意的，不要"修"回无条件校验**（见 journal 的 Review round 2）。
- env kill switch 生效：设置 `OCTO_FEATUREGATE_<KEY>_KILL=1` 后，无论 DB/Redis 规则为何，该 key
  的所有判定（含新增的 `AllowDisplay`）一律为拒，且**不查任何存储**。
- 新增 `TestFeatureGateAPINoLegacyResponseError` 守卫测试，覆盖模块内全部 handler 文件。

**新增用户侧只读端点 `GET /v1/featuregate/flags`**

- 挂 `AuthMiddleware` + `SharedUIDRateLimiter`（鉴权中间件之后）；未登录时的行为与其他
  已鉴权端点一致（以 `AuthMiddleware` 既有行为为准，不新增分支）。
- 请求**不接受任何 key 参数**；正常情况下响应包含代码内预注册白名单的**全部**键，
  值为布尔。
- **响应体是 `{"flags": map[string]bool}` 动态形状**：一条序列化测试断言"值为 `false`
  的 key 必须出现在 JSON 里"，钉住"不得用固定字段 struct、不得用 `omitempty`"——否则
  `false` 被吞掉会与"判定不可得"混淆，使灰度关不掉。
- **响应用 `client_key` 而非 `feature_key` 作字段名**：一条测试断言注册表里
  `feature_key != client_key` 的项，在响应 JSON 中出现的是 `client_key`。
- **注册表唯一性在构造期校验**：注册两项相同 `client_key`（或相同 `feature_key`）时
  **panic**，用测试钉住。这是防静默覆盖——`map[string]bool` 下重复 key 后写覆盖先写，
  不会报任何错。
- **读侧兜底**：规则要求的分桶维度在 `Dims` 中为空时（如客户端可见 key 被直接改库配成
  `bucket_by=group`），该 key fail-closed 返回 `false` 并打 Warn 日志，**不得按空串
  照算**——否则会出现"管理台显示 50%、实际全体开或全体关"的静默错配。
- 响应头带 `Cache-Control: private, no-store`。
- **存储故障 → 进 `unavailable`，不是 false**：单个 key 的规则/白名单因 DB/Redis 故障
  加载失败时，该 key **不出现在 `flags` 里，而是列进 `unavailable` 数组**，失败原因写
  日志；**不影响其它 key 正常返回，不导致整个请求 500**。用测试同时钉住两边。
- **规则不存在 → 明确 false**：查询成功但表中无该 key 的规则时，正常进 `flags` 且值为
  `false`（确定性的关），**不进 `unavailable`**。与上一条构成一对，用测试同时钉住两侧，
  避免"未配置"被当成"保留旧值"而使灰度关不掉。
- 用户 A 在某 key 的 `user` 白名单内 → 该 key 为 `true`；用户 B 不在白名单且规则非 `on`、
  未命中 percent → `false`。
- **全新部署零规则场景**：`feature_gate` 表为空时，端点对每个已注册 key 均返回 `false`
  （fail-closed 的直接推论，走上面"规则不存在"这一支，不进 `unavailable`），请求本身成功返回 200。
  这是预期行为——新部署下所有灰度功能默认隐藏，运维需显式建规则才放出；用一条测试钉住，
  避免日后被当成 bug 改成 fail-open。

**工程门**

- 新增/复活的 handler 文件（管理端四个 + 用户侧只读端点）全部纳入
  `TestFeatureGateAPINoLegacyResponseError` 的文件列表——本任务不再改动 `modules/user`，
  因此不涉及该模块的守卫。
- `go build ./...`、
  `go test -race ./pkg/featuregate/... ./modules/featuregate/...`、
  `make i18n-extract-check`、`make i18n-lint` 通过。
- `appconfig` 相关测试（`modules/common/*_test.go`）**零改动**且全部通过——用以验证本任务
  确实没有触碰 appconfig 的字段与鉴权契约。
- `modules/incomingwebhook` 与 `modules/user` **零改动**（`git diff` 对这两个目录为空）
  ——前者验证 PR #280 第二个 commit 确实被排除，后者验证端点归属决策落到了
  `modules/featuregate`。
