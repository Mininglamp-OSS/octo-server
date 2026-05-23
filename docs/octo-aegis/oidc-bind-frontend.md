# OIDC 自助绑定 — 前端对接文档

适用范围：dmwork OIDC 登录流程中 autolink（issuer+sub / email / phone 三种）全部失败时的自助绑定页面。

后端版本：参见 PR #73（feat/oidc-bind-self-service）。  
功能开关：`OCTO_OIDC_BIND_ENABLED=true` 时本流程生效；否则 callback 走旧"失败"路径（前端无需关心）。

---

## 1. 触发条件 & 入口

用户从 dmwork 登录页发起 OIDC：

```
GET /v1/auth/oidc/{provider}/authorize?authcode=<前端短码>&return_to=<回跳地址>&flag=<设备标志>
```

参数与现有 OIDC 登录一致；前端不需要改动。

IdP 回跳 `/callback` 后，后端尝试 autolink：

- **成功**：写 ThirdAuthcode key `<authcode>` = `LoginRespJSON`，前端短码轮询即拿到 token，正常进入主界面。
- **失败且 `Bind.Enabled=true` 且 issuer 在 allowlist 内**：后端签发 `bind_token`（32 字节，5min TTL，单次消费），302 跳转到：

  ```
  <OCTO_OIDC_BIND_REDIRECT_BASE>?token=<bind_token>&authcode=<原 authcode>&return_to=<清洗后的 return_to>
  ```

  例：`https://app.example.com/oidc/bind?token=AbCd...XyZ&authcode=front-123&return_to=/home`

- **失败但 bind 不接管**（flag off / issuer 不在 allowlist / Issue 内部错）：等价于旧路径，ThirdAuthcode 写 `"0"`，前端轮询拿到 `"0"` 即视为登录失败。

## 2. bind 页面应做的事

### 2.1 解析 URL 参数

