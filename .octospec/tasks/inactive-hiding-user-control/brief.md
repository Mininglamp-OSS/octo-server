---
type: Task
title: "Task: inactive-hiding-user-control"
description: Hand control of "when does a quiet conversation leave my list" back to users and thread creators — global config becomes a default, not the only truth. Batch 1 lays the groundwork: one visibility predicate across read paths, archive policy in system_settings, and unread/pinned exemptions so hiding can never swallow unread.
tags: ["thread", "space", "isolation", "wire-contract", "system-setting", "error-response", "i18n", "observability", "testing", "commit"]
timestamp: 2026-07-29T11:35:28+00:00
# --- octospec extension fields ---
slug: inactive-hiding-user-control
upstream: self
source: self
---

# Task: inactive-hiding-user-control

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

**把「会话 / 子区什么时候从我眼前消失」的决定权，从全局常量交回给用户与话题创建者。
全局配置退化为默认值，而不是唯一真相。**

终局是三层各归各位：

| 层 | 决定者 | 回答的问题 | 参照 |
|---|---|---|---|
| per-user 视图窗口 | 使用者 | 多久不活跃就从**我的**列表淡出 | Discord 折叠分类 |
| per-thread 归档时长 | 话题创建者 / 管理员 | 这个**话题**多久算结束 | Discord `auto_archive_duration` 四档 |
| 全局配置 | 运维 | 上面两层的**默认值** | Discord 频道级 `default_auto_archive_duration` |

**本批（Batch 1）只交付地基，不引入任何用户可见的新配置入口**：

- **P0** — 统一 archived 子区在三条读路径上的可见性谓词。
- **P1** — 子区自动归档策略配置从纯 env 迁入 `system_settings`。
- **P2** — 未读 / 置顶豁免：任何不活跃隐藏都不得吞掉未读或用户置顶的会话。

三者都是"交出控制权"的前置条件，理由见 Background §4。P2 同时是**当前就在伤用户**的行为修复（现网自动归档已开启）。

后续批次（per-user 覆盖、per-thread 档位）见 Out of scope，本 brief 记录路线以便每一批都能追溯到它服务的目标。

## Background

### 1. 现状：两套「不活跃」机制，都是全局常量，都不看未读

| | 自动归档 | 会话隐藏窗口 |
|---|---|---|
| 位置 | `modules/thread/archive_worker.go` | `modules/message/api_sidebar.go` |
| 配置 | **纯 env** `DM_THREAD_AUTO_ARCHIVE_*`（`archive_config.go:24-38`） | `system_settings` `sidebar.recent_filter_*_days`（`system_setting_schema.go:103-108`） |
| 默认窗口 | **3 天**（`archive_config.go:31`） | 群/子区 **3 天**、DM 0（`system_setting_schema.go:37`） |
| 机制 | 全局**写** `thread.status` 1→2 | 每请求现算的**读**投影 |
| 粒度 | 全局 | 全局 |
| 未读豁免 | 无 | 无 |
| 置顶豁免 | 无 | 无 |

两个窗口默认都是 3 天，语义高度重叠；`recent_filter_thread_days` 能过滤、而归档没管的子区，只剩三种边角情形（`last_message_at IS NULL`、有未处理 @我 被谓词钉住、被 `version < V` 赛跑跳过）。

**现网自动归档已确认开启**，因此下述危害链是活的，不是假设。

### 2. 危害链（当前已存在）

运维把 `sidebar.recent_filter_thread_days` 调大 → 归档 worker 仍按自己的窗口归档 → 子区 `status=archived` → 被 `/v1/conversation/sync` 的 active 白名单剔除（`api_conversation.go:492-513`）→ **运维改了配置，用户照样看不到，且无任何报错或日志说明原因。**

配置留在 env 还带来三个可观测性问题：没有接口能读到生效值（`system_settings` 有 `effective_value`）、改它要重启（配置在 `NewArchiveWorker(ctx, LoadArchiveConfig())` 构造期冻结，`1module.go:48`）、没有审计轨迹。**"现网到底开没开、窗口几天"目前无法从任何接口回答。**

