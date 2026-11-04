# 项目生命周期契约（octo-server ↔ Loop/Fleet）

本文是对端实现所依据的契约。**octo-server 侧的实现以本文为准**；本文与代码不一致时，
是本文或代码有 bug，不是「按实际情况理解」。

---

## 0. 读取接口与事件通道，各管一件事

下表中的 Project 服务间内部读取接口统一由 `modules/internal_membership` 承载。
模块名称沿用现有命名，职责覆盖 Project 相关的 internal 业务入口，包括 epochs、verify、
projects 和成员列表。具体成员读取业务保留在 `modules/project`。
成员列表 HTTP handler 直接调用 `modules/project.ListProjectMembersInSpace`，复用 Project
已有的成员读取与响应组装。原有 verify、projects 等接口继续使用现有 store，本次成员列表不增加 store 转调层。

| 通道 | 方向 | 回答的问题 | 语义 |
|---|---|---|---|
| `GET /v1/internal/membership/epochs` | 对端拉 | 「这个项目的成员关系变了吗」 | **变更检测器** |
| `POST /v1/internal/project-memberships/_verify` | 对端拉 | 「这些人现在还在这个项目里吗」 | **权威判定** |
| `GET /v1/internal/projects?space_id=...&uid=...` | 对端拉 | 「这个用户在指定 Space 有效加入了哪些已激活项目」 | **全量关系与基础信息列表** |
| `GET /v1/internal/project-memberships?space_id=...&project_id=...` | 对端拉 | 「这个 Project 的成员列表是什么」 | **与普通成员接口一致的分页列表** |
| 生命周期事件 | 我方推 | 「项目发生了什么」 | **变更通知** |

**事件不是数据同步。** 事件告诉你「有事发生了、以及发生的顺序」，需要具体内容就回来拉。
这条区分是硬约定，理由见 §5。

Project 全量列表使用现有 membership `X-Internal-Token` 及启用条件，直接返回含 `project_id`、`space_id`、`name`、`description`、`logo`、`role` 的数组，不分页。资格与项目来自同一 RR 只读快照；查询失败返回 `500`，真实空结果返回 `200 []`。完整契约与验收见 [Internal 指定用户 Project 全量列表设计](specs/2026-10-21-project-internal-user-list-design.md)。列表表示当前关系事实，不替代资源 ACL。

### Project 成员列表

`GET /v1/internal/project-memberships?space_id=...&project_id=...&page=1&limit=50`
使用相同的 internal 凭证、启用条件、共享 strict-IP 限流及读取超时。Fleet 必须先校验调用者的
Project 访问权限（包括需要时验证 owner 兜底），再发起服务间读取；该接口不接受用户或 Bot token，
也不需要 OBO。Server 在同一 RR 只读事务内校验 active Space 与 Project 的绑定并读取成员。
成员 SQL、模型和响应组装保留在 `modules/project`，internal 接口只调用其导出的读取服务。

成功响应与 `GET /v1/projects/{id}/members` 共用读取和投影：直接返回数组，附
`X-Total-Count` 和 `Cache-Control: no-store`。字段为 `uid`、`name`、`role`、`invite_uid`、
`robot`、`owner_uid`、`collaboration_roles`、`created_at`、`joined_at`，协作角色包含
`role_id`、可选 `builtin_key`、`name`、`source`。排序、成员过滤和时间格式保持一致，
空页及空角色返回 `[]`，不包装为 `{items,total}`。

`page/limit` 默认 `1/50`，上限 `100000/200`；非法或非正分页值沿用普通接口的默认值。
必填 scope 参数须单值、非空且无首尾空白，最长 256 字节；未知或重复参数、错误 query 编码返回
`400`。无效凭证返回 `401`；Project 不存在、不活跃、跨 Space 或 Space 不活跃返回 `404`；
限流返回 `429`；数据库失败返回 `500`，不返回空成功。普通成员接口的用户鉴权和权限保持原样。
成员列表是关系事实，资源授权仍使用既有 ACL / `_verify`。完整验收见
[实现说明](../.octospec/tasks/internal-project-memberships-list/brief.md)。

---

## 1. 事件传输与鉴权

| 项 | 定义 |
|---|---|
| 方法 / 编码 | `POST`，`application/json`（RFC 8259 未为该媒体类型定义 charset 参数，我方不发送它） |
| 地址 | 由我方 env 配置，绝对地址且必须含完整路径 |
| 签名 | `X-Octo-Signature`：HMAC-SHA256，密钥为该目标专用 secret |
| **签名覆盖** | `method + path + timestamp + event_id + body` |
| 不在签名内 | **query 参数**、以及 `X-Octo-Event-Version` 本身 |
| 时间戳 | `X-Octo-Timestamp`，Unix 秒，UTC |
| **幂等键** | `X-Octo-Event-ID`，同时出现在 body 的 `event_id` |
| **协议版本** | `X-Octo-Event-Version: 1` |

