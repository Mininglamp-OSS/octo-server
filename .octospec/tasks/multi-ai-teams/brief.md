---
type: Task
title: "Task: multi-ai-teams"
description: 保留自动且不可编辑的 AI 全员群，并增加用户可创建多个、显式选择 AI 成员的自定义 AI 群。
tags: ["space", "isolation", "auth", "acl", "bot-api", "wire-contract", "error-response", "i18n", "rate-limit", "thread", "test", "commit"]
timestamp: 2026-09-09T12:38:49+08:00
slug: multi-ai-teams
upstream: user-request
source: user
---

# Task: multi-ai-teams

## Goal

保留当前每个 `(space_id, user_uid)` 自动维护、成员不可编辑的“本人 + 全部 Agent”
AI 全员群，同时增加用户显式创建和管理的多个自定义 AI 群。创建自定义团队时用户填写
名称并选择 AI；创建者自动成为唯一人类成员，同一个 AI 可以加入多个自定义团队。
自定义 AI 群在聊天、最近会话、通讯录、群设置和子区方面基本等同普通群，但任何入群
路径都只能添加当前用户在当前 Space 内有权使用的 AI，不能加入其他人类。

## Background

- 当前 `modules/ai_team` 同时维护两种 `purpose`：每个 Agent 一个隐藏的
  `ai_session_container` 二人父群，以及每个用户唯一一个 `ai_team_group` 聚合群。
  聚合群直接存于 `group`，成员由全部 `ai_team_agent.is_added=1` 自动投影到
  `group_member`；它继续作为自动全员群，不改成可编辑群。
- 新增的自定义团队直接复用普通群的 `group`、`group_member`，并以
  `purpose=ai_custom_team_group` 区分受限成员规则；不新增团队表或成员镜像表。自动全员群
  同样复用 `group` / `group_member`，但名册由 Agent 目录自动维护，不与用户选择混用。
- Bot 创建后仍应自动注册为 Agent 并创建/修复其二人群；该能力是独立 AI 会话的基础，
  不等于自动加入任意 AI 团队。
- 现有普通群 mutation guard 把两种 AI purpose 都视为完全受保护对象。新模型需要继续
  封死二人容器，同时让 AI 团队复用普通群能力，并在所有成员入口增加 AI-only 权限校验。
- 原型包含团队名称、头像、AI 搜索/分类、已选列表、全选/清空和多个团队列表。搜索、
  分类和选择交互由客户端实现；Server 提供有界 Agent 列表、团队 CRUD 和成员契约。

## Load-bearing list

- `space` / `isolation` / `auth` / `acl`：所有团队接口继续使用 Auth ->
  SharedUIDRateLimiter -> SpaceMiddleware。团队必须属于请求的 Space，操作者必须是团队
  创建者及该 Space 的有效成员；不能用 `group_no`、Bot ID 或请求头跨 Space 读取或写入。
- 自定义团队是多实例普通群，不以 `(space_id,user_uid)` 唯一。每个团队有独立稳定的
  `group_no`、名称、头像元数据、群状态和成员名册；同一用户在同一 Space 可创建多个
  团队，同一个合格 AI 可同时出现在多个团队。
- 新增 `GET/POST /v1/ai-team/teams`、`GET/PUT/DELETE
  /v1/ai-team/teams/:group_no` 以及团队成员添加/删除接口。创建请求至少包含 trim 后
  1–50 字符的名称和一个 AI ID；响应返回可直接打开聊天的 `group_no`、配置状态、成员
  摘要和普通群展示所需字段。列表统一返回当前用户在当前 Space 的自动全员群和自定义团队，
  用 `type`、`editable`、`members_editable` 明确能力，并提供稳定分页。
- `GET /v1/ai-team/agents` 继续作为可选 AI 的权威目录，保留现有分组、分页和二人会话
  字段，但不再返回 `team_group`；自动全员群和自定义团队统一通过
  `GET /v1/ai-team/teams` 获取。自定义团队成员必须来自 Agent 目录的同一权限口径：
  有效的 `ai_team_agent`、有效 Bot 用户、有效 Robot、创建者有权使用且 Bot 在当前 Space
  有席位。当前阶段仍是自有 User Bot；保留未来数字员工接入点，但不借本任务放宽 App Bot
  或共享 Bot 权限。
- 创建者自动入群并保持唯一人类成员，不能被删除、退出或转让群主。创建、批量添加、
  邀请确认、二维码/扫码、Bot API、管理员 API、恢复软删成员等所有入口均须在服务端拒绝
  人类、他人 Bot、跨 Space Bot、失效 Bot 和未激活 Agent；不能只靠前端过滤。
- `group_member` 是自定义 AI 团队唯一的成员权威：创建者是唯一人类成员，所选 Agent 是
  Robot 成员；WuKongIM subscriber 是其外部投影。添加/移除、并发修改及 DB/IM 部分失败
  必须可重试并最终双向收敛，旧快照不能覆盖较新的成员选择。
- 创建自定义团队及成员变更复用普通群的版本、成员事件、CMD、WuKongIM 父频道和现有子区
  subscriber 同步语义。新加 AI 必须同步进入已有子区；移除 AI 必须从父群及所有未删除
  子区撤订并清理其 thread-local 设置，不能继续收到消息或发言。
- 自定义 AI 团队是普通可见群：允许群主改名、改头像、使用普通群设置、置顶/免打扰、清空本人
  历史、创建和管理子区；在产生会话后进入普通最近会话，并可在普通群/通讯录入口查询。
  普通列表继续排除 `ai_session_container` 和自动 `ai_team_group`，但不排除
  `ai_custom_team_group`。自定义团队解散直接采用普通群
  解散语义；邀请链接、扫码加入、增加人类、设管理员、群主转让
  等会破坏唯一人类成员不变量的功能保持禁用。
