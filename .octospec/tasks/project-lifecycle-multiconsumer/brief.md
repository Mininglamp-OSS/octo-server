---
type: Task
title: "Task: project-lifecycle-multiconsumer"
description: 生命周期事件从单消费方扩成多消费方 + 冻结事件目录 + per-consumer 凭据；作用对象在未合并 PR 上，等合并后开工
tags: [project, wire-contract, space, isolation, auth, test]
timestamp: 2026-09-09T00:00:00Z
slug: project-lifecycle-multiconsumer
upstream: 口头需求（#852 / #854 / #855 / #864 合并预演后的实测结论）
source: self
---

# Task: project-lifecycle-multiconsumer

> **本 brief 是 `project-subsystem-fanout` 拆出的阻塞部分**（2026-09-09）。那份任务
> 已完成并收窄为 R10 + R8-provisioning 两条；本份承接**作用对象尚不存在**的 9 条
> （R1–R7 / R9 / R11 / R12，编号沿用不重排，便于与原 brief 和 PR 讨论对照）。
>
> **为什么不能现在做**：对照 `main@566e625b` 逐条核实，下列代码在 main 上**零命中**——
> `octo_project_lifecycle_event` 表与 worker、`activation.go` / `confirmProjectActive`、
> `modules/internal_membership`、`TestEverySpaceMemberWriterIsAccountedFor`。
> 硬写等于把 #852/#864 重新实现一遍，且几乎必然与它们的真实实现分叉、合并时全冲突。
>
> **开工条件**：按 §实现顺序前提合并 PR。#855 已于 2026-09-09 合入 main（`566e625b`），
> 分身席位代码已就位，故 R5 的作用对象**部分已到**（仍缺 #864 的事件通道）。
>
> **下文标 `[实测]` 的结论**来自 2026-09-08 的合并预演：四个 PR 合成一棵树，真实
> MySQL 8.0 + Redis + WuKongIM + 三个会验签的 HTTP mock peer + **真实后台 worker
> 定时器**，7 个场景 6 通过 1 失败。测试源码 `.context/merge-dryrun/subsystem_e2e_test.go.txt`
> 在**预演所在 workspace**，本仓没有副本——实施前须取回（R5 复现基线依赖它）。
>
> **PR 拓扑（2026-09-09 merge-tree 核实）**：#852 / #854 / #864 均 base main、对 main
> 干净无冲突。#864 的 GitHub base 指向 #852 分支（堆叠残留），CONFLICTING 实际只有
> `.octospec/log.md`；#852↔#864 是共享底层（epoch 修复系列）的两条 lane。
> **#853 本次迭代不合入**——已核实无人依赖它（#852 自带 `main.go:833-905` 的
> `fixedInternalTokenEnvs` 9 条 + `main_internaltoken_test.go`，零 import
> `pkg/internaltoken`；#854/#855/#864 同样零引用），故不阻塞。但它**若在未来合入**，
> 见 §实现顺序前提第 3 条的前置条件，否则凭据覆盖面会 9→4 静默退化。

## Goal

把项目生命周期事件从**单消费方硬绑定**改成多消费方投递。现状
`octo_project_lifecycle_event` 表**没有 consumer 维度**，配置只有一组 URL/secret/开关，
结构上不可能接第二个对端。

外加一件必须同时做的事：**冻结事件目录**（§事件目录），并补上目录里当前缺的那条
（分身撤销 = R5），否则多消费方只是把一个已知缺陷复制 N 份。

**范围权威清单见 §必须实现（MUST）**，R1–R12 编号与原 brief 一致（R8 与 R10 已在
`project-subsystem-fanout` 落地，本份只承接 R8 的 lifecycle 半边）。

## 必须实现（MUST）

> 每条 = 是什么 / 为什么必须 / 验收锚点。实现形态未注明的由实现者定，但范围不得裁剪。

**R1 · 出站事件多消费方化（consumer 维度）**
`octo_project_lifecycle_event` 加 `consumer` 列；`UNIQUE (event_id)` 改为
`UNIQUE (consumer, event_id)`；`status / attempts / next_attempt_at / lease_owner /
lease_until / finished_at` 全部 per-consumer。每个启用消费方一行。
*为什么*：现状整张投递状态机全局单份，一方成功一方失败时同一行无法既是 delivered 又是 pending。
*验收*：同一事件集合上，消费方 A `delivered` 且 B `pending` 共存的测试。

**R2 · 业务事务内同事务 fan-out**
一次业务变更为 N 个启用消费方在同一事务内写 N 行。「变更提交 ⟹ 事件存在」必须靠构造成立。
*验收*：删掉任一消费方的入队调用 → 测试红。

