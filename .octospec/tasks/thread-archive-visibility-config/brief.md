---
type: Task
title: "Task: thread-archive-visibility-config"
description: Unify archived-thread visibility across the conversation/sidebar sync read paths, and move the thread auto-archive policy config from env-only into system_settings with an ordering constraint against sidebar.recent_filter_thread_days.
tags: ["thread", "space", "isolation", "wire-contract", "system-setting", "error-response", "i18n", "observability", "testing", "commit"]
timestamp: 2026-07-27T12:26:06+00:00
# --- octospec extension fields ---
slug: thread-archive-visibility-config
upstream: self
source: self
---

# Task: thread-archive-visibility-config

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

两件互相耦合的收口，一并做：

**P0 — 统一 archived 子区的可见性口径。** 同一个已归档子区，现在在
`/v1/conversation/sync` 被剔除、在 `/v1/sidebar/sync` 的 recent 与 follow 两个 tab
都照常返回。同一份数据在移动端会话列表消失、在 Web 侧边栏还在。本任务把三条读路径
收敛到**同一个谓词**：`status=active` 才进列表，archived 不进（全局搜索除外，见
Out of scope）。

**P1 — 归档策略配置迁入 `system_settings`，并与会话隐藏窗口建立偏序。** 子区自动
归档现在是纯 env（`DM_THREAD_AUTO_ARCHIVE_*`），而会话隐藏窗口
（`sidebar.recent_filter_thread_days`）在 `system_settings`。两者默认都是 3 天、
语义高度重叠、却分别在两个地方配、可以被独立调成互相打架。本任务把**策略项**
（enabled / days）迁入 `system_settings`（env 降级为 fallback），让 worker 每
tick 重读，并在管理写路径上强制
`thread.auto_archive_days >= sidebar.recent_filter_thread_days`。

两件必须同批：P0 把 archived 的"不可见"从一条路径扩到三条，等于**放大**了归档对
会话隐藏配置的覆盖效应；没有 P1 的偏序约束，P0 单独上线会让"运维把
`recent_filter_thread_days` 调大却看不到子区"这个既有问题从一个端点扩散到全部端点。

本任务**不**改变默认行为面：自动归档在本仓库默认关闭（`parseBool("")=false`，且
整个 thread 模块被 `DM_THREAD_ON` 门控），迁入 DB 后默认值保持一致，未开启的部署
零影响。P0 的收紧则是真实的行为变更，见 Open questions Q1。

## Background

### archived 子区在各读路径的现状（已逐条核对）

| 读路径 | archived 是否可见 | 依据 |
|---|---|---|
| `/v1/conversation/sync` | ❌ 剔除 | `QueryActiveShortIDs`（`modules/thread/db.go:382-394`，`status=ThreadStatusActive`）构成白名单，调用点 `modules/message/api_conversation.go:492-513`；查询失败时 `threadFilterEnabled=false` **fail-open 不过滤** |
| `/v1/sidebar/sync` recent tab | ✅ 可见，带 `Status=2` | `buildRecentItems` 无 status 谓词（`api_sidebar.go:1155-1207`）；`loadThreadStatuses`（`:709`）只回填字段 |
| `/v1/sidebar/sync` follow tab（IM 返回） | ✅ 可见 | `buildFollowItems` 的 CommunityTopic 分支只校验 ext 行存在 + 父群已关注/未取关，无 status 谓词（`api_sidebar.go:1094-1112`） |
| `/v1/sidebar/sync` follow tab（DB-only ext 行，非自建） | ✅ 可见 | `mergeThreadEntries` 的存活判定只要求 `status != deleted` |
| `/v1/sidebar/sync` follow tab（DB-only ext 行，自建） | ❌ 豁免被撤销 | XIN-1135，`api_sidebar.go:1296-1302` |
| 全局搜索 | ✅ active + archived | `modules/messages_search/search_global.go:876`，有意为之 |

两个关键事实：

