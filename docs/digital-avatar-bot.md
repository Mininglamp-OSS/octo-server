# Digital Avatar Bot（方案 B）

数字分身是 `robot.kind=avatar` 的组织身份。它没有个人 owner；`created_by` 只用于审计，
不得参与运行时授权。普通 User Bot 与 App Bot 继续保持原有模型和行为。

## 管理与生命周期

- `scope=platform`：仅 superAdmin 管理；发布时加入全部有效 Space，新 Space 创建事务也会补入。
- `scope=space`：该 Space 的有效 owner/admin 管理；发布时加入该 Space。
- 管理 API 位于 `/v1/manager/avatars`，提供创建、列表、详情、编辑、发布、下架、删除、
  token 查看/轮换和清理重试，全部使用登录 token，并共用 UID 限流。
- 创建态为 `draft`。只有 `published`、`status=1`、`lifecycle_pending=0` 且 scope 合法的
  Avatar 能通过 Bot API 认证。
- 下架或删除先在本地事务中关闭权限、轮换/清空 Bot token、关闭好友与 AI Team 关系，
  再由 `avatar_cleanup_job` 和 Space 成员移除 outbox 重试群、项目、IM token、缓存和订阅清理。
  外部系统失败不会重新开放权限。重新发布不会恢复旧普通群、项目、好友或 AI Team 关系。

运行时从管理接口取得 `bf_` token，随后调用 `POST /v1/bot/register`。register 响应的
`bot_type` 为 `avatar` 且 `owner_uid` 为空。请勿把 `created_by` 当作 owner。

## 协作权限

| 能力 | Avatar 行为 |
| --- | --- |
| 私聊 | 与对方至少共享一个有效 Space；好友残留不能绕过此检查 |
| 普通群、项目群 | 一律拒绝加入和访问；遗留群成员行不构成授权 |
| 项目 | 仅项目管理员添加/移除；项目席位不投影为项目群成员 |
| AI Team | 每个用户独立添加/移除；仅服务端受控容器群和“我的 AI 团队”群可容纳 Avatar，重加复用历史容器 |
| 子区 | 仅 AI Team 容器的会话子区可读、发消息、同步和回执 |
| 文件、卡片、命令、免 @ | 允许；资源权限仍需同时满足 |
| 建群、改群、群成员管理 | 拒绝 |
| 建删子区、修改群/子区 MD | 拒绝 |
| 搜索、语音、OBO、Space principal/目标解析、用户密钥 | 拒绝 |

Avatar Bot API 使用显式允许清单；任何未登记的新 `/v1/bot/*` 路由默认拒绝。

## 对外类型与通讯录

Bot 类型统一使用三个字符串：`user_bot`、`app_bot`、`avatar`。`/v1/bot/register`、
会话同步与侧边栏使用该口径。`GET /v1/space/directory` 在原有 `data` 外增加
`digital_employees`，其中每项 `bot_type=avatar`；个人 Agent 仍位于 owner 的 `agents` 中，
其 `bot_type=user_bot`。

## 队列、容量与限流

Avatar 沿用 User Bot 的事件设施，不另建队列：

- Redis 队列：`robotEvent:{robot_id}`；序号来自既有 per-Bot 事件序号；ACK 仍走
  `POST /v1/bot/events/{event_id}/ack`。
- 唤醒：每个写入方同时通知 `robotEventBell:{robot_id}`；长轮询、容量与切换规则见
  [bot-events-longpoll.md](bot-events-longpoll.md) 和 [botevent-cutover-runbook.md](botevent-cutover-runbook.md)。
- 容量按 Bot 共享：同一 Avatar 被多个用户加入 AI Team 时，仍消费同一 Bot 队列、同一
  长轮询占用预算和同一 per-Bot business/heartbeat/register 限流配置，不按用户复制额度。
- 管理 API 使用 `SharedUIDRateLimiter`；Bot API 继续使用既有 per-IP 与 per-Bot 桶。
  不为 Avatar 新增 Redis 通用请求计数器。

## 运维检查

1. 观察 `avatar_cleanup_job` 中长期处于 `pending` 的任务及 `attempts/last_error`。
2. 修复 WuKongIM、Redis 或 Space 清理依赖后，由正确范围管理员调用
   `POST /v1/manager/avatars/{robot_id}/cleanup/retry`。
3. `lifecycle_pending=1` 期间不要手工改回 `status=1`；发布接口会拒绝未完成清理的身份。
4. 不要直接修改 `kind`、scope 或 publication 字段；数据库 CHECK 与服务端入口共同维护形状。