**R3 · N 行共享同一份 payload 字节**
入队时 `json.Marshal` 一次，N 行引用同一份字节，投递时原样重放；守卫
`TestPayloadIsFrozenAtEnqueue` 扩展覆盖 fan-out 形状（worker 不做 `json.Marshal` 的断言保持）。
*注意*：现有守卫只覆盖 `lifecycle_event_worker.go` 一个文件；扩展时不得缩窄既有覆盖。
*验收*：变异验证——让 worker 重算 payload → 守卫必须红。

**R4 · worker 阻塞键与认领谓词 consumer 化**
现状的 per-project 阻塞**不在 worker 内存里，而在 claim SQL 的 `NOT EXISTS` 谓词**
（`db_lifecycle_event.go:97-99`：同 `project_id` 存在更早 pending 行则不可认领——任一时刻
每个项目最多一行 pending 可被 claim）。多消费方后该谓词的 `older` 子查询必须加
`consumer = e.consumer`；worker 阻塞键为 `(consumer, project_id)`；表的 **4 条**二级索引
（pending / project / finished / **age**）按 consumer 前导重新评估——其中 3 条以 status
前导，会退化成 per-consumer 扫描。
*验收*：消费方 A 的队头 503 时，消费方 B 的同项目事件仍在同一轮投出。

**R5 · 分身 member_revoked（D-1 落地）**
`#855` 的 agent 循环（`beginRemovalWithAgentsTx`，kick/leave/CascadeTx 转发三入口）与
Space 级联 agent 循环里，对**每个关闭的分身席位**各入队一条 `member_revoked`。
`member_epoch` 用**同一次名册变化的 bump 后值**——#855 的语义是一人 + N 分身只 bump 一次。
事件 `subject_uid` 集合 == 该次名册变化关闭的全部席位。
*为什么必须*：分身是代替主人在对端执行的主体，契约 §4 要求撤销迟到也必须应用。
`[实测]` Phase7 失败已代码级坐实：#864 的 `enqueueLifecycleEventTx` 全部 4 个调用点
（创建 / 资料变更 / 踢退 / Space 级联）都在人的路径上，分身路径零入队——任一 PR 单独看
都不存在该缺陷，合并树才有。
*验收*：带分身的成员经踢出 / 退出 / Space 移除三条路径离开后，撤销事件 subject 集合断言；
Phase7 基线变绿 + 改回旧代码重新变红（变异验证）。

**R6 · 可见性判定按 D-4 落地**
先定义**必需子系统集合**（建议：fleet 必需、drive 可选），可见性 = 全部必需子系统确认。
门控 SQL 保持只写一遍（`pkg/project/membership.go` 的 `activated_at IS NOT NULL` 谓词，
两个入站接口共用）；fleet target 未启用时维持创建事务内置位 + 两条补偿路径
（`latchUnconfirmableProjects` / `repairConfirmedButUnlatched`）。
*验收*：「必需子系统未确认 → 两接口答 epoch 0 / member:false」与「全部必需确认 → 翻转」两方向。

**R7 · 事件通道 nudge + 可配轮询（D-5 落地）**
`lifecycleEventPollInterval = 5s` 常量改可配；业务提交后 nudge lifecycle worker
（对齐 provisioning 侧 `nudgeProvisioningFn` 的 instance-seam 形态）。现状撤销推送延迟
下限 5s、吞吐 ≈2 条/秒（batch 10、串行 POST），批量踢人是真实队列。
*验收*：业务写到对端收到的 p99 延迟断言。

**R8-lifecycle · abandoned 人工重驱（lifecycle 半边；provisioning 半边已在
`project-subsystem-fanout` 落地）**
lifecycle 事件的 abandoned = 对端**永久漏掉该事件**，现状无任何自动或人工恢复路径
（`requeue|re-drive` 在 #854 树里只出现在注释）。要求：abandoned → 重驱 → pending →
delivered。

*形态已定：env，启动时生效*——**沿用 provisioning 半边的形状，不要引入 HTTP 端点**。
原 brief 要求「i18n envelope + auth + 审计」，实施时发现那与守卫
`TestNoHandlerReachesTheProvisioningTable`（执行 D12）直接冲突：HTTP 端点必然带 handler，
而该守卫禁止任何带 handler 的文件碰供给表或其 DAO。D12 真实禁的是**用供给行门控读路径**，
而重驱是运维触发的**写**，不门控任何用户可见的读——env 形态是绕开冲突而非削弱守卫
（已在 provisioning 半边验证该守卫仍通过）。lifecycle 半边照抄该结论，别重新踩一遍。

**幂等键不变**：`event_id` 绝不重发（对端在 event_id 后面对 payload 做指纹，同 id 不同
内容会被判永久冲突）。**`attempts` 必须重置为 0**，否则 sweep 会在任何投递发出前把行
重新判死（provisioning 半边已用变异验证坐实这条）。`last_error` 追加不覆盖。

