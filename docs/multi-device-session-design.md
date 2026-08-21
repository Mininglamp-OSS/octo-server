# 多设备登录与会话管理改进草案

> 状态：**草案，待评审**。本文描述现状事实、目标模型与分阶段路线，不代表任何一条已实现。
> 第 6 节的待决问题需要产品 / 运维签字后，P2 才能开工。
>
> 基线：`main@235efc8`；WuKongIM 按 `.github/workflows/ci.yml` 固定的
> `wukongim/wukongim:v2.2.4-20260313` 核对。

## 0. TL;DR

1. octo 现在把「谁能多端在线、谁会被顶掉」这条**策略**焊死在了
   `execLogin` 的 `if flag == config.APP` 一行判断里，而 `flag` 是**客户端自己填的、服务端不校验的**。
   结果是策略按平台随机：Android 单会话、iOS 无限多端、Web/PC 多端，且这个矩阵从未被文档化。
2. 由此产生两个安全缺陷（D1/D2）：账号封禁 / 注销 / 改密时，**iOS 的 IM 长连接踢不掉**；
   任何客户端只要填一个非 `{0,1,2}` 的 flag，就能拿到一个不受单会话约束、不被「全部设备退出」覆盖的会话。
3. 目标不是把顶号规则改得更严，而是**机制统一、策略配置化**：会话身份从
   `(uid, device_flag)` 迁到 `(uid, device_id)`，并发上限做成按平台类别可配，
   顶号从隐式副作用变成显式策略。桌面端上线时只需往矩阵里加一行。

---

## 1. 现状：机制与策略焊死在一起

### 1.1 现状矩阵

| 平台 | 上报 flag | device_level | 同平台二次登录 | HTTP token | 设备锁 | 被 `QuitUserDevice(-1)` 覆盖 |
|---|---|---|---|---|---|---|
| Android | `0` | master | **顶掉前一个** | 撤销旧 token 后新签 | 生效 | 是 |
| iOS | `3` | slave | 不顶，可无限多端 | **复用同一个 token** | 不生效 | **否** |
| Web / octo-web | `1` | slave | 不顶 | 复用同一个 token | 不生效 | 是 |
| PC | `2` | slave | 不顶 | 复用同一个 token | 不生效 | 是 |
| 桌面端（规划中） | 未定 | — | — | — | — | — |

### 1.2 这个矩阵是怎么来的

- `modules/user/api.go:1793-1795`：`deviceLevel = Master` **当且仅当** `flag == config.APP`（0），其余一律 slave。
- `modules/user/api.go:1845-1874`：`flag == APP` 才撤销旧 token；否则走 else 分支，
  注释写的是「PC暂时不执行删除操作，因为PC可以同时登陆」——iOS 的 `flag=3` 是被顺带捞进这个分支的。
- `modules/user/api.go:1798`：设备锁的条件同样是 `flag == 0`。
- WuKongIM 侧顶号只有两个触发点，都以 `device_level=master` 为前提，且只在**同一个 device_flag 桶内**比较：
  - `internal/api/user.go:360-377`（`POST /user/token`）：master → 踢掉同 flag 的所有旧连接，10s 后关闭，reason `账号在其他设备上登录`。
  - `internal/user/handler/event_connect.go:124-170`（CONNECT）：master 踢同 flag 但 device_id 不同的旧连接；slave 只关同 device_id 的旧连接。

**结论**：并发会话策略不是设计出来的，是 `flag` 分桶 + 一行 if 的副产物。
`device_flag` 同时承担了三件互不相干的职责——展示分类、IM 连接分桶、**安全策略分区键**。第三项是问题的根源。

---

## 2. 已确认的缺陷

### D1 —「全部设备退出」覆盖不全（P0，安全）

WuKongIM 的 `device_quit` 收到 `device_flag=-1` 时只展开成 `{APP, WEB, PC}`
（`internal/api/user.go:80-86`），**flag=3 不在其中**。

octo 侧共 4 处依赖 `-1`：

| 调用点 | 触发场景 |
|---|---|
| `modules/user/session_revocation.go:165` | 改密、重置密码、禁用、注销、管理员删号（经 `finishCommittedUserSecurityMutation`） |
| `modules/user/api.go:3748` | `destroyAccount` 注销账号 |
| `modules/user/api_manager.go:1457` | 管理后台禁用/删除用户 |
| `modules/oidc/api.go:1354` | OIDC logout、sync 收到 `invalid_grant`（IdP 侧禁用/删号） |

影响：**账号已封禁，iOS 那条 IM 长连接仍然在线并继续收消息**，直到 App 自己重连。
接口全部返回成功，日志无异常，所以这个洞不会自己暴露。

