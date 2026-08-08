---
type: Task
title: "Task: reminder-sync-membership-scope"
description: Scope channel-level reminders to channels the caller actually belongs to, closing a cross-tenant metadata leak in /v1/message/reminder/sync.
tags: ["space", "isolation", "acl", "wire-contract", "test"]
timestamp: 2026-08-07T16:19:08Z
# --- octospec extension fields ---
slug: reminder-sync-membership-scope
upstream: 渗透测试复测报告 2026-07-30 §4.11（报告归类为"逻辑漏洞垂直越权"）
source: self
---

# Task: reminder-sync-membership-scope

## Goal

`POST /v1/message/reminder/sync` 会把**全系统**的频道级（`@所有人`）提醒返回给任意
已登录用户，泄露其未加入频道的 `channel_id` / `publisher` / `message_id` /
`message_seq` / 时间戳。

本次把频道级提醒收敛到「调用方确实加入的频道」，在 SQL 层完成，使客户端传入的
`channel_ids` 退化为纯粹的**收窄**过滤器，而不再是授权依据。

## Background

复测报告 §4.11 的复现是：`POST /api/v1/message/reminder/sync`，
body `{"version":0,"limit":100,"channel_ids":[]}`。

- 带一个自己非成员的 `X-Space-Id` → 403 `{"msg":"无权访问该 Space"}`
- 删掉该请求头 → 200，26586 字节，返回大量 `"uid":""` 的记录

报告据此判为"垂直越权 / X-Space-Id 绕过"。**该归因不准确，且会导致假修复。**实际链路：

1. `modules/message/api_reminders.go:88` 的 `reminderSync` 全程**没有读取**
   `space.GetSpaceID(c)`。这条路由上 `SpaceMiddleware` 是唯一的 Space 门禁，
   而它在 `pkg/space/middleware.go:112` 无 `space_id` 时 `c.Next()` 放行。
   删头只是让请求绕过这道门，并不是数据泄露的成因。

2. 真正的成因在 `modules/message/db_reminders.go:93`（`version==0`）与 `:100`
   （`version>0`）——即 `channel_ids` 为空时走的分支：

   ```sql
   WHERE (reminders.uid=? OR reminders.uid='')
     AND NOT (reminders.uid='' AND reminders.publisher=?)
     AND reminders.version>? AND reminder_done.id IS NULL
   ```

   `reminders.uid=''` 按建表注释（`sql/20220418000001_message_legacy01.sql:8`）
   表示"提醒项为整个频道内的成员"，即 `@所有人` 广播。该分支**没有任何频道成员
   校验**，`channel_ids` 为空时连 `channel_id IN ?` 都不存在，于是返回全表。

3. 下游两处均不拦截：`filterChannelLevelByPublisher`（`api_reminders.go:118`）
   只丢弃自己发布的；`GetMembersWithUIDAndGroupIds`（`:130`）只把入群前的提醒标
   `done=1`，非成员群查不到成员行，反而原样透传（`done=0`）。

因此**只修中间件是假修**：攻击者改用一个自己确实是成员的 Space ID 填入
`X-Space-Id`，中间件放行，SQL 照样倒出全表。且该泄露与 Space 无关，单 Space
部署同样中招。

`reminders` 表没有 `space_id` 列，只有 `channel_id`——所以修复只能是
**频道成员校验**，不可能是空间过滤。

正常客户端不会触发：Web 端从会话列表拼 `channel_ids`
（`octo-web/packages/dmworkdatasource/src/im-callbacks/reminders.ts:15-28`），
空数组是构造出来的形状。服务端把"空列表"当成了"不过滤"而非"没有频道"。

## Load-bearing list

- `space` / `isolation` / `acl` — 跨租户读边界；本改动即该边界本身
- `wire-contract` — `/v1/message/reminder/sync` 响应内容收窄（见 Acceptance 的
  兼容性判据）；请求/响应 **结构** 不变，只是不再包含非成员频道的记录