**两条注意**：

1. **query 参数不在签名内**，所以不要把任何有语义的参数放 query——放了也不受签名保护。
   路径必须完整写在配置的 URL 里。
2. **版本放 header 不放 body**，这样你方路由和校验不必先解 body。它**不进签名**是刻意的：
   它决定你方用哪个版本去解析请求，所以必须在验签之前可读；而且它本身不携带权限——
   被篡改的版本号只会让 body 在该版本下解析失败，不会让请求变成另一个意思。

## 2. 事件信封

```json
{
  "event_id":        "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
  "event_type":      "project.created",
  "project_id":      "<canonical uuid>",
  "space_id":        "<space id>",
  "project_version": 7,
  "occurred_at":     "2026-09-08T04:00:00.000Z",
  "payload":         { }
}
```

| 字段 | 说明 |
|---|---|
| `event_id` | 规范 UUID。**重投复用同一个值**，是你方的幂等键 |
| `project_version` | 发出事务当时的 `lifecycle_version`；不按版本排序的事件类型为 `null` |
| `occurred_at` | **业务事实发生的时间**，不是投递时间 |
| `payload` | **入队时冻结**，投递时原样发送，绝不在投递时重算 |

## 3. 五类事件

| `event_type` | 何时发出 | `project_version` | `payload` |
|---|---|---|---|
| `project.created` | 项目创建事务提交 | 有 | `{"creator_uid": "..."}` |
| `project.metadata_updated` | 项目资料变更事务提交 | 有 | `{"archive_version":12,"archived":false}`，入队事务冻结的归档轮次及状态；首次归档前轮次 0 |
| `project.archived` | 归档事务提交 | 有 | `{"reason": "..."}` |
| `project.restored` | 恢复事务提交 | 有 | `{"archive_version": 12}`，引用本次恢复对应的归档版本 |
| `project.member_revoked` | 成员被移出项目 / 被移出 Space | **无（`null`）** | `{"subject_uid": "...", "member_epoch": 12, "reason": "..."}` |

**`payload` 里没有项目名和描述，这是刻意的**，见 §5。

## 4. 排序：两个计数器，别混用

| 计数器 | 跟踪什么 | 出现在 |
|---|---|---|
| `lifecycle_version` | 关于**项目本身**的陈述（名称、描述、是否活跃） | 事件的 `project_version` |
| `member_epoch` | 关于**成员名册**的陈述 | epochs 接口、`member_revoked` 的 payload |

**两者都只增不减**，写入语句只有 `col = col + 1` 一种形状，由源码守卫强制。

**你方应当**：
- 收到 `project_version` **不大于**已持有值的生命周期事件 → **丢弃**（是重投或乱序）；
- `member_revoked` 没有 `project_version`，因为它不是关于项目本身的陈述——用 payload 里的
  `member_epoch` 与你缓存的授权比对。
- 版本推进之前还必须应用归档/恢复前序屏障（§8）。metadata_updated 要求其冻结 archive_version 等于本地 last_applied_archive_version、archived 等于本地状态；不匹配返回可重试 503 PROJECT_WORKSPACE_NOT_READY，不推进版本或固化回执。较高资料版本不能使漏投的归档/恢复变成可丢弃旧事件。

**为什么不共用一个计数器**：共用的话，每一次改名都会让下游所有缓存的授权判定失效，
而每一次成员变动又都会被当成一条生命周期陈述。两者变化频率和原因都不同。

## 5. 项目名不外发

octo-server **权威持有项目名与描述，且不通过任何出站通道发送**。

- provisioning 请求里的 `name` 字段**不是项目名**，是一个固定的低信息标签——
  **每个容器拿到的都一样**。你方 UI 直接展示它会看到所有项目一个名字。
- `project.metadata_updated` 的 payload 只有归档轮次/状态结构事实，不携带项目名称、描述或其他用户内容；它只通知资料版本变化并支持生命周期前序校验。

**理由**：项目名是用户自由输入的文本。外发会让这条通道变成内容通道，带来转义、
信息披露、以及「同一份文本在两个系统里不一致」的问题。

**你方需要展示名称时**：用 `project_id` 构造展示标签，或通过读接口取。

