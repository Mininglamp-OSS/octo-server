---
type: Task
title: "Task: per-user-hiding-window"
description: 把"安静的会话何时从我的列表里淡出"这个决定交给用户 —— 先关掉两个前置缺口（偏序守卫的写竞态、静音但有未读永不淡出），再给 user_space_setting 加三态的每人隐藏窗口并从 /v1/user/space/setting 读写。
tags: [message, user, common, thread, space, isolation, wire-contract, error-response, i18n, rate-limit, system-setting, testing]
timestamp: 2026-08-03T13:43:37Z
# --- octospec extension fields ---
slug: per-user-hiding-window
upstream: self
source: self
---

# Task: per-user-hiding-window

## Goal

**把不活跃隐藏窗口的控制权交给用户。**

Batch 1（PR #679）铺完了地基：两条会话列表读路径的可见性谓词统一了、归档策略可热改了、
窗口有了「未读 / 置顶 / 系统 Bot 永不被吞」的安全垫。但**至今没有任何用户可见的开关** ——
现在仍是运维在管理台上给全站定一个数字。

本批交付真正的开关：每个用户可以在每个 Space 里自己决定「群 / 子区 / 单聊各自安静多久
就从我的最近列表淡出」，全局配置降级为**默认值**。

行为变化：

| | 现在 | 本批之后 |
|---|---|---|
| 窗口取值 | 全站一个数字（`sidebar.recent_filter_*_days`） | 用户没设 → 继承全站默认；设了 → 用自己的 |
| 用户能做什么 | 无 | 每 Space 独立设置三类窗口，可显式关掉过滤 |
| 静音的吵闹群 | 未读常驻 → **永远不淡出** | 静音 + 无未解决 @我 → 可以淡出 |

**但本批的前两项不是这个功能，是它的准入条件。** 两个已知缺口在「窗口是运维配的」时是
可接受的边界，在「窗口是用户配的」时就不是了 —— 见 Background §2、§3。

## Background

### 1 · 三层衰减模型（Batch 1 journal 已确立的轴）

| 旋钮 | 正确的轴 | 因为 |
|---|---|---|
| 归档时长 | **每子区**（+ 群级默认） | 是**话题**的属性 |
| 隐藏窗口 | **每用户**（+ 全局默认） | 是**观看者**的属性 |
| 全局配置 | **只作默认值** | 不是唯一真相 |

本批做中间那一行。第三行（每子区归档时长，Discord 的 `auto_archive_duration` 模型）留给 Batch 3。

推论，Batch 1 journal 已记录：**全局归档窗口是每个用户窗口的硬天花板** —— 每人窗口是
读投影，归档是全局写；被全局归档掉的子区，任何用户的窗口都救不回来。偏序守卫
（`archive_days >= recent_filter_thread_days`）编码的正是这条。

### 2 · 前置缺口 A —— 偏序守卫的写竞态

`modules/common/api_manager_system_setting.go:343-370`：守卫读的是
`m.systemSettings.ThreadArchiveOrdering()`，一个**进程本地快照**（`defaultReloadTTL` 60s），
校验完才在 `:375` 开事务，事务内既不重读也不加锁。

从 `enabled=true, archive=6, recent=3` 出发，A 写 `archive=4`、B 写 `recent=5`，各自对着同一份
快照都通过（A 见 `4>=3`，B 见 `6>=5`），双双提交，落到 `4 < 5` —— 正是守卫要禁止的状态。
跨副本更糟：只有写入方立即 `Reload`，其他副本最多可能对着一分钟前的快照校验。

PR #679 三个独立评审 pass 都标了这一条，最终定 **P2 并放行**，理由是「需要两个 SuperAdmin
在窗口内写两个不同的 key」。**本批直接推翻这个理由**：窗口一旦变成每用户写入，写入面就从
「少数几个管理调用」放大到「每个用户随时可写」。评审因此把它列为 Batch 2 的准入门。

注意：本批新增的每人窗口**也**要受同一条天花板约束，所以守卫不只是要修，还要能覆盖一条
全新的、高频的写入路径。

### 3 · 前置缺口 B —— 静音但有未读永不淡出

`keepDespiteRecentWindow` 无条件豁免 `unread > 0`。用户静音一个吵闹的群、从此不点开 →
未读常驻 → **永久豁免**。窗口是运维配的时候这只是个瑕疵；窗口交到用户手里之后，它精确地
违背用户的预期 ——「我静音它就是不想再看见它」。