- `version` / `limit` 游标语义 — 过滤必须在 SQL 完成。参照 `api_reminders.go:113`
  已记录的 YUJ-1377 决策：在 Go 侧后置过滤会让游标停在被隐藏的行上，客户端永久
  卡住。新谓词必须与 `OrderAsc("version").Limit(limit)` 同层
- `reminder_done` LEFT JOIN 与 `is_deleted` 语义 — 不得改变已读/已删判定
- 群成员语义 — 复用 `group_member` 的既有口径 `uid=? AND group_no=? AND is_deleted=0`
  （见 `modules/group/db.go:464`），不新造一套
- **频道类型矩阵** — 频道级提醒的 `ChannelType` 从消息原样拷贝
  （`api_reminders.go:247`），而 `hasMention`（`:427`）只检查 payload 有无
  `mention` 字段、**不判频道类型**（`ExpandAisToBotUIDs` 的 GROUP 限制只作用于
  `ais`，不作用于触发提醒的 `humans`）。octo-lib `common.ChannelType` 有 6 个值，
  各自的成员来源并不一致：

  | 值 | 类型 | 成员来源 | 本次是否加谓词 |
  |---|---|---|---|
  | 1 | Person | 无表，需从 fakeChannelID 推导 | 否 |
  | 2 | Group | `group_member` | **是** |
  | 3 | CustomerService | 未知 | 否 |
  | 4 | Community | 未知 | 否 |
  | 5 | CommunityTopic | 父群 `group_member` | **是** |
  | 6 | Info | 未知 | 否 |

  octo-server 内没有通用成员表（会话在 WuKongIM 侧，`conversation_extra` 只是
  元数据、非权威成员关系）。**对 1/3/4/6 一律要求 `group_member` 会静默丢弃
  合法提醒**（功能回归，非安全收益），故只覆盖 2/5。
- 子区（CommunityTopic）频道 ID 形状 — `groupNo____shortID`
  （分隔符 `"____"`，解析惯例 `SplitN(channelID, "____", 2)`，见
  `modules/group/api.go:3949`），成员归属落在父群。纳入本次范围虽超出报告字面
  （截图中记录全部为 `channel_type:2`），但它与类型 2 是同一成员域、同一代码
  路径、同一缺陷 —— 依 `trust-boundary` 规则的 adapter parity 条款，给一侧加防护
  而放着同源的另一侧是漏洞本身，不是范围扩大
- 热表查询计划 — `reminders` 是热表，新谓词不得退化为全表扫

## Out of scope

- **`SpaceMiddleware` 全局 fail-closed**（`pkg/space/middleware.go:112`）。
  该中间件是 opt-in 语义，多处路由注释（`modules/message/api.go:351`、
  `api_message_get.go:239`）写明未声明 `space_id` 时放行是为兼容旧客户端；
  改成拒绝会波及 conversation / message sync / reactions / search / pinned 等
  全部挂载点，需要逐路由评估，单独立项。**本任务把 reminders 的授权建立在成员
  关系上，不依赖该中间件**——所以不修它也不影响本漏洞的关闭。
- DM 跨 Space 过滤的同类 fail-open（`modules/message/space_filter.go:593`、
  `:482`，及 `api.go:1973` / `:2165`、`api_message_get.go:262`）。同一类缺陷，
  但影响面与判据都不同（只能读到自己参与的 DM），单独立项。
- 复测报告 §4.10 `real_name` 对他人下发。经查为 YUJ-413 明确产品决策
  （`modules/user/api.go:3893` 注释：friend/sync、conversation/sync 已对他人下发，
  三端 displayName 依赖），需产品口径而非代码修复。
- 复测报告中本任务范围之外的其余条目。不在此逐条列举：本仓公开，把「确认存在且
  尚未修复」的清单连同 file:line 一起提交等于对外公布可利用面，而这些条目在复测
  报告原件里已有完整记录，无需在此重复（PR#717 review P2-6）。
