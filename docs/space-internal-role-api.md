# Space 角色查询内网接口与角色定向投递（octo-marketplace）

> **权威声明**：本文档描述 octo-server 侧为 octo-marketplace 插件上架审核流程
> 提供的三处能力：`POST /v1/auth/verify?include=context` 的 `space_roles`
> 字段、新增的单主体 Space 角色查询内网接口，以及 `POST /v1/internal/notify`
> 的**角色定向投递**（`target_role`）。权威源码：
> `modules/space/api_internal.go`（角色接口 + 令牌解析）、
> `modules/space/admin_targets.go`（`ActiveAdminUIDs`）、
> `modules/space/api.go`（`Route()` 挂载顺序）、
> `modules/notify/target_role.go` + `modules/notify/api.go`（角色定向）、
> `modules/notify/config.go`（内网令牌互斥）、
> `modules/user/api.go`（`authVerifyToken` / `queryUserSpaceContext`）、
> `main.go`（`ValidateNotifyTokenExclusions` 启动期互斥校验）。
> 两者如有出入，以代码为准。
>
> 消费方规范源头：octo-marketplace `.octospec/tasks/plugin-space-review/brief.md`。

## 0. 变更记录：管理员名单接口已删除

本分支的早期版本提供过 `GET /v1/internal/spaces/:space_id/admins`，返回某个
Space 全部 owner/admin 的 `{uid, name, role}`。**该接口已删除**，原因有二：

1. **跨租户泄漏实名**。名单复用 `MemberDetailModel.DisplayName()`，其兜底链会
   回退到 `user_verification.real_name` —— CAS / 企微 / 飞书回传的**法定姓名**。
   这条兜底链是为 `queryMembers` 设计的，受众是**同一个 Space 的成员**。把它搬
   到一个服务间接口上，等于让任何持有那把共享令牌的进程，凭一个 `space_id` 就能
   拿到任意组织领导层的 `{uid, 法定姓名, 角色}`。
2. **令牌不绑定 Space，且空数组本身是存在性预言机**。原设计声称“未知 Space 返回
   空数组所以无法探测存在性”，但每个真实 Space 至少有一个 `role=2` 的创建者，
   因此“空 vs 非空”就是存在性信号。

消费方只用它做两件事，而两件都不需要名单：

- **投递定向**（给每个管理员发审批卡）→ 改为 §4 的 `target_role`：octo-server
  自己拥有 `space_member`、自己负责投递，marketplace 根本不需要先知道名单。
- **操作人复核**（管理员点“同意/拒绝”后，回调只带一个断言的 `operator_uid`，
  没有用户令牌，因为派发是带重试与 DLQ 的异步队列）→ 改为 §3 的单主体查询。

令牌本身保留，但**已改名**（见 §5）。

## 1. `space_roles`（`POST /v1/auth/verify?include=context`）

`authVerifyTokenResp` 在 `?include=context` 分支包含：

```json
{
  "uid": "u_x",
  "context_included": true,
  "spaces": ["sp_a", "sp_b"],
  "owned_bots_by_space": { "sp_a": ["bot_1"] },
  "space_roles": { "sp_a": 1, "sp_b": 0 }
}
```

- **编码是 `space_member.role` 原生值**：`0=普通成员`、`1=管理员`、`2=拥有者`
  （`modules/space/sql/20260307000002_space_legacy01.sql`）。octo-web 自己的
  Space 模型用的是**相反**的展示编码（`1=owner, 2=admin`）——本字段不做换算，
  前端如需展示自行映射。marketplace 的审批人判定是 `role >= 1`。
- **`space_roles` 的键恒等于 `spaces` 的元素**：两者由
  `queryUserSpaceContext` 中**同一份已截断的行集合**派生。查询 `LIMIT 101`
  并在 100 处截断，截断发生在构造集合**之前**，所以不可能出现
  “map 有 101 项、slice 只有 100 项” 的漂移。
- **默认响应形状不变**：不带 `?include=context` 时 `space_roles` 与它的两个
  兄弟字段一样被 `omitempty` 抹掉，老客户端（IM / admin）看到的 schema
  逐字节不变，`TestAuthVerifyToken_NoInclude_NoNewFields` 对此有断言。