Batch 1 已把这条写进 brief 的已知边界，并注明「Batch 2 之前必须处理」。

难点是**两个端点对 mute 的可见性不对称**，而消除这种不对称正是 Batch 1 存在的理由：

- `/v1/conversation/sync`：`api_conversation.go:699-710` 已经从 `userDetail.Mute` /
  `group.Mute` 解析出 mute，并写进 `SyncUserConversationResp.Mute`（`:1503`）。过滤器在其后运行，
  **拿得到**。
- `/v1/sidebar/sync`：`api_sidebar.go` 里 **mute 出现 0 次**。`buildRecentItems` 吃的是
  `[]*config.SyncUserConversationResp`（IM 原始类型，无 Mute 字段）。**拿不到**，需要新增查询。

只在能拿到的那一端接 mute，就会重新制造 Batch 1 刚消灭的 per-endpoint 分叉。

### 4 · 现有 `/v1/user/space/setting` 的实际状态（已核查）

端点已存在（`modules/user/api_space_setting.go`，路由 `modules/user/api.go:274-278`），但扩展它
之前有三件事必须知道：

1. **它没走 i18n 错误信封。** 全文用 `c.ResponseErrorWithStatus(errors.New(...))` 和
   `c.JSON`，CLAUDE.md 明令禁止。它现在能过 CI 只是因为 D23 lint 只 AST 计数
   `AbortWithStatusJSON` / `AbortWithStatus`（`tools/lint-direct-error-response/main.go:152`），
   抓不到 `ResponseErrorWithStatus`。它也不在 `TestMigratedUserFilesNoLegacyResponseError`
   的清单里（`api_i18n_test.go:81-84`）。
2. **PUT 的取值校验写死了布尔。** `val != 0 && val != 1` 一律 400（`api_space_setting.go:65-68`），
   `UpdateSpaceSetting` 的字段白名单也是硬编码 switch（`db_space_setting.go:47-52`）。
   天数是任意整数，且需要一个「取消设置」的表达 —— 现有形状表达不了。
3. **路由没挂限流。** `spaceSetting := r.Group("/v1/user/space", AuthMiddleware, SpaceMiddleware)`，
   没有 `SharedUIDRateLimiter`。CLAUDE.md 规定已认证端点默认要挂。

表结构 `user_space_setting`（`modules/user/sql/20260522000001_user_space_setting.sql`）：
`uid` / `space_id` / 两个 `voice_*` 列 / `UNIQUE KEY uk_uid_space (uid, space_id)`。
现有列都是 `NOT NULL DEFAULT`，**没有可空列的先例**。

### 5 · 三态取值，以及 0 为什么不能复用

| 存储 | 含义 |
|---|---|
| `NULL` | 未设置 → 继承全局默认 |
| `0` | 用户**显式**要求不过滤 |
| `N > 0` | 用户自定义 N 天 |

`0` 不能兼作「未设置」：在全局层 `0` 已经是「该类型不过滤」的哨兵
（`system_setting_schema.go:125-129` 三个 key 的描述都写着 `0=不过滤`）。若用 `0` 表示未设置，
用户就永远无法表达「我不要过滤」——运维一旦把全局默认调成 7 天，这个用户会被迫跟着变。

## Load-bearing list

- **`space` / `isolation`** — 窗口是 per-(uid, space)。读路径必须只拿当前请求 Space 的设置；
  写路径必须只能改**自己**的。`/v1/sidebar/sync` 存在无 Space key 的跨 Space 请求
  （Batch 1 已确认并写进 swagger），那条路径上「当前 Space 的窗口」无定义，必须显式决定语义。
- **`wire-contract`** — 三处：
  (a) `GET/PUT /v1/user/space/setting` 的请求/响应体扩展。现有响应是扁平 `gin.H`，三个字段恒非空；
      新字段需要能表达「未设置」，`omitempty` 会把合法的 `0` 一起吞掉。
  (b) **cursor 推进不得被任何新过滤影响** —— sidebar `respVersion` 基于 `rawConversations`、
      conversation/sync `lastVersion` 基于过滤前列表（PR #21 Round-4 B1 / PR-B #1377）。
      违反会复现客户端反复拉同一批的死循环。
  (c) **改了设置但 cursor 已推过去**：用户把窗口从 3 天调到 30 天，之前被过滤掉的会话
      version 低于客户端游标，增量同步永远拉不到它们。需要一个让客户端知道「该全量重拉」的信号。
