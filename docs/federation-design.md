# octo 跨实例联邦设计：两家自部署企业互信通信 / 跨实例外部群

**status: DRAFT — 待拍板**
**方案：A 单边托管（single-homed）** — 外部群寄居在其中一方实例上（下称 **Host / A**；对端下称 **Peer / B**）
**代码基线：** octo-server HEAD `a61b5411`（branch `feat/thread-peruser-visibility`）
**架构总览：** 面向评审者与新加入者的背景 / 架构图 / 端到端场景见 `federation-architecture.md`。本文档为详细设计与决策记录。
**证据说明：** 本草案的 file:line 结论来自一轮独立代码勘察，撰写时未逐条重开文件复核。凡标注「未验证 / 需实测」处，执行前必须先验证，并 drift check 对齐当时 HEAD。

---

## 0. 范围

**要**：可评审的架构设计，说明两家独立部署的 octo 实例如何建立互信、让双方用户在一个共享外部群里协作。
**不要**：实现代码。写码票在设计拍板 + 人工理解节点通过后再拆。
**已定方向（不重新论证）**：方案 A 单边托管。放弃方案 B（双向复制）理由已验证。
**架构硬约束**：联邦网关必须抽成独立模块 `modules/federation`，不把单边托管假设焊死进业务代码。

---

## 1. 核心设计立场

**联邦 = 复用现有 `is_external` 外部群语义骨架 + 把身份来源从「同实例另一 Space」替换为「对端实例」。** 远端用户在 Host 落一行**普通形态、但被硬约束为「不可认证」的本地 32 位 uid 影子账号**；联邦来源记在旁路表 + 现有 `is_external` 标记里，**绝不把联邦身份编进 uid 字符串**（规避 `@` 毒性与被回滚的列宽放大）。WuKongIM 当哑消息总线（目标零改动，但**发送方伪装能力必须先实测**，见 §5.4）。跨实例信任、授权、消息中继、可靠投递成本全部搬到 octo-server 侧的 `modules/federation`。


---

## 2. 与现有 `is_external` 机制的复用边界

| 现有能力 | 证据（file:line，来自前置勘察，未在本轮重核） | 联邦处置 |
|---|---|---|
| `is_external=1` 成员标记 + 外部角标渲染 | `docs/external-group-design.md`（v1.1，PR #1167） | **直接复用** |
| `allow_external` 安全阀 | 同上 | **直接复用**（联邦总开关前置） |
| 会话过滤放行 | `space_filter.go` | **直接复用** |
| 搜索放行 | `shouldIncludeGroupForSpace` | **直接复用** |
| 退群自动恢复 | `is_external_group` 逻辑 | **直接复用** |
| `source_space_id` 来源标识 | external-group-design.md | **扩展**：新增平行维度 `source_peer_instance_id`（落哪张成员表 / 与 `source_space_id` 共存 / 唯一键，**需实测**，见 §6） |
| 外部标记缓存 | `external_marker_cache.go:162-168` | **扩展 + 修 bug**（当前静默丢标记，见 §7） |
| IM 成员名单反查 datasource | `modules/webhook/api.go:141`、`api_datasource.go:84-113`、`modules/group/1module.go:66-78` `GetSubscribableMemberUIDs()` | **直接复用** |
| `IMAddSubscriber` | `octo-lib/config/msg.go:298`（只传 uid、无握手） | **直接复用** |
| webhook HMAC-SHA256 签名 | `modules/webhook/hmac.go:30-58`（fail-closed） | **复用为互信起步原语**（仅认证，非授权，见 §4.2） |
| `user_oidc_identity` 形状 | `modules/oidc/sql/20260427000002:5-24` | **复用形状**新建 `federation_identity` |
| `modules/federation` 网关 / `federation_peer` 信任表 / 授权层 / 中继协议 / outbox 可靠投递 / 附件异步拉取 / 生命周期事件 | `federat` 全零命中 | **全部新建** |

---

## 3. Q1 · 身份

### 3.1 影子 uid 形态
远端用户在 Host 上映射到**一个正常生成的本地 32 位 hex uid**（`pkg/util/string.go:15` 同款），**无 namespace 限定符**。理由：`@` 是毒（`octo-lib/common/msg.go:172-174` `IsFakeChannel`、`:157-169` `from@to` 拼 DM，9 处非测试调用点）；核心 `user` 表列宽 `uid VARCHAR(40)` 焊死（约 40 migration + 索引，现用 32 hex，仅余 8 字符）。影子 uid 为普通本地行 → 无声穿过 `QueryByUIDs`、DM 逻辑、全部索引。

