---
type: Task
title: "Task: ai-team-all-bots-group"
description: 以 AI Team Agent 为权威名册，同步维护每个 Agent 的二人群和本人加全部 Agent 的“我的AI团队”群。
tags: ["space", "isolation", "auth", "acl", "bot-api", "wire-contract", "error-response", "i18n", "test", "commit"]
timestamp: 2026-09-08T16:49:23+08:00
slug: ai-team-all-bots-group
upstream: user-request
source: user
---

# Task: ai-team-all-bots-group

## Goal

在 `main` 已有“每个 AI Team Agent 一个本人 + Bot 的受保护二人父群”之外，为每个
`(space_id, user_uid)` 自动维护一个可见的“我的AI团队”群。`ai_team_agent` 是两类群的
唯一权威名册：每个有效 Agent 必须同时拥有就绪的二人群，并且出现在该用户的 AI 全员群；
全员群中也不得出现没有有效 Agent 记录的 Bot。新建 Bot 完成 Space 绑定后自动注册为
Agent、创建二人群并加入全员群；移除或失效时两边同步收敛，普通入口不能拉入其他人。

## Background

- 当前 `modules/ai_team` 用 `ai_team_agent` 维护每个 User Bot 的二人容器，并把这些
  `purpose=ai_session_container` 群从普通群聊/会话界面隐藏；它没有用户级的 AI 总群。
- “全部 AI”定义为该 `(space_id,user_uid)` 下 `ai_team_agent.is_added=1` 且仍通过当前
  AI Team 权限校验的 Agent：`robot.status=1`、`robot.creator_uid=user_uid`，Bot 用户有效，
  并且本人和 Bot 都是目标 Space 的有效成员。App Bot、他人 Bot、跨 Space Bot 和系统
  Bot 不属于该集合，也不能只进入群而不写 Agent 记录。
- Server PR #855（远端分支 `origin/claude/project-p2-all-member-group-85vjhr`）是设计参考：
  权威名册投影、唯一群指针、并发建群租约、内部准入、普通成员变更保护，以及失败后的
  补建/对账。该 PR 尚未合入 `main`，本任务不能依赖其代码已存在，也不能把项目成员
  语义直接套到 AI 所有权语义上。

## Load-bearing list

- `space` / `isolation` / `auth` / `acl`：总群严格按 Space 和 Bot 所有权隔离；群主只能
  是该 `(space_id,user_uid)` 的有效本人，不能因客户端传参或旧快照混入跨 Space 成员。
- 每个 `(space_id,user_uid)` 最多一个有效 AI 总群；并发首次访问、并发建 Bot 和重试
  必须收敛到同一个 `group_no`，不能产生两个“我的AI团队”群或悬空指针。
- 总群与现有二人 session container 使用不同的持久化身份/`purpose`。两类群都只能从
  “我的 AI 团队”专属入口进入，不得出现在普通最近会话、关注、通讯录或普通群列表；
  二人群及其 thread 继续保持隐藏和二成员不变量。
- `ai_team_agent` 是二人群和总群的共同权威状态。Agent 激活必须同时确保二人群及总群
  成员就绪；Agent 移除/失效必须同步撤销总群成员资格，同时按既有历史保留语义保护已建
  二人群和 session，不允许出现“只在总群”或“有效 Agent 没有二人群”的正常态。
- 总群成员集合是权威 Agent 名册的投影。仅服务端内部 reconcile/admission 路径可改变它；
  邀请、扫码、确认邀请、Bot API、管理员 API、加/删成员、退群、转让、解散等普通入口
  都不得破坏“本人 + 全部有效自有 Bot”的精确集合。
- Bot 创建的所有生产入口（BotFather 对话创建、`/v1/user/bots`、`MintBotOBO`）都必须在
  Bot 核心记录和 Space 绑定成功后调用同一个幂等 Agent 激活编排：upsert Agent、确保
  二人群、确保全员群并入群。`POST /v1/ai-team/agents/:bot_id` 复用同一编排而不是另一套
  写法；Bot 删除、禁用、Agent 移除、Space 成员移除和用户离开 Space 必须同步或最终
  收敛两类群状态。
- 建群和成员写入必须复用 group 的统一写入、版本、通知和 WuKongIM 订阅语义；DB 已提交
  而 IM 调用失败时必须有可重试状态，不能把未就绪群返回成可聊天成功，也不能回滚已成功
  的 Bot 创建。