- **`error-response` / `i18n`** — 新的校验错误必须走 `httperr.ResponseErrorL` + 注册的
  `pkg/errcode` code。同时决定是否顺手迁移 `api_space_setting.go` 既有的 5 处 legacy 响应
  （见 Out of scope）。
- **`rate-limit`** — PUT 从「运维偶尔调一次」变成用户可写，必须挂 `SharedUIDRateLimiter`，
  且要挂在 `AuthMiddleware` **之后**否则读不到 uid、静默 fail-open。
- **`thread`** — 子区窗口与全局归档窗口的偏序天花板；`ArchiveStaleBatch` 的
  `reminders` / `reminder_done` 未解决 @我 判定（`modules/thread/db.go:446-453`）是
  「静音但保留」的现成机制，缺口 B 的修法应当复用它而不是另造一个。
- **`system-setting`** — 偏序守卫（`api_manager_system_setting.go:343-370`）、
  `ApplyThreadArchiveOrderingOverlay`、`ViolatesThreadArchiveOrdering`、
  `SystemSettings` 的 60s TTL 快照。守卫要从「事务外读进程快照」改成「事务内加锁重读」，
  且需同时覆盖管理台写入与新增的每用户写入。
- **两个端点的可见集合必须逐条一致** —— Batch 1 的核心不变量，由
  `TestRecentWindow_BothEndpointsAgree` 钉住。mute 与每人窗口都必须同时落到两条路径，
  否则该测试立即变红（这是设计意图：它就是用来挡这种分叉的）。
- **`SidebarItem` / `SyncUserConversationResp` 的 `mute` 可见性不对称** —— 见 Background §3。

## Out of scope

- **每子区归档时长（Batch 3）**。`thread.auto_archive_days` 本批仍是全局值。
- **PR #679 遗留的五条 advisory 不是 out-of-scope，而是本批的第一个 commit** —— 评审第 6/7 轮
  两次要求「转成 follow-up、不要回推那个已批准的分支」，所以它们落在这里。见下方 Acceptance §P2。
- **`ArchiveConfig.Enabled` / `Threshold` 死字段的删除** —— 要重构约 15 个 worker 测试，
  与本批目标无关，继续挂在 follow-up。
- **`modules/message` 的 `-tags integration` import cycle** —— 既有缺陷，`origin/main` 同样复现。
  本批不修，但它会继续挡住 handler 级验收（见 Acceptance 的诚实标注）。
- **运维调优项** `INTERVAL` / `BATCH_SIZE` / `BATCH_SLEEP` 仍留 env。
- **`api_space_setting.go` 中与本批无关的既有 legacy 响应的全量迁移** —— 待定，见 Open questions 2。
- **客户端实现**。本批只出服务端契约；web/iOS/Android 的设置界面另行排期。

## Acceptance

### P0 — 前置缺口 A：偏序守卫的写竞态

- [x] 守卫改为**事务内**校验，在锁内用行的真实值重跑
      `ApplyThreadArchiveOrderingOverlay` + `ViolatesThreadArchiveOrdering`。
      **锁的对象与原文不同,已修正**：原文写「`SELECT ... FOR UPDATE` 锁住三行」，
      照做**不成立**。那三行可能都不存在（迁移刻意不写行，这是 Batch 1 的上线保证），
      对不存在的行 `FOR UPDATE` 只拿到 gap lock，而 gap-X 锁彼此兼容 —— 两个写入会
      **全部通过**校验，随后各自 INSERT 抢 insert-intention lock 互等，退化成 InnoDB
      死锁(1213)，把一次合法并发写变成不透明的 500。本仓库在
      `incomingwebhook.insertWithQuota` 上踩过并记录过同一个坑
      （`modules/incomingwebhook/db.go:65`），结论是**锁一个必然存在的单行**；
      `card_template_catalog_capacity_guard` 是同一形状。
      实现改为新增单行锚点表 `system_setting_guard_lock`（CHECK 钉死主键=1），
      事务第一条语句即锁它，其后才读三个 key 的真实值。
- [x] 并发回归：两个 goroutine 从 `enabled=true, archive=6, recent=3` 出发分别写
      `archive=4` 与 `recent=5`，断言**恰好一个**成功、DB 终态不违反偏序。
      已按要求先在旧实现下验证必失败（实测 `codes=[200 200]`、终态 `archive=4 recent=5`），
      再确认修复后转绿。