### 3.2 `federation_identity`（复用 `user_oidc_identity` 形状）
```sql
CREATE TABLE federation_identity (
  id               BIGINT AUTO_INCREMENT PRIMARY KEY,
  peer_instance_id VARCHAR(64)  NOT NULL,
  remote_uid       VARCHAR(64)  NOT NULL,
  local_shadow_uid VARCHAR(40)  NOT NULL,
  status           TINYINT      NOT NULL DEFAULT 1,   -- 1=active 2=suspended 3=revoked
  lease_expires_at DATETIME     NULL,
  display_name     VARCHAR(128) NULL,                 -- 投影自对端，随 profile 事件更新（§8.3）
  avatar_url       VARCHAR(255) NULL,
  created_at       DATETIME     NOT NULL,
  updated_at       DATETIME     NOT NULL,
  UNIQUE KEY uk_peer_remote (peer_instance_id, remote_uid),
  UNIQUE KEY uk_shadow (local_shadow_uid)
) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
```
🔴 **影子身份的创建时机（P0，与 §4.2 授权闸门对齐）**：影子身份**不由入站消息创建**。§4.2 step 2 要求 `(peer, remote_uid)` 在消息被接受前**已存在且 active**，故创建只能发生在**成员开通（provisioning）**阶段：

- **开通路径（唯一创建入口）**：Host 与 peer 建立/变更某个联邦群的成员关系时，由 peer 推送 `member_add` 生命周期事件（§8.1）或管理员显式批准 → Host 在该事件的处理中创建 `federation_identity` 行（+ §6.1 的 `user`/`group_member` 落地）。这是**唯一**允许 INSERT 新影子的路径。
- **入站消息路径只做「解析 / 刷新」，不创建**：入站按 `(peer, remote_uid)` **查**已有行；查不到 = 未开通 → 按 §4.2 **fail-closed 拒收**（记审计，不静默建号）。允许的写操作仅限刷新既有行的 `display_name`/`avatar_url`/`last_seen_at`。
  ⚠️ 若允许入站创建，等于「谁能签出 HMAC 就能在 Host 上凭空造账号并进群」，step 2 的身份绑定校验将退化为永真，授权模型失效。
- **并发竞态（仍需处理，但只在开通路径）**：同一 `(peer,remote_uid)` 的开通事件可能重复/并发投递，撞 `uk_peer_remote` → 500。**开通路径必须用 DB 级 upsert（`INSERT ... ON DUPLICATE KEY UPDATE` 取回既有行）或 per-key 分布式锁**收敛，禁止「先 SELECT 再 INSERT」裸竞态。入站路径为纯读 + 字段刷新，无此竞态。
- 📌 Phase 1a 需交付该开通路径（否则联邦群无法拉入任何远端成员）；[api] 测试：未开通身份的入站消息被拒且不产生 `federation_identity` 行。

### 3.3 🔴 不可认证影子（Unauthenticatable Federation Shadow）—— 一等约束
设计评审指出：仅封「好友/推荐/搜索」远远不够。影子是普通本地行意味着**登录、token 铸发、密码/OIDC 绑定、设备注册、通知、@提及、DM 发起、审计、管理后台、导入导出、删除任务、权限缓存**都可能把它当真人。

**约束（spec 级强制）**：影子账号带一个**统一可判定标志**（如 `user.federation_shadow=1`，落 user 表，**列位置/迁移兼容需实测**），并要求执行者**逐路径审计**下表，每条给出「拒绝 / 降级 / 放行」处置，缺一条即 STOP：

| 路径 | 期望处置 |
|---|---|
| 登录 / 铸 IM/Web token | **拒绝**（影子永不登录，§5.1 依赖此） |
| 密码 / OIDC 绑定 | **拒绝** |
| 加好友 / 好友推荐 / 通讯录 | **排除** |
| 全局搜索 / 用户目录 | **排除**（仅联邦群内可见） |
| 发起 DM / 被 DM | **拒绝**（仅所属联邦群内可见；见 §16.6 推荐值） |
| @提及 | 仅群内成员范围 |
| 管理后台 / 导入导出 | 标注为联邦来源，不可当本地成员导出 |
| 删除 / GDPR 任务 | 随联邦生命周期（§8），不进本地注销流程 |
| 权限缓存 | 缓存键须含 shadow 判定，吊销时失效（§8.4） |

---

## 4. Q2 · 互信 + 授权

### 4.1 认证选型：HMAC 共享密钥起步
| 候选 | 结论 |
|---|---|
| HMAC 共享密钥（复用 `webhook/hmac.go`，fail-closed，零新基建） | **MVP 选它** |
| JWT+JWKS（基建已删 `modules/bot_provision/jwt.go`） | 不作起步 |
| mTLS（无 CA/轮换基建） | 后续 |
理由：起步是**少量、带合同的点对点配对**，非开放联邦。抽象 `FederationTrust` 接口（`Sign/Verify`）留 JWT/mTLS 迁移位，不动业务代码。