**多消费方后是 per-consumer 重驱**：只重驱指定 consumer 的行，不能把所有消费方的
abandoned 一起推回——一方对端仍然挂着时，全量重驱只会再走一遍 12 次失败。
*验收*：跑通 abandoned → 重驱 → delivered；per-consumer 隔离断言（A 重驱不动 B）。

**R9 · per-consumer 凭据（D-7 落地）**
入站：`OCTO_MEMBERSHIP_INTERNAL_TOKEN` 单值 → JSON registry（抄 `OCTO_BOT_TASK_SOURCES`
形态：per-consumer token + enabled + 32 字节地板 + `DisallowUnknownFields`）。
#852 已把「credential-scoped quota 需要 one-shared-token 契约不具备的 per-consumer 身份」
写成已知限制（其正文 :125 与 `config.go:107-142`），本任务兑现它。
出站：event secret 与 per-target ensure secret 保持独立。
**三重把关必须同步更新**：(a) main.go 的 `fixedInternalTokenEnvs`（#852 自带，
`main.go:833-905`，9 条 + `main_internaltoken_test.go` sweep 门禁）——**#853 本迭代不合入，
不要去找 `pkg/internaltoken`，它在 main 上不存在**；(b) 各模块本地拒绝列表；
(c) 全树 credential 字面量 sweep。撞值 fail-closed 对齐 #864 先例
（`resolveLifecycleEventSecret` 撞值即关停 feed，非仅日志）。
*验收*：per-consumer 配额、审计区分、单方轮换各一条测试；撞值拒绝测试。


**R11 · D-2 择一落地 + sweep baseline 对齐 #855**
无论 D-2 怎么拍（§待决策），`TestEverySpaceMemberWriterIsAccountedFor` 必须**变绿**：
#855 新增的 `modules/space/member_removal_all_spaces.go` 含 1 个 space_member 写点，
在 #852/#864 树上必红（baseline 补行）；#852 baseline 里 botfather 三处 KNOWN GAP 的
理由文本已因 #855 接上工单而过期（`reason=bot_deleted` 压制 Tip，非绕开 outbox），同步更新。
*验收*：sweep 绿 + baseline 理由与代码事实一致。

**R12 · 契约文档同步**
`docs/project-lifecycle-contract.md` 同步：事件目录、多消费方语义、per-consumer 幂等边界、
**失败分类精确表**（5xx/网络/401/403/408/429/3xx 可重试；409 与其余 4xx 终态——契约
「4xx 即永久失败」的兜底语已落后于实现，必须改）、延迟与吞吐特征（R7 结论）、
项目归档入站读谓词盲区（§Out of scope 第 2 条）列为已知限制。

## Background

### 三层能力的支持度现状

| 层 | 通道 | 多子系统 | 依据 |
|---|---|---|---|
| 资源初始化 | `octo_project_provisioning` + ensure HTTP | 🟡 **数据模型支持，配置不支持** | 表 `UNIQUE (project_id, target)` 每 target 一行（另有 `UNIQUE(container_id)`）；但 **URL/secret/narrowed/reclaim 是 per-target 进程 env（`provisionTargetEnvs`），不是行上的列**；`allProvisionTargetNames() = {fleet, drive}` 与两处 switch 硬编码（`config_provisioning.go:412`、`targetEnvNames`、`provisioning.go:43`） |
| 入站权威判定 | `GET /v1/internal/membership/epochs`、`POST /v1/internal/project-memberships/_verify`（#852） | 🟡 **多消费方可用，但共享一个凭据** | 只有 `OCTO_MEMBERSHIP_INTERNAL_TOKEN` 一个值（`subtle.ConstantTimeCompare`，无 consumer 维度）。#852 已把 per-consumer 配额/审计缺口写成已知限制 |
| 出站事件 | `octo_project_lifecycle_event` + HMAC POST（#864） | 🔴 **单消费方硬绑定** | 表无 consumer 列；`UNIQUE(event_id)` 单列；status/attempts/lease 全局单份；认领谓词按 `project_id`；`activation.go:165` `if target != TargetFleet { return }`；契约文档标题双边（`octo-server ↔ Loop/Fleet`） |

### `[实测]` 已经能用的部分

| 场景 | 结论 |
|---|---|
| 双子系统初始化 | fleet + drive 两个 ensure 都真的被调用，**各自 secret 的签名在对端验签通过**，两行都到 `ready` |
| 两阶段创建 | fleet 未确认时，两个入站接口对该项目答 `epoch 0` / `member:false`；fleet 恢复后翻转为真实 epoch / `member:true` |
| 事件出站 | 经**真实 HTTP client**投出，签名可验，header `X-Octo-Event-ID`（大写 ID）== 库里幂等键，`member_revoked` 不带 `project_version`（显式 `null`，故意不加 omitempty——省略键会被按 §2 校验的对端回 4xx，而 4xx 在此多为终态）、payload 的 `member_epoch` 与 epochs 接口报的值一致 |
| 资料变更 | 改名 → `metadata_updated` + `lifecycle_version` 递增；同名重复 `PUT` → 不写库不发事件 |
| 失败分类 | peer `503` → 保持 `pending` 可重试；peer `409` 或其余多数 4xx → `abandoned` 终态。**精确表**：5xx/网络/401/403/408/429/3xx 可重试，409 与其余 4xx 终态（`lifecycle_client.go:293-342`） |
| 故障隔离 | drive 挂：不拖累 fleet、不影响 `activated_at`、不影响事件通道 |

