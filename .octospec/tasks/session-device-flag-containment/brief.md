---
type: Task
title: "Task: session-device-flag-containment"
description: Reject unknown login device flags instead of truncating them, and make "quit all devices" cover every supported flag instead of relying on WuKongIM's -1
tags: [auth, security, session, session-revocation, wire-contract]
timestamp: 2026-08-21T20:34:26+08:00
# --- octospec extension fields ---
slug: session-device-flag-containment
upstream: TBD
source: self
---

# Task: session-device-flag-containment

> 本 brief 是 [`docs/multi-device-session-design.md`](../../../docs/multi-device-session-design.md)
> 的 P0 阶段，只做止血：**不改任何顶号 / 并发会话策略，不需要客户端发版**。
> 策略矩阵、会话清单、按 device_id 撤销分别在 P2 / P1，不在本任务内。

## Goal

1. 登录侧上报的 `device_flag` 收敛到一个显式白名单；未知值**拒绝登录**，
   而不是像现在这样被 `int → uint8` 静默截断后落进某个真实的会话桶。
2. 「退出全部设备」在 octo-server 侧显式对白名单全集 fan-out，不再依赖 WuKongIM
   `device_quit` 的 `-1`——后者只覆盖 `{APP, WEB, PC}`，漏掉了 iOS 在用的 `flag=3`。
3. 白名单成为撤销侧遍历集合的唯一来源，消除 `revokeDeviceTokens` 与登录侧各自维护一份的漂移。

## Background

### 已验证事实

基线 `main@235efc8`；WuKongIM 按 `.github/workflows/ci.yml:167` 固定的
`wukongim/wukongim:v2.2.4-20260313` 核对源码。

**（a）全链路无 flag 校验**

| 入口 | 取值 | 现状 |
|---|---|---|
| `modules/user/api_usernamelogin.go:163` | `loginReq.Flag int` → `config.DeviceFlag(req.Flag)` | 无校验；`-1`→`255`、`256`→`0` |
| `modules/user/api_emaillogin.go:356` | `Flag uint8`（`api_emaillogin.go:104`） | 无校验 |
| `modules/user/api.go:4371` | `registerReq.Flag uint8` → `createUserWithRespAndTx` | 无校验 |
| `modules/user/api.go:2645-2650` | 扫码登录 `?flag=`，`0`/解析失败 → `config.Web` | 无校验；`int64→uint8` 截断 |
| `modules/oidc/api.go:373-376` | `?flag=`，默认 `0` | 仅 `0 <= n < 256` |
| WuKongIM `internal/api/user.go:652-669` | `UpdateTokenReq.Check()` | 只校验 uid / token，不看 DeviceFlag |

`pkg/auth/session_store.go:275` 等处的 `deviceFlag < 0` 守卫**永远不会触发**——
`config.DeviceFlag` 是 `uint8`，负数在转换时已被截断成 `255` 才传进 store。

flag **不参与鉴权**：`auth.TokenInfo.DeviceFlag`（`pkg/auth/tokeninfo.go:33`）只是 payload 字段，
`pkg/auth` 中没有任何基于它的判断，所以这不是提权面。它的真实职责是**撤销与顶号策略的分区键**：
一次 `flag=7` 的登录会产出 `uidtoken:7<uid>` 一个独立 bearer 和 WuKongIM 里一条 `(uid,7)` device 行，
该会话不受 APP 单会话顶号约束、不受设备锁约束、不被 `-1` 覆盖。

**（b）`-1` 覆盖不全**

WuKongIM `internal/api/user.go:80-86`：

```go
if req.DeviceFlag == -1 {
    _ = u.quitUserDevice(req.UID, wkproto.APP)   // 0
    _ = u.quitUserDevice(req.UID, wkproto.WEB)   // 1
    _ = u.quitUserDevice(req.UID, wkproto.PC)    // 2
} else { ... }
c.ResponseOK()
```

iOS 在用 `flag=3`（`octo-ios` `WKOidcProviderConfig.m:119` 的 `?flag=3`、
`WKLoginVM.m:49,55` 的 native 登录、`WKConnectionManager.m:580` 的 CONNECT
`deviceFlag = 3`），因此**封禁 / 注销 / 改密 / OIDC logout 都踢不掉 iOS 的 IM 长连接**。