### 4.2 🔴 授权 ≠ 认证（P0）
HMAC 只证明「请求来自持共享密钥的一方」，**不证明**：① `remote_uid` 当前确属该 peer 且 active；② 该 uid 仍是目标群成员；③ 该 peer 被授权向该 `channel_ref` 写入。**必须显式三段校验，缺任一 fail-closed**：
1. **peer 认证**：HMAC 验签通过（含 §4.4 防重放）。
2. **身份绑定**：`federation_identity` 中 `(peer, remote_uid)` 存在且 `status=active` 且租约未过期。
3. **频道授权**：`channel_ref` 解析出的本地群 `allow_external=1`、且该群已与该 peer 建立联邦关系（新表 `federation_channel`，§4.5）、且该 remote 影子是该群成员。
> 🔴 **禁止**依赖 `pkg/space/middleware.go:112-115`「无 space_id → 跳过校验」的口子；联邦入站必须显式携带并强校验频道/租户归属（[api] 测试：缺归属的入站请求被拒）。

### 4.3 `federation_peer`
```sql
CREATE TABLE federation_peer (
  peer_instance_id VARCHAR(64) PRIMARY KEY,
  display_name     VARCHAR(128) NOT NULL,      -- 对端组织名（外部标记用，§7）
  base_url         VARCHAR(255) NOT NULL,      -- 对端网关入口（见 §4.6 SSRF 约束）
  hmac_key_id      VARCHAR(32)  NOT NULL,      -- 当前密钥 ID（envelope 回填，§4.4）
  hmac_secret_enc  VARBINARY(512) NOT NULL,    -- 加密存储
  hmac_key_id_prev VARCHAR(32)  NULL,          -- 轮换窗口旧密钥 ID
  hmac_secret_prev VARBINARY(512) NULL,
  status           TINYINT NOT NULL DEFAULT 1, -- 1=active 2=suspended 3=revoked
  created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
```

### 4.4 🔴 密钥管理 + 防重放（P0/P1）
- **规范化签名输入**：签名覆盖 `key_id + method + path + sha256(body) + timestamp + nonce`（明确定义拼接顺序，避免歧义）。
- **时间窗**：`timestamp` 超出 ±N 分钟拒绝；**nonce** 在窗口内去重（Redis SETNX，保留期 ≥ 时间窗），杜绝重放。区分「网络重试（同 idempotency_key，§5.5 幂等吸收）」与「重放攻击（nonce 撞）」。
- **轮换**：envelope 带 `key_id`，接收方据此选 current/prev 密钥；双密钥窗口内两者都验，窗口结束废弃 prev。
- **密钥托管**：`hmac_secret_enc` 依赖 KMS / 应用层加密密钥 + 版本管理；**带外人工配对交换，密钥永不回显/入日志/进文档**。删除 peer → 立即 fail-closed，同时失效积压重试队列、连接池、权限缓存。

### 4.5 `federation_channel`（联邦群 ↔ peer 的权威映射）
🔴 设计评审修正：`channel_ref` 不能是自由字符串。新表把**本地群**与 **peer + 对端频道 ID** 双向绑定，`channel_ref` 用**不可猜测的联邦频道 token**（非本地 group id 直出），Host 为解析权威方：
```sql
CREATE TABLE federation_channel (
  federation_channel_token VARCHAR(64) PRIMARY KEY,  -- 不可猜测
  local_group_id           VARCHAR(64) NOT NULL,
  peer_instance_id         VARCHAR(64) NOT NULL,
  peer_channel_id          VARCHAR(64) NOT NULL,
  status                   TINYINT NOT NULL DEFAULT 1,
  created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
  UNIQUE KEY uk_local_peer (local_group_id, peer_instance_id)
);
```

### 4.6 🔴 `base_url` 的 SSRF / 路由劫持面（P1）
管理员配置的 peer URL 未约束会让网关向内网/错主机推数据。**必须**：仅 https；解析 IP 后拒绝私网/环回/元数据段（RFC1918、169.254/16 等）；禁跟随重定向；建议证书 pin 或固定 CA。HMAC 不防错目的地。

---

## 5. Q3 · 消息流

### 5.1 分岔口（2.3(b)）：服务端中继（选定）
B 客户端只连 **B 自己的 IM**；跨实例由两侧 `modules/federation` 服务端中继。A 的 IM 把影子当**从不连接的订阅者**（2.2：`IMAddSubscriber` 无握手、token 只在真实登录铸）。**不选** Option 2（Host 给 B 用户铸 IM token）——那是给管不着的人铸凭证、把 IM 暴露给不受信客户端，攻击面巨大。