- **频道类型 1 / 3 / 4 / 6 的频道级提醒不加成员谓词**（知情保留的残留）。理由**不
  一致**，分开说明，因为初稿把它们混为一谈是错的：
  - 3 / 4 / 6 —— 全仓无生产者（`grep` 三个常量在非测试代码中零命中），没有可暴露
    的东西；
  - 1 Person —— **成员来源是存在的**：channel ID 自描述（`<uidX>@<uidY>`），无需
    任何表即可判定调用方是否为当事人。它是唯一今天就能关掉的一个。留着的真实理由
    是「无已知生产者」（没有客户端在 Person 频道发 `mention.humans=1`），而不是
    「判定不了」。这个理由比前者弱，因此记为后续项而不是设计约束。
  给这四类硬套 `group_member` 会在它们将来变成真实数据时静默丢弃合法提醒 —— 那是
  功能回归而非安全收益。残留以守卫测试监控，见下方 Acceptance。经维护者确认后保留。
- 不改 `reminders` 表结构，不加 `space_id` 列。

## Acceptance

- [ ] **主判据（必须先红后绿）**：调用方 A 是 Space S 的合法成员，且**带上**
      `X-Space-Id: S`（即中间件放行的正常路径），发送
      `{"version":0,"limit":100,"channel_ids":[]}`，响应中不包含任何 A 未加入
      频道的频道级提醒。此判据刻意不依赖删头，用于证明修复不是只堵了中间件。
- [ ] 报告原样复现（不带 `X-Space-Id`、`channel_ids:[]`）同样不返回非成员频道记录。
- [ ] `channel_ids` 传入 A 未加入的频道 ID 时，该频道的记录不出现在响应中
      （客户端传参只能收窄，不能扩权）。
- [ ] A 自己的 per-uid 提醒（`reminders.uid = A`）不受影响，无论
      `channel_ids` 为空还是非空、无论是否带 `X-Space-Id`。
- [ ] 子区频道（`groupNo____shortID`）的频道级提醒按**父群**成员关系判定：
      父群成员可见，非成员不可见。
- [ ] `version` / `limit` 游标不回退、不停滞：连续分页拉取能越过被过滤掉的行，
      不出现"客户端反复请求同一 version"。
- [ ] `done` 判定不变：已 `reminder_done` 的记录在 `version==0` 分支仍被排除；
      入群前的提醒仍标 `done=1`。
- [ ] 自己发布的 `@所有人` 仍不返回给自己（YUJ-1377 行为不回归）。
- [ ] **频道类型 1 / 3 / 4 / 6 的频道级提醒行为与改动前逐字节一致**（证明未误伤
      成员来源不可解析的类型）。
- [ ] **守卫测试**锁定「能产生频道级提醒的频道类型集合」与「谓词覆盖的类型集合」
      之差（即残留集合）。收紧谓词、放宽产生方、或新增频道类型，三者都必须让该
      测试转红。

      注：初稿在此写的是「当前写入路径只会生成类型 2/5」，**那是错的** ——
      `hasMention` 不判频道类型，六种类型结构上全都能产生频道级提醒，守卫测试的
      `wantResidual = {1,3,4,6}` 正是这一点的机器化断言。Out of scope 的论证不能
      靠那句话，真正的依据见下（3/4/6 无生产者；1 可判定但无已知生产者）。
- [ ] `go test ./modules/message/...` 通过；新增用例覆盖上述每一条。
- [ ] `golangci-lint run ./...`、`make i18n-extract-check`、`make i18n-lint` 通过。
- [ ] 新谓词**不劣化**执行计划：改动前后 `EXPLAIN` 的 type / key / rows / Extra
      一致（`EXPLAIN` 记录在 journal）。

      原验收写的是"执行计划不是全表扫"，实测后改掉：该查询在 `origin/main` 上
      **本来就是** `type: ALL` + `Using filesort`（顶层 `OR reminders.uid` 加
      `ORDER BY version`，而 `reminders` 上没有 `version` 索引）。要求本次改动达成
      一个改动前也不成立的标准，只会逼出一个超范围的索引迁移。索引缺失单独记录为
      后续项，本次不动 DDL。
