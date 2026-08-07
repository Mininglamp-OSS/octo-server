---
type: Task
title: "Task: scanlogin-poll-binding"
description: Bind the scan-login poll channel to the browser that minted the QR, closing the QRLJacking account-takeover path.
tags: ["auth", "scan-login", "security", "rate-limit", "wire-contract"]
timestamp: 2026-08-07T00:00:00Z
# --- octospec extension fields ---
slug: scanlogin-poll-binding
upstream: pentest-retest-2026-07-30 (未列条目，代码审计新增)
source: self
---

# Task: scanlogin-poll-binding

## Goal

扫码登录链路上，`auth_code` 会被下发给**任何知道 uuid 的匿名轮询方**。本任务给轮询侧
加上会话绑定：`GET /v1/user/loginuuid` 额外下发一枚 `poll_secret`（只进响应体，不进
二维码），`GET /v1/user/loginstatus` 只有携带匹配的 `poll_secret` 才能拿到敏感字段。

> **⚠️ 修订（code review 后）：本任务不关闭 QRLJacking。**
>
> 初版 brief 声称 `poll_secret` 断掉了「攻击者自建二维码钓鱼」的链路，**这是错的**。
> 攻击者是**自己调用 `loginuuid`** 的那一方，`poll_secret` 会连同 uuid 一起发给他；
> 他拿自己的密钥轮询自己的 uuid，校验通过，`auth_code` 照拿。会话绑定挡住的是
> **另一个更弱的威胁**：第三方看到了二维码（肩窥、截图转发、录屏）但并未 mint 它。
>
> QRLJacking 无法在服务端单独关闭 —— 服务端区分不出「受害者扫了攻击者的码」与
> 「本人扫了自己的码」，**只有确认的人能**。唯一的真断点是让确认者看见「请求登录的
> 是一台陌生设备、来自陌生 IP」并因此拒绝。本任务补齐该能力的服务端半边
> （`ScanLoginOrigin` 随扫码结果下发给手机），**但在 iOS/Android 把它渲染进确认弹窗
> 之前，QRLJacking 依然成立**。移动端工作是本漏洞的阻塞项，另开任务跟踪。

## Background

当前链路（代码位置为 `8a9df20`）：

| 步 | 接口 | 鉴权 | 行为 |
|---|---|---|---|
| 1 | `GET /v1/user/loginuuid` | 无认证、无限流 | `api.go:2024` 生成 uuid（`util.GenerUUID()` = crypto/rand UUIDv4，**不可爆破**） |
| 2 | `GET {QRCodeInfoURL}?code=uuid` | AuthMiddleware | `qrcode/api.go:167` 签发 `auth_code`，绑定 `scaner` |
| 3 | `GET /v1/user/grant_login` | AuthMiddleware | `api.go:2374` 校验 `scaner == loginUID` ✅；`api.go:2381` 把 `auth_code` **明文**写进 qrcode 状态 |
| 4 | `GET /v1/user/loginstatus?uuid=` | 无认证、无限流 | `api.go:2094` 把 `qrcodeModel.Data` 整包吐出 → 含 `auth_code` |
| 5 | `POST /v1/user/login_authcode/{code}` | loginLimit | 只验 code 存在 + type，**不验兑换者身份** → 发 token |

第 3 步的 `scaner == loginUID` 校验是正确的，洞不在那里。洞在于 **uuid 与「申请它的
浏览器」之间没有任何绑定**，且第 5 步没有兜底校验。前端 `login_vm.tsx:451` 印证：轮询
就是裸 `user/loginstatus?uuid=${uuid}`，没有第二个凭据。

两份渗透报告（2026-05-15 首测 / 2026-07-30 复测）均未覆盖此路径 —— 扫码是交互式流程，
自动化扫描器进不去。

## Load-bearing list

- `auth` — 扫码登录是**未认证 → 已认证**的凭据签发链路，任何 fail-open 都是账号接管
- `rate-limit` — `loginuuid` / `loginstatus` 目前无限流；`loginstatus` 长轮询挂 10s
- `wire-contract` — `loginuuid` 响应新增 `poll_secret` 字段；`loginstatus` 响应在未
  携带 secret 时收窄（`auth_code` / `uid` / `encrypt` 被剥离）
- `error-response` / `i18n` — `modules/user` 已迁移 i18n，新增的拒绝路径必须走
  `respondUserError` + 注册的 `pkg/errcode` 码，不得用裸 `c.ResponseError`
- `test` — 需覆盖「无 secret / 错 secret / 对 secret」三条路径

## 设计要点

1. **`poll_secret` 生成与存储**：`getLoginUUID` 生成 `util.GenerUUID()`，把 **SHA-256**
   存进独立 Redis key `scanlogin:poll:{uuid}`（TTL 与 qrcode 同步），明文只在 HTTP 响应
   体里回给申请方一次。**绝不写进 `qrcode:{uuid}` 的 payload** —— 那个 payload 正是
   `loginstatus` 要回显的内容，写进去等于自我否定。
2. **比对**：`subtle.ConstantTimeCompare`，避免比对环节留时序侧信道。
3. **降级语义（fail-closed）**：secret 缺失或不匹配 → 仍返回真实 `status`，但剥离
   `auth_code` / `uid` / `encrypt`。选择「剥字段」而非「报错」是为了让部署窗口内仍持有
   旧 bundle 的浏览器停在授权页而不是陷入 `expired → 重新申请` 的死循环；旧 bundle 不会
   拿到 `auth_code`，安全性不打折。