- **查询失败时 fail-soft**：`context_included` 仍为 `true`，三个集合都被赋成空
  （非 nil）值。**注意**：`omitempty` 对 nil map 与空 map 的处理**完全相同**，
  两者在线上**字节一致**；赋空值是**进程内**约定（不把 nil map 交给后续读者），
  不是消费方可以观察到的差异。消费方必须、且只能用 `context_included` 作为
  判别式。

## 2. 为什么 marketplace 还需要一次服务端角色查询

审批卡的动作回调是**异步队列**（带重试和 30 天 DLQ），回调体里只有一个由
octo-server 断言的 `operator_uid`，**没有用户令牌**可以转发。marketplace 必须在
落库前独立确认“这个 uid 现在**仍然**是该 Space 的管理员”。这是一个
**单主体**问题，不是名单问题。

## 3. `GET /v1/internal/spaces/:space_id/members/:uid/role`

### 3.1 契约

| | |
|---|---|
| 方法 / 路径 | `GET /v1/internal/spaces/:space_id/members/:uid/role` |
| 认证 | `X-Internal-Token: <OCTO_MARKETPLACE_INTERNAL_TOKEN>` |
| 用户态认证 | **无**。不走 `AuthMiddleware`，不走 Space 中间件 |
| 限流 | 每 IP 严格限流，tag `space_internal_member_role` |

成功响应（HTTP 200），**两种形状**：

```json
{ "data": { "role": 2 } }
```

```json
{ "data": { "role": null } }
```

失败响应走仓库统一的双信封 `{"error": {"code", ...}}`：

| 状态 | code | 触发条件 |
|---|---|---|
| 400 | `err.shared.param.invalid` | `space_id` 或 `uid` 为空/全空白，或长度超过 40（列宽）|
| 401 | `err.shared.auth.token_invalid` | header 缺失、令牌不匹配、或服务端未配置令牌 |
| 500 | `err.shared.internal` | 查询失败 |

### 3.2 语义约定

- **`role` 是可空整数，不是省略字段**。`0` 是一个**真实角色**（普通成员），
  必须能与“不存在”区分，所以 wire 上用显式 `null` 而不是 `omitempty`。
- **“不存在”只有一个答案**。以下情况返回**逐字节相同**的
  `{"data":{"role":null}}`：
  1. 该 uid 不是该 Space 的活跃成员（含 `status=0` 的已移除成员）；
  2. 该 `space_id` 不存在；
  3. 该 Space 已解散（`space.status != 1`；解散只翻 `space.status`、不清
     `space_member` 行，所以查询带 `INNER JOIN space ON s.status=1`）。

  用 404 或任何可区分的响应来表达其中之一，都会把一把共享服务令牌变成**跨租户
  的 Space 存在性预言机**——正是被删掉的名单接口犯的错。200 + 可空字段也与
  同类内网接口一致：`modules/internal_resolve` 的 `resolve-bot-owner` 对未知
  uid 返回 `200 {robot: 0}` 而非 404。
- **零 PII**。响应里没有 `name`、没有 `username`、没有 `short_no`、没有
  `real_name`，甚至不回显 `uid`（调用方自己传的）。SQL 只 `SELECT sm.role`，
  不 JOIN `user` / `user_verification`，所以名字**在物理上**无法从这条路径漏出。
  `TestInternalMemberRole_ResponseCarriesNoPII` 对此有断言。

### 3.3 中间件顺序

`Route()` 把限流与鉴权都挂在**具体的 GET 路由**上，`/v1/internal` 组本身不挂
任何中间件：

```go
memberRoleIPLimit := r.StrictIPRateLimitMiddleware(
    context.Background(), rlRedis, spaceMemberRoleRateLimitTag,
    sanitizedSpaceMemberRoleRPS(), sanitizedSpaceMemberRoleBurst(),
)
internal := r.Group("/v1/internal")
internal.GET(
    "/spaces/:space_id/members/:uid/role",
    memberRoleIPLimit,                       // 1. 每 IP 严格限流
    s.marketplaceInternalTokenMiddleware(),  // 2. 常量时间令牌比对
    s.getSpaceMemberRole,
)
```