1. **方向性先例已经存在。** XIN-1135 的注释（`api_sidebar.go:1234-1240`）把「归档
   子区刷新后重现」判定为线上 bug，修法明确写着「对齐 `/conversation/sync` 的
   `QueryActiveShortIDs` status=active 语义」。但那次只给**自建豁免**这一条路径补了
   active 守卫，follow tab 的常规路径和整个 recent tab 仍然放行 archived。P0 是把
   同一个判断补齐到剩下的路径。
2. **命名陷阱。** `QueryActiveByGroupShortIDs`（`modules/thread/db.go:217-247`）名字
   带 `Active`，实际谓词是 `status != ThreadStatusDeleted`，**返回 active + archived**。
   sidebar 侧的"存活"判定全部建立在它之上，是两端口径分叉的直接来源。实现时不要
   靠名字判断语义。

### 归档 worker 与配置的现状

- `LoadArchiveConfig()` 纯 `os.Getenv`（`modules/thread/archive_config.go:43-52`）：
  `DM_THREAD_AUTO_ARCHIVE_{ENABLED,DAYS,INTERVAL,BATCH_SIZE,BATCH_SLEEP}`，默认
  3 天 / 1h / 500 / 100ms，`maxArchiveBatchSize=5000` 截断保护。
- **默认关闭**：`Enabled: parseBool("")` → false；仓库内无任何部署配置打开它；thread
  模块整体还被 `DM_THREAD_ON` 门控（`modules/thread/1module.go:38`）。
- **配置在构造期冻结**：`NewArchiveWorker(ctx, LoadArchiveConfig())`
  （`1module.go:48`），`Start()` 的 `Enabled`/`Interval` 门也读构造期的 `w.cfg`
  （`archive_worker.go:63`）。所以即便把值搬进 `system_settings`，不下沉读取点就
  **拿不到 60s 热更新**。`Start()` 的注释已经预留了这个方向（"避免未来热更新
  enabled: true→false 再 Start 留下孤儿 ticker"）。
- **两个独立的 3 天窗口**：`defaultArchiveDays=3`（`archive_config.go:31`）与
  `defaultSidebarRecentFilterThreadDays=3`（`modules/common/system_setting_schema.go:37`）。
- **既有危害链**：运维把 `sidebar.recent_filter_thread_days` 调到 30 天 → 归档 worker
  仍按 3 天归档 → 子区 `status=archived` → 被 `/v1/conversation/sync` 的 active 白名单
  剔除 → **运维改了配置，用户照样看不到**。归档静默覆盖了会话隐藏的配置意图。P0
  之后这条链会扩散到 sidebar 两个 tab，故 P1 必须同批。

### 不受影响的既有机制

- `RecordMessageAndReactivate`（`modules/thread/db.go:781`）：任何人发消息就把
  archived 抬回 active。归档不是终态，"隐藏 archived"必须与这条复活路径闭环。
- `ArchiveStaleBatch`（`db.go:441-457`）：`version < V` 防赛跑、`last_message_at IS
  NOT NULL` 保护新建空子区、`NOT EXISTS(未处理的 per-uid @我)` 钉住有未处理提及的
  子区。本任务只换配置来源，**不动谓词**。
- 归档无 IM 退订副作用；`archived` 子区保留订阅，只有退群才摘
  （`modules/group/thread_cleanup.go:19-21`）。

## Load-bearing list

- **`thread`** — 子区生命周期 status 语义（active/archived/deleted）、
  `ArchiveStaleBatch` 谓词与 version 防赛跑、`RecordMessageAndReactivate` 复活路径、
  归档 worker 的 ticker/批控制/长事务保护（batch cap 5000）。
- **`wire-contract`** — `/v1/sidebar/sync` 响应 items 的组成变化；`SidebarItem.Status`
  字段（`api_sidebar.go:145`，`omitempty`）此前会把 `Status=2` 的 archived 条目发给
  客户端，收紧后该值在列表里不再出现。**客户端可能已依赖"收到 archived 条目并自行
  决定渲染"**——这是 P0 唯一的破坏性面，见 Q1。
- **`wire-contract`** — `/v1/conversation/sync` 现有的 archived 剔除语义与其
  **fail-open** 降级（查询失败宁可透出 archived 也不阻塞整批 sync）。P0 以它为基准，
  新增的 sidebar 过滤必须沿用同一 fail-open 方向。