### 5.2 流转（文本，单条，B→A 入站）
```
B 用户发消息 → B server 识别频道为联邦 → B 网关 POST A 网关（HMAC 签名 + nonce）
 → A 网关：§4.2 三段授权 → 解析 remote_uid→shadow_uid（查既有已开通身份，查不到即 fail-closed 拒收；§3.2）
 → A 写 outbox（§5.5）→ 以影子 uid 服务端发送写入 A 的 IM 频道 → IM 广播群内订阅者
```
A→B 反向为对称链路（Phase 1b，见 §12）。

### 5.3 中继 envelope
```json
{ "key_id":"<签名密钥ID>", "peer_instance_id":"<发送方>", "federation_channel_token":"<§4.5>",
  "remote_uid":"<发送方本地 uid>", "msg_type":"text|image|file|edit|revoke|profile|lifecycle",
  "payload":{...}, "idempotency_key":"<对端消息唯一键>", "nonce":"<防重放>", "sent_at":"<RFC3339>" }
```
🔴 `sent_at` 来自对端，**不可信作排序权威**；Host 以本地接收序 + 自身时钟为准，`sent_at` 仅供展示，避免时钟偏移乱序。

### 5.4 ✅ Phase 0 spike（已执行，四条断言全 PASS）
**状态：已验证通过**（IM 版本 `wukongim/wukongim:v2.2.4-20260313`，commit `94b06a4`；结论仅对该版本成立）。

| 断言 | 结果 | 证据 |
|---|---|---|
| A1 未登录合成 uid 可加入频道 | **PASS** | `POST /channel/subscriber_add` → `{"status":200}` |
| A2 IM 订阅者名单含它（且后续操作不冲掉） | **PASS** | `GET /cluster/channels/{ch}/2/subscribers` 含该 uid |
| A3 🔴 以该 uid 作 `from_uid` 服务端发送被接受 | **PASS** | `POST /message/send` → 200 + message_id；真实用户端离线补拉与在线实时均可见 |
| A4 出站消息可捕获 | **PASS** | gRPC `msg.notify` → `modules/webhook/api.go:208 → :374 → :263`；落库 `message2` |

→ **方案 A 的 IM 地基成立。** 但 spike 同时暴露三个原设计未覆盖的实现项，根因同一：**影子 uid 只存在于 IM 订阅表，octo 侧不存在**。见 §6.1。
仅测「真实用户向含合成 uid 的频道发消息不报错」**不够**。**必须同时实测**「以合成 uid 作为 `from_uid` 走服务端 API 发送（伪装发送者）IM 是否接受」——这才是 Option 1 的真正前提。
实测断言（全绿才进 Phase 1）：
1. `IMAddSubscriber` 把从不登录的合成 uid 加入频道 **成功**；
2. `getSubscribers` datasource 返回列表**含**该 uid；
3. **以该 uid 为 from_uid 服务端发消息，IM 接受并广播、不校验发送者 token**；
4. A 能**捕获**群内消息作为出站事件（§5.5 前提）。
任一失败 → **STOP 上报**，Option 1 前提崩，回设计（评估 Option 2 或消息落库不进 IM 的降级）。

### 5.5 🔴 出站捕获 + 可靠投递（outbox，P0/P1）
- ✅ **出站捕获（已实测确认，见 §5.4 A4）**：WuKongIM 经 **gRPC** 推事件到 octo-server（`wk.yaml` 的 `webhook.grpcAddr`，仅 docker 网内）。链路：`modules/webhook/api.go:208` `SendWebhook` → `:368` `handleEvent` → `:374-380` 分发 → `:263` `handleMessageNotify`。
  🔴 **中继必须挂 `EventMsgNotify = "msg.notify"`**（`modules/webhook/common.go:39`）——它对频道内**每条**消息触发一次，与收件人在线/离线无关。**不要挂 `msg.offline`**（`common.go:33`），那条仅在有离线收件人时才来。
  HTTP 等价入口（备选，HMAC-SHA256 签名）：`api.go:141` `POST /v1/webhook`、`:143` `/v2/webhook`、`:147` `/v1/webhook/message/notify`。
  ⚠️ 接入时注意别与已挂在同一消息上的 `modules/bot_api/obo_fanout.go:214` OBO fan-out 链相互干扰。