Gin 会把组中间件排在路由中间件**之前**，所以“鉴权挂组、限流挂路由”实际执行
顺序是 `auth → ipLimit → handler`：令牌错误的请求会在消耗严格 IP 桶之前就
abort，撞库流量只受更宽的全局桶约束。挂在具体路由上还有第二个好处——将来在
同一个 `/v1/internal` 组下加第二个端点时，必须显式为它选择鉴权与限流，而不是
静默继承。与 `modules/internal_resolve/api.go` 的结论一致。

**这段顺序由 `modules/space/route_wiring_test.go` 读取 `api.go` 源码钉死**
（限流器已构造、组不带中间件、GET 上三个 handler 的精确顺序、路径字面量），
并额外断言被删掉的名单接口没有被复活。它取代了此前那版“自己注册再自己断言”
的假守卫——那版即使生产代码把鉴权挪到组上也依然会通过。

## 4. 角色定向投递：`POST /v1/internal/notify` 的 `target_role`

### 4.1 契约

`NotifyReq` 现在有**两种互斥**的收件人指定方式：

```jsonc
{
  "space_id": "spc_xxx",
  "service":  "marketplace",
  // 二选一，恰好一个：
  "targets":     ["uid_a", "uid_b"],   // 现状：显式名单
  "target_role": "space_admin",        // 新增：由服务端解析
  "approval_card": { /* ... */ }
}
```

- `target_role` 目前**只接受 `"space_admin"`**：该 Space 的活跃 owner + admin
  （`space_member.status=1 AND role>=1`），且 Space 本身活跃，**排除机器人**。
- **恰好一个**。都给 → 400；都不给 → 400；无法识别的取值 → 400。三者都是
  `err.shared.param.invalid`。无法识别的取值**绝不**静默兜底成默认受众。
- 首尾空白会被 trim；大小写必须精确匹配（`"SPACE_ADMIN"` 是 400）。
- `target_role` **只有 action 能力可用**，见 4.1.1。除此之外，它与
  `payload` / `card` / `docs_card` / `approval_card` 的四选一规则**正交**：
  收件人怎么指定和发什么内容是两件独立的事。

### 4.1.1 谁能用 `target_role`（凭据范围）

`POST /v1/internal/notify` 接受三类凭据（`internalAuthMiddleware`）：

| 凭据 | 来源 | 可发内容 | 可用 `target_role` |
|---|---|---|---|
| legacy | `NOTIFY_INTERNAL_TOKEN` | `payload` / `card` | **否** |
| docs | `OCTO_DOCS_NOTIFY_TOKEN` | `docs_card` | **否** |
| action | `OCTO_CARD_ACTION_ROUTES` 里各路由的 `notify_token_env` | `approval_card` | **是** |

只有第三类可用。理由：用了 `target_role` 之后 `delivered` **就是**该 Space 的
管理员名单，把它交给前两个固定令牌，等于在另一个端点上重建本次改动刚删掉的
跨租户名单能力——一个共享令牌、单次最多 200 个管理员 uid、任意 `space_id`、
调用方与该 Space 无任何成员关系。

被拒时返回 400 `err.server.notify.card_not_allowed`，且**不查库、不投递**：
角色解析在能力校验之后才发生，所以 uid 根本没离开数据库。

判定点是 `notifyCapabilityAllows`（`modules/notify/api.go`）——与既有的
`Card` / `DocsCard` / `ApprovalCard` 能力规则同一处，"哪个凭据能要求什么"
只有一个答案点。action 能力本身还被 `CanNotify` 收窄到它注册的 `action_type`，
因此实际可用面正好等于运维在 `OCTO_CARD_ACTION_ROUTES` 里配出来的那些路由。

octo-marketplace 走的正是这一类：它用自己的 per-route notify token 发
plugin-review 的 `approval_card`。**注意**它不是用
`OCTO_MARKETPLACE_INTERNAL_TOKEN`——那个令牌只授权 3. 的角色查询接口，
`modules/notify` 根本不接受它（反而把它当作需要互斥的外部值，见 5.1）。

### 4.1.2 校验顺序：校验结果不依赖租户状态

零管理员是 200（见 4.2）。因此**所有 payload / 卡片校验都必须跑在收件人解析
之前**，否则同一个畸形请求会在"有管理员的 Space"是 400、在"没有管理员的
Space"是 200：既是一个可以拿来探测别人 Space 的信号，也让生产者面对一个
"只是有时候才报错"的契约违规无法排查。

