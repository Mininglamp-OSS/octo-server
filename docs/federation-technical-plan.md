# octo 跨企业联邦 · 完整技术方案

**status: DRAFT — 待拍板（本文档为技术方案主文档）**
**方案：A 单边托管（single-homed）**
**代码基线：** octo-server `main` @ `a61b5411` 系（本文档所有 file:line 已在撰写时对该基线复核，标注 ⚠️ 者为例外）
**实测基线：** WuKongIM `v2.2.4-20260313`（Phase 0 spike 结论仅对该版本成立）

---

## 文档定位与阅读顺序

本仓已有三份联邦文档，职责如下，**不要重复维护同一事实**：

| 文档 | 定位 | 读者 |
|---|---|---|
| **本文档** `federation-technical-plan.md` | **技术方案主文档**：完整架构、数据模型、链路、安全模型、分期与验收 | 评审者、实施者、决策者 |
| `federation-architecture.md` | 背景 / 图 / 端到端场景（入口读物） | 新加入者 |
| `federation-design.md` | 逐决策点的推理与 STOP 条件（决策记录） | 深度评审 |
| `external-group-design.md` | **前置依赖**：同实例跨 Space 外部群（联邦复用其骨架） | 实施者必读 |

冲突时以本文档为准。

---

## 1. 目标与非目标

### 1.1 目标
两家**各自独立自部署** octo 的企业（下称 **Host / A** 与 **Peer / B**），在**一个共享外部群**内长期协作：B 的员工在 A 托管的群里收发消息、看到彼此、被 @、被指派待办，且**全程带明确的「外部成员 · 归属 B」标识**。

### 1.2 硬性非目标（明确不做）
- ❌ **不做开放联邦**：不是任何 octo 实例都能接入。这是**带合同的点对点配对**，peer 需显式互相授权，逐群 allowlist。
- ❌ **不做 B 的用户直连 A 的 IM**：B 的用户从不持有 A 的凭证，也从不与 A 的 IM 建立连接。
- ❌ **不做双向全量复制**：不追求两边各存一份完整副本（理由见 §3.2）。
- ❌ **不改 WuKongIM**：IM 作为 uid 无关的消息总线保持不动。
- ❌ **不做联邦群的端到端加密**：物理上不成立（§8.3），诚实关闭并向用户明示。

### 1.3 架构硬约束
联邦网关**必须**抽为独立模块 `modules/federation`。**不允许**把「单边托管」假设焊死进 `modules/group` / `modules/message` 等业务代码 —— 将来若要换协议（Matrix / IETF MIMI）或换信任原语，业务层不应重写。具体接缝见 §9.4。

---

## 2. 为什么需要它：现有三条路都不通

| 现有做法 | 为什么不行 |
|---|---|
| 给对方员工开我方账号 | 对方员工在组织上成了「我方员工」：进通讯录、可被全局搜索、可被 DM、可看到与该群无关的组织信息。权限收敛不到单群。 |
| 改用公有云 IM | 自部署的意义（数据在自己机房、可审计、可管控）直接消失。 |
| 微信 / 邮件外挂 | 上下文碎裂在两个系统里，无法审计，附件与消息脱节，待办无法闭环。 |

联邦要解决的正是「**既让对方员工进得来，又不让他成为我方组织成员**」这个矛盾。

---

## 3. 方案选型

### 3.1 选定：方案 A 单边托管

共享群**寄居在一方实例上**（Host）。远端用户在 Host 上被投影为**不可认证的本地影子账号**；B 的用户连自己的 B 实例，由 B 的网关代理收发。

**三方独立收敛到同一形状 —— 这是选它的最强理由：**

1. **IETF MIMI**（正在制定的 IM 互通标准）：每房间**恰好一个 hub server**，其余为 follower，follower 只跟 hub 打交道，房间状态住 hub、消息由 hub 排序与 fan-out。这就是单边托管。
2. **MIMI 架构选项文档**（`draft-rosenberg-mimi-arch-options`）把我们讨论过的两条路用几乎相同的术语论证过：
   - *guest model*（群住 provider 1，B/C 用户直连 provider 1）→ 连接负担重、客户端要连每个 provider，**不 scalable**；
   - *proxied guest model*（群住 provider 1，用户连自己的 provider，后者代理）→ 即我们的方案 A。
3. **Mattermost Connected Workspaces**（Apache-2.0，Go，单体，可直接读码）：`Users` 表加 `RemoteId` 列、`GetByRemoteID()`、`user.IsRemote()` —— **把联邦来源记在列上，而不是编进 uid 字符串**，与我们的结论一致。

### 3.2 放弃：方案 B 双向复制

参考 Mattermost 的实际代价（其官方文档原文）：
- 「Content is **synchronized across all participating** Mattermost instances」→ **每一方都有完整副本**；
- 「权限在本地服务器生效，**在远端服务器不生效**」→ **你无法约束对方那份副本怎么被访问**；
- v10.10 后，从任一方移除共享频道 → **两边都被删除**。