### `[实测]` 失败的那一条

```
TestE2E_Phase7_AgentPulledOutWithItsOwnerIsNotPushedToThePeer
  revocation subjects the peer was told about: map[p7owner:true]
```

主人被移出 Space 后，主人和它的分身的项目席位在**同一事务**里都关闭了（前置断言已验证
「两个席位都关」），但对端只收到主人一条 `member_revoked`。根因（已代码级坐实）：
#864 的 4 个 `enqueueLifecycleEventTx` 调用点全在人的路径上；#855 的 agent 循环
（`beginRemovalWithAgentsTx` service.go:1913-1943 与 Space 级联
`space_member_removal.go:346-376`）只有 `beginMemberRemovalTx` + `enqueueRemovalJobTx`，
没有事件入队。**这个缺陷在任一 PR 单独看都不存在**——#864 那会儿没有分身概念，
#855 那会儿没有事件通道，两边测试各自全绿。授权正确性还在（epoch 变了，对端回来拉
`_verify` 会发现分身也没了），丢的是**推送**；而分身正是「代替主人在对端跑东西」的主体。

## 现状：合并四个 PR 后，一次建项目实际发生什么

```
POST /v1/space/{space_id}/projects
  │
  ├─ 事务 T1（锁序 space_member 行 S 锁 → space 行 X 锁 → 配额 COUNT ×3
  │            → octo_project → octo_project_member（人 + 分身）
  │            → octo_project_lifecycle_event → octo_project_provisioning 最后一条）
  │    ① INSERT octo_project（project_id = 36 位规范 UUID；
  │       activated_at = NULL 当且仅当 fleet target 已启用，service.go:452-455）
  │    ② INSERT 创建者 owner 席位
  │    ③ INSERT 创建者自己的分身席位（#855，role=0，计入 max_members）
  │    ④ bump member_epoch → 1（#852：创建即 bump，bumped==0 整单失败；
  │       0 是对外契约「项目不存在」的保留哨兵）
  │    ⑤ bump lifecycle_version → 1（两计数器写入都只有 col = col + 1 形状，源码守卫强制）
  │    ⑥ INSERT octo_project_lifecycle_event（project.created，payload 入队即冻结）
  │    ⑦ INSERT octo_project_provisioning × 每个启用的 target ← 锁序要求它是最后一条
  │  COMMIT
  │
  ├─ 提交后（不在事务内，失败不回滚创建）
  │    ⑧ 全员群 hook（#855）：modules/group 开自己的事务建群 + IM channel
  │    ⑨ nudgeProvisioningWorker()：立刻跑一轮供给（此外定时器 15s 一轮，带 jitter）
  │
  ├─ provisioning worker：每 target 一次 POST {ensure_url}
  │       body   {container_id, project_id, octo_space_id, name, issue_prefix?}
  │              （name 固定常量 "octo-project"；issue_prefix 永不设置）
  │       签名   X-Octo-Signature: "v1=" + hex(HMAC-SHA256(该 target 专属 secret,
  │                 "v1\n" + METHOD + "\n" + path + "\n" + timestamp + "\n"
  │                 + event_id + "\n" + lowercase_hex(sha256(body))))
  │                 event_id = lowercase_hex(sha256(container_id))
  │                 （6 段 \n 连接、body 取摘要而非原文——规范串见
  │                  internal/projectprovision/conformance.go:28-41，勿凭记忆实现）
  │       校验回显 container_id 一致 → status=ready；回空 {} → 重试（#854 修复）；
  │       回不同 id → 永久失败 + 告警
  │       fleet 且 ready → confirmProjectActive() 置位 activated_at
  │                        ← 此后对端才「看得到」这个项目（activation.go 只认 fleet）
  │
  └─ lifecycle worker：5s 常量轮询、batch 10、串行 POST、无 nudge
        认领谓词含 NOT EXISTS：任一时刻每项目最多一行 pending 可被认领
        5xx/网络/401/403/408/429/3xx → 退避重试；409 与其余 4xx → abandoned；
        attempts ≥ 12 → abandoned（另有 5 分钟一次的 sweepExhaustedLifecycleEvents）
```