## 6. 重试、重放与终态

**我方行为**

- 事件在**业务事务内**入队，进程崩溃不会丢；
- lease + 指数退避；
- attempts 上限 **12**（固定值，不可配置）；超限置 `abandoned` **终态并告警**，不再重试。

**要求你方**

- **同一 `event_id` 重投，返回首次结果**（幂等）；
| 你方返回 | 我方处理 |
|---|---|
| `2xx` | 已接收。**「已应用」和「因版本过旧而忽略」都算成功**——忽略正是版本字段存在的目的，重试一条被忽略的事件会无限循环 |
| `401` / `403` | **重试**。凭据轮换先到你方、后到我方时正好是这个形状，一个部署周期内自愈；当成永久失败会在一次例行轮换中丢掉所有排队的撤权 |
| `408` / `429` | 重试 |
| `409`（下列两码） | **不重试**，终态 + 告警 |
| 其它 `4xx` | 视为**永久失败**，终态 + 告警，不重试 |
| `5xx` / 超时 / 连接失败 | 重试 |
| **`3xx`** | **重试**。我方**不跟随重定向**——签名覆盖 path，跟随会把已签名的 body 发给签名并未覆盖的主机；而 Go 会把 301/302/303 的 POST 改写成 GET 并**丢掉 body**，若目标返 2xx 就会被判成「已投递」，而你方从没收到。所以配置的 URL 必须由你方**精确地**服务那个路径，不要用尾斜杠归一化或 ingress 改写去接它 |

**两个 409（待你方确认触发条件）**

| 码 | 我方处理 |
|---|---|
| `IDEMPOTENCY_CONFLICT` | 不重试，终态 + 告警 |
| `WORKSPACE_ID_CONFLICT` | 不重试，终态 + 告警 |

> ⚠️ 这两个码的**确切触发条件需要你方给出**，我方据此实现判定。在拿到之前，我方按
> 「4xx 即永久失败」处理。

## 7. 两阶段创建（O6）

**目标**：EVA 不应该在 Fleet 尚未确认前读到「项目可用」。

```
用户创建
  └─ 事务：写项目行（status=provisioning）+ 入队 provisioning + 入队 project.created
                                    ↓ 提交
  worker ── ensure ──▶ Fleet
                     ◀── 返回 workspace_id
       校验 workspace_id == project_id
                ├─ 一致 ─▶ status=active   ← 此后 EVA 才读得到「可用」
                └─ 不一致 ─▶ 终态 + 告警，不自动重试
```

- **`workspace_id` 必须等于 `project_id`**（都是 36 位带横线规范 UUID，**按字符串比对**）。
  这是我方把 project_id 改成规范 UUID 的唯一原因——形式不统一则这个比对永远不成立。
- 超时/失败以**同一 `event_id`** 重试。
- 项目停在 `provisioning` 期间，两个入站接口对它的回答与「项目不存在」**不做区分**
  （epoch `0`、`member: false`）。
- `GET /v1/internal/projects` 同样只返回 `activated_at IS NOT NULL` 的项目；等待确认的项目不进入列表，确认后的新请求可查到。

  ⚠️ **覆盖范围的边界，明确写出来免得你方按「全部接口」理解**：这里的「两个入站接口」
  指 `/v1/internal/membership/epochs` 与 `/v1/internal/project-memberships/_verify`。
  用户态另有 `POST /v1/auth/verify`（网关每请求都调）——
  它**不受**本节的 provisioning 门控制，未确认的项目在那里会照常答 `member: true` 带角色。

  它对你方不构成缺口：那个接口的 uid 取自**调用方出示的 token**，不取自请求体，所以它
  能透露的未确认项目只会是「问的人自己已经有席位的项目」，实际上就是项目创建者本人在
  provisioning 窗口内看见自己刚建的项目。你方拿不到、也问不到第三方的答案。

  加不加这道门是产品取舍而不是正确性问题：加了之后创建者自己的客户端在 fleet 确认前
  看不见自己刚建的项目。若将来决定加，本节、`modules/project/activation.go` 的前言、
  以及迁移 `20260908000007` 的头注释三处要一起改。

**我方实现现状（本节唯一与上图有出入的地方，写在这里以免你方按图纸调试）**

- 「未确认」在我方是 `octo_project.activated_at IS NULL`，**不是第三个 status 值**。
  理由同 §8：约二十处谓词读 `status = 1` 且各自含义不同，加第三个值会一次性改变全部含义。
  **你方观察不到这个差别**——两个入站接口的回答与上图完全一致。