`prepareApprovalCard` 是唯一真源，`sendNotify` 的前置校验与
`deliverApprovalCardNotification` 调用的是同一个函数，因此两处不会漂移。它包含
`space_id` 可接受性、非空 `title`、`CanNotify`、以及**渲染卡片文档**（卡片
schema 错误正是在渲染时被发现的）。渲染因此挪到了 `memberCache.verify` 之前，
与 summary / docs 卡片路径既有的 C1 政策（`docs/platform-card-base.md` §10）一致。

唯一不在前置校验里的是 `len(targets) == 0`：角色路径下解析还没跑，此时它必然
为空。

### 4.2 响应：调用方靠 `delivered` 知道发给了谁

响应契约不变，仍是 `NotifyResp{delivered:[], filtered:{uid:reason}}`。用了
`target_role` 之后调用方不再知道目标集合，因此**`delivered` 就是它唯一的
投递记录**：

| `delivered` | `filtered` | 含义 |
|---|---|---|
| `["u1","u2"]` | `{}` | 两人都收到了 |
| `[]` | `{}` | **该 Space 没有活跃的人类管理员**（成功，非错误；服务端打 WARN 并带上 `space_id`）|
| `[]` | `{"u1":"send_failed"}` | 有管理员，但全部投递失败 / 被过滤 |

“零管理员”是世界的一个合法状态，返回错误只会让生产者无限重试；但它必须能与
“全部被过滤”区分开，否则就是这个特性反复制造的那类静默失败。

#### 4.2.1 残留的 Space 存在性信号（已知、已接受，未关闭）

`delivered: []` 同时覆盖“Space 不存在”“Space 已解散”“Space 存在但没有活跃的
人类管理员”三种情况。由于每个真实 Space 都有一个 `role=2` 的创建者，实践中
`delivered: []` 基本等价于“没有这个 Space”。也就是说：**能用 `target_role`
的凭据，可以拿任意 `space_id` 去探测该 Space 是否存在。**

这一点被**收窄**了但**没有被关闭**，如实记录在此：

- 收窄：4.1.1 把 `target_role` 限制到 action 能力，探测面从“三类内网凭据”
  缩到“运维在 `OCTO_CARD_ACTION_ROUTES` 里显式配出来的那些路由令牌”。legacy
  与 docs 凭据在能力校验阶段就被拒，**根本不会查库**。
- 未关闭：投递本身就必须知道这个 Space 有没有人，所以这个信号无法在保持功能
  的前提下消除。3. 的角色查询接口做得到字节级不可区分（它不投递任何东西），
  投递接口做不到。
- 与被删掉的名单接口的差别：名单接口对**任意** `space_id` 直接返回
  `{uid, name, role}`；这里泄漏的是一个布尔量，且要求调用方持有一条被
  `CanNotify` 收窄到具体 `action_type` 的路由令牌，并且每次探测都会真的给该
  Space 的管理员发一张审批卡——探测不是静默的。

> ⚠️ 留给人判断的一点：`POST /v1/internal/notify` **没有**挂
> `StrictIPRateLimitMiddleware`（只有全局 IP 桶），这一点早于本次改动。上面
> 那个布尔探测的速率因此只受全局桶约束。是否给这个内网端点补一条严格限流，
> 属于部署侧决定，不在本次改动范围内。

### 4.3 上限与截断

- 显式 `targets`：`> 200` 仍然是**调用方的错误**（行为未变）。
- `target_role`：解析结果**截断**到 200（多取一条以便检测），并打 WARN 带上
  `space_id`、`target_role`、`limit`、`resolved`。理由：管理员数量不由生产者
  决定，让一个有 201 个管理员的组织的审批通知直接失败，是生产者修不了的 DoS。
- **`actor_uid` 在截断之前就被排除。** 投递路径本来就会剔掉 `actor_uid`，所以
  先截断等于把 200 个名额里的一个花在一个随后会被丢掉的 uid 上：一个有 201 个
  管理员、且触发者恰好落在前 200 名内的 Space，只会收到 199 份投递，而第 201
  个本来合格的管理员就卡在截断线外，永远收不到这张审批卡。多取的那一条正是为
  吸收（最多一个）触发者而存在的，顺序修正后投递集合才是满的。顺带地，WARN 里
  的 `resolved` 现在统计的是"真能投递到的收件人数"，也就是运维看这条日志时想
  知道的那个数。

