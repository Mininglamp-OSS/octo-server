---
type: Task
title: "Task: digital-avatar-bot-kind — Route B"
description: "Implement robot.kind=avatar with AI Team-only groups, projects, administrator management and lifecycle revocation."
tags: ["space", "isolation", "auth", "acl", "bot-api", "thread", "wire-contract", "error-response", "i18n", "rate-limit", "testing", "commit"]
slug: digital-avatar-bot-kind
source: user
status: verified
decision: "B — robot.kind"
approval: "方案B吧 重新整个worktree实现"
base_commit: 0bc8edaca4201a43beaf40342bc48d08da4dcb1a
---

# 数字分身：方案 B 实施规格

## 决策与状态

**方案 B 已确认：在 robot 表增加 kind=user|avatar。当前状态：已实现并验证。**
用户最新确认：Avatar 不加入普通群或项目群，只允许由 AI Team 服务投影到受控 AI 群。
本文件是唯一实施与验收依据。原始 A/B 探索移至 [background.md](background.md)，仅作历史参考。

- 分支：`feat/digital-avatar-bot-kind`
- Worktree：`/Users/kense/Projects/octo/octo-server-digital-avatar-bot-kind`
- 基线：`feat/multi-ai-teams` 的 `0bc8edac`（含自定义 AI 团队、普通入口隐藏规则及仅展示 octo_hosted 个人分身的通讯录规则）。
- 收尾授权：用户已要求重新 rebase、提交并提 PR；不包含合并或部署。
- 其他 digital-employees worktree 仅作参考，不采用其 App Bot 会话特例作为最终模型。

## Goal

交付可实际运行的组织管理数字分身。Avatar 没有个人主人，由相应管理员创建、发布、
配置、下架和删除，不随创建者、邀请人或使用者退出级联。
有效 Space 成员能私聊、独立加入自己的 AI 团队并创建会话；
项目管理员能将它加入项目，但不会因此加入项目群。Avatar 只在受控 AI 群及其会话子区收发消息。

## Decisions

### 实体、管理、运行时

- `robot.kind=user|avatar`，默认 user，旧数据不自动升级。
- Avatar 的 `creator_uid` 为空；`created_by` 只作审计。不能根据 owner 为空、
  token 形状或 `agent_hosting` 判断 avatar。
- 类型、管理范围和发布状态只能由管理员服务写入。用户 API、botfather、
  register 自报均不能修改。Bot API 认证上下文明确识别 avatar。
- **Q1：支持 platform 和 space。**发布后在相应有效 Space 建立真实
  `space_member` 席位；平台 avatar 覆盖已有 Space，并在新建 Space 时补齐。
  发布与 Space 创建并发不能漏席位。可见不等于群或项目成员资格。
- **Q2：支持两种管理员。**superadmin 管理平台 avatar；有效 Space 的
  owner/admin 管理本 Space avatar。提供创建、列表、详情、编辑、发布、下架、
  删除、token reveal/rotate；描述、名字、头像和偏好采用同一授权。
- **Q3：管理员凭据启动运行时。**平台或 Space 管理员通过受范围限制的已认证接口
  领取 Bot token；运行时使用 token 调用 register 获取 IM 连接信息。
  不新增服务身份，不改 fleet/daemon。个人 creator provisioning 不得泄露 avatar token。
- 保留普通 App Bot 的存储、接口和权限；本任务不迁移或自动升级 App Bot。

### AI 群、项目、私聊和 AI 团队

- Avatar 不得通过普通群、邀请确认、预设群、组织事件或项目全员群路径加入群；
  即使遗留 `group_member` 行存在，运行时也必须拒绝访问。
- 只有 AI Team 服务可将 Avatar 投影到私有容器群、自动全员群和自定义 AI 团队；
  群/子区访问仍须验证 AI Team 关系、有效 Space 席位与父资源。
- 三类 AI 的群与子区均从普通群、会话和分类入口隐藏；普通群接口不能绕过
  AI Team 成员管理。数字员工加入自定义团队仍需当前用户已激活该 Agent，
  以及有效发布状态和对应 Space 席位；其他用户的个人 Bot 仍不可加入。
- `ai_team_group` 旧表和两份旧迁移已废弃，不恢复。自动与自定义团队以现有
  `group` / `group_member` 为事实来源，并关联 `ai_team_agent` 校验使用者权限。
- 项目管理员可添加和移除 avatar。当前基线 project D15 已实施个人 Agent
  owner 校验，必须新增明确的 avatar 准入分支，不能仅依赖 Space 席位。
- 个人用户的离开不级联带走 avatar；管理员显式移除和下架清理仍生效。
- 私聊采用同有效 Space 自动通过，完成 friend/IM 白名单处理；
  离开 Space 后遗留 friend 行不能继续授权。
- AI 团队受保护两人容器保持免 @。
- 多个用户可独立添加同一 avatar，各自持有 Space/user/avatar 关系、容器和会话。
  一人移除不影响他人；停止该用户的主动路由，保留历史，重新添加可复用并修复投影。
- AI 会话目标由服务器存储的 Agent/session 关系派生，不信任请求 robot_id/purpose。
  运行时上下文按 Space、bot、channel/session 隔离。
- 沿用现有每 Bot 队列、事件序号、唤醒和限流设施；文档明确共享容量/消费/限流配置，
  不新增手写通用 HTTP 计数器。

### 能力矩阵

Avatar 使用显式允许清单；新路由、未知能力默认拒绝。
类型权限与资源权限必须同时满足。