**`name` 字段不是项目名**，是每个容器都一样的固定低信息标签——项目名与描述由
octo-server 权威持有，**不经任何出站通道外发**（契约 §5）。

**容器 id 当前不可推导**（`octows-` / `octods-` + 16 字节随机），不是 `project_id`。
这是 #850 的临时防护：同 Space 成员能列出 `project_id`，而 fleet 的 workspace 门目前按
Space 成员放行并把调用者物化成永久 workspace 成员。顺序是**对端先收窄、我方后换 id**
（决策注释由 #854 加在 `provisioning.go:71-93`；届时仅 fleet 换 id，guard 收窄到 drive）。

## 事件目录（冻结）

信封：`{event_id, event_type, project_id, space_id, project_version?, occurred_at, payload}`；
`event_id` 是规范 UUID，重投复用同值，是对端的幂等键。

| `event_type` | 触发点（代码） | `project_version` | payload | 状态 |
|---|---|---|---|---|
| `project.created` | `createProjectOnce` 事务内（bump 两计数器之后） | 有 | `{creator_uid}` | ✅ `[实测]` 投递成功 |
| `project.metadata_updated` | 资料变更事务内（**仅当有字段实际变化**，`len(set)==0` 短路） | 有 | `{}`（空对象非 null） | ✅ `[实测]` |
| `project.member_revoked` | `beginRemovalWithCascadeTx`（#864；#855 合并后为 `beginRemovalWithAgentsTx`，kick/leave/Cascade 转发）、Space 级联工单内（`if changed`） | **无（显式 null）** | `{subject_uid, member_epoch, reason}` | ⚠️ `[实测]` 人的有、**分身的没有**（R5 修复） |
| `project.archived` | — | 有 | `{reason}` | ⛔ RESERVED，无人发出 |
| `project.restored` | — | 有 | `{}` | ⛔ RESERVED，无人发出 |

**两个计数器不可混用**：`lifecycle_version` 排序「关于项目本身」的陈述（出现在
`project_version`）；`member_epoch` 排序「关于名册」的陈述（出现在 epochs 接口和
`member_revoked` 的 payload）。两者都只增不减，写入语句只有 `col = col + 1` 一种形状，
由源码守卫强制（main 上是 `TestMemberEpochOnlyEverIncrements` 正则 sweep；
`TestEverySpaceMemberWriterIsAccountedFor` 是 #852/#864 引入的 writer-baseline 双向断言）。
共用一个计数器会让每次改名失效下游所有缓存授权。

**bump 语义**（实现 R5 时必须遵守）：每一次真实影响行的成员/角色写入 bump **一次**
（条件 `RowsAffected > 0`）——一人 + N 分身同批关闭只 bump 一次，分身事件的
`member_epoch` 必须取这同一次 bump 后的值。

**会改变成员判定但不动 `member_epoch` 的情况**（契约 §9，逐条核过）：Space 封禁/解封、
**Space** 解散、项目归档/恢复——由 epochs 谓词「父 Space 不活跃 → 折成 0」覆盖，
不靠 epoch 移动。注意区分：**项目解散**会 bump（先 bump 再翻 status，契约 §9 表格 ✅），
只有 **Space** 解散才不动。本仓**不会**级联解散 Space 之下的项目。

**拆除是 pull 而非 push**：项目解散把供给行置为 `disband_pending`（status=3），子系统靠轮询
`POST /v1/internal/projects/status` 得知，出站不推任何东西。清理这些行由
`OCTO_PROJECT_PROVISION_{FLEET,DRIVE}_RECLAIM_CONSUMER_LIVE` 逐 target 把关——删行是本片
唯一不可逆操作，不能建立在假设上。（这两个 env 是 #850 已合并的产物，非 #854 引入。）

## Load-bearing list

- **`octo_project_lifecycle_event` 的唯一键与租约语义。** 加 consumer 维度要把
  `UNIQUE (event_id)` 改成 `(consumer, event_id)`，并让 `status/attempts/next_attempt_at/
  lease_owner/lease_until/finished_at` 变成 per-consumer。现有 **4 条**二级索引
  （pending(status,next_attempt_at,lease_until) / project(project_id,id) /
  finished(status,finished_at) / age(status,created_at)）都要按 consumer 前导重新评估——
  其中 3 条以 status 前导，否则「稳态即最坏情况」的全表扫描会回来。 touches: `wire-contract`
- **事件在业务事务内入队。** 「变更提交 ⟹ 事件存在」是靠构造成立的，不是靠发布者记得。
  fan-out 写多行必须仍在同一事务内。 touches: `wire-contract`
- **payload 入队即冻结、投递时原样重放。** 对端在 `event_id` 后面对 payload 做指纹，
  同 id 不同内容会被判永久冲突。fan-out 的 N 行必须共享同一份序列化字节，绝不能在投递时重算。
  守卫 `TestPayloadIsFrozenAtEnqueue` 目前只钉 `lifecycle_event_worker.go` 一个文件，
  fan-out 后扩展之。 touches: `wire-contract`, `test`