方案 A 数据只有一份、在 Host 上，主权边界反而更清晰（§10）。

### 3.3 放弃：直接采用 Matrix 协议

**🔴 硬技术冲突，不是工作量问题：**

Matrix 的用户 ID 形如 `@alice:company-b.com`，而 **octo 中 `@` 是单聊频道的保留分隔符**：
- `IsFakeChannel(id)` 的判定就是 `strings.Contains(id, "@")`；
- 单聊频道 ID 用 `from@to` 拼接；
- 非测试调用点含安全相关路径：`modules/webhook/api_datasource.go:120-121`（黑名单校验）、`modules/messages_search/search_global_groups.go:453`（全局搜索）。

叠加列宽约束：核心 `user.uid VARCHAR(40)`（`modules/user/sql/20191106000003_user_legacy01.sql:8`），现用 32 位 hex，仅余 8 字符 —— `@alice:company-b.com` 装不进去。

> 📌 **勘误（原文档错误，此处修正）**：早期草案称「另一模块曾放宽 uid 到 64 并已回滚」。**该说法不成立**。byte-check：`modules/oidc/sql/20260428000002_oidc_legacy01.sql` 的 `+migrate Up` 是 `ALTER TABLE oidc_audit_log MODIFY uid VARCHAR(64)`，`VARCHAR(40)` 仅存在于同文件 `+migrate Down`（回滚子句）；后续 `20260515000001_oidc_bind_uniques.sql` 未动列宽。故 `oidc_audit_log.uid` **当前生效宽度即 64，从未回滚**。真正的约束是**核心 `user.uid` 的 40**，与该审计表无关。

**结论**：不采用 Matrix 作为传输协议，但**在 §9.4 的接缝上保持协议无关**，将来可换。

---

## 4. 全局架构

```
┌─────────────────────── 企业 A（Host，群寄居方） ───────────────────────┐
│                                                                        │
│   A 的真实用户 ──── WebSocket ────┐                                    │
│                                   ▼                                    │
│                            ┌─────────────┐                             │
│                            │  WuKongIM   │  （uid 无关的消息总线，不改）│
│                            └──────┬──────┘                             │
│                                   │ gRPC msg.notify                    │
│                                   ▼                                    │
│   ┌──────────────┐         ┌──────────────┐      ┌──────────────────┐  │
│   │ modules/group│◄────────│ octo-server  │─────►│modules/federation│  │
│   │  （复用）    │         │              │      │   （新增网关）   │  │
│   └──────────────┘         └──────────────┘      └────────┬─────────┘  │
│                                                            │           │
└────────────────────────────────────────────────────────────┼───────────┘
                                                             │
                          HTTPS + HMAC-SHA256 双向通道        │
                          （inbox / outbox，带 ACK 重试）      │
                                                             │
┌────────────────────────────────────────────────────────────┼───────────┐
│                    企业 B（Peer，成员来源方）               │           │
│                                                   ┌────────┴─────────┐ │
│   B 的真实用户 ──── WebSocket ──► B 的 octo ─────►│modules/federation│ │
│                                       │           └──────────────────┘ │
│                                  ┌────┴────┐                           │
│                                  │ B 的 IM │                           │
│                                  └─────────┘                           │
└────────────────────────────────────────────────────────────────────────┘

关键不变量：B 的用户从不接触 A 的 IM，也从不持有 A 的任何凭证。
```

---

## 5. 身份模型（方案的地基）

### 5.1 影子账号：形态正常，能力被硬阉

远端用户在 Host 上映射到**一个正常生成的本地 32 位 hex uid**（同 `pkg/util/string.go:15`），**不带任何 namespace 限定符**。

**为什么不用 `uid@peer` 这种域限定形式**：见 §3.3（`@` 是 DM 频道保留分隔符 + 列宽 40 焊死）。

**为什么用「正常形态 uid」是优势**：影子是一行普通本地成员记录 → 无声穿过 `QueryByUIDs`、成员列表、索引、DM 逻辑，**不需要在几十处调用点插特判**。

### 5.2 🔴 不可认证影子（Unauthenticatable Federation Shadow）—— 一等约束

影子 uid **形态与本地用户无异**，这既是优势也是风险：它绝不能被当作可登录主体。**必须逐路径审计并 fail-closed 关闭**：