- **双写难题**：「写 IM」与「记幂等/outbox」非同一事务 → 崩溃窗口造成重复或永久丢消息。**采用 outbox 模式**：入站先落 `federation_inbox`（`(peer, idempotency_key)` 唯一，事务内），再由 worker 幂等地推 IM；出站先落 `federation_outbox`，worker 带 ACK/重试/死信地投递 peer。定义：保留期、最大重试、DLQ、崩溃恢复（重启后扫未 ACK）。
- **IM 侧去重（P2）**：即便 DB 幂等，网络抖动重试写 IM 仍可能双气泡；需确认 IM 是否支持外置 client-msg-no 去重（**需实测**），否则 worker 必须保证「已确认写入 IM」的状态持久化后才不重发。
- 🔴 **回环终止规则（P0，spec 级强制）**：`msg.notify` 对频道内**每条**消息触发，**包含中继 worker 自己代影子 uid 写入的入站消息**。若不设终止规则，Phase 1b 双向打通后立即形成 A→B→A 无限回环 / 重复投递。
  **规则**：出站中继在入队 `federation_outbox` 前**必须**丢弃满足任一条件的消息：
  1. 该消息行的 `source_peer_instance_id` **非空且等于目标 peer** —— 即「从这个 peer 收来的，绝不再发回这个 peer」（消除 A→B→A 直接回环）；
  2. 该消息的发送者 uid 是**影子 uid**（`federation_identity` 命中）—— 影子发言在语义上属于其归属 peer，Host 不为其代理出站（消除多 peer 拓扑下的间接扩散）。
  ⚠️ **幂等键不能替代回环终止**：`(peer, idempotency_key)` 只能压掉「同一条消息被重复处理」，而回环里每一跳都是**新** message id + 新 idempotency_key，幂等表看它们是不同消息，全部放行。二者解决不同问题，必须都有。
  📌 落地前置：本规则要求 `source_peer_instance_id` 在**出站路径可读**。若成员/消息表最终未能承载该列（见 §6 与 §14 STOP），则回环终止退化为仅靠条件 2，需在评审中重新确认是否足够。

---

## 6. Q4 · 成员投影
影子 uid 是真实本地成员行 → `GetSubscribableMemberUIDs()` 天然返回，datasource 无需改。加成员走 `IMAddSubscriber`，带 `is_external=1` + `source_peer_instance_id`。
🔴 **成员存储模型（P1）**：`source_peer_instance_id` 落**哪张成员表**、能否与 `source_space_id` 共存、唯一键、迁移兼容——**均需实测现有 group member 表后确定**，不得假设。
🔴 静默消失（2.4）：`service.go:1452 QueryByUIDs`→`:1497` 遍历 DB 结果丢弃未知 uid——因影子已是真实行而被规避；[api] 测试须断言投递含影子 uid 名单无静默丢弃。

### 6.1 🔴 影子 uid 必须双侧落地（Phase 0 spike 实测结论，P0）
spike 证明：只把影子 uid 加进 IM 订阅表，**IM 侧完全可用**（收发/广播/fan-out 都正常），但 **octo 侧对它一无所知** —— 由此产生三个必须修的后果，且**根因同一、解法同一**：

| # | 后果 | 实测证据 | 处置 |
|---|---|---|---|
| 1 | 发送者**无名无头像**（联邦用户显示为幽灵） | `GET /api/v1/channels/{uid}/1` → **400**；`/users/{uid}/avatar` → **404**；日志 `【User】用户不存在` | 影子 uid 必须建 octo `user` 记录（或 web 端提供 fallback 展示源） |
| 2 | 🔴 **若启用 WuKongIM datasource，影子 uid 会被订阅重载静默抹除** | 本部署 `wk.yaml` 未配 `datasource` 段、`/v1/datasource` 命中 0 次故当前安全；但代码已注册该回调（`modules/group/1module.go:66-78` 返回 `group_member` 权威名单，`db.go:521-537` 注释确认重载会覆盖订阅表） | 影子 uid 必须写入 `group_member`（带 `is_external=1`），使两侧视角一致。**不得**依赖「永久禁用 datasource」这种环境约定 |
| 3 | 每条群消息刷一条 error 日志（按成员数放大） | `modules/webhook/api.go:644` `pushTo` → `【Webhook】没有找到toUser`（IM 确实把影子当正常收件人 fan-out） | `pushTo` 对影子 uid 早退 |

**统一处置：影子 uid 同时落 `user` + `group_member`（带 `is_external` 标记），而非仅存在于 IM 订阅表。**

✅ 附带利好（实测）：`modules/group` 下 8 处 `IMAddSubscriber` **无一处传 `Reset`**（增量添加），故正常建群/拉人/踢人流程不会误伤影子订阅。

---