octo 侧 `-1` 调用点共 4 处：

- `modules/user/session_revocation.go:165`（`finishCommittedUserSecurityMutation`：改密、重置、禁用、注销、管理员删号）
- `modules/user/api.go:3748`（`destroyAccount`）
- `modules/user/api_manager.go:1457`（管理后台禁用/删除用户）
- `modules/oidc/api.go:1354`（`ctxKiller.Kick`：OIDC logout + sync `invalid_grant`）

**（c）fan-out 是安全的**：WuKongIM 的 `quitUserDevice` 对不存在的 device 行返回
`errors.New("设备信息不存在！")`，但 `deviceQuit` handler 用 `_ =` 吞掉并始终 `c.ResponseOK()`
（`internal/api/user.go:80-90`），所以对没登录过的 flag 无条件调用不会产生错误响应。
代价是每次「全部退出」从 1 次 HTTP 调用变成 N 次（N = 白名单大小），这是低频操作，可接受。

**（d）白名单取值 `{0,1,2,3}` 已覆盖全部在用客户端**

- Android：登录与 CONNECT 均为 `0`（`OidcAuthActivity.java:246`、`WKConnectMsg.java:43`）。
- iOS：`3`。
- octo-web / PC：`1` / `2`。
- 管理后台登录服务端硬编码 `int(config.Web)`（`modules/user/api_manager.go:385`），不读客户端输入。
- Bot 侧（`app_bot` / `bot_api` / `botfather` / `robot` / `notify`）只调 `UpdateIMToken`
  且固定 `config.APP`，不经过用户登录入口，不受本次改动影响。

## Load-bearing list

<!-- touches tags: auth, session, session-revocation, wire-contract, error-response, testing -->

- **登录 wire contract**：`flag` 是已发布的公开请求字段。合法值 `{0,1,2,3}` 的行为必须
  **逐位不变**——包括 `flag=0` 走 master + 撤销旧 APP token、`flag∈{1,2,3}` 走 slave + 复用旧 token。
  本任务只改「非法值怎么处理」，不改「合法值怎么处理」。
- **扫码登录的 `flag=0` 语义**：`api.go:2645-2650` 里 `flagI64 == 0`（含缺省与解析失败）
  一律映射成 `config.Web`，与其他入口的「0 = APP」相反。这是既有 wire 行为，
  P0 **保持不变**，只在其后追加白名单校验；如需统一另立任务。
- **APP 单会话策略**：`execLogin` 的 `flag == config.APP` 分支（`api.go:1793-1795`、`1845-1874`）
  与设备锁分支（`api.go:1798`）**一行都不动**。
- **会话撤销矩阵**：`finishCommittedUserSecurityMutation`（`session_revocation.go:150-169`）
  的「commit 后必须尝试 HTTP 撤销 + 独立的 IM 退出」这条边界语义不变；
  只把其中的 `quit(uid, -1)` 换成 fan-out，且**任一 flag 失败必须 join 进返回错误**，
  不能因为多了几次调用就把失败吞掉。
- **v3 会话撤销**：`sessionRevocationActive` 为真时 HTTP 侧走 `RevokeAll(uid)`（与 flag 无关），
  为假时走 `revokeDeviceTokens`（`api_manager.go:775`，目前只遍历 `{APP, Web, PC}`）。
  后者的遍历集合必须换成同一个白名单常量，否则 legacy 模式下 `flag=3` 的 bearer 仍然漏网。
- **错误响应 i18n**：user 模块的拒绝走 `respondUserRequestInvalid(c, "flag")`
  （`modules/user/api_i18n.go:60`），不得使用 `c.ResponseError` / 裸 `c.JSON`。
- **oidc 的 lint 豁免预算**：`tools/lint-direct-error-response/baseline.txt:27` 给
  `modules/oidc/api.go` 记的是 **14** 条 EXEMPT 裸响应。authorize 里新增一条
  `AbortWithStatusJSON` 必须同步把计数改成 15 并在该行注释里说明原因；
  否则 `make i18n-lint` 会红。（authorize 是 browser-facing redirect flow，
  沿用同文件的 `errMsg()` 风格是对的，不要在这里引入 httperr。）