| 能力 | 要求 | 理由 |
|---|---|---|
| 密码 / 验证码登录 | **禁止** | 影子无凭证主体，任何可登录都是越权入口 |
| 被加好友 | **禁止** | 会把对端用户变成 A 的本地通讯录成员 |
| 被 DM | **禁止** | 与「可见性收敛到该群」自相矛盾 |
| 全局搜索命中 | **禁止** | 组织外人员不应出现在 A 的全局人员搜索 |
| 出现在通讯录 / 组织架构 | **禁止** | 同上 |
| 分配 `short_no` | **禁止** | `short_no` 是本地组织内的人员编号语义 |
| 在**联邦群内**被 @ / 被指派待办 / 被引用 | **允许** | 这是功能目标本身 |

> 落地要求：以上每一条都要有对应 [api] 测试断言，而不是靠"代码里应该没有这条路"。

### 5.3 `federation_identity`（身份映射表，复用 `user_oidc_identity` 形状）

```sql
CREATE TABLE `federation_identity` (
  id              BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  peer_id         VARCHAR(40)  NOT NULL DEFAULT '',  -- → federation_peer.peer_id
  remote_uid      VARCHAR(64)  NOT NULL DEFAULT '',  -- 对端的用户标识（对端命名空间，不解释其含义）
  shadow_uid      VARCHAR(40)  NOT NULL DEFAULT '',  -- Host 本地 32-hex 影子 uid
  display_name    VARCHAR(100) NOT NULL DEFAULT '',  -- 投影：昵称
  avatar_url      VARCHAR(512) NOT NULL DEFAULT '',  -- 投影：头像
  status          SMALLINT     NOT NULL DEFAULT 1,   -- 1=active 2=suspended 3=revoked
  lease_expires_at TIMESTAMP   NULL,                 -- fail-closed 兜底租约
  last_seen_at    TIMESTAMP    NULL,
  created_at      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_peer_remote (peer_id, remote_uid),
  UNIQUE KEY uk_shadow (shadow_uid),
  KEY idx_status (peer_id, status)
);
```

**只投影 `display_name` + `avatar_url`**。不投影部门、手机、邮箱 —— 跨企业没必要，且扩大信息暴露面。

### 5.4 🔴 影子的创建时机：只在开通路径，入站绝不建号

这是一条**安全关键**规则（早期草案在此自相矛盾，已修正）：

- **唯一创建入口 = 成员开通（provisioning）**：B 推送 `member_add` 生命周期事件，或 A 侧管理员显式批准 → 才创建 `federation_identity`（+ §6.2 的双侧落地）。
- **入站消息路径只做「解析 / 刷新」，不创建**：按 `(peer_id, remote_uid)` **查**已开通身份；查不到 = 未开通 → 按 §7.2 **fail-closed 拒收**并记审计，**不静默建号**。允许的写操作仅限刷新既有行的 `display_name` / `avatar_url` / `last_seen_at`。
- **⚠️ 若允许入站创建的后果**：任何能签出合法 HMAC 的一方，即可在 Host 上**凭空造账号并把它送进群** → §7.2 step 2 的身份绑定校验退化为**永真**，整个授权模型失效。
- **并发竞态（仅存在于开通路径）**：同一 `(peer_id, remote_uid)` 的开通事件可能重复/并发投递，撞 `uk_peer_remote` → 500。开通路径**必须**用 DB 级 upsert（`INSERT ... ON DUPLICATE KEY UPDATE` 取回既有行）或 per-key 分布式锁；**禁止「先 SELECT 再 INSERT」裸竞态**。入站路径为纯读 + 字段刷新，无此竞态。

---

## 6. 成员投影：复用现有外部成员骨架（**已验证，比原设计更省**）

### 6.1 🟢 关键发现：承载列与读路径都已存在，不需要新增平行维度

`modules/group/sql/20260424000001_group_legacy01.sql` **已经**提供了全部所需列：

```sql
ALTER TABLE `group`        ADD COLUMN `is_external_group` SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE `group_member` ADD COLUMN `is_external`       SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE `group_member` ADD COLUMN `source_space_id`   VARCHAR(40) NOT NULL DEFAULT '';
CREATE INDEX `idx_group_member_external` ON `group_member` (`uid`, `is_external`, `is_deleted`);
```

读路径也是**现成的**：`modules/group/service.go:405` 用一条 LEFT JOIN 同时取出 `is_external` / `source_space_id` / `space.name`，`:458` 将 `SourceSpaceID` 赋给 `marker.HomeSpaceID`；出站可读性由 `QuerySourceSpaceIDForMember`（`service.go:950`）保证。带单测 `service_member_external_fields_test.go:36`（断言「外部成员 `home_space_id` 应 = `source_space_id`」）。

**因此设计相对早期草案做两处简化：**

1. **不新增 `source_peer_instance_id` 平行列**，改为**复用 `source_space_id` 承载「来源域」**（本地 Space **或** 联邦 peer）。
   - *理由*：新增平行维度会迫使 `service.go:405` 的 JOIN、`idx_group_member_external` 索引、以及前端 9 个渲染面**全部改造去认第二个来源维度**；复用则让联邦成员天然走完现有外部成员链路。
   - *代价*：需要一个**不会与本地 space_id 相撞的命名空间约定**（见 §6.3）。这比改 JOIN + 索引 + 9 个渲染面便宜一个数量级。