## 7. Q5 · 外部标记绝不能丢
现状 bug：`GetSpaceName` 对远端 space 返回空串 → `external_marker_cache.go:162-168` 丢标签 → 联邦成员渲染成无标记（最坏）。
**不变量（spec 级强制）**：`source_peer_instance_id` 非空而 space name 不可解析时，**强制**用 `federation_peer.display_name` 渲染外部角标，**绝不渲染成无标记**；缺标记 = 硬 bug。
🔴 验收精化（P2）：「每一处」需给**完整渲染面清单**（成员列表 / 消息气泡 / 会话列表 / 通知 / @提及 / 历史消息 / 各客户端版本）+ 缓存失效规则；单个 browser 测试不足以证明全覆盖——按渲染面拆多条 [api]/[browser] 断言。移动端渲染为 [manual]（无移动自动化 lane）。

---

## 8. Q6 · 撤销与生命周期（事件驱动为主）
🔴 设计评审修正：轮询心跳在数百成员下 O(N) 无效轮询 + 生命周期竞态可绕过。修正：
- **8.1 事件驱动为主**：B 主动推 `lifecycle`（`user_suspended`/`user_revoked`/入群/退群）事件，Host 即时处置。放弃周期性全量轮询。
- **8.2 租约作 fail-closed 兜底**：`lease_expires_at` 仅作「长时间无任何事件」的保险；过期→`suspended`（禁发言、标记不活跃），非高频轮询。网关断线重连时做一次全量 sync 对账。
- **8.3 profile 更新通道（P1）**：B 用户改昵称/头像 → 推 `profile` 事件 → 更新 `federation_identity.display_name/avatar_url`，避免影子信息永久停在首条消息快照。
- **8.4 🔴 吊销的线性化（P1）**：revoke 必须**原子失效**在途请求 + 成员缓存 + 权限缓存，防止「已 revoke 用户借在途请求/陈旧缓存继续写入」。历史消息作者改标注须与现有作者缓存一致（定义失效顺序）。

---

## 9. Q7 · 附件跨域（异步拉取）
🔴 设计评审修正：同步推送几十~上百 MB 文件经 HTTP 网关会堆积/内存暴涨/超时导致中继失败。**改为**：
- B 中继消息只带**附件元数据 + B 侧签名下载 token**；A 落消息后由**异步 Worker 回 B 拉取**并转存 A 本地 MinIO，改写 URL 为 Host-local（保「内容落 Host」的数据主权属性）。
- 必须定义：流式**大小/类型前置校验**、内容哈希完整性、（可选）恶意文件扫描、对象写入与消息引用的**原子提交/失败清理**、重复上传幂等、下载授权、**撤销后历史附件访问策略**。
- 惰性签名 URL（读时回源）作为备选，但依赖对端长期在线，MVP 不选。

---

## 10. Q8 · E2E 加密
`signal_identities` 表存在。**结论：Option 1 服务端中继下 E2E 不成立**（server 必须见明文以影子 uid 存/发；影子在 Host 无真实设备/密钥；附件在 Host 落明文）。
**硬门（fail-closed，双向拦截）**：① 开 E2E 的群拒绝开联邦；② 🔴 **开联邦的群也必须拒绝 `EnableE2E` 接口**（P2——防只拦 CreateFederation 漏改 EnableE2E 造成损坏混合态）。判定须有**单一事实源 + 事务约束**，防群设置并发变更下短暂双开。向用户明示「跨企业协作，非端到端加密」。

---

## 11. Q9 · 安全边界（联邦版信息暴露表）
**总开关**：群 `allow_external=1`（复用）**且** `federation_peer.status=active`（新增），双前置缺一不可，默认关。
| 信息面 | 联邦影子能看到 | 不能看到 |
|---|---|---|
| 该联邦群消息/附件 | ✅ | — |
| 群成员列表（影子投影，带角标） | ✅ | 成员跨群/组织内其它信息 |
| Host 其它 Space/群 | ❌ | ✅ 隔离 |
| Host 用户目录/全局搜索 | ❌（仅群内） | ✅ 隔离 |
| Host 好友图/组织架构 | ❌ | ✅ 隔离 |
复用 `space_filter.go`/`shouldIncludeGroupForSpace` 收敛可见性到该联邦群。
🔴 复用 §4.2 授权 + 禁用 `middleware.go:112-115` 跳过口子。补：**per-peer 限流 / DoS 防护**（一个被攻陷 peer 洪泛）；**跨实例流量审计日志**（合规需要，记录谁经边界发了什么）。

---

## 12. Q10 · 分阶段落地
为避免范围自相矛盾（声称单向却验反向），最小切面拆分如下：

