---
type: Task
title: "Task: oidc-bearer-jwt-redemption-ledger"
description: /exchange-jwt 的新鲜度锚点从 iat 固定上限换成兑换台账（首次兑换上限 + 空闲窗口）
tags: [oidc, auth, bearer-jwt, replay]
timestamp: 2026-09-06T10:01:13Z
# --- octospec extension fields ---
slug: oidc-bearer-jwt-redemption-ledger
upstream: Mininglamp-OSS/octo-server#829
source: self
---

# Task: oidc-bearer-jwt-redemption-ledger

## Goal

`/exchange-jwt` 现在用 `bearerJWTMaxAge = 10min`（从 `iat` 起算）做新鲜度判定
（`bearer_jwt.go:75`、`bearer_jwt_verifier.go:83`）。这个锚点选错了：`iat` 是上游
签发时刻，跟"用户什么时候真的来兑换"没有关系。后果是两头都不对：

- **误伤合法用户**：客户端登录后隔 36 分钟才兑换，被拒，返回的是与"凭据无效"
  不可区分的 401（线上已发生，`err.server.oidc.exchange_token_rejected`）。
- **拦不住真实重放**：攻击者在签发后 10 分钟内抓到 token 照样能兑换。

把锚点换成**兑换行为本身**：服务端为每张兑换过的 token 记一条台账，用两个边界
代替原来那一个：

| 边界 | 约束什么 | 默认值 | 防的是 |
|---|---|---|---|
| `F` 首次兑换上限 | 首次兑换距 `iat` 的间隔 | 24h | token 在**首次使用前**被窃取 |
| `T` 空闲窗口 | 相邻两次兑换的间隔 | 7d | token 在用户**弃用/登出后**被窃取 |

合法客户端只要在持续使用，`T` 每次兑换都被刷新，永远不会被拒；`F` 从 10 分钟
放宽到 24 小时，覆盖观察到的 36 分钟以及"登录后当天才打开客户端"。两个边界都
比 token 自己的 `exp`（约 15 天）紧。

## Background

- 这条路径整体由 #829 引入（commit `5437764`），`bearerJWTMaxAge` 是同一个 PR
  加的加固措施之一，见 `.octospec/tasks/oidc-oauth2-provider-abstraction/brief.md`
  的 P1-4 与 guard-matrix 的 G8 行。加它的理由成立：在那之前 `exp` 是唯一的新鲜度
  控制，一张抓到的 assertion 在 15 天里可以反复兑换会话，**包括用户登出之后**，
  而我方查不到上游的吊销状态。
- 同一份 brief 的"Deliberately not fixed"记着：`/exchange-jwt` 规格上是一次性兑换，
  但**没有实现一次性消费**，10 分钟上限就是它的替代缓解；正规做法（Redis 记录
  token 摘要）当时被判为设计变更未做。本任务做的就是那件事，只是不做成"一次性"
  ——客户端是否会用同一张 token 重复兑换，仓库里没有记录，做成一次性会以同样
  形态的 401 打断它们。`T`/`F` 可配，等客户端用法确认后可以收紧到一次性语义。
- **不能靠 Redis key 的 TTL 表达空闲窗口**。若 TTL = `T`，key 过期后台账消失，
  这张 token 在下次兑换时会被当成"首次兑换"重新放行——窗口等于没有。台账记录
  必须活到 token 自己的 `exp`，空闲判定用记录里的 `last_at` 时间戳比对。

## Load-bearing list

- `auth` — `/exchange-jwt` 是未认证的会话签发端点，改的是它的准入判定。
- `error-response` — 失败仍必须收敛到同一个 401 码（反枚举），真实原因只进日志。
- `wire-contract` — 请求/响应形状不变；变的只是"哪些 token 被接受"。
- `test` — #829 的 `bearer_jwt_test.go` / `bearer_jwt_hardening_test.go` /
  `api_exchange_jwt_test.go` 里有直接断言 10 分钟上限的用例，必须一起改并说明。
- 凭据归属判定 `api_exchange.go:162` 复用 `VerifyForRedemption` 做**分类**
  （"这是不是发错端点的业务 JWT"）。台账绝不能进这条路径：那会让一张投错端点的
  token 在台账里留下或刷新记录。验签保持无副作用，台账只由 `/exchange-jwt`
  handler 调用。