2. **回环终止规则的退化 caveat 可以撤销**：`source_space_id` 为 `VARCHAR(40)` 且在出站路径可读 → §7.4 的规则 a 与 b **都能落地**，Phase 1b 的硬门是干净的。

### 6.2 🔴 影子必须双侧落地（Phase 0 spike 实测结论）

spike 证明：只把影子 uid 加进 IM 订阅表，**IM 侧完全可用**（收发/广播/fan-out 正常），但 **octo 侧对它一无所知**，由此产生三个后果，**根因同一、解法同一**：

| # | 后果 | 实测证据 | 处置 |
|---|---|---|---|
| 1 | 发送者**无名无头像**（联邦用户显示为幽灵） | `GET /api/v1/channels/{uid}/1` → **400**；`/users/{uid}/avatar` → **404**；日志 `【User】用户不存在` | 影子必须建 octo `user` 记录 |
| 2 | 🔴 **启用 WuKongIM datasource 时，影子会被订阅重载静默抹除** | 本部署 `wk.yaml` 未配 `datasource` 段故当前安全；但回调已注册（`modules/group/1module.go:66-78` 返回 `group_member` 权威名单，`db.go:521-537` 注释确认重载会覆盖订阅表） | 影子必须写入 `group_member`（`is_external=1`），使两侧视角一致。**不得**依赖「永久禁用 datasource」这种环境约定 |
| 3 | 每条群消息刷一条 error 日志（按成员数放大） | `pushTo` → `【Webhook】没有找到toUser`（IM 把影子当正常收件人 fan-out）⚠️ 该函数在当前基线位于 `modules/webhook/api.go:495`（早期草案记为 `:644`，已漂移） | `pushTo` 对影子 uid 早退 |

> **统一处置：影子 uid 同时落 `user` + `group_member`（带 `is_external=1`），而非仅存在于 IM 订阅表。**

✅ 附带利好（实测）：`modules/group` 下 8 处 `IMAddSubscriber` **无一处传 `Reset`**（均为增量添加），故正常建群/拉人/踢人不会误伤影子订阅。

### 6.3 🔴 来源域命名空间与**伪造面**（安全关键）

复用 `source_space_id` 承载 peer 标识，必须解决两件事：

**(a) 命名空间不相撞**：联邦来源值需带不可与本地 space_id 混淆的前缀（如 `fed:<peer_id>`，总长受 `VARCHAR(40)` 约束，故 `peer_id` 需短）。渲染时按前缀路由到 `federation_peer.display_name`。

**(b) 🔴 绝不接受对端自述的来源域**。已有前人踩坑记录在案：`modules/group/api.go:1603` 注释说明 `inviterSpaceID` **来自 `X-Space-ID` header**，而 `api.go:1676-1692`（YUJ-201 / GH#1268）补的纵深防御明确写着「**client 可以任意伪造**」，故在写入 `source_space_id` 前用 `spacepkg.CheckMembership` 校验，失败则降级空串 + `Warn`。

> **联邦规约**：入站 envelope 中**不得**存在由对端自述并被直接采信的来源域字段。`source_space_id` **必须由 Host 网关依据「已验签的 peer 身份」服务端填入**。否则对端可伪装成另一个 peer、甚至伪装成 A 的本地 Space → **绕过外部角标**（外部成员被渲染成内部人员，这是最坏后果）。
> [api] 测试：envelope 携带自述来源域时，该字段被忽略，落库值等于按验签身份推导的值。

### 6.4 🟢 前端：9 个渲染面零改动

octo-web 的 `Utils/externalViewer.ts` 中 `resolveExternalForViewer()` 判定为 `homeSpaceId !== viewerSpaceId` —— 是**相对视角模型**，而非读取绝对 `is_external` 布尔值；优先取 `home_space_id` / `home_space_name`，旧字段 `is_external` 仅降级兜底。11 个调用点，带完整单测。

**推论**：后端只要在成员/消息接口把影子的 `home_space_id` / `home_space_name` 填成对端公司，下列 9 个面**自动点亮，前端一行不改**：消息气泡发送者、成员列表、成员侧栏、引用预览、@ 下拉、全局搜索、转发选择器、待办指派人、会话列表。

> **设计原则：联邦成员不做成第四种身份，直接复用现有「外部成员」语义。**

### 6.5 🔴 外部标记绝不能丢

现状 bug：`GetSpaceName` 对远端来源返回空串 → `external_marker_cache.go:162-168` 丢标签 → 联邦成员渲染成**无标记**（最坏后果）。

**不变量（spec 级强制）**：来源域非空而 space name 不可解析时，**强制**用 `federation_peer.display_name` 渲染外部角标，**绝不渲染成无标记**；缺标记 = 硬 bug。