- 我方当前校验的是「目标回显的容器 id == 我方要求创建的 id」，`internal/projectprovision`
  在不一致时永久失败并告警。这与「`workspace_id == project_id`」是**同一个校验**，
  只是两个 id 目前还不是同一个值：fleet target 的容器 id 仍是 PR #850 的随机不可推导形式，
  它是**临时形态**，待你方 workspace 门收窄（V1.1 §3.2/§3.3）落地后改为 `project_id`。
  届时我方**这一处代码不变**，两个 id 自然相等，校验就字面等同于本节的写法。
  在那之前把它改成 `project_id` 会打开一条已知的提权链路（同 Space 成员列出 project_id →
  当 workspace_id 用 → fleet 按 Space 成员放行并把调用者物化成 workspace 成员），
  所以这一步的顺序是**你方先收窄，我方后换 id**。
- fleet target 未开启时，新项目在**创建事务内直接置位** `activated_at`。否则没有任何东西
  会去确认它，项目将对你方永久不可见。
  - 滚动升级期间由旧二进制写入、因而漏掉这次置位的行，由对账扫描补上——**最长晚一个
    对账周期**，不是在创建事务内。
  - 「fleet target 已配置但**启动时被拒**」（URL 非法、密钥与其它能力冲突）期间新建的项目
    **会被补置**，而不是继续等待。本文早先写的是相反的承诺（「改一个环境变量它就能重新
    确认」），那是错的：被拒时 fleet 不在生效 target 列表里，这些项目**根本没有 fleet 工单**，
    改回环境变量也不会补出来，两条修复路径都够不着——继续等待的实际含义是**永久不可见**。
    补置的代价是「容器还没建好时对端就先看见了」，与今天已经存在的那个窗口同形，
    比永久不可见轻。
  - 判定依据是**每个项目自己有没有 fleet 工单**，不是这个进程的配置。这一条是为了滚动
    启用 fleet 的场景：新旧 pod 并存时，旧 pod 的配置里没有 fleet，若按进程配置判定，
    它会把新 pod 刚建的、工单还在跑的项目一起闩上。

## 8. 归档 / 恢复（OCTO-59 目标契约）

本节记录 2026-11-03 确认的设计与 Server/Fleet 接口约定；源码实现及验收、部署状态分别以代码变更和发布记录为准。完整用户 API、状态字段和验收见 [工作空间转让、退出与归档：Server 契约](specs/2026-11-03-workspace-transfer-leave-archive-design.md)；Fleet 按其配套消费规格实施。

- 归档是可逆的只读状态。`octo_project.archived_at` 独立于现有 normal/disbanded `status`，成员与历史保留。
- 归档期间 `_verify` 和 epochs 继续返回真实成员与实际 `member_epoch`；归档本身不使成员变为不存在。账号、Space 与真实成员撤权仍按既有资格裁决。
- 归档期间允许当前 owner 转让给空间内有效真人 admin/member，转让后唯一 owner、原 owner 成为 admin，归档状态不变；有效非 owner 成员可自行退出，owner 必须先转让。转让/退出沿现有成员 API，HTTP 与最终事务按当前身份校验。
- 真实归档与恢复分别递增 `lifecycle_version`，同事务写入 `project.archived` / `project.restored` outbox；重复目标状态不增版本或重复事件。`member_epoch` 不因归档或恢复变化。
- `archive_version` 记录最近一次归档时的生命周期版本，支撑成员级提醒关闭；恢复保留该轮次，再次归档替换为新版本。
- 用户 API 为 `PUT /v1/projects/:id/archive`（`{"archived":true|false}`，仅当前有效真人 owner；幂等重复不增版本）、`GET /v1/space/:space_id/projects?archive_status=active|archived|all`（默认 active）及 `GET /v1/space/:space_id/projects/archive-notices`；详情、列表和内部用户项目关系包含 lifecycle/archive 字段。归档期间仅 owner 可恢复，成员可关闭本人当前轮次提醒，旧轮次关闭冲突；pin 与提醒偏好互不覆盖。
- 内部 `GET /v1/internal/projects/:project_id/lifecycle?space_id=...` 使用既有 membership internal token、strict-IP 限流及请求超时；所有响应 `Cache-Control: no-store`。成功仅返回 `project_id`、`space_id`、`lifecycle_version`、`archive_version`、`archived_at`。仅有效、已激活且父 Space 活跃的 normal Project 返回 200；不存在/跨 Space/未激活/父 Space 不可用返回 404，凭证失败 401，scope 参数无效 400，存储失败 500。该接口不要求成员席位且不携带项目内容或用户数据。
- 归档/恢复要求 Project 写开关与生命周期事件集成有效、匹配的 Fleet Workspace 已就绪；失败不改变状态。归档不改变 `member_epoch`。Fleet 新业务写与执行准入需校验权威生命周期状态，并要求本地已应用版本追平；不能仅依赖延迟到达的事件或成员资格缓存。权威读取不推进消费版本。归档事务成功后记录 last_applied_archive_version；恢复必须匹配 payload.archive_version，缺少前序归档时回滚该次事件接收事务并返回可重试 503，不推进版本或固化失败回执。快速恢复及重试终态都不能越过归档副作用。
- metadata_updated 及其他版本推进不能跨越尚未应用的归档/恢复。即使归档/恢复投递耗尽，后续资料事件也须满足冻结轮次/状态与本地副作用一致，否则可重试 503，不抬高 external_version。启用前排空旧空 payload 事件并退出旧实例；不能投递时改写已冻结 payload。
- Fleet 取消本工作空间排队和运行中的未完成任务、撤销任务凭据；仅本空间专属 Runtime 可离线，共享 Runtime 不影响其他空间。归档保留每条自动化 status 和触发器 enabled，workspace 状态提供额外执行门禁；恢复按原开关开放执行并重算未来调度时间，不补跑，不复活已取消执行或凭据。
- Drive 仅将 Project 同步分量限制为只读，退出只撤销该来源；独立授权保留。Docs 合并自身独立角色、分享与 Drive reader，来源失效通知加活跃绑定批量版本探测处理已打开连接，不把 Project 归档写成整个文档归档。