### 4.4 排除机器人

`space_member` 里包含 bot 行。`queryMembers`（`modules/space/db.go`）已有“过滤
非本人所有的 bot”的先例；这里没有“本人”，而且把审批卡投给一个 bot，等于把一个
机器 uid 放进审批人集合。因此 `ActiveAdminUIDs` 排除**任何**在 `robot` 表里有
行的 uid（不看 `robot.status`：软删的 bot 也不是人）。

App Bot（`app_bot` 表）不需要额外过滤：`createBot` 把 scope 写进
`app_bot.space_id`，**刻意不**把 App Bot uid 插入 `space_member`
（见 `modules/app_bot/app_bot.go:1122-1126`），因此它根本不会出现在结果里。

### 4.5 `/notify/batch` 不接受 `target_role`

批量端点一条含 `target_role` 即整批 400，与 `Card` / `DocsCard` /
`ApprovalCard` 只能走单条端点同理：角色解析是每条一次查询，50 条批量会变成
50 次无界扇出。

判定用的是**未经 trim 的原始值**：`strings.TrimSpace` 会把 `"   "` 当成
“未设置”放过去，而这里的规则是“批量里带了这个字段就整批拒绝”，不是“带了一个
能解析的 `target_role` 才拒绝”。全空白值是生产者 bug，必须按 bug 报出来。

这是批量端点在本次改动里**唯一**新增的拒绝规则；`targets` 的既有行为一字未
改，见 4.7。

### 4.6 模块耦合：notify 如何在不复制查询的前提下读到 space_member

`space_member` 的隔离谓词（`status=1`、`role>=1`、`INNER JOIN space ON
s.status=1`、排除机器人）只有一个所有者：`modules/space.ActiveAdminUIDs`
（`modules/space/admin_targets.go`）。`modules/notify` 直接调用它。

三个方案里选它的理由：

1. 把 SQL 抄一份进 `modules/notify` —— 否决。这些谓词是一条授权边界，一旦存在
   两份就会漂移，每次漂移都是一个静默的越权。`modules/notify` 已经有一处手写的
   `space_member` 查询（`space_verify.go` 的 `memberCache.refresh`）；再加一处更
   微妙的，正是不变量丢失的方式。
2. 挪进中立叶子包 `pkg/space` —— 否决。`pkg/space` 是无依赖的 channel-id 解析 +
   成员中间件辅助包，给它塞一个 dbr session 和 `space_member` 读，等于给同一份
   schema 造第二个家。
3. **从 `modules/space` 导出、由 `modules/notify` 调用** —— 采用。

**没有循环依赖**：`modules/notify` 本来就（经 `modules/user`、`modules/group`）
传递依赖 `modules/space`，而 `modules/space` 既不依赖 `modules/notify` 也不依赖
`modules/user`（`go list -deps ./modules/space` 里没有任何能绕回 notify 的
octo-server 模块）。函数签名收 `*dbr.Session` 而不是 `*space.DB`，因为
`modules/notify` 手上就是 `ctx.DB()`。

### 4.7 向后兼容

**这是纯增量改动。** docs-backend（见
`.octospec/tasks/card-message-internal-dispatch/docs-notify-contract.md`，
docs-backend 的唯一入站契约）、smart-summary 卡片试点
（`summary-notify-contract.md`）、以及各 action-card 生产者**全部只发
`targets`**（仓内没有任何测试之外的 `NotifyReq` 构造点，所以 wire 契约就是全部
兼容面）：

- `target_role` 带 `omitempty`，不设置就不出现在任何序列化里；
- `validateTargeting` 对“只有 `targets`”这一支的判定与原先的
  `binding:"required"` 完全一致；
- 角色解析在 `sendNotify` 里**一次性**完成并把结果写回 `req.Targets`，
  所以 `deliverNotification` / `deliverCardNotification` /
  `deliverDocsCardNotification` / `deliverApprovalCardNotification`
  四条投递路径**一行未改**，去重、排除 actor、`memberCache` 成员校验、
  200 上限、`delivered`/`filtered` 语义全部原样复用；
