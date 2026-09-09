---
type: Task
title: "Task: project-subsystem-fanout"
description: 把 per-target 分派点收敛成一张 target 注册表，并给 abandoned 供给行加 env 驱动的人工重驱
tags: [project, wire-contract, auth, test]
timestamp: 2026-09-09T00:00:00Z
slug: project-subsystem-fanout
upstream: 口头需求（#852 / #854 / #855 / #864 合并预演后的实测结论）
source: self
---

# Task: project-subsystem-fanout

> **范围（2026-09-09 收窄）**：本任务只做**作用对象已在 main 上**的两件事——
> R10（target 注册表收敛）与 R8 的 provisioning 半边（env 驱动的 abandoned 重驱）。
> 两者的作用对象都来自已合并的 #850。
>
> 原 brief 还包含 9 条（R1–R7 / R9 / R11 / R12），其作用对象在 main 上**零命中**
> （`octo_project_lifecycle_event` 表与 worker、`activation.go`、
> `modules/internal_membership`、`TestEverySpaceMemberWriterIsAccountedFor`），
> 依赖未合并的 #852 / #864。它们**已连同全部核实结论移入**
> `.octospec/tasks/project-lifecycle-multiconsumer/brief.md`（编号沿用不重排）。
> 修剪前的完整原文留在 `.context/attachments/SeL7SI/brief-full-before-prune.md`（不进 git）。
>
> 基线：`main@98d209206`（#855 与 #861 均已合入）。

## Goal

**接第三个子系统不该是五处代码的考古工作，卡死的供给行不该只能等着。** 两件事：

1. **R10 · target 注册表收敛。** 原状接一个子系统要改 5 处分派点
   （`allProvisionTargetNames()` 字面量 slice、`targetEnvNames()` case、
   `containerIDPrefix()` case、`refreshProvisioningMetrics()` 字面量 slice、
   `provisioningTargetLabel()` case），外加声明常量与 4 个 env。
   **最后两处是沉默失效**：漏改不报错，新子系统照常供给，只是在看板上不可见——
   而那个「未收窄容器」gauge 正是本 slice 能先上线的安全论据。
2. **R8-provisioning · abandoned 人工重驱。** 供给行重试 12 次耗尽后落入 `abandoned`
   终态，**此后无任何自动或人工恢复路径**（`requeue|re-drive` 在 #854 树里只出现在注释），
   而 fleet 的 `confirmProjectActive` 不会再跑 ⟹ **该项目对对端永久不可见**。
   一次几分钟的网络抖动就能永久废掉一个项目，运维无招。

## 必须实现（MUST）

**R10 · target 注册表收敛 — ✅ 已落地**
**保留硬编码枚举**（未知 target 名启动即拒的收益），把 **5 处**分派点收敛为一张
`provisionTargetSpec` 表：接第三个子系统 = 注册表加一条（name / 4 个 env / 容器前缀）
+ 声明它的 4 个 env 常量。刻意**不**做配置驱动：那要另写前缀形状、密钥地板、是否必需、
是否参与可见性四套校验器，等于用四个待写的校验替换一个已经生效的启动期拒绝。
容器前缀（`octows-` / `octods-`）随之从 `provisioning.go` 迁入注册表条目，
`newContainerID` 仍是唯一生产者。

**R8-provisioning · abandoned 人工重驱 — ✅ 已落地**
`OCTO_PROJECT_PROVISION_REQUEUE_PROJECT_ID` 填单个 project_id，pod 启动时在 `Enabled()`
门内、定时器挂载前执行一次：`abandoned` → `pending`，worker 随即重试。

*形态：env，启动时生效*（**取代原「i18n envelope + auth + 审计」的 HTTP 形态**）。
改形态的原因是一条真实冲突，不是偏好：HTTP 端点必然带 handler，而守卫
`TestNoHandlerReachesTheProvisioningTable` 禁止任何带 handler 的文件碰供给表或其 DAO，
并把表名钉死在 `db_provisioning.go` 一个文件里。那条守卫执行的是 **D12**（见
§Load-bearing list），所以 R8 的原字面形态与 D12 的守卫不能同时成立。
D12 真实禁的是**用供给行门控读路径**；重驱是运维触发的**写**，不门控任何用户可见的读，
因此 env 形态是绕开冲突而非削弱守卫——**已验证该守卫仍通过**。

三条实现约束都是不做就出错的，且各有变异验证：
- **`attempts` 重置为 0**——不重置则 sweep 在任何调用发出前把行重新判死。
- **`container_id` 不重发**——它是对端的幂等键，换了会孤立掉前面 at-least-once 可能
  已建好的容器（ensure 是至少一次，响应丢失时对端已有真容器而我方行仍未 `ready`）。