HTTP 侧是否被覆盖取决于 session rollout floor：v3 `revoke`/`enforce` 模式走
`RevokeAll(uid)`（`pkg/auth/session_v3.go`），与 flag 无关；更早的模式走
`revokeDeviceTokens`（`modules/user/api_manager.go:775`），同样只遍历 `{APP, Web, PC}`。
**线上当前 floor 值需要运维核查后填入本节。**

### D2 — flag 由客户端决定且全链路无校验（P0，安全）

| 入口 | 取值方式 | 校验 |
|---|---|---|
| 账密登录 | `loginReq.Flag int` → `config.DeviceFlag(req.Flag)`（`api_usernamelogin.go:163`） | 无；`int→uint8` 截断（`-1`→`255`，`256`→`0`） |
| 邮箱登录 | `Flag uint8`（`api_emaillogin.go:104`） | 无 |
| 注册 | `registerReq.Flag uint8`（`api.go:4371`） | 无 |
| OIDC | `?flag=`（`modules/oidc/api.go:373-376`） | 仅 `0 <= n < 256` |
| 扫码登录 | `?flag=`，`0` 或解析失败 → Web（`api.go:2645-2650`） | 无；`int64→uint8` 截断 |
| WuKongIM `/user/token` | `UpdateTokenReq.Check()`（`internal/api/user.go:652-669`） | 只校验 uid / token，**不校验 DeviceFlag** |

flag **不参与鉴权**（`auth.TokenInfo.DeviceFlag` 只是 payload 字段，`pkg/auth` 中无任何基于它的判断），
所以这不是提权面。真正的风险是它是**撤销与顶号策略的分区键**：客户端选 flag 等于自己选
「受不受单会话约束、在不在 `-1` 的覆盖范围里」。

一次 `flag=7` 的登录会实打实产出：`uidtoken:7<uid>` 一个独立 bearer + WuKongIM 里一条
`(uid, 7)` device 行。这个会话不受 APP 单会话顶号、不受设备锁、不被 `-1` 覆盖。
IM 长连接连不上（官方 SDK 的 CONNECT deviceFlag 是硬编码的），所以它是一个
**纯 HTTP 的隐形会话**——REST 全通，只是收不到实时消息。

iOS 的 `flag=3` 就是这条路径的一个善意实例，它证明这条路在生产上是通的。

### D3 — iOS 无单会话约束、多端共享同一个 bearer（P1）

`flag=3` 落进 else 分支的连带后果：

- 多台 iPhone 登录同一账号 → `DeviceToken(uid, 3)` 复用，**共享同一个 token 字符串**。
  A 机调 `POST /v1/user/quit` 删掉 `token:<T>`，B 机立刻 401。
- 设备锁（`user.device_lock`）对 iOS 完全不生效。
- 两台 iPhone 都是 slave + 不同 device_id → WuKongIM CONNECT 不互踢，可无限并存。

### D4 —「登录设备管理」是空壳（P1，功能）

- `DELETE /v1/user/devices/:device_id`（`modules/user/api_device.go:34`）**只删 `device` 表一行**，
  不撤 HTTP token、不踢 IM 连接。
- `GET /v1/user/devices` 返回的是**登录历史**（`device` 表按 `last_login` 排序），不是活跃会话；
  「（本机）」只是把第一条标上，并非真的当前会话（`api_device.go:76-80`）。
- 对照 OWASP ASVS **V3.3.4**（用户须能查看并登出任意/全部活跃会话与设备），这条目前不满足。

好消息：真正的会话清单已经存在——v3 session index 是个 ZSET，成员就是
`{token, device_flag, device_id}`（`pkg/auth/session_v3.go:971`），缺的只是读接口和按 device_id 的撤销。

### D5 — 语义错位（P2，非功能）

- `device_flag` 表只 seed 了 `0/1/2`（`modules/user/sql/20220919000001_user_legacy01.sql:17-19`），
  flag=3 的 weight 走 `IFNULL(...,0)`，iOS 永远不会被选为「主设备」。
- `handleOnlineStatus` 用 `onlineStatus.DeviceFlag != config.APP` 判断「是不是 PC/Web」
  （`modules/user/webhook.go:57`），flag=3 会走进 PC/Web 分支；客户端只认 1/2 所以忽略，行为不炸但语义已错。
- `user_online` 唯一索引是 `(uid, device_flag)`，同一 flag 下的多个设备只有一行——桌面端若沿用 flag=1，
  在线状态将无法区分浏览器与桌面端。