### 3. archived 子区在各读路径的现状（逐条核对）

| 读路径 | archived 是否可见 | 依据 |
|---|---|---|
| `/v1/conversation/sync` | ❌ 剔除 | `QueryActiveShortIDs`（`thread/db.go:382-394`，`status=Active`）白名单；查询失败时 fail-open 不过滤 |
| `/v1/sidebar/sync` recent tab | ✅ 可见，带 `Status=2` | `buildRecentItems` 无 status 谓词（`api_sidebar.go:1155-1207`） |
| `/v1/sidebar/sync` follow tab（IM 返回） | ✅ 可见 | `buildFollowItems` CommunityTopic 分支只校验 ext 行 + 父群关注（`api_sidebar.go:1094-1112`） |
| `/v1/sidebar/sync` follow tab（DB-only，非自建） | ✅ 可见 | `mergeThreadEntries` 存活判定只要求 `status != deleted` |
| `/v1/sidebar/sync` follow tab（DB-only，自建） | ❌ 豁免被撤销 | XIN-1135（`api_sidebar.go:1296-1302`） |
| 全局搜索 | ✅ active + archived | `messages_search/search_global.go:876`，有意为之 |

两个要点：

- **方向性先例已存在。** XIN-1135 注释（`api_sidebar.go:1234-1240`）把「归档子区刷新后重现」判为线上 bug，修法明写「对齐 `/conversation/sync` 的 `QueryActiveShortIDs` status=active 语义」。它只补了自建豁免一条路径，其余仍放行。P0 是把同一判断补齐。
- **命名陷阱。** `QueryActiveByGroupShortIDs`（`thread/db.go:217-247`）名字带 `Active`，谓词实为 `status != Deleted`，**返回 active + archived**。sidebar 侧全部「存活」判定建立在它之上，是口径分叉的直接来源。**实现时不要靠函数名判断语义。**

### 4. 为什么 P0/P1/P2 是「交出控制权」的前置条件

- **P0** — 不可能让用户去控制一个在两个端点行为不一致的机制；per-user 覆盖必须作用在单一谓词上。
- **P1** — env 里的值**没法作为 per-user 覆盖的 fallback**。全局值必须先进到支持「默认值 → 覆盖」链条的配置系统里，per-user 才有地方接（参照 `SidebarRecentFilter*Days` 已有的 `getIntClamped` + DB→yaml→code 回落）。
- **P2** — 给控制权的**安全前提**。现在的机制藏东西时不看未读、不看置顶；一旦用户能自己设窗口，一个激进设置就会让未读消息消失。Discord 的折叠之所以敢交给用户，正因为未读永远穿透（见 §6）。

### 5. Web 端现状（octo-web@b5fe0a8，已核对）

- **关注 tab 的子区列表 = IM 缓存 ∩ sidebar items。** `followedKeys` 由 sidebar items 构建（`useFollowSidebar.ts:280-285`），`ConversationListGrouped/index.tsx:335-336` 用它先做交集过滤。**服务端一旦不返回 archived，它们在这一步就被挡掉，走不到后面的 `filterArchivedThreads`** —— 故 P0 收口对 Web 关注 tab **行为不变**。
- `SidebarItem.status` 有三处消费：冷启动快路径 statusMap（`ConversationListGrouped/index.tsx:318`，修 issue #340「先闪一下再消失」）、follow 角标排除 archived 未读（`Pages/Chat/index.tsx:175`）、`archivedThreads.ts` 的三级优先级（channelInfo 权威 > sidebar status > fail-open）。P0 后这三处退化为**冗余兜底**，均无害。
- 前端已为旧后端做兼容：「缺失 status 字段（旧后端）的子区不写入，据此回退到 channelInfo」（`index.tsx:317`）——**不假设 status 一定存在，服务端少返回不会让它崩。**
- **Web「最近」tab 不走 `/sidebar/sync?tab=recent`**，走 `conversation/sync` + `recent_filter: true`（`dmworkdatasource/src/im-callbacks/conversations.ts:20-25`）。sidebar recent tab 在 Web 上只被转发弹窗（`useSidebarScopes.ts:38`）和总结会话选择器（`ChatSelectorModal.tsx:177`）用作候选范围，而转发侧已用 `filterArchivedThreads` 挡住（`useForwardCandidates.ts:174-177`，注释：「转发目标永不包含已归档子区」）。
- **「已归档」浏览入口已存在**：`ThreadPanel/index.tsx:1490-1511`，默认折叠（`archivedExpanded: false`），可展开、可直接取消归档。**这是 P0 的安全垫**——归档子区离开列表可接受的前提是有回来的路。
- 取消归档 / 收消息复活依赖 `syncThreadArchiveState` 无条件 `emit("sidebar-reload")`（`Service/threadArchiveSync.ts:63-65`）。P0 后这条链从「双保险」变成**单点**，需专门验收。
- 该仓库只有 `apps/web` + `apps/extension`（桌面端为 web 打包），**无移动端**——移动端为独立仓库，其是否使用 `sidebar/sync?tab=recent` 待确认（见 Open questions）。