- 现有 `GET /v1/ai-team/agents` 作为旧数据的惰性补建/双向校准触发点：为符合权限但尚无
  Agent 行的既有自有 Bot 补 Agent，为每个有效 Agent 补二人群，并校准全员群；响应以
  向后兼容字段暴露总群 `group_no`/就绪状态。路由继续使用 Auth ->
  SharedUIDRateLimiter -> SpaceMiddleware，用户可见失败继续使用 i18n error envelope。
- 总群采用现有普通群消息规则；`@某 AI`、`@所有 AI` 和免 @ 开关沿用当前 group/robot
  实现，不为该群增加隐式全 Bot fan-out。总群仍允许从专属页面创建普通子区；二人群的
  “新会话”由 AI Team session API 创建同一 thread/CommunityTopic 形态的子区。二人父群
  只是服务端容器，不作为一条用户可见会话；新建会话的默认标题在首条用户消息后自动替换
  为消息摘要，用户手动改名后不再自动覆盖。

## Out of scope

- 不替换或合并每个 Bot 的二人父群，不改变 AI session/thread、runtime session key、
  历史消息和会话控制语义。
- 不纳入 App Bot、共享 Bot、他人 Bot、跨 Space Bot或普通人类成员，也不创建跨 Space
  的全局 AI 群。
- 不实现前端界面，不改变普通群的 @、免 @、Bot 回复或消息扇出协议。
- 不合入或重做 PR #855 的项目全员群功能；只复用经验证的并发、保护和收敛思路。
- 不允许通过本任务新增的内部能力任意维护其他受保护群。

## Acceptance

- 一个拥有 N 个符合条件 User Bot 的用户首次进入 AI Team 时，系统为它们补齐 N 条有效
  Agent 记录和 N 个各含“本人 + 对应 Bot”的二人群，并且最终只有一个名为“我的AI团队”
  的可见群，其活跃成员恰好是本人 + 这 N 个 Agent；N=0 时不创建空壳群。
- 已有 Bot 用户无需重建 Bot：调用 AI Team 列表即可幂等补建/修复 Agent、二人群和总群，
  并返回稳定的总群 `group_no`；重复和并发调用不创建重复群、不重复成员、不重复产生
  可见副作用。
- 三条生产建 Bot 路径在 Space 绑定成功后自动创建有效 Agent、确保对应二人群，并把 Bot
  加入已有总群；若它是首个 Agent，则创建总群。`POST /v1/ai-team/agents/:bot_id` 对已有
  Bot 得到相同结果。任一步 DB/IM 临时失败都不得把“二人群已就绪、总群未就绪”暴露成
  完整成功，失败有日志/指标，并能由后续列表或生命周期触发重试后双向收敛。
- `DELETE /v1/ai-team/agents/:bot_id`、删除/禁用 Bot 或撤销其目标 Space 席位后，该 Bot
  不再是总群活跃成员/IM subscriber；Agent 行转为非激活。其二人群和 session 按现有
  可逆移除/历史保留语义继续受保护，再次激活时复用原二人群并重新加入总群。
  删除最后一个 Bot 后保留还是解散总群必须采用一个确定且有测试覆盖的策略，默认保留
  本人单成员群以保留聊天历史，后续新 Bot 复用原 `group_no`。
- 任意普通入口尝试邀请另一名人类/他人 Bot、移除有效自有 Bot、让本人退群、转让或解散
  总群时均被拒绝，且 DB 成员、群主和 IM 订阅不变；服务端生命周期清理仍可执行。
- 总群和现有 `ai_session_container` 父群及其 thread 均不显示在普通最近会话、关注、
  通讯录或普通群列表中，但专属 AI Team 页面可按已知 `group_no` 打开群详情
  和聊天。用户级免打扰、置顶和清空本人聊天记录继续可用。
- 总群允许创建、打开和使用普通子区；二人群的“新会话”创建并打开同一
  CommunityTopic 形态的子区，且其 IM subscriber 固定为本人和对应 Bot。未新建时会话数
  为 0，专属页面不展示二人父群；首条用户消息把默认“新对话”标题更新为消息摘要。
- 测试显式断言双向一致性：每个有效 Agent 都有唯一就绪二人群且是总群成员；总群里的
  每个 Bot 都有同 Space 的有效 Agent 和对应二人群。并发建 Bot、建群写回落空、DB/IM
  部分失败、跨 Space/他人 Bot 注入、软删除成员恢复、混合 collation，以及旧数据惰性
  补建均有回归测试。
- 运行聚焦的 `modules/ai_team`、`modules/group`、`modules/botfather` 测试，再运行
  `go test ./...`；若改动错误码或本地化行为，同时运行 `make i18n-extract-check` 和
  `make i18n-lint`。需要真实 MySQL、Redis、WuKongIM 的场景记录到 `verification.md`。