- **`last_error` 追加不覆盖**——原始放弃原因是运维唯一的持久证据。

**configmap 通道的两个固有代价**（写在 env 声明处，不是实现缺陷）：① 改完要**重启**才
生效，救第二个项目要再改再重启；② 值**留在 configmap 里不会消失**，此后任何重启都会再读。
②带来的「幽灵重驱」由**让行自己的状态当幂等凭据**消解——UPDATE 只匹配
`status = abandoned`，所以对已救好的项目是空操作，**不需要另建「已处理」账本**（那会是
关于同一决定的第二个真相源）。同一机制覆盖多副本竞争：各 pod 启动都跑，状态复查让
只有一个改到，其余匹配 0 行。若该项目**再次**进入 `abandoned`，下次重启会再救一次——
这是「运维把指令留在原处」的正确解读。零行匹配走 Warn 日志（零是运维最需要看到的答案：
id 写错、已被上次重启救过、或这个项目从未失败过）。

## Load-bearing list

- **供给行不可用于任何读路径的门控（D12）。** `status=ready` 的含义是「我们成功调用过
  一次」，不是「容器现在存在」——容器可能已在子系统侧被删除/归档/迁移而没人通知我方。
  由 `TestNoHandlerReachesTheProvisioningTable` 守卫（禁 handler 文件碰供给表/DAO，
  且表名只许出现在 `db_provisioning.go`）。R8 的 env 形态正是为不违反它而选的。
  touches: `wire-contract`
- **容器 id 不可由 project_id 推导。** 同 Space 成员能列出 `project_id`，而 fleet 的
  workspace 门按 Space 成员放行并把调用者物化成永久 workspace 成员——可推导性会把
  「能列项目」补成「永久是每个项目 workspace 的成员」。`newContainerID` 是唯一生产者，
  由 `TestContainerIDHasOneProducerAndItIgnoresTheProjectID` 守卫。R10 把前缀字面量
  迁入注册表时**必须保持该守卫的单一生产者语义**。 touches: `auth`, `wire-contract`
- **容器 id 不得被供给切片外的任何包读到。** 由仓库级守卫
  `TestNoPackageOutsideTheProvisioningSliceCanReadAContainerID` 保证（whole-repo walk +
  前缀字面量作切片标记）。R10 迁移前缀 = 动了该守卫的白名单，**放宽白名单必须重做
  变异验证**，否则守卫会静默失效。 touches: `auth`, `test`
- **供给的出站调用只许在 worker 里。** handler 调 fleet/drive 会让用户请求依赖别的服务
  存活；由 `TestProvisioningClientIsConfinedToTheWorker` 守卫。R8 的重驱入口是 env +
  worker 内执行，不引入新的出站点。 touches: `wire-contract`
- **`abandoned` 语义：重试预算耗尽的终态，非 claim 目标。** `claimProvisioningJob` 要求
  `attempts < max`、sweep 要求 `attempts >= max`，两个谓词互斥。重驱把 `attempts` 归零
  正是为了回到可 claim 的一侧；只改 status 不改 attempts 会被 sweep 立刻打回。
  touches: `wire-contract`
- **`target IN ?` 过滤在所有供给 SQL 上都是非装饰性的。** 关掉一个 target 必须是
  非破坏的：没有该过滤，收窄 `OCTO_PROJECT_PROVISION_TARGETS` 会让 sweep 把另一个
  target 的停放行推向终态 `abandoned`，而回滚开关**不会**恢复它们。重驱同样按启用
  target 过滤——重驱一个已关闭 target 的行只会把它停在 `pending` 且无执行者。
  touches: `wire-contract`
- **`provisioningRows` 看板是本 slice 的安全论据。** 「未收窄容器」这个 gauge 是
  R2/R3 落地前唯一的攻击面度量，所以任何 per-target 判定漏掉一个 target 都是安全度量
  失真，而非美观问题——这正是 R10 收敛 `refreshProvisioningMetrics` /
  `provisioningTargetLabel` 的理由。 touches: `test`

## Out of scope

- **不做 lifecycle 事件通道的任何改动**（多消费方、consumer 维度、事件目录、
  lifecycle 侧重驱）——见 `project-lifecycle-multiconsumer`。
- **不把容器 id 改成 `project_id`。** 顺序是对端先收窄 workspace 门、我方后换 id
  （决策锚在 `provisioning.go` 的 `newContainerID` 注释）。提前换会打开一条已知提权链路。
- **不改拆除模型。** 解散仍是 pull（`disband_pending` + 对端轮询
  `/v1/internal/projects/status`），不新增出站拆除事件。
- **不做 provisioning ensure 响应的认证加固。** 未认证回显 + `ValidateTarget` 允许
  `http://` 是独立前置风险（#854 已知），另立任务。R8 的重驱不得降低
  `out.ContainerID == req.ContainerID` 这层回显校验。