- 自动全员群和自定义 AI 团队都沿用普通群的消息和 @ 规则：默认需要 `@具体 AI` 才触发该 AI，同一条消息只按
  明确 mention 路由；不做无 mention 的全 AI fan-out。二人 `ai_session_container` 仍固定
  免 @、仍从普通群列表/最近会话隐藏，并继续由现有 session API 管理。
- 创建新的 User Bot/Agent 继续自动加入唯一 AI 全员群，同时进入自定义团队的候选目录，
  但不自动加入任何自定义团队。删除、禁用、从 Space 移除或
  `DELETE /v1/ai-team/agents/:bot_id` 使 Agent 失效时，必须从全员群和它所在的所有自定义
  团队及子区中收敛移除，但不得删除其他 AI 或整个团队。
- 现有 `purpose=ai_team_group` 群及其 `group_no`、消息、子区和自动成员同步完全保留，不迁移
  成自定义团队。自动与自定义团队都只写入现有 `group` / `group_member`，本任务不新增
  数据库表或迁移；升级不得复制出一个同名自定义团队。
- `wire-contract` / `error-response` / `i18n`：参数、无权限、团队不存在、非法 AI、
  重复/冲突和 IM 暂不可用均使用注册的本地化错误 envelope，不返回原始 Gin 错误。
- 明确区分“自定义团队成员”和“Agent 激活”：每个有效 Agent 必须属于自动全员群，同时
  可不属于任何自定义团队，也可属于多个自定义团队；删除一个自定义团队成员不删除
  Agent、二人群、全员群成员资格或其独立 sessions。
- 显式创建的 AI 团队按普通用户建群计入当日建群配额及普通群成员上限，避免受保护群用途
  成为无限建群旁路；服务端自动维护的二人容器继续不计入配额。

## Out of scope

- 不改变单 Agent 二人会话、thread/runtime session key、标题自动生成、历史消息和归档
  语义，也不把二人父群展示成普通会话。
- 不在本任务接入数字员工 App Bot、共享 Bot、他人 Bot、跨 Space Bot 或第二个人类成员。
- 不实现工作流编排、多个 AI 自动协作、无 @ 广播、自动轮询或 AI 间消息转发。
- 不重新设计普通群 UI。Server 只提供正式接口和普通群兼容行为；geelyocto Web 的多团队
  页面、AI-only picker 和原型交互在对应客户端分支实现并单独验收，先前本地临时测试页
  不作为可提交产品代码。
- Swagger 文档和聚合入口由独立变更维护，不纳入本任务提交。
- 不提交本地认证、LLM 网关、开发数据库、环境变量或运行时配置。

## Acceptance

- 同一用户在同一 Space 始终只有一个自动 AI 全员群，其成员精确等于本人加全部有效 AI，
  且不能手动增删；此外可连续创建至少两个不同 `group_no` 的自定义 AI 团队。每个自定义
  团队名称和所选 AI 独立，创建者自动存在且是唯一人类成员，同一个 AI 可同时加入两个团队。
- 创建团队时空名称、超过 50 字符、空 AI 列表、重复 ID、非 Bot、人类、他人 Bot、失效
  Bot、未激活 Agent 或跨 Space Bot 均得到确定的 4xx i18n 错误，且不会留下缺少成员的
  `group` / `group_member` 半成品；每次合法创建请求创建一个独立群。
- 列表、详情、改名/头像、成员添加/删除和解散只作用于请求 Space 内当前用户创建的目标
  团队。并发创建、并发加删同一 AI、IM 临时失败和重试后，权威 `group_member`、父群
  subscriber 和所有子区 subscriber 最终一致。
- 新 Agent 创建后自动进入全员群并出现在可选目录，但不自动进入任何自定义团队；显式添加
  后仅进入指定团队。从一个自定义团队移除不影响全员群、其他团队和二人会话；Agent 失效
  则从全员群及所有自定义团队移除。
- 自定义 AI 团队能通过普通群详情、最近会话、群列表/通讯录和子区入口正常使用，支持普通群的
  名称、头像和个人设置；普通群入口无法邀请人类或未授权 AI，也无法让唯一人类退出、
  转让群主或把团队转换为普通多人群。
- 在 AI 团队及其子区中，未 @ 的消息不触发 Bot；`@A` 只触发 A，`@A @B` 可分别触发
  A/B。二人 AI session 中未 @ 仍能触发绑定 Bot，且二人父群继续不出现在普通列表/最近。
- 升级已有数据后，旧自动全员群仍使用原 `group_no`，原消息和子区可读，且新建 Agent
  仍会自动加入；它不能被改成自定义成员集。用户可另行创建、编辑和解散多个自定义团队。
- 回归测试覆盖多团队、多对多成员、Space/所有权拒绝、普通群替代入口、生命周期移除、
  无新增迁移、并发/失败恢复、可见性、子区订阅及 @ 行为。运行聚焦的
  `modules/ai_team`、`modules/group`、`modules/message`、`modules/bot_api`、
  `modules/botfather` 测试，随后运行 `go test ./...`、`go build ./...`、`go vet ./...`、
  `make i18n-extract-check`、`make i18n-lint` 和 `git diff --check`；真实 MySQL、Redis、
  WuKongIM 与 CUA 全流程结果写入 `verification.md`。