## 9. 成员判定与生命周期变化的失效边界

成员资格变化与工作空间读写状态分别排序；授权缓存失效不能替代生命周期写入门禁。

| 变化 | `member_epoch` 会动吗 | 对端如何感知 |
|---|---|---|
| 项目成员增删 / 改角色 | ✅ 同事务 +1 | epoch 变 |
| 项目解散 | ✅ 先 bump 再翻 status | 随后从 epochs 掉出，读作 `0` |
| **Space 封禁 / 解封** | ❌ 不动 | epochs 的谓词把父 Space 不活跃的项目折成 `0`；解封后恢复原值 |
| **Space 解散** | ❌ 不动 | 同上。注意本仓**不会**级联解散其下项目 |
| **Space 成员移除** | ✅ **同事务 +1** | 关席位是异步的，但 epoch 在移除提交时就动 |
| 归档 / 恢复 | ❌ 不动 | 成员事实及 epoch 保留；以 lifecycle_version、权威状态读取和既有事件约束业务写与执行准入（见 §8） |
| **项目由未确认变为已确认**（两阶段创建置位 `activated_at`） | ❌ 不动 | 置位前 epochs 的谓词不命中，读作 `0`；置位后下一次轮询读到真实 epoch（自创建起就是 1）。**置位不发事件，也不动 `lifecycle_version`** —— 与 Space 封禁/解封同形，靠你方的周期性 epochs 轮询在一个缓存窗口内收敛 |

成员资格变化使用 epoch 检测与有限期成员核验；归档读写限制使用 §8 的独立生命周期准入。账号封禁/销毁不会推进 Project epoch，版本相等不能延长旧成员事实。本次 Drive/Docs peer 上界为 30 秒，Drive 成员结果最长 15 秒；新判权的 epochs 与 lifecycle 读取 no-store，_verify/epochs 分批分别最多 50 UID/50 Project。

Server→Drive 来源提示复用 Drive 内部 token，目标 URL 由 `OCTO_PROJECT_DRIVE_ACCESS_CHANGED_URL` 配置，通知接口为 POST /internal/v1/docs/project-access-changed，body 为 event_id、space_id、project_id、subject_uid、member_epoch。既有 member_revoked 出站完成 Fleet 本次投递与状态记录后旁路提示；Drive 不是现有单目标 outbox 的第二个可靠消费者，提示失败不改变 Fleet 回执/重试/顺序。权威事实查询及有限期证明负责通知缺失时收敛；详见 Server、Drive、Docs 的 2026-11-03 配套规格。

## 10. 未定项

| | 需要你方给 |
|---|---|
| 1 | 两个 409 的确切触发条件（§6） |
| 2 | ensure 接口的联调地址 + Mock 契约 |
| 3 | 两个入站接口「未配置 token」返回 401（合同原写 503）——需确认你方把 401 当**可重试** |
| 4 | `_verify` 的 URI 目前是临时路径，改动时我方提前通知 |