- **per-project 阻塞在 claim SQL 里，不在 worker 内存。** `NOT EXISTS` 谓词保证任一时刻
  每项目最多一行 pending 可认领（防 `member_revoked` 越序被对端判 4xx 永久放弃）。多消费方后
  该谓词与阻塞键必须变 `(consumer, project_id)`，否则一个慢消费方会挡住其他所有消费方的
  同项目事件。#864 正文 :82 对此的 in-loop 描述是过时写法，勿据此实现。 touches: `wire-contract`
- **签名层零重写。** `pkg/octosign` 的规范串（`v1\n` 六段 + body 摘要 + `v1=` header 前缀）
  有 conformance vectors 钉住，R1–R4 不触碰它；契约 §1 的表述「method+path+timestamp+
  event_id+body」是字段清单不是拼接式，实现接收端一律以 conformance 文件为准。
  touches: `wire-contract`
- **`activated_at` 只认 fleet。** `confirmProjectActive` 里 `if target != TargetFleet { return }`
  （`activation.go:165`），且 `twoPhaseCreateApplies()` 是第二处「只认 fleet」判定。
  多子系统后必须先定义「可见」是任一必需子系统确认、还是全部确认——这会改变对端可见时机，
  是产品决策（D-4）。fleet target 未启用时 `activated_at` 在创建事务内直接置位，
  并有滚动升级补偿路径。 touches: `wire-contract`, `isolation`
- **两个入站接口的读序与谓词一致性。** `ProjectEpochsInSpace` 两步（项目行 → Space 状态
  fail-closed）；`ProjectMemberships` 四步（复用它作第一步 → 项目席位 → Space 活跃成员 →
  账号存活，三个「只能收窄」的读在最后）。两个谓词必须对「什么算在答案里」保持一致——
  #852 review round 5-8 有一轮就是因为只给其中一个加了 Space 条件，导致封禁后授权不失效、
  解封后永久拒绝（正文自认 unban 方向是本分支引入的回归）。 touches: `auth`, `isolation`, `space`
- **每个能关闭 `space_member` 席位的 writer 都必须在同事务 bump `member_epoch`。**
  该规则与 sweep guard 只存在于 #852/#864（main 上尚无）；#855 已合入 main，其
  `modules/space/member_removal_all_spaces.go` 将是第一个未登记 writer（R11）。
  touches: `auth`, `space`, `test`
- **凭据互斥。** 每个 target 的 secret、事件 secret、membership token 之间两两互斥，
  main.go 的固定 env 注册表 + 各模块本地拒绝列表 + 全树 credential 字面量 sweep 三重把关。
  新增 per-consumer 凭据必须同时进这三处。(a) 的载体是 #852 自带的 `fixedInternalTokenEnvs`
  （9 条，`main.go:833-905`）——**#853 本迭代不合入**，`pkg/internaltoken` 在 main 上不存在。
  #853 若未来合入，其 registry 的 4 条清单**不含** membership token，必须显式 append，
  否则 #852 建立的 9 条覆盖面静默回退且无测试会红。 touches: `auth`, `test`
- **供给行不可用于任何读路径的门控（D12）。** `status=ready` 的含义是「我们成功调用过一次」，
  不是「容器现在存在」——容器可能已在子系统侧被删除/归档/迁移而没人通知我方。
  touches: `wire-contract`
- **锁序：供给入队必须是创建事务的最后一条语句。** 全员群 hook 必须在事务提交之后跑
  （跨模块持项目行锁 + HTTP 关进行锁；「锁序反转」的完整表述见 #855 正文 :70 的锁序表）。
  touches: `space`
- **事件目录本身是对端实现依据。** `docs/project-lifecycle-contract.md` 与代码不一致时
  是其中之一有 bug，不是「按实际情况理解」。新增/改变事件必须同步该文档。
  touches: `wire-contract`
- **provisioning ensure 响应是未认证的，且 ValidateTarget 允许 `http://`。**
  `out.ContainerID == req.ContainerID` 回显是「行指向自己容器」的唯一保障（#854 已知前置
  风险）。本任务不修它（out of scope），但 R8 的重驱入口不得降低这层校验。
  touches: `auth`

## 已定决策（不再开放）

| | 决定 | 依据 |
|---|---|---|
| **D-1** | 分身被连带摘除时**推** `member_revoked`，每席位一条，`member_epoch` 取同一次 bump 后值 | 已固化为 R5。分身是代替主人在对端执行的主体；契约 §4 要求撤销迟到也必须应用 |
| **D-3** | `member_epoch` 创建即 bump 到 **1**（0 是「项目不存在」保留哨兵） | **#852 已单方面落地**（`createProjectOnce` 内 bump，`bumped==0` 整单失败）。#855 的 D11（保持 0）与其冲突且未记录，#855 rebase 时必须接受本决定——不再是本任务的决策项 |