验收需给**完整渲染面清单** + 缓存失效规则；单个 browser 测试不足以证明全覆盖 —— 按渲染面拆多条 [api]/[browser] 断言。移动端渲染列为 [manual]（无移动自动化 lane）。

---

## 7. 信任、授权与消息流

### 7.1 互信原语：HMAC 共享密钥起步

**选 HMAC-SHA256 共享密钥**，藏在 `FederationTrust` 接缝后（§9.4）。

*理由*：JWT/JWKS 基建在本仓已被删除，为联邦重建是**纯负债**；起步形态是「带合同的点对点配对」，不是开放联邦，共享密钥足够。将来若真需多方联邦再换 JWKS，业务层不受影响。

```sql
CREATE TABLE `federation_peer` (
  id             BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  peer_id        VARCHAR(40)  NOT NULL DEFAULT '',   -- 本地分配的短标识（受 VARCHAR(40) 来源域约束）
  display_name   VARCHAR(100) NOT NULL DEFAULT '',   -- 对端企业名（角标兜底渲染用，§6.5）
  base_url       VARCHAR(512) NOT NULL DEFAULT '',   -- 对端网关（⚠️ SSRF 面，§7.6）
  secret_current  VARCHAR(128) NOT NULL DEFAULT '',  -- 当前密钥（加密存储）
  secret_previous VARCHAR(128) NOT NULL DEFAULT '',  -- 轮换重叠窗口
  status         SMALLINT     NOT NULL DEFAULT 1,    -- 1=active 2=suspended 3=terminated
  created_at     TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_peer (peer_id)
);

CREATE TABLE `federation_channel` (   -- 逐群 allowlist：群 ↔ peer 的权威映射
  id          BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
  peer_id     VARCHAR(40) NOT NULL DEFAULT '',
  group_no    VARCHAR(40) NOT NULL DEFAULT '',
  status      SMALLINT    NOT NULL DEFAULT 1,
  created_at  TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_peer_group (peer_id, group_no)
);
```

### 7.2 🔴 授权 ≠ 认证：三段闸门，缺一 fail-closed

HMAC 只证明「请求来自持共享密钥的一方」，**不证明**：① `remote_uid` 当前确属该 peer 且 active；② 该 uid 仍是目标群成员；③ 该 peer 被授权向该群写入。**必须三段显式校验**：

1. **peer 认证** — HMAC 验签通过（含 §7.5 防重放）。
2. **身份绑定** — `federation_identity` 中 `(peer_id, remote_uid)` 存在、`status=active`、租约未过期。（**不创建**，见 §5.4）
3. **频道授权** — 目标群 `allow_external=1` **且** `federation_channel` 中该 `(peer_id, group_no)` active **且** 该影子确为该群成员。

> 🔴 **禁止**依赖 `pkg/space/middleware.go:106-115` 的「`spaceID == "" → c.Next()`」跳过口子（已复核：`space_id` 取 query 再取 `X-Space-ID` header，两者皆空即放行）。联邦入站**必须**显式携带并强校验频道/租户归属。
> [api] 测试：缺归属的入站请求被拒。

### 7.3 入站链路（B → A，文本）

```
B 用户发消息
  └─► B 的 octo 捕获 ──► B 网关：签 HMAC + nonce + idempotency_key
        └─► HTTPS ──► A 网关 /federation/inbound
              ├─ ① HMAC 验签（+ 防重放）
              ├─ ② 解析 remote_uid → shadow_uid（查已开通身份；查不到 → 拒收，§5.4）
              ├─ ③ 频道授权（federation_channel）
              ├─ ④ 服务端填 source_space_id（依验签身份，忽略自述值，§6.3）
              ├─ ⑤ 落 federation_inbox（(peer, idempotency_key) 唯一，事务内）
              └─► worker 幂等推 IM：以 shadow_uid 作 from_uid 服务端发送
                    └─► IM 广播 ──► A 的真实用户（在线实时 + 离线补拉，均已实测）
```

### 7.4 出站链路与 🔴 回环终止规则（P0）

**出站捕获落点（已实测）**：WuKongIM 经 **gRPC** 推事件到 octo-server。链路 `modules/webhook/api.go:208 SendWebhook → :368 handleEvent → :374-380 分发 → :263 handleMessageNotify`。

**必须挂 `EventMsgNotify = "msg.notify"`**（`modules/webhook/common.go:39`）——它对频道内**每条**消息触发，与收件人在线/离线无关。**不要挂 `msg.offline`**（`common.go:33`），那条仅在存在离线收件人时才来。

> ⚠️ 接入时注意勿与已挂在同一消息上的 `modules/bot_api/obo_fanout.go:214` OBO fan-out 链相互干扰。

