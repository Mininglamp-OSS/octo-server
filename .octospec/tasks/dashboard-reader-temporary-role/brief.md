---
type: Task
title: "Task: dashboard-reader-temporary-role"
description: Add a fixed dashboardReader manager role as a temporary least-privilege bridge before general RBAC exists.
tags: ["auth", "manager", "dashboard", "access-control", "rate-limit"]
timestamp: 2026-08-10T00:00:00Z
# --- octospec extension fields ---
slug: dashboard-reader-temporary-role
source: self
---

# Task: dashboard-reader-temporary-role

## Goal

允许 SuperAdmin 把一个现有账号临时收口为 `dashboardReader`：该账号可登录管理台并读取
运营 Dashboard，但不能触发 ETL，也不能调用用户、群组、Space 或其他管理接口。

这是通用 RBAC 上线前的固定角色过渡方案。复用现有 `user.role`、RoleResolver 与角色缓存，
不新增 capability 表，不把 `dashboardReader` 加进 octo-lib 的公共 `CheckLoginRole()`。

## Load-bearing list

- `auth` — `dashboardReader` 只能通过专用 Dashboard 读 gate，不能继承 admin 权限
- `wire-contract` — `/v1/manager/me` 对该角色只声明 `dashboard.read=true`
- `rate-limit` — 新增的授权/撤销接口必须挂 `SharedUIDRateLimiter`
- `error-response` / `i18n` — 所有拒绝和失败复用已注册的本地化错误响应
- `test` — 覆盖登录资格、capability 图、六个 Dashboard 读接口、ETL 拒绝、其他管理接口拒绝、授权与撤销

## Design

1. 在 octo-server 本地定义固定角色 `dashboardReader`；不修改 octo-lib 的 `Admin` /
   `SuperAdmin` 或 `CheckLoginRole()`。
2. `/v1/manager/login` 和 `/v1/manager/me` 额外承认该固定角色；`/me` 对它只返回
   `dashboard.read=true`，其他 capability 全部为 false。
3. `modules/opanalytics` 的六个只读 handler 使用本地 Dashboard-read 策略；
   `POST /etl/run` 保持 `CheckLoginRoleIsSuperAdmin()`。
4. 新增 SuperAdmin-only、幂等的 `PUT` / `DELETE`
   `/v1/manager/user/:uid/dashboard-read`：授予时把非 SuperAdmin 账号 role 设为
   `dashboardReader`，撤销时只清除该固定角色；两者都失效 `user_role:{uid}` 热缓存。
   缓存失效失败时不得返回成功；幂等重试必须再次失效缓存，修复 DB 已变更但缓存仍旧的
   部分失败状态。
5. 授权只接受启用中、未注销的真人账号；撤销保持宽松，保证异常账号仍可回收权限。
6. `GET /v1/manager/user/dashboard-read` 提供 SuperAdmin-only 的授权清单，支持过渡期
   盘点和最终下线，不扩展成通用角色管理。
7. 授权/撤销写审计日志，包含 actor UID 与 target UID；权限查询失败不得放行。
8. 这是账号级授权，不是独立管理台 session：RoleResolver 会把当前角色注入该账号后续
   建立的有效 IM 会话，这些 bearer 也能调用 Dashboard 读接口；该取舍在临时方案中明确接受。
9. 本任务不修改 token 签发、TTL、撤销、账号封禁或注销链路；会话生命周期由独立的
   token 安全改造负责，避免临时角色方案再实现一套并行机制。

## Out of scope

- 通用 RBAC、角色/权限组合、角色管理 UI
- capability 数据表或任意 capability 的 CRUD
- 改造已有 admin/superAdmin 管理接口的角色判断
- token 生命周期、manager session TTL、账号封禁后的 bearer 失效机制
- 前端授权管理页面
- 将 Dashboard 数据收窄到单个 Space；该看板仍是全局运营读面

## Acceptance

- [x] `dashboardReader` 可通过 `/v1/manager/login` 与 `/v1/manager/me`
- [x] `/me` 仅返回 `dashboard.read=true`，`dashboard.trigger` 和其他能力均为 false
- [x] 六个 `/v1/manager/dashboard` GET 接口允许 `dashboardReader`
- [x] `POST /v1/manager/dashboard/etl/run` 拒绝 `dashboardReader`
- [x] 代表性的其他 manager 接口拒绝 `dashboardReader`
- [x] 只有 SuperAdmin 能授予/撤销；不能修改 SuperAdmin；撤销为幂等操作
- [x] 可列出全部 `dashboardReader`，用于权限盘点和过渡方案下线
- [x] 机器人、禁用账号和注销中/已注销账号不能被授予；撤销不受账号状态阻断
- [x] 授予/撤销（含幂等重试）后清理目标账号角色缓存；失效失败返回明确错误且可重试修复
- [x] 新授权接口挂 `AuthMiddleware` + `SharedUIDRateLimiter`
- [x] 聚焦 Go 测试、`go build ./...`、相关包 `go vet` / `golangci-lint`、i18n
      extract/check/lint、错误响应源码守卫与 `git diff --check` 通过

全量 `go test ./modules/user/... ./modules/opanalytics/...` 已尝试，但共享测试库包含当前
checkout 未知的 migration 记录，`testutil.NewTestServer` 在用例执行前拒绝建 migration plan；
未修改共享测试库。新增测试改用模块级 route-only harness，仍以真实 MySQL/Redis 验证角色
写入、限流响应和缓存失效。
