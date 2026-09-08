---
type: Task
title: "Task: robot-test-systemsettings-closed-pool"
description: modules/robot 的测试在 -shuffle=on 下会随机报 "sql: database is closed"。根因是 common.EnsureSystemSettings 这个进程级单例锁定了第一个调用者的 *config.Context（连同它的 *sql.DB 连接池），而 TestOwnedBots_CheckMembershipDBError 故意关掉了自己那个池。给单例加 test-only 的快照/还原，让关池只影响关它的那个测试。
tags: ["testing", "commit"]
timestamp: 2026-09-08T00:00:00+08:00
# --- octospec extension fields ---
slug: robot-test-systemsettings-closed-pool
upstream: 无 issue（在 all-member-group 工作中顺带发现，main 上同样可复现）
source: self
---

# Task: robot-test-systemsettings-closed-pool

> One task = one `.octospec/tasks/<slug>/` directory. This brief is the spec for
> the work. AI may draft it from existing code; a human confirms it.

## Goal

让 `modules/robot` 的测试在任意执行顺序下都稳定通过。`go test -race -shuffle=on`
下约每 8～12 次会有一次失败，报 `sql: database is closed`，而且失败的是哪个测试取决
于洗牌结果——它本身没有任何问题。

## Background

一条完整的因果链（已实测，非推断）：

1. `robot.New(ctx)` 调 `commonmodule.EnsureSystemSettings(ctx)`
   （`modules/robot/api.go:148`，`:406` 再来一次）。
2. `EnsureSystemSettings` 是进程级单例：它锁定**第一个**调用者的 `*config.Context`
   ——连同那个 context 的 `*sql.DB` 连接池——此后无视传进来的 ctx，一律返回同一个实例
   （`modules/common/system_settings.go`，`sharedSystemSettings`）。
3. `TestOwnedBots_CheckMembershipDBError`（`modules/robot/api_test.go`）自建 context、
   调 `New(ctx)`，然后**故意** `ctx.DB().Close()` 去走数据库错误分支，且不再重开。
4. 洗牌后如果它恰好是全进程第一个调 `New(...)` 的测试，单例此后一直握着一个已关闭的
   池；之后每个 `setupBotSettings`（`bot_setting_integration_test.go:48-60`）里的
   `EnsureSystemSettings(ctx).Reload()` 都失败。

每个 `config.NewContext` 确实有自己的池（`mysqlOnce` 是 per-context，`db.NewMySQL`
没有全局缓存），所以泄漏路径只有这个单例，不是共享池。

## Load-bearing list

- `modules/common` 的 `sharedSystemSettings` 单例语义：**只能快照/还原，不能裸重置**。
  历史上的 `resetSharedSystemSettingsForTest` 就是因为裸重置被删掉的——octo-lib 的
  `register.GetModules` 用 `sync.Once` 把 moduleList 缓存了整个测试二进制的生命周期，
  清空单例会让下一个调用者拿到一个 Manager 永远看不到的新实例，两者从此分叉。还原
  「原来那个实例」才不会分叉。
- `TestOwnedBots_CheckMembershipDBError` 现有断言（`err.server.robot.query_failed` +
  `http_status: 500`）不变——它仍然靠关池来走那条分支。
- 仓库既有的同形状先例：`modules/project/all_member_group_registry.go` 的
  `SnapshotAllMemberGroupHooksForTest` / `RestoreAllMemberGroupHooksForTest`。

## Out of scope

- 不改 `EnsureSystemSettings` 的生产语义（仍然是「锁定第一个 ctx」的单例）。把它改成
  per-ctx 是另一件事，牵动所有模块。
- 不动 `modules/bot_task`、`modules/bot_mention`、`modules/ai_team` 里同样关池的测试：
  它们的 ctx 不经过 `EnsureSystemSettings`，没有这条泄漏路径。
- 不修 `modules/robot` 里三个依赖 WuKongIM broker 的删除测试（本地无 broker 时必失败，
  与执行顺序无关，也与本改动无关）。

## Acceptance

- 强制坏顺序可复现、且修复后消失：
  `go test -count=1 -run '^(TestOwnedBots_CheckMembershipDBError|TestBotSettings_BatchValidationRejectsAtomically)$' ./modules/robot/`
  ——修复前必失败（`sql: database is closed`），修复后通过。
- `for i in $(seq 1 20); do go test -race -shuffle=on -count=1 ./modules/robot/; done`
  20 次里没有任何一次因为 `sql: database is closed` 失败。
- 新增源码守卫 `TestRobotDBClosingTestsRestoreSystemSettings`：本包内任何调
  `.DB().Close()` 的测试文件必须同时出现 `SnapshotSystemSettingsForTest`；去掉修复后
  它确定性失败（不像洗牌那样看运气）。
- `go vet` / `golangci-lint run` / `make i18n-extract-check` / `make i18n-lint` 全绿。