- **`wire-contract`** — **cursor 推进不得被新过滤影响**：sidebar 的 `respVersion`
  基于 `rawConversations`（`api_sidebar.go:604-609`，PR #21 Round-4 B1），
  conversation/sync 的 `lastVersion` 基于过滤前 conversations
  （`api_conversation.go:762-771`，PR-B #1377）。status 过滤若参与 cursor 计算会
  复现客户端反复拉同一批的死循环。
- **`space` / `isolation`** — 子区读路径已有的 `filterThreadConvsByParentMembership`
  （父群成员 fail-closed，`api_sidebar.go:280-294`）与 `FilterRawConversationsBySpace`。
  新增的 status 过滤是**叠加**，不得绕过、替代或改变这两者的相对顺序。
- **`thread`** — XIN-1135 的自建豁免 active 守卫（`api_sidebar.go:1296-1302`）。若通用
  status 过滤前置生效，这段守卫可能变冗余，但它同时承担 follow tab 的重复 emit 防护，
  不得顺手删除。
- **`system-setting`** — `SystemSettings` 进程级单例快照 + 60s 后台 reload
  （`modules/common/system_settings.go:40,73`）、`getIntClamped` 越界回退默认
  （`:246`）、`[settingIntMin, settingIntMax] = [0, 3650]`
  （`system_setting_schema.go:25-28`）、`settingDef.Effective` 供管理 UI 显示生效值。
- **`system-setting`** — 管理写路径当前是**逐项校验**
  （`modules/common/api_manager_system_setting.go:208-236`）。偏序约束是**跨 key** 的，
  需要在"本批内两个 key 同时写"与"只写其一、与 DB 存量比较"两种情形下都成立。
- **`error-response` / `i18n`** — 偏序约束被违反时的拒绝响应必须走
  `httperr.ResponseErrorL` + 新注册的 `pkg/errcode` 码，并补 zh-CN 翻译。注意
  `api_manager_system_setting.go` 现存分支用的是裸 `c.JSON(http.StatusBadRequest, ...)`
  这一 legacy 形态，新增分支不要照抄。
- **`observability`** — 归档 worker 改为每 tick 重读配置后，enabled/days 的变更应可从
  日志观测（当前只在 `Start()` 打一次启动日志，`archive_worker.go:77-81`）。
- **`testing`** — `EnsureSystemSettings` 是进程级单例，测试改 `sidebar.*` / `thread.*`
  后必须 `Reload()`，否则 60s 内的陈旧快照跨测试串味（`api_sidebar.go:790-794` 记录过
  这个坑）。

## Out of scope

- **per-user 的归档 / 可见性覆盖**，以及把 `thread.status` 拆到用户维度。整条 per-user
  路线（含 `user_space_setting` 三态覆盖、用户级 version、稀疏覆盖表）另立 task。
- **会话隐藏的 pinned / unread 豁免**，以及 sidebar 侧系统 Bot 补齐与 recent 过滤的
  顺序修正。独立项，可并行但不在本 task。
- **全局搜索对 archived 的可见性**（`search_global.go:876`）——归档内容仍应可被搜到，
  不动。
- **`ArchiveStaleBatch` 的归档谓词本身**，包括 `NOT EXISTS(未处理 @我)` 与
  `r.uid<>''` 排除 @所有人 的取舍。只换配置来源。
- **手动 archive/unarchive 的权限模型**（`canOperate`，创建者或管理员，
  `modules/thread/service.go:695-703`）。
- **`listThreads?status=` 的语义**（active/archived/all）——归档子区仍可被显式列出。
- **运维调优项**（`INTERVAL` / `BATCH_SIZE` / `BATCH_SLEEP`）留在 env，本任务只迁
  **策略项**（`ENABLED` / `DAYS`）。
- **IM 订阅关系**（归档不退订，退群才摘）。

## Acceptance

### P0

- [ ] 新增测试：archived 子区不出现在 `/v1/sidebar/sync` **recent tab** 的 items 中
      （IM 返回路径）。