- `/notify/batch` 的 `targets` 行为**逐字节保持不变**：某一条目 `targets`
  缺失、为 `null`、或是显式 `[]` 时，该条目仍然是 207 里的**单条错误**
  （`results[i].error`），批量里其余条目照常投递。

  这里纠正了本分支早先的一版实现：它把这些形状改成了“整批 400、一条不投”，
  并在注释里声称是在“恢复” `Targets` 摘掉 `binding:"required"` 之前的行为。
  对着本仓的 validator（v10.14.0、`TagName("binding")`）实测，这个说法两处
  都不成立：

  ```
  body={}                nil=true  len=0 required_err=true
  body={"targets":[]}    nil=false len=0 required_err=false   ← 显式 [] 通过了 required
  ```

  而且 `BatchNotifyReq.Notifications` 没有 `dive` 标签，
  go-playground/validator 不会下钻到切片**元素**，所以 `NotifyReq` 自己的
  binding 标签**从来就没有**作用在批量条目上——批量里一个完全没有 `targets`
  键的条目同样能绑定成功。两种形状历来都会走到 `deliverNotification` 并以
  单条错误的形式回到 207 里；`BatchNotifyResult.Error` 存在的意义正是
  “一条坏条目不牵连其余 49 条”。

**唯一的 wire 变化**在一个错误分支上：单条 `/notify` 缺少 `targets` 时，
400 的 body 从 legacy 的 `参数格式错误` 变成 `err.shared.param.invalid`
（状态码不变，仍是 400）。这符合 `.octospec/rules/error-handling.md`；没有任何
既有生产者会发出这种请求。

## 5. 环境变量（**含一次改名，影响部署**）

| 变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `OCTO_MARKETPLACE_INTERNAL_TOKEN` | 是（否则接口全拒） | 空 | ≥32 字节；与其它固定内网令牌及所有 `OCTO_CARD_ACTION_ROUTES` 路由令牌/回调密钥两两不同 |
| `DM_SPACE_MEMBER_ROLE_IP_RPS` | 否 | `2.0` | 每 IP 令牌桶速率（≈120 次/分钟）|
| `DM_SPACE_MEMBER_ROLE_IP_BURST` | 否 | `20` | 每 IP 令牌桶突发 |

> ⚠️ **改名（部署相关）**
>
> | 旧 | 新 |
> |---|---|
> | `OCTO_MARKETPLACE_ADMIN_LIST_TOKEN` | `OCTO_MARKETPLACE_INTERNAL_TOKEN` |
> | `DM_SPACE_ADMIN_LIST_IP_RPS` | `DM_SPACE_MEMBER_ROLE_IP_RPS` |
> | `DM_SPACE_ADMIN_LIST_IP_BURST` | `DM_SPACE_MEMBER_ROLE_IP_BURST` |
> | 限流 tag `space_internal_admin_list` | `space_internal_member_role` |
>
> 令牌不再授权“列出任何东西”，用一个描述着进程并不具备的能力的变量名，本身
> 就是配置事故的邀请函；新名字与同类的 `OCTO_DRIVE_INTERNAL_TOKEN` 对齐。
> 该分支尚未合并/发布，没有需要迁移的外部消费方。**部署前必须同步改写
> marketplace 侧的环境变量名。**

限流两项都经过 `ratelimit.SanitizeRPS` / `SanitizeBurst`：
`wkhttp.ParseRPSFromEnv` 会放行 `NaN` / `+Inf`，而这两个值进到 Redis Lua 脚本
后会走 fail-open 分支，等于**静默关闭限流**。写错值只会回落到编译期默认值。

### 5.1 令牌互斥（两层，失败强度不同）

`OCTO_MARKETPLACE_INTERNAL_TOKEN` 授权的是“读取任意 Space 里任意 uid 的角色”，
一旦与别的能力共用同一个值，一次泄漏就同时拿到两种能力。两层校验都在启动期跑，
但**失败强度不同，请按实际行为部署**：