4. **限流**：`loginuuid` / `loginstatus` 补 `StrictIPRateLimitMiddleware`。注意
   `loginstatus` 是长轮询、且企业 NAT 下大量用户共用出口 IP，阈值必须给足，宁松勿断
   （具体值见 Acceptance，并保持 env 可调）。
5. **TTL 收敛**：`authcode` 10min → 5min；`qrcode` 扫码/授权后续期维持 5min。加了
   `poll_secret` 之后窗口本身已不是主要风险面，取保守值以避免弱网/慢操作用户回归。
6. **长轮询泄漏**：`getloginStatus` 的 select 补 `c.Request.Context().Done()` 分支，
   客户端断开即释放，不再空转满 10s。
7. **前端**（octo-web）：`login_vm.tsx` 保存 `loginuuid` 返回的 `poll_secret`，轮询时
   带上；uuid 轮换时一并轮换。

## Out of scope

- **取消 `login_authcode` 这一跳**（让 `loginstatus` 直接下发 token）—— 协议重构，
  影响面超出本次；`poll_secret` 已经堵住利用链
- **手机确认页 UI 渲染设备上下文** —— 服务端已在本次补齐数据下发
  （`ScanLoginOrigin` → `HandlerTypeLoginConfirm.origin`），但渲染在 iOS/Android，
  那两个仓不在本次范围。**这是关闭 QRLJacking 的阻塞项**，必须另开任务跟到底
- **`modules/file` 预签名上传/下载越权**（复测 4.2/4.4/4.6）—— 独立任务
- **`pkg/space` SpaceMiddleware fail-open**（复测 4.11）—— 独立任务
- **octo-lib 的 `common/constant.go`** —— 外部模块，新前缀在 octo-server 本地定义
- 扫码入群（`QRCodeTypeGroup`）链路 —— 不签发登录凭据，不在本次

## Acceptance

**octo-server**

- [ ] `GET /v1/user/loginuuid` 响应含 `poll_secret`，且 `qrcode:{uuid}` 的 Redis
      payload 中**不含**该值（断言 payload 反序列化后无 `poll_secret` 键）
- [ ] `GET /v1/user/loginstatus?uuid=X`（无 `poll_secret`）在 status=`authed` 时，
      响应**不含** `auth_code` / `uid` / `encrypt`，但 `status` 仍为 `authed`
- [ ] `GET /v1/user/loginstatus?uuid=X&poll_secret=<错值>` 同上被剥离
- [ ] `GET /v1/user/loginstatus?uuid=X&poll_secret=<对值>` 返回完整 Data 含 `auth_code`
- [ ] 端到端回归：mint → 扫码 → grantLogin → 带对 secret 轮询 → `login_authcode` 兑换成功
- [ ] `loginuuid` / `loginstatus` 命中 `StrictIPRateLimitMiddleware`，tag 分别为
      `scanlogin_uuid`（20 req/min，burst 10）与 `scanlogin_status`（120 req/min，
      burst 60），阈值可经 env 覆盖
- [ ] `authcode` TTL = 5min（用户选择的保守值），扫码/授权后 `qrcode` TTL 维持 5min；
      `scanLoginPollSecretTTL` 严格大于最坏可读窗口 60s+5min+5min=660s 并留余量
- [ ] `getloginStatus` 的 select 含 `c.Request.Context().Done()` 分支
- [ ] `poll_secret` 经**请求头** `X-Scan-Poll-Secret` 传递，不出现在 URL / access log
- [ ] 敏感字段过滤用**白名单**（`scanLoginPublicDataKeys`）而非黑名单——payload 有
      三处写入方（`getLoginUUID` / `handleScanLogin` / `grantLogin`），黑名单对新增
      字段 fail-open
- [ ] 长轮询只回收自己注册的 channel（`removeQRCodeChanOwned`），避免匿名调用方
      连了就断即可踢掉合法轮询方
- [ ] `respondStatus(nil)` 必须写出响应，不得产生空 200（会让前端状态机停摆）
- [ ] `loginWithAuthCode` 兑换成功后吊销 `poll_secret`
- [ ] 前端在 `auth_code` 缺失时回退重新申请二维码，不得用 undefined 调兑换接口
- [ ] `mint` / `matches` / `delete` 有**真实行为测试**（注入内存 store），覆盖
      对/错/缺失密钥与存储故障 fail-closed
- [ ] 新增拒绝路径全部走 `respondUserError` + 已注册 errcode（无裸 `c.ResponseError`）
- [ ] `make i18n-extract-check` && `make i18n-lint` 通过
- [ ] `go build ./...` && `go vet ./...` 通过；`go test ./modules/user/... ./modules/qrcode/...` 通过
- [ ] `TestUserNoLegacyResponseError` 源码守卫仍通过

**octo-web**

- [ ] `login_vm.tsx` 保存并回传 `poll_secret`；uuid 轮换时 secret 同步轮换
- [ ] `pnpm lint` 与 `cd apps/web && pnpm test` 通过

**安全回归（手工验证脚本）**

- [ ] 攻击者用自己 mint 的 uuid、不带 secret 轮询，在受害者确认后**拿不到** `auth_code`