- [x] 行不存在时（回落 env/默认）的加锁语义有明确定义并有用例 ——
      `TestManagerSystemSetting_OrderingSerialisesWhenNoRowsExist`：锚点行与三个 setting
      行的存在与否无关，所以「FOR UPDATE 锁不住不存在的行」这条不再构成绕过路径。
- [ ] 每用户窗口的写入走同一条校验，不得另开一条不加锁的旁路。
      **随 P1 交付**（本 PR 不含 P1）。守卫与锚点已就位，P1 的写入路径直接复用即可。
- [x] 既有 `TestManagerSystemSetting_Ordering*` 全部仍绿（含重置为默认那条）。
- [x] 附带：守卫从「事务前 return」变为「事务内 return」，新增整批回滚回归
      （`TestManagerSystemSetting_OrderingRejectionRollsBackWholeBatch`）——
      同批中与偏序无关的合法 key 不得留下部分写入。
- [x] 附带：锚点行自愈（事务外 `INSERT IGNORE`）。`CleanAllTables` 会清掉它，任何
      基于 truncate 的恢复同理；没有自愈的话，锚点一丢就让每次配置写入硬失败、
      看起来像数据库故障。锚点不存任何状态，恢复因此是安全的。

### P0 — 前置缺口 B：静音但有未读

- [x] `keepDespiteRecentWindow` 的未读豁免加入 mute 维度，**两个端点行为一致**，
      `TestRecentWindow_BothEndpointsAgree` 保持绿。
- [x] `/v1/sidebar/sync` 侧补齐 mute 来源，且与 `/v1/conversation/sync` 的
      `userDetail.Mute` / `group.Mute` 同源 —— 不同源就是新的 per-endpoint 分叉。
- [x] **静音 + 有未解决的 @我 仍然保留**，复用 `reminders` / `reminder_done` 的判定
      （与 `ArchiveStaleBatch` 同一机制，`modules/thread/db.go:446-453`）。
- [x] mute 查询失败 **fail-open**：按未静音处理（宁可多显示，绝不误藏），与本仓库既有降级方向一致。
- [x] 用例矩阵：`{静音, 未静音} × {有未读, 无未读} × {有@我, 无@我}` 在超窗输入上的可见性。

### P1 — 每人窗口存储与解析

- [ ] `user_space_setting` 新增三个**可空** int 列，迁移不写任何行、不改现有行 →
      上线瞬间行为与现网完全一致。
- [ ] 三态解析：`NULL` 继承全局 / `0` 显式不过滤 / `N` 自定义。三条各有用例。
- [ ] 解析顺序：per-user → 全局 `system_setting` → env → 代码默认。
- [ ] **天花板**：用户的子区窗口不得超过生效的全局归档窗口。越界时的行为需明确
      （拒绝写入 vs 写入后读时钳制），并与管理台守卫用同一个判定函数。
- [ ] 读路径每请求最多一次设置查询，且失败时 **fail-open** 回落全局默认。

### P1 — `GET/PUT /v1/user/space/setting` 扩展

- [ ] GET 返回三个窗口的当前值，**能区分「未设置」与「设置为 0」**。
      现有响应是扁平 `gin.H` 且字段恒非空，需要显式决定表达方式（`null` / 独立的
      `*_source` 字段 / 嵌套对象），并写进 swagger。
- [ ] PUT 接受天数并支持**取消设置**（回到继承）。现有 `val != 0 && val != 1` 的布尔校验
      必须为新字段放开，且不得放松既有三个布尔字段的校验。
- [ ] `UpdateSpaceSetting` 的字段白名单同步扩展（`db_space_setting.go:47-52` 的硬编码 switch）。
- [ ] 越界/非法值走 `httperr.ResponseErrorL` + 注册的 `pkg/errcode` code，
      `make i18n-extract-check` 与 `make i18n-lint` 通过，zh-CN 译文补齐。
- [ ] 路由挂上 `SharedUIDRateLimiter`，且在 `AuthMiddleware` **之后**。
      测试须在 setup 里清 `ratelimit:uid:*`（该 bucket 不被 `CleanAllTables` 清理）。
- [ ] **Space 隔离**：A 用户改不了 B 的设置；Space X 的设置不影响 Space Y。各有用例。

### P1 — cursor 与设置变更的交互

- [ ] 改设置后，先前被过滤掉的会话能重新出现 —— 不能因为它们的 version 低于客户端游标就永久丢失。
- [ ] cursor 推进仍基于**过滤前**的原始列表（sidebar `respVersion` / conversation `lastVersion`），
      构造「本批最高 version 的会话恰被新窗口过滤掉」的用例断言游标仍前进。
      *（注：handler 级用例受既有 import cycle 阻塞，见 Out of scope；本条须给出可运行的替代形式。）*