- [ ] 新增测试：archived 子区不出现在 `/v1/sidebar/sync` **follow tab** 的 items 中，
      IM-present（`buildFollowItems`）与 DB-only ext 行（`mergeThreadEntries`）**两条
      路径各一例**。
- [ ] 回归测试：archived 子区仍不出现在 `/v1/conversation/sync`（保护既有语义）。
- [ ] 新增测试：`RecordMessageAndReactivate` 复活后，同一子区在**三条路径**上重新可见
      （闭环验证归档非终态）。
- [ ] 新增测试：status 过滤**不影响 cursor** —— 构造"本批最高 version 的会话恰好是
      archived 子区"，断言 `respVersion` / `lastVersion` 仍前进（形状照
      `TestE2E_ConvSync_CursorNotStalledByFilter`）。
- [ ] 新增测试：thread status 查询失败时 **fail-open**（不过滤，不阻塞整批 sync），与
      `/v1/conversation/sync` 现有降级方向一致。
- [ ] 新增测试：status 过滤不削弱 Space / 父群成员隔离 —— 非父群成员即便子区
      active 也拿不到条目。

### P1

- [ ] 新增测试：`thread.auto_archive_enabled` / `thread.auto_archive_days` 的 DB 值
      覆盖 env；DB 未配置时回落 env；env 也缺失时回落代码默认（false / 3）。
- [ ] 新增测试：越界值（负数 / >3650）回退默认（`getIntClamped` 同款纵深防御）。
- [ ] 新增测试：`auto_archive_days=0` 保留 env 语义 —— 禁用时间阈值但 `enabled` 仍可为
      true（`RunOnce` 的 `Threshold<=0` 短路，`archive_worker.go:114`）。
- [ ] 新增测试：worker **每 tick 重读配置** —— 运行中把 `enabled` 由 true 改 false，
      下一 tick 不再产生归档；反向亦然。
- [ ] 新增测试：偏序约束 `thread.auto_archive_days >= sidebar.recent_filter_thread_days`
      在管理写路径被强制，覆盖两种情形：(a) 同一批同时写两个 key 且违反；
      (b) 只写其一、与 DB 存量比较后违反。
- [ ] 新增测试：约束的拒绝响应走 i18n 错误封套（`httperr.ResponseErrorL` + 已注册
      errcode），不是裸 `c.JSON`。
- [ ] `thread.auto_archive_*` 出现在 `GET /v1/manager/common/system_setting` 的
      `effective_value` 中（`settingDef.Effective` 已填）。

### 全局

- [ ] `make i18n-extract && make i18n-extract-check && make i18n-lint` 通过；新增码在
      `pkg/i18n/locales/active.zh-CN.toml` 有 zh-CN 翻译。
- [ ] `go test ./modules/message/... ./modules/thread/... ./modules/common/...` 通过。
- [ ] `golangci-lint run ./...` 通过。
- [ ] 默认配置下（自动归档关闭）P1 无行为变化；P0 的行为收紧在 CHANGELOG / PR 描述中
      显式记录。

## Open questions（需人工确认后再进入 Implement）

1. **P0 的收敛方向。** 建议向「archived 不进列表」收口（与 `/v1/conversation/sync`
   一致，且有 XIN-1135 先例）。反向做法是让 conversation/sync 也放行 archived、交由
   客户端按 `Status` 字段渲染。前者是**行为收紧**，若 Web 端已依赖收到 archived 条目
   自行折叠/置灰，会是可见回归。**需要确认 Web 端当前如何消费 `SidebarItem.Status`。**
2. **偏序约束违规的处理**：拒绝写入（硬失败，建议）还是接受并告警？拒绝更安全，但会让
   "先调大归档窗口再调大隐藏窗口"的顺序成为运维必须知道的隐式约定——文案里要写清。
3. **是否同时把 `thread.auto_archive_days` 的下界与 `recent_filter_thread_days` 绑定**
   （即隐藏窗口也不允许调到大于归档窗口），还是只单向约束归档侧。单向更简单，双向更
   难被绕过。