| 能力 | Avatar |
| --- | --- |
| 私聊收发、typing、已读 | 同一有效 Space 允许 |
| 普通群/项目群消息收发、sync、已读 | 拒绝 |
| AI Team 受控群消息收发、sync、已读 | 有效 AI Team 关系与 Space 席位允许 |
| 群信息、成员、群 MD 读取 | 资源授权后允许 |
| 建群、改群信息、增删群成员 | 拒绝 |
| 子区读取、消息收发、同步、回执 | 仅有效 AI Team 父群下允许 |
| 建删子区、修改群/子区 MD | 拒绝 |
| 项目成员 | 项目管理员添加/移除，不跟随个人级联 |
| AI 团队成员 | 有效 Space 成员独立添加/移除 |
| 文件、卡片、命令、免 @ 偏好 | 相应业务允许；管理配置受授权约束 |
| 搜索、语音、OBO | 拒绝；不得成为 OBO grantee |
| 用户密钥、Space principal 调用/解析目标 | 拒绝 |
| register、心跳、自己的事件/ACK | 已发布且凭据有效时允许 |

### 生命周期

- 集中处理发布、下架、删除、凭据撤销与成员关系清理。
- 下架/删除后不能继续认证或投递；群、项目、AI 团队有效关系关闭。
- 外部 IM、连接与缓存清理必须幂等、可重试，持久记录待清理状态。
  DB/Redis/IM 不是一个原子事务；外部失败不得恢复权限。
- 重新发布不静默恢复项目或已移除 AI 团队关系；普通群关系始终不被恢复。
- 历史会话、消息不物理删除。

## Load-bearing list

- **space / isolation / acl / thread:** 真实 Space 席位、群/项目成员、父资源及并发入驻。
- **auth / bot-api:** 服务端类型、独立 avatar 身份、能力允许清单、全路由族无旁路。
- **lifecycle:** 发布、凭据、成员、IM 投影一致收敛；清理重试和无个人级联。
- **wire-contract:** discovery、Space bots、会话、Bot identity/register 类型一致；
  存量 App/User Bot 行为不回归。
- **error-response / i18n:** 注册错误码、统一 envelope、不泄露 token/租户信息。
- **rate-limit:** 管理入口共享 UID 限流、Bot 现有限流、共享队列运维配置。
- **testing:** 正反能力、跨 Space、独立成员、生命周期失败/重试/并发和存量回归。
- **commit:** 使用英文 Conventional Commits 提交；按用户授权推送 feature 分支并创建 PR。

## Implementation checklist

- [x] robot 类型、管理范围、发布/待清理状态、数据约束和迁移。
- [x] 统一能力、avatar 身份/资源授权、register、token 生命周期。
- [x] 管理员 CRUD、发布/下架/删除、token、设置与头像授权。
- [x] 平台/Space 席位与新 Space 入驻、发现/通讯录/会话类型。
- [x] 普通群拒绝、AI Team 受控群/子区收发、无个人级联。
- [x] Project 管理员准入/移除；Avatar 不投影到项目群。
- [x] AI 团队资格、分类、独立成员、容器/会话路由。
- [x] 同 Space 私聊、撤权、所有禁止能力与各入口保护。
- [x] 生命周期清理、外部重试、重新发布语义。
- [x] 测试、i18n、build、vet、diff 检查及 API/运维说明。

## Acceptance

1. Space avatar 发布后，同 Space 两名用户可私聊、独立加入 AI 团队并在会话收到回复；
   跨 Space 拒绝，一人移除不影响另一人，历史可复用。
2. 普通群、邀请确认、预设群、组织事件和项目群均拒绝 Avatar；遗留成员行不授予访问。
3. AI Team 私有容器及会话子区读取/发消息/同步/回执成功；建删子区、修改 MD 返回注册拒绝码。
4. 项目管理员添加/移除成功、普通成员失败；Avatar 不进入项目群；
   创建者/邀请人离开时 avatar 保留。
5. 平台 avatar 在已有和新建有效 Space 建立席位；并发发布/建 Space 不漏席位；
   不自动加入任何群和项目。
6. 真正调用所有禁止路由：建群、改群、成员增删、搜索、voice、OBO、principal；
   直接/复用路由均不可旁路，未知路由默认拒绝。
7. kind/管理范围/发布状态不能经用户、botfather、register 上报升级；
   普通 ownerless robot 继续 fail-closed。
8. 正确管理员可配置和领取/轮换凭据；普通用户与其他 Space 管理员失败；
   旧 token 失效，owner_uid 不伪装成创建管理员。
9. 下架/删除撤销授权、清理群/项目/Agent、撤销 IM token；外部失败可重试，
   不重新开放权限，不删除消息历史。
10. 存量 App/User Bot、AI 团队、project 测试通过。
11. 运行聚焦及跨模块集成/并发测试，i18n-extract-check、i18n-lint、
    go build ./...、go vet ./...、git diff --check。
    使用隔离测试状态，命令与结果记入 verification.md。
12. 不存在 `ai_team_group` 表时，数字员工仍可加入、读取和退出自定义 AI 团队，
    并读取自己的私有容器和自动全员群；移除成员或 Agent 后立即撤权。
    本次融合按用户要求使用真实接口验证，不进行 BUA 页面测试。

## Out of scope

- 迁移/升级现有 App Bot（包括 Octo Assistant）。
- OBO/个人分身实现、把 runtime hosting 当权限。
- octo-web、OpenClaw 插件、fleet/daemon 修改。
- 部署、合并；commit、push、PR 已由后续用户指令纳入收尾范围。

## 名称

Avatar 是组织管理的数字分身/数字员工，有独立协作身份；
不同于 OBO Persona Clone 和跟随主人级联的个人 hosted Agent。