## 待决策（需要人拍板，不由实现者决定；两项都阻塞实现开工）

| | 问题 | 建议 |
|---|---|---|
| **D-2** | `CloseAllSpaceSeats`（#855 D14，botfather 删 bot）要不要同事务 bump epoch？ | **建议 bump**（在 `closeOneSeatAndEnqueueTx` 事务内调现成的 `bumpMemberEpochForSpaceMemberTx`）。完整链路：space 席位在同事务关闭 → `_verify` 的 Space 活跃成员轴随即答 false；但对端**感知失效**取决于 epoch 何时动——现状靠 Space 级联工单执行时 bump，工单 abandoned 则 epoch 永不动，对端缓存只能靠自身 TTL 过期（#852 只披露未实现）。提前 bump 让对端下次重拉即得 false，不再依赖异步工单成败。**需要产品/架构签字** |
| **D-4** | 「项目对对端可见」的判定：任一必需子系统确认，还是全部确认？ | 先定义**必需子系统集合**（可能只有 fleet 是必需，drive 是可选），可见性 = 全部必需子系统确认。默认全部可选则退回创建事务内置位。落 R6 |

## 实现顺序前提

本任务在以下合并序列完成后开工；顺序错会放大冲突面：

1. **#852 先合**——它定了 member_epoch 创建 bump、凭据注册表、sweep guard 三件事实标准。
2. ~~#855 rebase 到 #852 之后~~ — **已于 2026-09-09 合入 main（`566e625b`）**，早于 #852。
   故 D-3（创建即 bump 到 1）与 #855 的 D11（保持 0）冲突**改由 #852 rebase 时解决**：
   #852 落地 `createProjectOnce` 内 bump 时，必须在已含全员群/分身逻辑的 main 上重做，
   并确认 `bumped==0` 整单失败的语义仍成立。
3. **#853 本次迭代不合入**（用户 2026-09-09 决定），且**不阻塞**：已核实无人依赖它。
   若未来合入，前置条件是——`pkg/internaltoken` registry 显式 append
   `OCTO_MEMBERSHIP_INTERNAL_TOKEN` + 另 3 条遗留 env（`TS_WEBHOOK_SECRET_KEY` /
   `OCTO_MAIL_GATEWAY_SECRET` / `TS_GRPC_AUTH_TOKEN`），并保留 #852 的
   `main_internaltoken_test.go` 作门禁；否则覆盖面 9→4 静默退化且无测试会红。
4. **#854 随时可合**（独立 lane）；**#864 最后合**（base 改回 main；`.octospec/log.md`
   的琐碎冲突顺手解决）。
5. 取回预演 workspace 的 `.context/merge-dryrun/subsystem_e2e_test.go.txt` 作为 R5 的
   复现基线；取不回则按 §分身丢事件的代码事实重建基线测试。

## Out of scope

- **不实现 `project.archived` / `project.restored`。** 契约 §8 已冻结形状（独立列
  `archived_at`，不是第三个 status 值），本片不发出它们。
- **不修项目归档的入站读谓词盲区。** Space 没有归档态；O1b 的 `archived_at` 列未落地，
  `ProjectEpochsInSpace` / `ProjectMemberships` 也没有 `archived_at IS NULL` 谓词——
  归档项目对对端仍报活跃 epoch。这是 #852 的 follow-up，本任务不夹带，但 R12 须把它
  列为契约已知限制。
- **不把容器 id 改成 `project_id`。** 顺序是对端先收窄 workspace 门、我方后换 id
  （#854 已把该决策锚在 `provisioning.go:71-93`）。提前换会打开一条已知提权链路。
- **不改拆除模型。** 解散仍是 pull（`disband_pending` + 对端轮询
  `/v1/internal/projects/status`），不新增出站拆除事件。
- **不改项目名外发策略。** 名称/描述不进任何出站通道；`metadata_updated` 的 payload 保持为空。
- **不动 `MembershipsInSpace`（`/v1/auth/verify` 用的那条）。** 它答的是 token 持有者
  自己、在 SpaceMiddleware 之后，不参与 `activated_at` 门控——否则供给期间用户看不到
  自己的项目。
- **不做 provisioning ensure 响应的认证加固。** 未认证回显 + 允许 `http://` 是独立前置
  风险（#854 已知），另立任务。
- **不做多 pod 并发压测。** 预演是单进程；租约争抢的正确性由现有 lease 语义承担，
  多 pod 验证另立。
- **不动 #858 / #861**（bot hosting、群列表/sidebar/pinning 等周边产品面）。

## Acceptance

结构（可机器验证，逐条对应 R 编号）：