### 6. Discord 对照（本项目 3 天窗口的原始参照）

- **归档是 per-thread**：`auto_archive_duration` 四档 60 / 1440 / 4320 / 10080 分钟（1 小时 / 1 天 / 3 天 / 7 天），创建或编辑单个 thread 时选择；未指定则回落频道级 `default_auto_archive_duration`。归档不删除，频道有 Archived tab 可浏览，发消息自动解档（除非被锁）。
- **per-user 层是折叠分类**：每个用户独立折叠，**折叠后仍显示有未读的频道**；muted + collapsed 才全部隐藏。

**本项目照抄了「3 天」这个数字，没照抄机制**：4320 在 Discord 是四档之一、挂在每个子区上、有频道级默认兜底；这里成了一个全局 env 常量。per-user 层同理——Discord 的触发是**用户显式折叠**且**未读必然穿透**，这里是**系统按时间自动**且**未读一并吞掉**。

两处差距正是本 task 三层目标与 P2 豁免的由来。

## Load-bearing list

- **`thread`** — 子区生命周期 status 语义；`ArchiveStaleBatch` 的 `version < V` 防赛跑、`last_message_at IS NOT NULL` 新建空子区保护、`NOT EXISTS(未处理 per-uid @我)` 谓词（`thread/db.go:441-457`）；`RecordMessageAndReactivate` 收消息复活（`db.go:781`）；worker 批控制与长事务保护（batch cap 5000）。
- **`wire-contract`** — `/v1/sidebar/sync` items 组成变化与 `SidebarItem.Status` 字段（`api_sidebar.go:145`）。按 §5 已验证 Web 兼容，但仍是 wire 行为收紧。
- **`wire-contract`** — `/v1/conversation/sync` 现有 archived 剔除语义及其 **fail-open** 降级（查询失败宁可透出 archived 也不阻塞整批 sync）。P0 以它为基准，新增过滤须沿用同一 fail-open 方向。
- **`wire-contract`** — **cursor 推进不得被任何新过滤影响**：sidebar `respVersion` 基于 `rawConversations`（`api_sidebar.go:604-609`，PR #21 Round-4 B1）；conversation/sync `lastVersion` 基于过滤前 conversations（`api_conversation.go:762-771`，PR-B #1377）。违反会复现客户端反复拉同一批的死循环。
- **`wire-contract`** — per-Space 未读（`fillPersonSpaceUnread`）同样基于过滤前的原始会话。
- **`space` / `isolation`** — `filterThreadConvsByParentMembership`（父群成员 fail-closed，`api_sidebar.go:280-294`）与 `FilterRawConversationsBySpace`。新增 status / 豁免逻辑是**叠加**，不得绕过、替代或改变二者相对顺序。
- **`thread`** — XIN-1135 自建豁免 active 守卫（`api_sidebar.go:1296-1302`）。通用 status 过滤前置后它可能变冗余，但仍承担 follow tab 重复 emit 防护，**不得顺手删除**。
- **`system-setting`** — `SystemSettings` 进程级单例 + 60s reload（`common/system_settings.go:40,73`）；`getIntClamped` 越界回退（`:246`）；`[settingIntMin, settingIntMax] = [0, 3650]`（`system_setting_schema.go:25-28`）；`settingDef.Effective` 供管理 UI 显示生效值。
- **`system-setting`** — 管理写路径当前为**逐项校验**（`common/api_manager_system_setting.go:208-236`）。偏序约束是**跨 key** 的，需在「同批写两个 key」与「只写其一、与存量比较」两种情形下都成立。
- **`system-setting`** — 归档 worker 配置在构造期冻结（`1module.go:48`、`archive_worker.go:63`）。要让 `system_settings` 热更新生效，读取点必须下沉到每个 tick；`Start()` 注释已预留此方向。
- **`error-response` / `i18n`** — 偏序约束拒绝响应须走 `httperr.ResponseErrorL` + 新注册 errcode + zh-CN 翻译。注意 `api_manager_system_setting.go` 现存分支用裸 `c.JSON(http.StatusBadRequest, ...)`，**新增分支不要照抄**。
- **`observability`** — 归档 worker 改为每 tick 重读配置后，enabled/days 变更须可从日志观测（当前只在 `Start()` 打一次，`archive_worker.go:77-81`）。
- **`testing`** — `EnsureSystemSettings` 是进程级单例，测试改 `sidebar.*` / `thread.*` 后必须 `Reload()`，否则 60s 内陈旧快照跨测试串味（`api_sidebar.go:790-794` 记录过此坑）。UID 限流路由测试需清 `ratelimit:uid:*`。