| 阶段 | 切面 | 可验证验收 |
|---|---|---|
| **Phase 0（spike）** | §5.4 扩展 spike（订阅 + **伪装发送** + 出站捕获） | ✅ **已完成，四断言全 PASS**（IM `v2.2.4-20260313`） |
| **Phase 1a（MVP，真·单向）** | HMAC 配对 + 三段授权 + `federation_identity`(开通路径 + 并发 upsert) + 不可认证影子约束 + `federation_channel` + inbox/outbox + **仅 B→A 文本入站** + 硬外部标记 | [api] B 文本经中继出现在 A 群、带正确角标、无静默丢弃；[api] 缺归属/未授权入站被拒；[api] 重放（nonce 撞）被拒、重试（同 idempotency）幂等；[browser] 群内每处见角标 |
| **Phase 1b** | A→B 反向出站（对称链路） | [api] A 消息经出站 outbox 到达 B、ACK/重试可靠 |
| **Phase 2** | 附件异步拉取 + 花名册事件同步 + profile 更新 + 吊销/租约生命周期 | [api] 附件落 Host 存储且原子;[api] revoke 后影子移出+发言被拒+缓存失效;[api] profile 事件更新影子;[api] 租约过期→suspended |
| **Phase 3（加固）** | 密钥轮换（key_id 双窗口）+ SSRF 约束 + per-peer 限流 + 审计日志 + E2E 双向硬 gate | [api] 轮换期 old+new 均验、切换后 old 失效;[api] base_url 私网被拒;[api] E2E 群开联邦 & 联邦群 EnableE2E 均被拒 |
优先级：[api] > [browser] > [manual]。移动端渲染面为 [manual]。

---

## 13. Out-of-scope
双向复制（方案 B）；开放联邦 / 多于两家动态发现（起步只做带合同点对点）；联邦 E2E（§10 已知限制）；WuKongIM 源码改动；修改现有 go 业务文件行为（本轮只出设计）；`@` 域名限定 uid。

---

## 14. STOP conditions（执行者遇到即停并上报）
1. ~~Phase 0：IM 拒绝未知订阅者 **或** 拒绝伪装 from_uid 发送 **或** 无法捕获出站消息~~ → ✅ 已通过（§5.4）。**但：若 IM 升级过 `v2.2.4-20260313`，必须重跑这四条断言** —— 结论仅对已测版本成立。
1b. 🔴 若发现任何环境已启用 WuKongIM `datasource` 回调而影子 uid 未写入 `group_member`（§6.1 #2）→ 停，影子会被静默抹除。
2. 无法建立出站事件捕获钩子 / 无法实现 inbox·outbox 幂等与崩溃恢复 → 停。
3. 无法建立 `federation_channel` 双端映射 → 停。
5. `source_peer_instance_id` 无既有成员表可安全承载 / 与 `source_space_id` 冲突 → 停。
6. `short_no` / `federation_shadow` 列不可空且无安全落位 → 停。
7. §3.3 影子路径审计发现无法统一拦截的认证入口 → 停。
8. 发现 uid 上本 spec 未覆盖的强假设（除 `@`/DM 外）→ 停。
9. 数据主权 / 合规决策点（§16.1）未获明确确认 → 不得进 Phase 1 之后。

---

## 15. 未验证 / 需实测清单（诚实标注）
1. ~~**2.3(a) 扩展**：IM 接受未知订阅者 **且** 接受伪装 from_uid 服务端发送 **且** 可捕获出站~~ —— ✅ **已实测 PASS**（§5.4，IM `v2.2.4-20260313`；结论仅对该版本成立，升级 IM 需重测）。
2. ~~**出站事件捕获钩子在消息管线的确切落点**~~ —— ✅ **已实测定位**：gRPC `msg.notify` → `modules/webhook/api.go:208 → :368 → :374 → :263`（§5.5）。
3. **IM 是否支持外置 client-msg-no 去重** — 仍未验证。
4. ~~**oidc uid 列宽 64 放大被 revert 的原因**~~ —— ✅ **已字节核实：不存在 revert**。`modules/oidc/sql/20260428000002_oidc_legacy01.sql` 的 `+migrate Up` 将 `oidc_audit_log.uid` 改为 `VARCHAR(64)`；`VARCHAR(40)` 仅出现在同文件的 `+migrate Down`（回滚子句）中，后续 `20260515000001_oidc_bind_uniques.sql` 未动列宽。故该列**当前生效宽度就是 64**。支撑「不用域限定 uid」的真实约束是**核心 `user.uid VARCHAR(40)`**（`modules/user/sql/20191106000003_user_legacy01.sql:8` 等），与 oidc 审计表无关。
5. **`short_no` 可空性 / `federation_shadow` 及 `source_peer_instance_id` 列落位与迁移兼容** — 仍未验证。
6. **本文档其余 file:line 证据** — 来自一轮独立勘察，未逐条重开复核；执行者需 drift check 对齐当时 HEAD。（§5.4/§5.5/§6.1 为 spike 实测所得，已核。）

---

## 16. 决策点