- `pcQuit`（`modules/user/api_online.go:18-46`）一次踢掉 Web + PC，没有「只踢其中一个」的粒度。

---

## 3. 目标模型

### 3.1 四条原则

**P1. 机制统一：会话身份是 `(uid, device_id)`，不是 `(uid, device_flag)`。**
`device_flag` 降级为纯展示 / 路由提示，不再承担任何安全语义。

**P2. 策略配置化。** 并发上限与超限行为写在配置里、写进文档，不散落在 handler 的 if 里。
ASVS V3 明确要求文档写出「允许几个并发会话、达到上限时的行为」——这份文档目前不存在。

**P3. 不把「顶号」当安全手段。** OWASP ASVS V3 原文：

> "Blocking simultaneous sessions is no longer appropriate ... in most of these
> implementations, **the last authenticator wins, which is often the attacker**."

即：攻击者拿到密码登录 → 把真实用户顶下线 → 攻击者独占唯一会话。
顶号是可用性/授权策略，不是安全措施。**「手机要严」的正确实现是新设备登录需要额外授权
（octo 已有半成品：`DeviceLock` 的新设备验证流程），不是新设备自动顶掉旧设备。**

**P4. 看得见 + 踢得掉。** 对齐 ASVS V3.3.4（用户可查看并登出任意/全部会话）与
V3.3.3（改密后终止其他所有会话，且跨联邦登录生效）。后者 octo 已经有了
（`updatePasswordAndRevokeSessions`），前者没有。

### 3.2 概念

- **platform class**：策略维度，取代 `device_flag` 承担策略职责。初始三类：
  `mobile`（iOS + Android 同池）、`desktop`（PC + 桌面端）、`web`。
- **session**：一个 `(uid, device_id)` 对应一个独立 bearer + 一条 IM device 记录。
  同一 platform class 下的多个 session 互相独立，不共享 token。
- **device_id**：客户端上报的稳定标识（iOS `UIDevice getUUID`、Android `WKConstants.getDeviceID()`）。
  已经存在于登录请求和 v3 session index 中，但目前不参与任何策略判断。

### 3.3 目标策略矩阵（默认值，**待第 6 节拍板**）

| platform class | 并发上限 | 超限行为 | 新设备首次登录 |
|---|---|---|---|
| mobile | 1 | **拒绝新登录**，提示去踢旧的 | 二次验证（复用 DeviceLock） |
| desktop | 2 | 拒绝新登录 | 二次验证 |
| web | 2 | 踢最旧 | 不需要 |

超限行为默认选「拒绝」而非「自动踢最旧」，是 P3 那句 `last authenticator wins` 的直接推论：
要踢就让用户在当前设备上显式确认，不由服务端替他决定。

行业参照（详见附录）：企业微信有登录设备管理 + 强制退出 + 设备绑定；飞书「多端同登」由超管配置设备数上限；
Slack 的管理员可以「只登出 mobile app 或只登出 desktop app」；
WhatsApp / Signal 是 1 台主手机 + N 台配对设备（4 / 5）且长期不活跃自动解绑。

---

## 4. 分阶段路线

### P0 — `session-device-flag-containment`（止血）

**范围**：修 D1 + D2。纯服务端，不改任何顶号策略，不需要客户端发版。

1. 登录入口加 flag 白名单 `{0,1,2,3}`，未知值**拒绝**而非截断；修掉 `int→uint8` 的截断路径。
2. 「全部设备退出」在 octo 侧显式展开成白名单全集，逐个调 `QuitUserDevice(uid, flag)`，
   不再依赖 WuKongIM 的 `-1`。（WuKongIM 的 `deviceQuit` 对不存在的 device 行返回 200，
   见 `internal/api/user.go:80-90`，所以无条件 fan-out 是安全的。）
3. `revokeDeviceTokens` 的遍历集合与白名单收口到同一个常量。

**风险**：低。唯一的行为变化是「以前会被静默接受的畸形 flag 现在 400」——已核对，
在用客户端只发 `0/1/2/3`，管理后台登录服务端硬编码 `config.Web`（`api_manager.go:385`），
bot 的 `UpdateIMToken` 走 `config.APP` 且不经过用户登录入口。

详见 `.octospec/tasks/session-device-flag-containment/brief.md`。

### P1 — `session-inventory-and-revoke`（看得见 + 踢得掉）

**范围**：修 D4，补齐 ASVS V3.3.4。

1. 会话清单读接口：从 v3 session index 读 `{token, device_flag, device_id}`，
   联 `device` 表补设备名/型号/最后登录时间，返回活跃会话列表（标注当前会话）。