## Out of scope

### 后续批次（本 brief 记录路线，不在 Batch 1 实现）

- **Batch 2 — per-user 视图窗口**：`user_space_setting` 三态覆盖（NULL=继承 / 0=显式不过滤 / N=自定义，**0 不可复用为"未设置"**，它在全局层已是"关闭过滤"哨兵）、用户级 version 解决「改了设置但 cursor 已推过去」、`GET/PUT /v1/user/space/setting` 扩展 + `SharedUIDRateLimiter` + i18n 迁移。
- **Batch 3 — per-thread 归档时长**：Discord 四档模型 + 群级默认值；`thread` 表加列；创建/编辑子区时可选。
- **Batch 4（可选）— 显式收纳**：更贴近 Discord 折叠分类的用户主动收起，而非纯时间自动。

### 永久不动

- `thread.status` 的 per-user 化（归档是全局共享写状态 + version 协议 + 手动归档权限语义，拆到人头上会搅烂这三者；per-user 应作用在**视图层**）。
- 全局搜索对 archived 的可见性（`search_global.go:876`）——归档内容仍应可搜。
- `ArchiveStaleBatch` 归档谓词本身，含 `NOT EXISTS(未处理 @我)` 与 `r.uid<>''` 排除 @所有人的取舍。
- 手动 archive/unarchive 权限模型（`canOperate`，创建者或管理员）。
- `listThreads?status=` 语义（active/archived/all）。
- 运维调优项 `INTERVAL` / `BATCH_SIZE` / `BATCH_SLEEP` 留 env，本批只迁**策略项** `ENABLED` / `DAYS`。
- IM 订阅关系（归档不退订，退群才摘，`group/thread_cleanup.go:19-21`）。

## Acceptance

### P0 — 统一可见性谓词

- [ ] archived 子区不出现在 `/v1/sidebar/sync` **recent tab** items（IM 返回路径）。
      **未交付 handler 级用例**：`modules/message` 的 handler 测试受既有
      `-tags integration` import cycle 阻塞（见下方 §Pre-existing），当前只有
      `dropArchivedThreadItems` 的纯函数覆盖。保持未勾，作为 follow-up 跟踪。