- **OIDC state 透传**：`StateData.DeviceFlag`（`modules/oidc/state_store.go:30`）
  是 authorize → callback 之间的一次性载荷，校验必须发生在 **authorize 写入 state 之前**，
  而不是 callback 消费之后——否则用户走完整个 IdP 授权流程才被拒，体验不可接受。
- **`QuitUserDevice` 的调用契约**：`octo-lib config.Context.QuitUserDevice(uid, flag int)`
  是共享库方法，本任务**不改 octo-lib**，只在 octo-server 侧包一层 fan-out helper。

## Out of scope

- 顶号策略、并发会话上限、platform class —— P2（`session-policy-matrix`）。
- 会话清单接口、按 `device_id` 撤销、`DELETE /v1/user/devices/:device_id` 的假删除 —— P1
  （`session-inventory-and-revoke`）。
- iOS 从 `flag=3` 归位、桌面端 flag 分配 —— 需要客户端发版，P2 且依赖设计草案 Q3/Q4 拍板。
- `device_flag` 表补 `3` 这一行、主设备权重、`pcQuit` 语义 —— D5，非功能，P2 一并处理。
- 修改 WuKongIM 上游 `device_quit` 的 `-1` 展开集合 —— 属于上游，本任务用 fan-out 绕开。
- 扫码登录 `flag=0 → Web` 与其他入口 `0 → APP` 的语义统一 —— 另立任务。
- 任何客户端仓库（octo-ios / octo-android / octo-web）的改动。

## Acceptance

- **白名单拒绝**：对 `/v1/user/login`、`/v1/user/emaillogin`、`/v1/user/register`、
  扫码登录兑换、OIDC `/authorize` 各写一条表驱动测试，断言 `flag ∈ {4, 7, 99, 255, 256, -1}`
  一律被拒（user 模块返回 i18n 信封且 `error.code` 为参数非法码；oidc authorize 返回 400 `errMsg`），
  且**没有任何副作用**：Redis 无新增 `uidtoken:*` key、无 `token:*` key、
  未调用 `UpdateIMToken`（用 mock/spy 断言调用次数为 0）。
- **合法值零回归**：`flag ∈ {0,1,2,3}` 的现有登录测试全部原样通过；
  特别是 `TestReplaceAPPTokenRevokesPreviousBearerBeforeIssue`、
  `TestLoginCheckPhoneUsesAPPTokenReplacementBeforeIMUpdate`、
  `session_revocation_test.go` 中依赖 `{APP, Web, PC}` 顺序的用例不需要修改断言。
- **截断路径已封死**：单测直接覆盖 `-1` 与 `256`——断言它们走的是「拒绝」分支，
  而不是被转成 `255` / `0` 之后继续登录。
- **fan-out 覆盖**：新增测试用 fake `QuitUserDevice` 记录调用参数，断言一次
  「退出全部设备」对白名单里**每个** flag 各调用一次（含 `3`），顺序不敏感；
  并断言其中任意一次失败时，错误会被 join 进 `finishCommittedUserSecurityMutation` 的返回值
  而不是被吞掉。四个 `-1` 调用点（`session_revocation.go:165`、`api.go:3748`、
  `api_manager.go:1457`、`oidc/api.go:1354`）全部改用同一个 helper，
  仓库内 `grep -rn "QuitUserDevice(.*-1)" --include="*.go"` 无残留。
- **单一来源**：`revokeDeviceTokens` 与登录白名单引用同一个导出常量；
  加一条守卫测试断言两者集合相等，防止后续只改一边。
- **端到端（需真实 MySQL + Redis + WuKongIM）**：用 `flag=3` 登录拿到 bearer →
  触发一次改密 → 断言该 bearer 立即 401，且 WuKongIM `(uid, 3)` 的 device token 已被清空
  （即再次用旧 token CONNECT 会 auth fail）。这是 D1 的回归护栏。
- `go test ./modules/user/... ./modules/oidc/... ./pkg/auth/...` 全绿；
  `make i18n-extract-check`、`make i18n-lint`、`golangci-lint run ./...` 全绿
  （含 baseline 计数已同步）。