1. **与其它固定内网令牌互斥 —— 不致命：只禁用撞车的那些能力。**

   `modules/space` 的 `resolveMarketplaceInternalToken` 在构造 `Space` 时读取本
   变量，若它等于 `NOTIFY_INTERNAL_TOKEN`、`OCTO_DOCS_NOTIFY_TOKEN`、
   `OCTO_DOCS_BOT_MENTION_TOKEN` 或 `OCTO_DRIVE_INTERNAL_TOKEN` 中的任意一个，
   则**不启用**本接口：令牌落为空串，
   `marketplaceInternalTokenMiddleware` 对每一个请求返回 401，同时打一条
   **ERROR 日志**点出撞车的两个变量名。**进程照常启动，健康检查照常通过。**

   这四个持有方（`modules/space`、`modules/notify`、`modules/bot_mention`、
   `modules/internal_resolve`）都做了**镜像比较**：本变量出现在它们各自的排除
   列表里，它们的变量也出现在 `modules/space` 的排除列表里。所以一个共用值会让
   **撞车的两个能力都关掉**，而不是随机留一个还在用泄漏值对外服务。

   > ⚠️ 因此**这一层不会替你把配错的部署拦在启动之前**。撞车的表现是「接口一直
   > 401」，唯一的线索是启动时那一行 ERROR 日志。上线前请确认这四个变量（以及
   > 下面第 2 层的路由凭据）两两不同——本仓库没有实现「任意两个固定内网令牌相同
   > 就拒绝启动」的集中校验，此前版本的本节曾如此描述，与代码不符，现已更正。

   长度校验排在互斥校验**之前**——不足 32 字节的令牌无论是否撞车都不可用，
   报错信息也不应泄漏“你和 X 撞了”。

   比较是**逐字节**的，两边都用未经处理的原始环境变量值，四个模块口径一致。
   本次改动不引入任何归一化：`NOTIFY_INTERNAL_TOKEN` 与
   `OCTO_DOCS_NOTIFY_TOKEN` 是既有的生产凭据，认证中间件拿去和请求头比较的就是
   这个原值，在加载时 trim 会悄悄改变它们接受什么（配了首尾空白、客户端按字面
   值发送的部署会开始收到 401）。**从文件挂载 secret 时请注意去掉结尾换行**：
   带换行与不带换行会被判定为两个不同的值，撞车检查不会发现它们其实是同一个
   secret。

   `modules/notify` 内部 `NOTIFY_INTERNAL_TOKEN == OCTO_DOCS_NOTIFY_TOKEN` 的
   既有处理（禁用 docs、保留 legacy，不致命）保持原样，本次改动没有动它。

2. **与动态路由凭据互斥 —— 致命：拒绝启动。**

   `main.go` 的 `cardactiondispatch.Registry.ValidateNotifyTokenExclusions` 是
   唯一能同时看到 `OCTO_CARD_ACTION_ROUTES` 里各路由 `notify_token_env` /
   `secret_env` 的地方，本令牌以**限定名常量**
   （`space.MarketplaceInternalTokenEnv`）而非字符串字面量传入。它返回错误时
   `installCardActionDispatch` 直接 panic，进程起不来。
   `modules/space/main_wiring_test.go` 对这一实参做源码级断言，仓库根的
   `main_marketplace_token_test.go` 复现该实参列表验证撞车确实会被拒。

所有错误信息都不含令牌值。

## 6. 与消费方的配套配置

marketplace 侧还需要 `OCTO_MARKETPLACE_NOTIFY_TOKEN`、
`OCTO_MARKETPLACE_CARD_ACTION_SECRET` 以及一条 `OCTO_CARD_ACTION_ROUTES` 条目
（`action_type = marketplace.plugin_review.decision`）。这些属于既有的
`POST /v1/internal/notify` 与卡片动作派发链路，见
`docs/card-action-callback-consumer.md` 与 `docs/card-action-callback-dispatch.md`，
本文不重复。marketplace 的几个密钥必须 ≥32 字节且两两不同。

> ⚠️ 这条路由条目现在是**必需**的，不再只是“想发卡片才要”：`target_role` 只对
> action 能力开放（4.1.1），而 action 能力正是由这条 `OCTO_CARD_ACTION_ROUTES`
> 条目的 `notify_token_env`（即 `OCTO_MARKETPLACE_NOTIFY_TOKEN`）承载的。没有它，
> marketplace 带 `target_role` 的请求会拿到 400
> `err.server.notify.card_not_allowed`。

## 7. 关于 swagger

本仓库没有 swag/OpenAPI 代码生成，各模块的 `modules/*/swagger/api.yaml` 是手写
的**用户态**接口文档。内网接口按既有惯例不写进这些 YAML——
`/v1/internal/user/resolve-bot-owner`、`/v1/internal/notify` 同理。内网契约以
`docs/` 下的文档为准，本文即是本接口的契约文档。