| 参数        | 含义                                                                                  |
| ----------- | ------------------------------------------------------------------------------------- |
| `token`     | bind_token，**所有 /bind/* 请求都用它**。**视为凭据**（同效力于一次密码登录会话）   |
| `authcode`  | 原 dmwork 短码，confirm 成功后用作 ThirdAuthcode key 让原发起设备拿到 LoginRespJSON |
| `return_to` | 用户原本想去的页面，confirm 完成后前端自行 navigate                                  |

### 2.2 token 安全处理（**必须**）

bind_token 在 5 分钟 TTL 内：

- 能调 `/bind/info` 拿到脱敏 claims（社工攻击面）
- 通过 verify 后能调 `/bind/confirm` 拿到 `login_resp`（**等同会话签发**）

因此前端**必须**：

1. **绝不**打到任何遥测、SDK（Sentry / GA / 神策等）、`console.log`
2. **绝不**作为 React/Vue 全局 store 的可观察字段
3. 拿到 token 后立即 `history.replaceState({}, '', location.pathname)` 清除 URL，避免：
   - 浏览器历史记录留存
   - 用户截图泄漏
   - 被反代/前端日志采集器记下 Referer
4. 内部用 closure 或 React `useRef` 持有，组件树流转传引用而不是全局变量

> 后端已经在所有日志/审计里用 `subHash(token)` 替代明文。前端是这条防线的最后一公里。

### 2.3 时序

```
[B 设备: bind 页面]
  │
  │ 1. GET /bind/info?token=XXX
  ▼ ─→ { masked_email, masked_phone, name, methods[], support_contact }
  │
  │ 2. 用户选 methods[0] (password / sms_otp)
  │
  ├─→ password 路径:
  │    │ POST /bind/verify/password { token, identifier, password }
  │    ▼ ─→ { status: "verified" } | 401/429/410
  │
  └─→ sms_otp 路径:
       │ POST /bind/verify/otp/send { token }
       │ ▼ ─→ { status: "sent" } | 400/429/410
       │ 用户输入 OTP
       │ POST /bind/verify/otp/check { token, code }
       ▼ ─→ { status: "verified" } | 401/429/410
  │
  │ 3. POST /bind/confirm { token }
  ▼ ─→ { status: "ok", login_resp: "<JSON 字符串>", uid: "..." }
  │
  │ 4. 同步:
  │    - 当前设备(B)直接用 login_resp 进入主界面
  │    - 原发起设备(A)的轮询 GET /thirdauthcode/<authcode> 也会拿到同样的 login_resp
  │      (由 confirm 端点回填,FR-6.3 跨设备流转)
  │
  │ 5. navigate 到 return_to
  ▼
```

`login_resp` 是 **JSON-encoded string**（不是嵌套 JSON 对象）。前端要先 `JSON.parse(resp.login_resp)` 拿到真正的登录响应体（与原 OIDC `LoginRespJSON` 同 schema）。

## 3. 端点 schema

所有端点路径 prefix 为 `/v1/auth/oidc/{provider}`（如 `/v1/auth/oidc/aegis`）。所有 4xx/5xx 响应体均为 `{"msg": "<英文短描述>"}`。

### 3.1 GET `/bind/info?token=<bind_token>`

返回脱敏身份信息 + 当前可用的二次验证方法。

**200** 响应：

```json
{
  "masked_email": "a***@example.com",
  "masked_phone": "****5678",
  "name": "Alice",
  "methods": ["password", "sms_otp"],
  "support_contact": "support@example.com"
}
```

- `masked_email` / `masked_phone` **始终出现**在响应中；IdP 没下发或不可信时序列化为 `""`（空字符串），前端按 truthy 判断隐藏对应行即可（GH#148）
- `methods` 来自后端 `cfg.Methods ∩ claims 支持`：claims 无 verified phone 时不会出现 `sms_otp`
- `support_contact` 来自 env，可空，用于 FR-7 "联系管理员"兜底

错误码：

| 码  | 含义                                  | 前端行为                          |
| --- | ------------------------------------- | --------------------------------- |
| 400 | token 格式非法                        | 引导用户重新走 OIDC 登录          |
| 410 | token 已过期 / 已消费 / 未知          | 同上                              |
| 500 | 内部错误                              | 显示 "请稍后重试"                 |
| 503 | bind service 未就绪（Discovery 失败） | 显示 "服务暂不可用" + 重试按钮    |

### 3.2 POST `/bind/verify/password`

请求体：

```json
{ "token": "<bind_token>", "identifier": "<dmwork 用户名>", "password": "<明文密码>" }
```

**200**：`{"status": "verified"}` — 已通过验证，可进入 confirm 步骤。

错误码：

| 码  | 含义                                                                                             |
| --- | ------------------------------------------------------------------------------------------------ |
| 400 | 入参非法 / 该 method 已被运维关闭                                                                |
| 401 | 用户名或密码错（与"未知用户" / "账号被运维封禁" / "账号在注销冷静期"统一兜底，反账号枚举 SR-6） |
| 409 | session 状态已 verified 或更高（重复 verify 已被 CAS 拒绝）                                      |
| 410 | token 已过期 / 未知                                                                              |
| 429 | 验证尝试超 `VerifyMax`、uid 维度超 `UIDFailPerDay`、或 dmwork 该账号触发了底层 `loginGuard` 锁定 |
| 500 | 内部错误                                                                                         |
| 503 | bind service 未就绪                                                                              |

> 429 的三种来源对用户体验等价（都应当提示"请稍后重试"），但 dashboard 上能从 `oidc_bind_request_total{endpoint="verify_password",result="rate_limited"}` 区分。前端不需要再细分。

### 3.3 POST `/bind/verify/otp/send`

请求体：`{ "token": "<bind_token>" }`

后端用 OIDC claims 里的 `phone_number` + `phone_number_verified` 决定发到哪个号码；前端**不能**传 phone。

**200**：`{"status": "sent"}`

错误码：

| 码  | 含义                                                     |
| --- | -------------------------------------------------------- |
| 400 | token 非法 / claims 无 verified phone / sms_otp 被关闭   |
| 410 | token 已过期 / 未知                                      |
| 429 | 发送次数超 `OTPSendMax`                                  |
| 500 | SMS provider 异常                                        |
| 503 | bind service 未就绪                                      |

### 3.4 POST `/bind/verify/otp/check`

请求体：`{ "token": "<bind_token>", "code": "<6 位 OTP>" }`

**200**：`{"status": "verified"}` — 已通过，可进入 confirm 步骤。

错误码语义与 `/bind/verify/password` 同构（401 不区分"phone 不匹配"和"OTP 错"以防枚举）。

### 3.5 POST `/bind/confirm`

请求体：`{ "token": "<bind_token>" }`

**200**：

```json
{
  "status": "ok",
  "login_resp": "<JSON.stringified LoginResp>",
  "uid": "u-12345"
}
```

- `login_resp` 是字符串，**前端需 `JSON.parse` 后**才能拿到 OAuth token / 用户信息等字段
- `uid` 是绑定到的 dmwork 用户 id

错误码：

| 码  | 含义                                                                          | 前端行为                                       |
| --- | ----------------------------------------------------------------------------- | ---------------------------------------------- |
| 400 | token 非法                                                                    | 引导重新登录                                   |
| 401 | session 还在 `issued` 状态（未完成 verify 步骤就 confirm）                   | 回到 verify 步骤                              |
| 409 | identity 已绑定（含 IssueSession 失败后重试命中 uk_uid_issuer 的常见场景）   | 文案："已绑定，请重新走 OIDC 登录"            |
| 410 | token 已过期 / 未知                                                          | 引导重新登录                                  |
| 429 | confirm 次数超 `ConfirmMax`                                                  | 提示稍后重试                                   |
| 500 | 内部错误（DB / session 签发异常）                                            | 提示重试 + 联系管理员                          |
| 503 | bind service 未就绪                                                          | 显示 "服务暂不可用"                            |

### 3.6 哪些 dmwork 账号能被绑定

- `is_destroy=0`（既不在冷静期也未注销）
- `status<>0`（未被运维封禁/停用）

不满足上述条件的账号在 locator 层就被过滤掉了：
- 密码路径：`username` 命中但账号停用 → handler 返 401，文案与"账号或密码错误"一致（SR-6 反枚举）
- SMS 路径：phone 命中的所有候选 uid 都被过滤 → handler 返 401，引导走 FR-7 联系管理员兜底

`/bind/confirm` 在 `identity.Insert` 之前还会再调一次同样的可绑定性检查，覆盖"verify 通过后账号才被运维 disable 或用户自助 destroy"的 TOCTOU 窗口；不可绑定时同样返 401。

> 历史问题：早期版本 locator 只过滤 `is_destroy`，停用账号也能通过 verify；confirm 时 `IssueSession` 才拒绝，但 `user_oidc_identity` 行已写入，导致该用户后续 OIDC 登录持续失败需要人工 DB 清理。已修复。

### 3.7 bind_token TTL 是绝对截止

- token 在 `/authorize → callback → 签发` 那一刻起算 5 分钟（`OCTO_OIDC_BIND_TOKEN_TTL_SEC`）
- verify 通过**不会**续 TTL；如果用户在 token 快过期时才点 verify，剩下的时间就是 confirm 必须完成的窗口
- 已过期的 token 走任何端点都会返 410；前端拿到 410 后应当引导用户重新走 OIDC 登录
- `masked_email` / `masked_phone` 字段：claims 无对应值时序列化为 `""`(空字符串,字段**始终存在**),存在时形如 `"a***@example.com"` / `"****5678"`;前端按 truthy 判断隐藏对应行(GH#148)

## 4. 关键约束 / 注意事项

1. **同一 bind_token 只能 confirm 成功一次**（SR-1 单次消费）。confirm 失败可以重试（直到 `ConfirmMax`），重试也是用同一个 token。
2. **identity 已写但 IssueSession 失败的恢复路径**：用户首次 confirm 返 5xx，重试 confirm 返 409（"identity already bound; sign in again via OIDC to continue"）—— 此时引导用户回 OIDC 登录入口，他们的 OIDC 登录会通过 `(issuer, sub)` autolink 直接成功。
3. **状态机方向**：`issued → verified → (consumed on confirm)`。不存在回退到 `issued` 的路径。verify 通过后再调 verify 端点会返 409。
4. **跨设备流转**：当前 bind 页面所在的设备（B）会立即拿到 `login_resp`；同时原发起设备（A）的 ThirdAuthcode 轮询也会拿到。两台设备最终都登入同一账号。
5. **失败时的 ThirdAuthcode**：用户中途放弃 bind 页面，原发起设备 A 的轮询会等到 5min TTL 才知道失败。这是已知体验问题；产品决定是否在 bind 页面加"放弃绑定" → 调一个清场端点（**未提供，需后端补 follow-up**）。
6. **`return_to` 二次校验**：后端会再校验一次 host 白名单，非法时直接落空。前端拿到的 `return_to` 一定是合法的，但仍建议前端做基础 sanity check。

## 5. 已知限制 (follow-up)

- **bind_token 在 URL 中传递**：浏览器历史/Referer/反代日志可能截获。已通过 (a) 后端 token 哈希化记录、(b) 本文档要求前端 `replaceState` 清除 URL 做缓解。彻底方案是 verify 成功后签发独立的 confirm secret，本期未做，列为 follow-up。
- 端点级 IP rate-limit 后续 PR 添加，当前依赖 per-token 计数器（5 次 verify、3 次 confirm）+ uid 维度 10 次/天兜底。

## 6. 联调 checklist

- [ ] bind 页面入口拿到三个参数：`token` / `authcode` / `return_to`
- [ ] 立即 `history.replaceState` 清 URL
- [ ] `/bind/info` 200 → 渲染脱敏信息 + methods 按钮
- [ ] 选 password → POST `/bind/verify/password` → 200 → 跳 confirm 步骤
- [ ] 选 sms → POST `/bind/verify/otp/send` → 显示验证码输入框 → POST `/bind/verify/otp/check` → 200
- [ ] POST `/bind/confirm` → 拿 `login_resp` → `JSON.parse` → 写入登录态 → 跳 `return_to`
- [ ] 各错误码文案：400/401/409/410/429/500/503 都给出明确指引
- [ ] 拒绝把 token 打到任何遥测 / log / 全局 store

---

后端联系人：见 PR #73 author。问题请走 OIDC bind 的 Linear ticket 而非 Github issue（含敏感配置）。