- **不做 target 的配置驱动化。** 保留编译期封闭枚举，理由见 R10。
- **不动项目名外发策略**（ensure body 的 `name` 保持固定常量，项目名不出站）。
- **不做多 pod 并发压测。** 重驱的多副本安全由 UPDATE 的 `status` 复查承担，已有测试
  覆盖；租约争抢的正确性由现有 lease 语义承担。

## Acceptance

- [x] **R10**：5 处分派点全部改读 `provisionTargetRegistry`；接第三个子系统只需表加一条
      + 声明 4 个 env。
- [x] **R10 守卫**：`TestEveryTargetDecisionReadsTheRegistry` 钉住「注册表文件外不得枚举
      target 常量」，并对两处**沉默站点**（`refreshProvisioningMetrics` /
      `provisioningTargetLabel`）各做变异验证——改回枚举即报红并指名文件，恢复即变绿。
- [x] **既有守卫未被打瞎**：`TestContainerIDHasOneProducerAndItIgnoresTheProjectID` 改为
      断言不变量（前缀字面量只在一张表、生产者唯一）+ 新增「每条注册表条目必须声明非空
      前缀」；`TestNoPackageOutsideTheProvisioningSliceCanReadAContainerID` 白名单增补
      注册表文件后**经变异验证**（往 handler 文件塞前缀字面量仍报红）。
- [x] **R8-provisioning 主链路**：`TestRequeueRevivesAnAbandonedRowAndReachesReady`
      跑通 abandoned → 重驱 → **ready**（不止状态翻转，worker 真的把行驱动到 ready），
      且重试落在**同一 container_id**、未创建第二个容器。
      变异验证：`attempts` 不重置 → 红。
- [x] **R8-provisioning 幽灵重驱 + 多副本**：
      `TestRequeueIsANoOpOnASecondBootWithTheEnvStillSet` 断言残留 env 对已 ready 的项目
      是空操作（finished_at 不变、不追加 requeue 标记、不产生新的 ensure 调用），
      连跑两次模拟第二个 pod / 第二次重启。变异验证：去掉 `status=abandoned` 过滤 → 红。
- [x] **R8-provisioning 作用域**：`TestRequeueLeavesOtherProjectsAlone` 断言只动指定
      项目，旁观项目留在 `abandoned`。变异验证：忽略 `project_id` → 红。
- [x] **D12 未被绕过**：`TestNoHandlerReachesTheProvisioningTable` 通过；新 DAO
      `requeueAbandonedProvisioningJobs` 仅 worker 一个调用者（该文件无 handler 标记），
      新 env 不进任何读路径。

门禁（2026-09-09 在 `main@98d209206` 上实测，#855 + #861 均已合入）：

- [x] `go build ./...` / `gofmt` / `go vet ./modules/project/` 全干净。
- [x] `golangci-lint run ./modules/project/... ./internal/projectprovision/...` → **0 issues**。
- [x] `make i18n-extract-check` + `make i18n-lint` → 均 exit 0（本任务不新增错误响应）。
- [x] 13 个关键测试全部真实执行并通过（`-v` 逐个列名确认非空集）：3 个 requeue 行为测试
      + `TestEveryTargetDecisionReadsTheRegistry` + 5 个既有供给守卫 + 4 个既有供给行为测试。
- [x] `internal/projectprovision` / `pkg/project` / `pkg/space` / `modules/space` 全部 ok。
- [x] **`modules/project` 整包全绿**（2026-09-09 在 `main@98d209206` 上实测：`ok`，0 失败，
      跑完库仍有 130 张表即未被外部清）。此前几轮整包红是**共享测试库被其他 workspace
      周期性 drop**，已用对照实验证伪与本改动的关系（stash 掉全部改动、纯净 main 上跑同一批
      失败测试 → 同样红、同样 `Error 1146 Table doesn't exist`）。
      **跑法要求**：每次跑前 drop & recreate 测试库，且**必须显式
      `COLLATE utf8mb4_general_ci`**——`CleanAllTables` 只 DELETE 数据不改结构，所以
      main 上新增迁移（#855 的 `all_member_group_no`/`removing`、#861 的
      `project_user_setting`）后不重建会残留旧 schema 报 1054/1146。
      `octo-lib` `testutil/test.go:28` 硬编码 `root:demo@tcp(127.0.0.1)/test` 无 env 覆盖，
      无法用私有库隔离，故与其他 workspace 并发时仍可能被打断——重建后重跑即可。
- [x] `go test -race`（requeue + 注册表守卫 + abandon）干净。
- [x] 相邻包全绿：`internal/projectprovision` / `pkg/project` / `pkg/space` / `modules/space`。