**🔴 回环终止规则（spec 级强制）**：`msg.notify` 对每条消息触发，**包含中继 worker 自己代影子 uid 写入的入站消息**。若无终止规则，Phase 1b 双向打通后**立即**形成 A→B→A 无限回环 / 重复投递。

出站中继在入队 `federation_outbox` 前**必须**丢弃满足任一条件的消息：
1. 该消息行的**来源域标识目标 peer** —— 「从这个 peer 收来的，绝不再发回这个 peer」（消除 A→B→A 直接回环）；
2. 该消息的发送者是**影子 uid**（`federation_identity` 命中）—— 影子发言语义上属于其归属 peer，Host 不为其代理出站（消除多 peer 拓扑下的间接扩散）。

> ⚠️ **幂等键不能替代回环终止**：`(peer, idempotency_key)` 只能压掉「同一条消息被重复处理」；而回环中**每一跳都是新 message id + 新 idempotency_key**，幂等表视其为不同消息，**全部放行**。二者解决不同问题，必须都有。

### 7.5 可靠投递、防重放、幂等

- **双写难题**：「写 IM」与「记幂等/outbox」非同一事务 → 崩溃窗口造成重复或永久丢消息。**采用 outbox 模式**：入站先落 `federation_inbox`（事务内），worker 幂等推 IM；出站先落 `federation_outbox`，worker 带 ACK/重试/死信投递 peer。需定义保留期、最大重试、DLQ、崩溃恢复（重启扫未 ACK）。
- **防重放 ≠ 幂等**：nonce + 时间窗防**重放攻击**（拒绝）；`idempotency_key` 防**正常重试**（幂等接受）。两者必须分别实现、分别测试。
- **IM 侧去重（P2）**：即便 DB 幂等，网络抖动重试写 IM 仍可能双气泡；需确认 IM 是否支持外置 client-msg-no 去重（**需实测**），否则 worker 必须在「已确认写入 IM」状态持久化后才不重发。

### 7.6 🔴 `base_url` 的 SSRF / 路由劫持面

`federation_peer.base_url` 是**管理员可写的出站请求目标** → SSRF 与流量劫持面：
- 拒绝私网/环回/元数据地址（`127.0.0.0/8`、`10/8`、`172.16/12`、`192.168/16`、`169.254/16`、`::1` 等），仅允许 HTTPS；
- 校验证书；限制重定向跟随；
- 变更 `base_url` 视为**高危操作**（审计 + 二次确认）。
[api] 测试：`base_url` 指向私网时被拒。

---

## 8. 安全模型

### 8.1 信息暴露边界

| B 侧信息 | 是否进入 A | 说明 |
|---|---|---|
| 昵称、头像 | ✅ | 投影，供渲染 |
| 消息正文、附件 | ✅ | 群协作内容本身（**数据主权关键**，§10） |
| 部门、手机、邮箱 | ❌ | 不投影 |
| B 的组织架构 / 通讯录 | ❌ | 不投影 |
| B 的其他群 | ❌ | 联邦关系逐群 allowlist |

| A 侧信息 | 是否流向 B | 说明 |
|---|---|---|
| 该联邦群内的消息 | ✅（Phase 1b） | 目标功能 |
| A 的成员名单（该群内） | ✅ | 仅该群 |
| A 的其他群 / 组织架构 | ❌ | 不出站 |

### 8.2 生命周期与吊销（事件驱动为主）

> 早期草案用周期性轮询心跳，在数百成员下是 O(N) 无效轮询且存在生命周期竞态，已修正。

- **事件驱动为主**：B 主动推 `member_add` / `member_remove` / `user_suspended` / `user_revoked` / `profile` 事件，A 即时处置。
- **租约作 fail-closed 兜底**：`lease_expires_at` 仅作「长时间无任何事件」的保险；过期 → `suspended`（禁发言、标记不活跃），而非高频轮询。网关断线重连时做一次全量 sync 对账。
- **profile 更新通道**：B 用户改昵称/头像 → 推 `profile` 事件 → 更新 `federation_identity`，避免影子信息永久停在首条消息快照。
- **🔴 吊销的线性化**：revoke 必须**原子失效**在途请求 + 成员缓存 + 权限缓存，防止「已 revoke 用户借在途请求/陈旧缓存继续写入」。历史消息作者标注变更须与现有作者缓存定义明确的失效顺序。

### 8.3 E2E 加密：诚实关闭

服务端中继形态下，Host 必须能读明文才能投递给本地成员 → **E2E 物理上不成立**。

**处置**：联邦群**不启用** E2E，并**双向硬 gate** —— 已启用 E2E 的群不可转为联邦群，联邦群不可开启 E2E；UI 明示该群不具备端到端加密。**不做「看起来像 E2E 实际不是」的伪装**。

### 8.4 附件跨域：异步拉取转存