- [ ] ~~archived 子区不出现在 `/v1/sidebar/sync` **follow tab** items~~ —— **已撤销**
      （PR #679 review, yujiawei）。follow tab 的响应不只是「要显示什么」，它是三端
      唯一的**关注状态真源**（每条带 `is_followed`，Web/iOS/Android 都只从它构建
      followedKeys）。把 archived 从中移除会让已关注的归档子区 `is_followed` 变
      false，「取消关注」反转成「关注」—— 取消关注变得不可能，而这恰好发生在
      「已归档」浏览界面，也就是本 brief 指认为 P0 安全垫的那个入口。
      产品目标在 follow tab 上已由三端自行满足（Web 与 IM 缓存取交集 + channelInfo
      优先过滤，iOS 按 thread REST status 排除非 active，Android 构建关注列表时跳过
      thread 条目）。**归档收口因此只作用于 recent tab。**
- [ ] 回归：archived 子区仍不出现在 `/v1/conversation/sync`。
- [ ] **复活闭环**：`RecordMessageAndReactivate` 后，同一子区在三条路径重新可见（P0 后这是唯一回归路径，见 Background §5）。
- [ ] **cursor 不被影响**：构造「本批最高 version 的会话恰为 archived 子区」，断言 `respVersion` / `lastVersion` 仍前进（形状照 `TestE2E_ConvSync_CursorNotStalledByFilter`）。
- [ ] thread status 查询失败时 **fail-open**（不过滤、不阻塞整批 sync）。
- [ ] 不削弱隔离：非父群成员即便子区 active 也拿不到条目。

### P1 — 配置迁入 system_settings

- [ ] `thread.auto_archive_enabled` / `thread.auto_archive_days` 三级回落：DB → env → 代码默认（false / 3）。**迁移不写任何 DB 行**，上线瞬间行为与现网一致。
- [ ] 越界值回退默认 —— **仅适用于 DB / 管理台来源的值**。env 层**不设上界**：它是
      现网既有配置的兼容垫，`DM_THREAD_AUTO_ARCHIVE_DAYS=9999` 的语义是「实质不
      归档」，上线时折成 3 天会批量归档长期存活的子区，与上一行「上线瞬间行为与
      现网一致」直接冲突。原先本行写成无差别回退，与上一行自相矛盾，且 PR 断言了
      没实现的那条 —— 已按此调和（PR #679 review, Jerry-Xin / yujiawei）。
      env 层只保留 legacy `parseDays` 语义：空/非法/负值回退默认，0 合法，其余非负
      整数原样生效。
- [ ] days → `time.Duration` 转换设上限（`archiveThresholdFromDays`）。env 层放开上界
      后，超过约 106,751 天会让 int64 回绕成**小的正数**阈值（如 ~213,504 天 ≈ 25
      分钟），把几乎所有子区归档掉 —— 与「大值 = 不归档」的意图完全相反。
- [ ] `auto_archive_days=0` 保留 env 语义：禁用时间阈值但 `enabled` 仍可为 true（`RunOnce` 的 `Threshold<=0` 短路，`archive_worker.go:114`）。
- [ ] **worker 每 tick 重读配置**：运行中把 `enabled` 由 true 改 false，下一 tick 不再归档；反向亦然。
- [ ] 偏序约束 `thread.auto_archive_days >= sidebar.recent_filter_thread_days` 在**两个 key 的写入口都**强制（只在一侧校验可从另一侧绕过）；覆盖「同批写两个」与「只写其一、与存量比较」。
- [ ] 拒绝响应走 i18n 错误封套，非裸 `c.JSON`。
- [ ] `thread.auto_archive_*` 出现在 `GET /v1/manager/common/system_setting` 的 `effective_value`——**"现网开没开、窗口几天"变成可查询事实**。

### P2 — 未读 / 置顶豁免

- [ ] 未读 > 0 的会话**不被时间窗口隐藏**（`buildRecentItems` 与 `filterRecentConversations` 两处）。
- [ ] 用户置顶的会话**不被时间窗口隐藏**；`buildRecentItems` 当前先过滤后打 `IsPinned`（`api_sidebar.go:1165` vs `:1168`），须调整判定顺序。
- [ ] sidebar 侧系统 Bot 在 person 窗口 >0 时不得丢失（占位 `Timestamp` 为零值）。
      **实现方式偏离本条原措辞**（原文要求把 `EnsureSystemBotsPresentRaw` 挪到过滤
      之后）：改为在 `keepDespiteRecentWindow` 里豁免系统 Bot。结果等价，但不必在
      Space 过滤块内部调整顺序，风险更低；顺带关闭了 `/v1/conversation/sync` 注释里
      自陈的「不带 space_id + person 窗口>0」缺口。记录为机制替换而非静默吸收
      （PR #679 review, yujiawei）。