2. 按 `device_id` 撤销单个会话：撤 HTTP token + 踢对应 IM 连接。
3. `DELETE /v1/user/devices/:device_id` 从「删历史记录」改成「撤销该设备会话」，或明确拆成两个语义不同的接口。
4. 新设备 / 异地登录通知（`login_log` 已有数据，缺通知链路）。

**依赖**：v3 rollout floor 已到 `bounded` 或以上（session index 才有数据）。
**IM 侧缺口**：WuKongIM 对外的 kick API 只认 `device_flag`，按 `device_id` 踢单条连接需要往上游提
（`event_connect.go` 内部已有 device_id 概念，但没有对应的 HTTP 接口）。
在上游支持之前，P1 只能保证 HTTP 会话被撤销，IM 连接需等其重连才断——这个降级必须在验收里写明。

### P2 — `session-policy-matrix`（策略配置化 + 客户端归位）

**范围**：修 D3 + D5，落地第 3.3 节的矩阵。需要客户端配合发版。

1. 引入 platform class，`execLogin` 不再按 `flag == APP` 分支，改为查策略表。
2. iOS 归位（见 Q3）。
3. 桌面端 flag 分配（见 Q4）。
4. `device_flag` 表补齐、主设备权重与 `pcQuit` 语义随之修正。

---

## 5. 兼容性与灰度

- **老客户端必须继续工作**：`flag=3` 在白名单内长期保留；P2 的 iOS 归位需要新老两个值并存一段时间。
- **v3 rollout 依赖**：P1 依赖 session index 有数据，上线顺序必须排在 floor 推进之后；
  P0 无此依赖，可以先走。
- **桌面端**：若沿用 `flag=1`，将与浏览器共享同一个 bearer（一端退出另一端 401）、
  在线状态无法区分、且 `pcQuit` 会连坐——**建议在 P0 之后、桌面端提测之前先定 Q4**。

---

## 6. 待决问题（需签字）

| # | 问题 | 需要谁拍板 | 阻塞 |
|---|---|---|---|
| Q1 | 每个 platform class 的并发上限取值 | 产品 + 安全 | P2 |
| Q2 | 超限行为：拒绝新登录 / 踢最旧 / 按 class 分别配 | 产品 | P2 |
| Q3 | iOS 归位方式：改回 `flag=0` 与 Android 同池（需发版 + 兼容期），还是服务端把 `3` 纳入 mobile class（不需发版） | 客户端 + 服务端 | P2 |
| Q4 | 桌面端用 `2`(PC) 还是新分配一个 flag | 客户端 + 服务端 | 桌面端提测 |
| Q5 | 上限是否做成企业级管理员可配（对齐飞书「多端同登」） | 产品 | P2 |
| Q6 | 线上 session rollout floor 当前值 | 运维 | P1 排期、D1 影响面评估 |

---

## 附录：行业做法参照

| 产品 | 模型 | 上限 | 兜底手段 |
|---|---|---|---|
| 微信 | 主设备锚定 | 手机 1 台；PC 需手机扫码授权 | 手机退出 → PC 跟着退出 |
| WhatsApp | 主设备锚定 | 1 主机 + 4 companion | 主机 14 天不活跃自动解绑 companion |
| Signal | 主设备锚定 | 1 主机 + 5 linked device | 45 天不活跃自动解绑 |
| Telegram | 对称会话 | 不限 | 活跃会话清单 + 终止单个/全部其他会话 + 不活跃自动终止 |
| Slack | 对称会话 | 不限 | 用户「登出其他所有会话」；管理员可**只登出 mobile 或只登出 desktop** |
| 企业微信 | 策略可配 | 二手资料称最多 2 台（未取到官方原文） | 管理后台登录设备管理 + 强制退出 + 设备绑定 |
| 飞书 | 策略可配 | 超管可配设备数上限 | 「多端同登」设置（官方页面被网络策略挡，未取到具体数值） |
| 钉钉 | 对称会话 | 较宽松 | — |

参考：
- OWASP ASVS 4.0 V3 Session Management —
  <https://github.com/OWASP/ASVS/blob/master/4.0/en/0x12-V3-Session-management.md>
  （V3.3.2 重认证周期、V3.3.3 改密后终止其他会话、V3.3.4 会话清单与登出）
- Signal Linked Devices — <https://support.signal.org/hc/en-us/articles/360007320551-Linked-Devices>
- Slack 管理员登出成员 — <https://slack.com/help/articles/360041717053-Sign-members-out-of-Slack>
- 飞书 管理员设置多端同登 — <https://www.feishu.cn/hc/zh-CN/articles/124160085067>
