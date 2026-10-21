---
type: Task
title: "Task: project-internal-user-list"
description: 为服务间调用增加指定用户在指定 Space 下的有效 Project 全量列表。
tags: [project, internal-api, space, isolation, auth, wire-contract]
timestamp: 2026-10-21
slug: project-internal-user-list
upstream: Mininglamp-OSS/octo-server#933
source: self
---

# Task: project-internal-user-list

## Goal

新增 `GET /v1/internal/projects?space_id=...&uid=...`，完整返回指定用户在指定 Space 下有效加入且 `activated_at IS NOT NULL` 的 Project 基础信息及角色。

## Background

用户已确认设计，规格已完成一轮独立审查；完整契约及验收以 [Internal 指定用户 Project 全量列表设计](../../../docs/specs/2026-10-21-project-internal-user-list-design.md) 为准。服务实现、相关包全量测试及独立 HTTP smoke 已完成，验证命令与边界见规格实施记录；尚未部署。

## Load-bearing list

- 复用 internal membership token、启用门槛、strict IP 限流和真实 HTTP 状态错误 envelope。
- 有效账号、Space、Space 席位、Project、Project 激活标记和 Project 席位共同决定可见性；所有查询在同一 RR 只读事务内完成。
- Project 与成员的实际 Space 均绑定到同一快照解析、校验后的 legacy Space；不以 SQL 宽松判等或 Space 管理员身份绕过成员与 Space 边界。
- 不分页、不截断，直接数组，角色编码沿用 `0/1/2`；数据库失败与真实空结果分开。

## Out of scope

- 用户态列表、既有成员验证和 epoch 契约变更。
- 数据迁移、业务写入、搜索、个人偏好、资源 ACL、下游缓存或事件扩展。

## Acceptance

- 满足设计规格中的全部验收标准，重点覆盖全量结果、激活前后可见性、跨 Space 隔离、撤权和移除中席位、角色及错误状态。
- 已完成真实 MySQL/Redis 验证、真实模块路由测试及独立 socket HTTP smoke；`go test -p 1 ./pkg/project ./modules/internal_membership ./modules/project -count=1`、全仓库 build、相关包 vet 和 i18n 门禁通过。