### 全局

- [ ] `make i18n-extract && make i18n-extract-check && make i18n-lint` 通过；新增码有 zh-CN 翻译。
- [ ] `go test ./modules/message/... ./modules/thread/... ./modules/common/...` 通过。
- [ ] `golangci-lint run ./...` 通过。
- [ ] P1 在默认配置下无行为变化；P0 的行为收紧与 P2 的豁免在 PR 描述中显式记录。

## 已知边界（评审要求显式记录，非缺陷）

- **P0 与 P2 冲突时 P0 赢。** 一个既 archived 又置顶/有未读的子区仍会被 recent tab
  丢弃 —— 归档是话题生命周期的客观事实，压过每人视图的豁免。三端本就在客户端隐藏
  archived，且任何新消息都会复活，故无回归。
- **静音但有未读的群永远不淡出。** `keepDespiteRecentWindow` 没有 mute 入参，两个
  调用点也都拿不到（`config.SyncUserConversationResp` 无 mute 字段，`SidebarItem`
  不暴露；用 `/v1/conversation/sync` 的 `Mute` 会重新制造 per-endpoint 分叉）。后果：
  用户静音一个吵闹的群并从此不点开，未读常驻，该群永不隐藏。**Batch 2 把窗口交给
  用户前必须处理**，否则用户会期待「静音且忽略的群应当消失」。
- **偏序守卫不是不可绕过的。** 它对着进程本地快照（60s TTL）校验且在写事务之外，
  所以同实例并发写、或 60s 内跨副本的两次写，都可能提交守卫本要禁止的组合。与既有
  space-welcome 复合校验同形，且结果是可恢复的配置错误 —— 但守卫的价值主张是「运维
  到不了那个状态」，这一点并不严密。
- **守卫只覆盖 DB→DB。** `ViolatesThreadArchiveOrdering` 在 `!ArchiveEnabled` 时短路，
  因此「DB 里写了很宽的 `recent_filter_thread_days`，之后用 env 打开归档」不会被检查；
  纯 env 来源的 `auto_archive_days` 也从不与 `recent_days` 比较。
- **`effective_value` 可能超出写路径接受的范围。** env 放开上界后
  `thread.auto_archive_days` 的 `effective_value` 可能是 9999，而把同一个值 POST 回去
  会被 `[0, 3650]` 拒绝 —— 管理表单「原样保存」会 400。这是兼容性选择的真实代价，
  要么放宽该 key 的写入上界，要么在响应里标注「env 提供，超出管理范围」。
- **`ArchiveConfig.Enabled` / `Threshold` 在生产是死字段。** `resolvePolicy` 只在
  `policy == nil`（即单测）时回落 cfg，于是同一个 env 变量有两个解析器，而 worker
  测试套件钉的是没人用的那个。建议后续删除这两个字段，让每个 env 变量只有一个解析者。
- **跨 Space 未读豁免今天就可达。** `filterRecentConversations` 读的是
  `fillPersonSpaceUnread` 之前的跨 Space `r.Unread`；管理员把 person 窗口设为非零即可
  触发（本 PR 自己的 e2e 就这么做）。方向是 fail-open。

## Pre-existing（阻塞验收但非本任务引入，`origin/main` 同样复现）

- **`go test -tags integration ./modules/message/` 无法编译** —— import cycle
  `api_card_action_test.go → app_bot → bot_api → messages_search → message`，导致该
  模块 **16 个** integration 文件全部失效，包括本任务验收引用的两个 recent-filter e2e。
  这是上面两条验收保持未勾的直接原因。