- [ ] R1：`octo_project_lifecycle_event` 有 `consumer` 列，`UNIQUE (consumer, event_id)`；
      消费方 A `delivered` 且 B `pending` 能在同一行集合上共存。
- [ ] R2：一次业务变更为 N 个启用消费方写 N 行，**在同一事务内**；
      测试：删掉任一消费方的入队 → 断言失败。
- [ ] R3：N 行共享同一份 payload 字节；`TestPayloadIsFrozenAtEnqueue` 扩展且不缩窄；
      变异验证：让 worker 重算 payload → 守卫必须红。
- [ ] R4：claim `NOT EXISTS` 谓词与阻塞键为 `(consumer, project_id)`；4 条二级索引按
      consumer 前导重评（EXPLAIN 佐证无全表扫描）；测试：A 队头 503 时 B 的同项目事件
      同轮投出。
- [ ] R5：`member_revoked` 覆盖分身：一个带分身的成员经踢出 / 退出 / Space 移除三条路径
      离开后，撤销事件的 `subject_uid` 集合 == 该次名册变化中关闭的全部席位，且各事件
      `member_epoch` 相同（同一次 bump）；**复现基线**：merge-dryrun Phase7（取回或重建）
      修复后变绿，改回旧代码重新变红（变异验证）。
- [ ] R11 + D-2：`TestEverySpaceMemberWriterIsAccountedFor` 绿（D-2 择一落地；若进
      baseline 须写明「对端失效依赖工单成败 + 对端 TTL」的理由）；baseline 增补
      `member_removal_all_spaces.go`、更新 botfather 三处过期理由。
- [ ] R6：可见性判定按 D-4 实现并有测试覆盖「必需子系统未确认 → 两接口答 0 / false」
      与「全部必需确认 → 翻转」两方向。
- [ ] R7：lifecycle 轮询间隔可配 + 提交后 nudge；业务写到对端收到的 p99 延迟有断言。
- [ ] **R8-lifecycle**：跑通 abandoned → 重驱 → delivered；per-consumer 隔离断言
      （A 重驱不动 B）。形态沿用 env，**不要引入 HTTP 端点**——见 R8-lifecycle 的 D12 冲突。
      （provisioning 半边已在 `project-subsystem-fanout` 落地并变异验证，照抄其结论。）
- [ ] R9：入站 per-consumer JSON registry（32B floor、DisallowUnknownFields、撞值
      fail-closed）；per-consumer 配额、审计区分、单方轮换各一条测试；新凭据进三重把关
      （#852 自带的 main.go `fixedInternalTokenEnvs` / 模块本地拒绝列表 / credential sweep；
      `pkg/internaltoken` 本迭代不存在）。
- [ ] R12：`docs/project-lifecycle-contract.md` 同步（事件目录、多消费方语义、per-consumer
      幂等边界、失败分类精确表、延迟吞吐、归档谓词盲区列为已知限制）。

端到端（沿用预演 harness，`//go:build dryrune2e`）：

- [ ] 三个及以上子系统同时启用时，每个都收到一次 ensure，各自 secret 验签通过，
      任一子系统 5xx 不影响其他子系统与事件通道。
- [ ] 两个及以上事件消费方同时启用时，各自独立收到全部事件、独立重试、独立 abandon。
- [ ] R7 落地后 p99 延迟断言在 harness 中生效（当前是 5s 起）。
- [ ] R8 lifecycle 半边重驱跑通一次：abandoned → 重驱 → delivered
      （provisioning 半边已在单测层跑通，见上）。

回归：

- [ ] `go test ./modules/project/ ./modules/space/ ./modules/group/ ./pkg/project/ ./pkg/space/ .`
      全绿。**每个包跑前 drop & recreate 测试库，且必须显式
      `COLLATE utf8mb4_general_ci`**（否则 category 迁移报 1267）。共享 `test` 库会被其他
      workspace 灌入别分支的迁移，症状是 `unknown migration in database` 或
      `Table 'xxx' already exists`——预演中造成过 167 个假失败。
      注意：**私有 MySQL 实例目前不可用**，`octo-lib/testutil` 把地址硬编码为
      `root:demo@tcp(127.0.0.1)/test`（`testutil/test.go:28`），换实例需改库。
      所以现阶段只能靠「跑前重建 + 避开与其他 workspace 并发」。
- [ ] `TestMemberEpochOnlyEverIncrements` 与 `TestEverySpaceMemberWriterIsAccountedFor`
      双绿（两个 guard 的 baseline 都与合并后代码一致）。
- [ ] `make i18n-extract-check && make i18n-lint` 通过（本任务预期不新增 HTTP 端点，
      故通常无新错误码；若确实新增，错误码进 `pkg/errcode` 并补 zh-CN 翻译）。
- [ ] 签名层零改动：`pkg/octosign` conformance vectors 测试原样通过。