**选异步拉取转存 Host**，不选同步推送。
*理由*：同步推大文件会造成网关堆积 / OOM / 超时，反而把整条中继搞挂。
要求：拉取需鉴权 + 大小上限 + 类型校验 + 超时；转存**原子性**（避免半截文件可见）；失败有重试与用户可见的失败态。

---

## 9. 分期落地

### Phase 0.5 — 并行起跑（不阻塞工程）
- **法务/对端确认三件事**（§10）：对方员工的消息+附件+昵称长期存放于 Host 机房是否可接受；controller 是谁；联邦解除后数据如何处置。
- **确定第一个真实对端**：哪家、对口人、预计群规模 —— HMAC 配对、`federation_peer`、带外密钥交换都需要真实对象。
- 工程侧同期做 §11 剩余实测项，不等法务。

### Phase 1a — MVP，真·单向（B → A 文本入站）
交付：`modules/federation` 骨架 + HMAC 配对（`FederationTrust` 后）+ 三段授权 + `federation_identity` + **成员开通路径** + 不可认证影子约束（§5.2 逐条）+ `federation_channel` + inbox/outbox + 服务端填来源域 + 硬外部标记。

**验收**：
- [api] B 文本经中继出现在 A 群、带正确角标、无静默丢弃
- [api] 缺归属 / 未授权入站被拒
- [api] 重放（nonce 撞）被拒；重试（同 idempotency）幂等
- [api] **未开通身份的入站消息被拒，且不产生 `federation_identity` 行**（§5.4）
- [api] **envelope 自述来源域被忽略**（§6.3）
- [api] 影子不可登录 / 不可被加好友 / 不可被 DM / 不可全局搜索（§5.2 逐条）
- [browser] 群内每个渲染面均见外部角标（§6.5）

### Phase 1b — 双向打通
**前置硬门：§7.4 回环终止规则必须先绿。**
- [api] 入站消息（来源域 = peer B）在出站处被丢弃，**不产生**回 B 的 outbox 行
- [api] 影子 uid 发出的消息不产生任何出站 outbox 行
- [api] 双向配对下，B 发一条消息，A 侧最终只存在 **1 条**，B 侧不再收到自己的回声（跑满一个 relay 周期后断言计数不增长）
- [api] **回环终止与幂等键分别生效**：构造「新 message id + 新 key 但带来源域标记」的消息，断言被**回环规则**拦下（证明不是幂等表兜的）

### Phase 2 — 富内容与规模化
附件转存、profile 事件、生命周期事件全通道、密钥轮换演练、多 peer 拓扑、可观测性（中继延迟/失败率/DLQ 深度告警）。

### 9.4 🔴 模块接缝（防止焊死）

| 接缝 | 抽象什么 | 为什么 |
|---|---|---|
| `FederationTrust` | 签名/验签、密钥来源 | HMAC → JWKS 可换 |
| `FederationEnvelope` | 线上消息格式（编解码） | 将来可换 MIMI / Matrix 表示 |
| `FederationTransport` | 出站投递（HTTP / 队列） | 可换传输 |
| `ShadowProjector` | 影子落地（`user` + `group_member`） | 隔离对现有 group 模块的写入 |
| `PeerDirectory` | peer / channel allowlist 查询 | 授权决策集中一处，便于审计 |

---

## 10. 🔴 数据主权：唯一的硬闸（无工程解，需拍板）

**这是第 0 步，不是最后一步。** 若对端法务不接受「其员工的消息、附件、昵称长期存放于 Host 机房」，方案 A 从形状上即不成立；而方案 B 已被论证更差（§3.2）→ **前置工程设计全部作废**。故必须与 Phase 1a **并行**推进，且在 Phase 1b 前关闭。

**业界现状：三家都没「解决」，只是各自换了说法** ——

| 项目 | 处理方式 |
|---|---|
| **Mattermost** | 内容在每一方**都有完整副本**；不定义「数据归谁」，而是重定义主权 = 「你那份副本在你自己的基础设施上，权限由你的服务器独立裁决」。并明说「权限**在远端服务器不生效**」→ 无法约束对方副本被如何访问。 |
| **Matrix** | 房间状态在所有参与 homeserver 间复制；退出联邦不回收既有副本。 |
| **IETF MIMI** | 定义互通机制，**把数据主权留给部署方与合同**，不在协议层裁决。 |

**方案 A 的相对优势（可作为对外说明）**：数据只有**一份**、在 Host 上，边界清晰、可审计、可一次性清除 —— 不存在「对方还留着一份我管不着的副本」。

**必须由 Yu / 法务书面回答（工程不能代答）**：
1. 对端是否接受其员工的消息/附件/昵称留存于 Host 机房？
2. 谁是 data controller，谁是 processor？
3. 联邦解除时数据如何处置（清除 / 保留 / 归档），SLA 多长？
4. 是否需要「按对端要求删除其成员全部历史消息」的能力？（**若需要，会反向改动数据模型与保留策略，属架构级影响**）