- **`TestE2E_ConvSync_*` 在 `TestE2E_RecentFilter_*` 之后运行必失败** ——
  `testutil.NewTestServer` 把 handler ctx 绑到进程内**第一个** server 的 config，
  fake IM 必须在首次调用前注册。

## Open questions

1. **移动端是否使用 `/v1/sidebar/sync?tab=recent`**（待定）。本仓库只有 web +
   extension（桌面端为 web 打包）；若独立移动端拿它当主列表，P0 对移动端即为可感知
   变更。走 `conversation/sync`（不传 `recent_filter`）则不受影响。这是**事实查证**
   而非设计选择 —— 在移动端仓库 grep 该路径即可。不阻塞 Batch 1。

## 已定设计决策（不再作为开放项）

- **P0 收敛方向**：向「archived 不进列表」收口。依据 XIN-1135 先例 + Web 兼容性已核对（Background §5）+ 已归档浏览入口已存在。
- **P1 迁移策略**：三级回落，迁移不写 DB 行，零上线风险，且无需知晓现网 env 值。
- **偏序约束违规处理**：写路径**拒绝**（非告警）；按提交后的**最终状态**校验而非单字段，消除运维的顺序知识负担；同批提交两个 key 时顺序无关。

- **两级衰减取值：隐藏 7 天 / 归档 14 天**（原 Open questions 1、2，已拍板）。
  - **隐藏 7 天，保持自然日**。周一回看恰好覆盖上一个完整工作周；3 天跨周末后只剩
    约一个工作日的记忆（周一 09:00 的 3 天窗口只到周五 09:00，周四下午的话题已出窗）。
    **不做工作日/节假日日历** —— 那是独立功能而非参数，7 个自然日已解决周末问题，
    Discord 同样把 7 天作为档位上限而不引入工作日。
  - **归档 14 天**。语义分层：7 天无动静 → 从**我的**列表淡出（可逆、每人不同、
    未读/置顶豁免）；14 天无动静 → 全局归档、进「已归档」分组，是"话题结束"的宣告。
    2× 便于记忆，两个完整工作周的沉默在企业场景足以判定结束。Discord 的 7 天是
    **唯一机制**的全部预算，这里归档是第二级，故应更长。
  - **落地方式：管理台配置，不改代码默认值**。守卫按合并后最终状态校验，运维一个
    请求同批提交两个 key 即可（`auto_archive_days=14` + `recent_filter_thread_days=7`，
    合并后 14 >= 7 通过，顺序无关）；DB 值覆盖 env，worker 一个 tick 内生效、无需
    重启。**因此本决策零代码改动、零契约影响。** 只提交其中一个会被拒（合并后
    `3 < 7`），错误文案已指明需同时提交 —— 这是正确行为。
  - **代码默认值改为 7/14 是可选的后续 PR**，仅影响未做任何配置的新部署，且必须
    **两个默认值一起改**：只改隐藏默认会让「env 显式 `DM_THREAD_AUTO_ARCHIVE_DAYS=3`
    + 新隐藏默认 7」的部署落入 `3 < 7` 的违规生效状态。读路径照常工作（守卫只在写
    路径跑），但运维之后写这三个 key 中任一个都会被拒、需两个一起提交才能解开。
    那个 PR 应同时给守卫加**「不使其更糟」容忍**：存量已违规时只拒绝加剧违规的写入，
    允许持平或修复的写入，从而任何 env/默认值组合都不会把运维锁在外面。
  - **明确不做读路径 clamp**（把隐藏窗口压到归档窗口）。看似优雅，但对逃过归档的
    子区（`last_message_at IS NULL`、有未处理 @我、版本赛跑）会让它们比配置更早消失
    —— 那是「藏更多」的方向，违反本任务贯穿的 fail-open 原则。
  - **上线前需实测**：归档 3→14 天使 `active` 子区增多，而移动端
    `/v1/conversation/sync`（不传 `recent_filter`）会全量带上，**离线同步 payload 会涨**。
    建议先在管理台改成 14/7 观察 payload 与 `ArchiveStaleBatch` 扫描耗时，再决定是否
    动代码默认值。