- `modules/integration` 的两个端点用 `VerifyForAuthentication`（maxAge=0），
  本任务不改它们，也不让台账影响它们。

## Out of scope

- 一次性消费（`N=1`）。需要先确认客户端是否重复兑换；`T` 可配已经留出了收紧路径。
- 客户端侧改动（重新签发 token、改走我方会话续期）。
- `modules/integration` 两个端点的重放窗口（等于 `exp`，是"上游 JWT 无 aud/jti"
  的直接后果，仍记在 #829 的 Pending 里）。
- 我方登出时主动作废台账（需要会话 → 摘要的反查，另开一任务）。
- `/exchange`（上游 access_token 那条路）的任何行为。

## Acceptance

- `verifyBearerJWT` 的 `maxAge` 参数与 `ErrJWTTooOld` 哨兵保留（纯函数的通用能力，
  将来要给某条路径重新加 iat 上限时不必重写）；**`bearerJWTMaxAge` 常量删除** ——
  改造后两个生产调用方都传 0，一个生产不用的策略常量留在 `bearer_jwt.go` 里，迟早
  会被接回某条路径，把 iat 锚点的老问题重新装上。它的取值移进测试
  （`testBearerJWTMaxAge`），那里验的是参数本身的行为。
- `VerifyForRedemption` 不再套 iat 上限，改为只验签名/exp/claims，并返回
  `RedeemedBearerJWT{Claims, IssuedAt, ExpiresAt}` —— 台账要用 iat/exp，而让调用方
  再解一次 payload 就是第二处 JWT 解析，两处对同一张 token 得出不同 iat 则判定失效。
- 新增台账：key `oidc:bjwt:redeem:{sha256(token) hex}`，值含 `last_at`，
  TTL = `min(exp - now, redemptionRecordMaxTTL)`，下限 1 秒 —— 记录与 token 同寿，
  但不让一张 `exp` 离谱的 token 在 Redis 里占一条近乎永久的记录。
- 两个边界都收敛到**可执行取值**：非正回落默认、截到整秒且不低于 1 秒、
  都不超过记录寿命，且 **`F` 不超过 `T`**。最后一条是安全前提而非整洁：记录丢失
  之后判定只剩 `F`，`F > T` 会把一张本该 `reject_idle` 的 token 当成首次兑换放行
  （fail-open，且信号从 `reject_stale_first` 变成 `admit_first`）。因此 `T < F`
  不是受支持的配置，配了会被收敛成 `F = T` 并在启动日志里说明。
- 判定表（全部有测试）：
  | 台账状态 | 条件 | 结果 |
  |---|---|---|
  | 无记录 | `now - iat ≤ F` | 放行，写记录 |
  | 无记录 | `now - iat > F` | 拒 401 |
  | 有记录 | `now - last_at ≤ T` | 放行，刷新 `last_at` |
  | 有记录 | `now - last_at > T` | 拒 401 |
- 拿不到台账时按 `min(F, T)` 判一个上限（不查历史，直接按 `iat` 算），并计独立
  metric label + `Warn` 日志。方向必须比"只用 exp"紧，且**绝不比正常路径松**。
  收敛后 `F ≤ T`，所以这个 `min` 恒等于 `F`；保留它表达的是不变量本身。
  台账**未配置**与台账**报错**用两组不同的 label（`unconfigured_*` / `degraded_*`）：
  前者不会自愈且 `T` 永远不生效，共用一组会被看板读成"Redis 在抖"。
- 客户端可见行为不变：所有拒绝仍是 `ErrOIDCExchangeTokenRejected`（401），
  不新增错误码，不在响应里区分原因。
- 新增 metric label 进 `exchangeJWTResultLabels()`，`metrics_label_coverage_test.go`
  的扫描守卫通过。
- `F` / `T` 由 env 配置，有默认值；非法值回落默认值而不是禁用台账。
- 回归：36 分钟前签发、首次兑换 → 200（今天是 401）。
- `go test ./modules/oidc/... ./modules/integration/...` 通过；
  `make i18n-extract-check`、`make i18n-lint`、`golangci-lint run` 通过。