每条给出**推荐值**与理由。推荐值为设计侧建议，**尚未获批**；评审时逐条确认或否决。
第 1 条为合规判断，设计侧不提供推荐值。

### 16.1 🔴 数据主权 — 待定，无推荐值（硬闸）
单边托管下，Peer 的协作数据（消息 / 附件 / 成员 profile）**物理存放在 Host 的服务器上**。

需明确：两家法务是否接受？谁是数据控制者（controller）与处理者（processor）？是否跨境传输？**联邦关系解除后，Host 是否必须清除 Peer 侧数据（GDPR 删除权）？**

**这是法律与合规判断，非工程判断，设计侧不给推荐值。**
🔴 **硬闸：本条未获明确确认前，不得推进到 Phase 1 之后**（见 §12、§14.9）。

### 16.2 ~ 16.8 推荐值

| # | 决策点 | **推荐值** | 理由 |
|---|---|---|---|
| 2 | 互信原语 | **HMAC 共享密钥起步**，抽 `FederationTrust` 接口留 mTLS/JWT 迁移位 | JWT/JWKS 基建已被移除（`modules/bot_provision/jwt.go`），重建为纯新增负债；起步形态是**少量、带合同的点对点配对**，非开放联邦，共享密钥的信任模型与之匹配。若合规要求传输层双向认证，则起步即 mTLS——此时仅换 `FederationTrust` 实现，不动业务代码 |
| 3 | E2E 加密 | **接受「联邦群非端到端加密」**，并施加 §10 双向硬 gate | 服务端中继下 E2E **物理上不成立**（Host 必须见明文才能以影子 uid 落库/投递；影子在 Host 无真实设备密钥；附件在 Host 落明文）。唯一诚实处置是明确关闭并向用户明示，而非做出无法兑现的加密承诺 |
| 4 | 消息流分岔 | **服务端中继**（Peer 客户端只连自己的 IM） | 备选方案是给 Peer 用户铸 Host 的 IM token，即**为不受自己管理的人铸发凭证**并把 IM 暴露给不受信客户端，攻击面显著更大。§5.4 A3 已实测证明服务端中继可行 |
| 5 | 附件 | **异步 Worker 回源拉取并转存 Host** | 同步推送数十至数百 MB 经 HTTP 网关会造成连接堆积 / 内存膨胀 / 超时，进而使整条中继失败；异步拉取仍保留「内容落 Host」属性。惰性签名 URL 依赖对端长期在线，MVP 不选 |
| 6 | 影子 profile 投影范围 | **仅投影昵称 + 头像**；**影子不可被 DM，仅在所属联邦群内可见** | 昵称与头像是群内可读性的最小必要集；部门 / 手机 / 邮箱属组织通讯录信息，跨企业投影缺乏必要性。开放 DM 等于把对端用户提升为本地通讯录成员，与 §11 的「可见性收敛到该联邦群」相矛盾 |
| 7 | `short_no` 分配 | **不为影子分配 `short_no`** | 影子并非真实用户，占号会污染号段并可能触发唯一键冲突。前提是该列可空或可安全留空——**待实测确认**（见 §15.5） |
| 8 | Host 归属与授权粒度 | **发起方托管**；per-peer 授权**显式白名单到具体群**，默认拒绝 | 避免「任意可信 peer 可写任意群」（与 §4.2 第 3 段频道授权一致）。跨企业协作通常有明确发起方，与合同关系天然对齐 |

## 17. 测试要点汇总
Phase 0 扩展 spike（阻塞）；影子穿 `QueryByUIDs` 无丢弃；外部标记按渲染面多点 fail-safe；缺归属入站被拒 / 影子不可全局搜索·加好友·登录；防重放（nonce）与幂等（idempotency）区分；outbox 崩溃恢复 / ACK；附件原子转存；生命周期 revoke·租约·profile 事件；HMAC 双密钥轮换；base_url SSRF 拒私网；E2E 双向 gate。

🔴 **回环终止（P0，§5.5）—— Phase 1b 双向打通前必须绿：**
- [api] 入站消息（`source_peer_instance_id` = peer B）在出站中继处被丢弃，**不产生**回 B 的 outbox 行。
- [api] 影子 uid 发出的消息不产生任何出站 outbox 行。
- [api] 双向配对下，B 发一条消息，A 侧最终只存在 **1 条**该消息、B 侧不再收到自己的回声（跑满一个 relay 周期后断言计数不增长）。
- [api] 回环终止与幂等键**分别**生效：构造「新 message id + 新 idempotency_key 但带 source_peer 标记」的消息，断言被回环规则拦下（证明不是靠幂等表兜的）。

🔴 **身份开通（P0，§3.2）**：未开通 `(peer, remote_uid)` 的入站消息被拒，且**不新建** `federation_identity` 行。

---