---

## 11. 已验证 / 未验证清单（诚实标注）

### ✅ 已实测
1. **IM 接受未注册订阅者、接受伪装 `from_uid` 服务端发送、出站可捕获** —— Phase 0 spike 四条断言 **A1–A4 全 PASS**（IM `v2.2.4-20260313`）。其中 **A3（以影子 uid 作 `from_uid` 服务端发送，IM 接受并广播，真实用户在线+离线均收到）是整个单边托管的生死线**。
2. **出站钩子精确落点** —— gRPC `msg.notify`，`api.go:208 → :368 → :374 → :263`；**非** `msg.offline`。
3. **成员表承载列与读路径已存在** —— `is_external` / `source_space_id` / `idx_group_member_external` + `service.go:405/:458` JOIN + `QuerySourceSpaceIDForMember`（§6.1）。
4. **前端为相对视角模型** —— `resolveExternalForViewer()` = `homeSpaceId !== viewerSpaceId`，9 个渲染面可零改动点亮（§6.4）。
5. **`oidc_audit_log.uid` 当前宽度为 64 且从未回滚** —— 勘误见 §3.3。
6. **`IsFakeChannel` 的 `@` 冲突真实存在于非测试路径** —— `webhook/api_datasource.go:120-121`、`messages_search/search_global_groups.go:453`。
7. **`X-Space-ID` header 可伪造且已有纵深防御先例** —— `group/api.go:1603`、`:1676-1692`（§6.3）。

### ⚠️ 未验证 / 需实测（执行前必做）
1. **来源域命名空间在 `VARCHAR(40)` 内的具体编码** —— `fed:<peer_id>` 的 `peer_id` 长度上限、与既有 space_id 取值域的碰撞面（§6.3a）。
2. **IM 是否支持外置 client-msg-no 去重** —— 决定 worker 重试是否会产生双气泡（§7.5）。
3. **`short_no` 可空性** —— 影子不分配 `short_no`，需确认现有代码路径不假设其非空。
4. **`external_marker_cache` 的失效规则** —— peer `display_name` 变更后角标刷新时机（§6.5）。
5. **IM 升级回归** —— Phase 0 结论仅对 `v2.2.4-20260313` 成立，升级 IM 必须重跑 A1–A4。

---

## 12. STOP conditions（实施者遇到即停并上报，不得自行绕过）

1. §5.2 任一「禁止」能力**无法**在代码层关闭（例如影子可被搜索且无收敛点）→ 停。
2. 来源域**无法**在 `VARCHAR(40)` 内与本地 space_id 安全共存 → 停（需重新评估是否新增列）。
3. 出站路径**读不到**来源域 → 停（回环终止规则将退化，Phase 1b 不得开启）。
4. 发现任何路径**接受对端自述的来源域**并落库 → 停（§6.3b，安全红线）。
5. `msg.notify` 与 OBO fan-out 链存在无法解耦的干扰 → 停。
6. 数据主权（§10）在 Phase 1b 前未获书面答复 → 停在 Phase 1a。

---

## 13. 决策点状态

| # | 决策 | 推荐值 | 状态 |
|---|---|---|---|
| 1 | 互信原语 | HMAC 共享密钥起步（`FederationTrust` 接缝后） | 推荐，待批 |
| 2 | E2E | 接受联邦群非 E2E + 双向硬 gate | 推荐，待批 |
| 3 | 消息流 | 服务端中继 | 推荐，待批 |
| 4 | 附件 | 异步拉取转存 Host | 推荐，待批 |
| 5 | profile 投影 | 只投昵称+头像，影子不可被 DM | 推荐，待批 |
| 6 | `short_no` | 影子不分配 | 推荐，待批 |
| 7 | 托管方 | Originator-hosted + 逐群 allowlist | 推荐，待批 |
| 8 | 成员表承载 | **复用 `source_space_id`，不新增平行列** | 推荐，待批（新增，依据 §6.1） |
| 9 | **数据主权** | **无推荐值** | 🔴 **硬闸，需 Yu / 法务书面答复（§10）** |

---

## 14. 测试要点汇总

Phase 0 IM 断言（升级需重跑）；影子穿 `QueryByUIDs` 无丢弃；§5.2 七条禁止能力逐条断言；未开通身份入站被拒且不建号；envelope 自述来源域被忽略；外部标记按渲染面多点 fail-safe；防重放（nonce）与幂等（idempotency）分别生效；**回环终止与幂等分别生效**；outbox 崩溃恢复 / ACK / DLQ；附件原子转存；生命周期 revoke 线性化 · 租约 · profile 事件；HMAC 双密钥轮换重叠窗口；`base_url` SSRF 拒私网；E2E 双向 gate。