### P2 — PR #679 结转的五条 advisory（本批第一个 commit）

评审第 6/7 轮均判定「不值得为它们再开一个 head」，因为每个新 head 都会作废全部批准并触发一次
完整重审。因此它们结转到这里，作为独立的、先于功能代码的一个 commit。

- [x] `modules/common/system_settings.go:681` —— `threadAutoArchiveDaysFromEnv` 的
      `strings.TrimSpace` 与 legacy `parseDays` 不一致：`" 9999 "` 在新实现下解析成 9999，
      在 legacy 下回落默认。**这是「上线逐字节相同」唯一不成立的输入。**
      要么去掉 `TrimSpace`，要么改注释别再宣称精确镜像，并把 brief / PR 里的措辞
      改成「除首尾空白外逐字节相同」。
- [x] `modules/thread/archive_config.go:13` —— `Enabled` 的注释描述的是 PR 之前的启动门。
      标记为死字段或指向 `system_settings`。
- [x] `modules/thread/archive_config.go:63-70` —— `parseDays` 回落路径未设上界，
      按 `archiveThresholdFromDays` 同样的方式钳住，避免该路径仍能回绕。
- [x] Batch 1 `brief.md:211` —— web「无回归」的理由不准确：真正的理由是**主最近列表
      从不消费 `/v1/sidebar/sync`**（它走 `/v1/conversation/sync`），而不是「客户端会自己过滤
      这个端点」。后者会让 Batch 2 依赖一个并不存在的客户端过滤。
- [x] **worker 启动时打一次生效的归档/隐藏偏序**（第 7 轮新提）。当前守卫只覆盖 DB→DB
      写入，`ViolatesThreadArchiveOrdering` 在 `!ArchiveEnabled` 时短路，所以「纯 env 配出的
      违规状态」不写一次管理接口就永远不暴露。启动日志正好补上这个洞。

### 全局

- [ ] `go build ./...`、gofmt、`golangci-lint run` 0 issues。
- [ ] `make i18n-extract-check` + `make i18n-lint` 通过。
- [ ] `modules/{user,message,common,thread}` 默认 tag 全量测试通过（**分包跑并重建 test 库** ——
      各包注册的模块子集不同，同一次 `go test` 跑两个包会让 sql-migrate 报 unknown migration 并 panic）。

## 已拍板

- **静音 + 有未读 → 允许淡出，`@我` 兜底。**（2026-08-03 确认）
  静音的语义是「我主动降低它的优先级」，与「窗口让安静的东西淡出」同向；`@我` 是别人点名，
  优先级不由自己定，不该被自己的静音吞掉。判定复用 `ArchiveStaleBatch` 已有的
  `reminders` / `reminder_done` 未解决 `@我` 机制，不另造。P0-B 的验收按此写。

## Open questions

1. **越界写入：拒绝还是钳制？** 用户把子区窗口设成 30 天、而全局归档是 14 天 ——
   400 拒绝（诚实但需要客户端解释「为什么我不能设 30」），还是接受并在读时钳到 14
   （宽容但用户看到的和他设的不一致）。倾向**接受 + 读时钳制 + GET 回一个 effective 值**，
   理由是全局归档窗口可能事后调小，任何写时校验都会被时间打破。
2. **顺手迁移 `api_space_setting.go` 的既有 legacy 响应吗？** 该文件 5 处
   `ResponseErrorWithStatus` + 1 处 `c.JSON`，D23 lint 抓不到（只计 `Abort*`），
   但违反 CLAUDE.md 明文。倾向**顺手迁完并加进
   `TestMigratedUserFilesNoLegacyResponseError` 清单** —— 半迁移的文件会让下一个人无所适从，
   而本批本来就要在同一个文件里加新的错误路径。代价是 diff 变大、混入与功能无关的改动。
3. **无 Space key 的 `/v1/sidebar/sync` 请求用谁的窗口？** 该路径返回跨 Space 列表
   （Batch 1 已确认并写进 swagger），「当前 Space 的每人窗口」无定义。
   候选：退回全局默认（最简单、可预期）/ 取各 Space 设置的并集或最宽值（最贴近用户意图、但要 N 次查询）。
   倾向**退回全局默认**，并写进 swagger。
