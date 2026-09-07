# 实施计划：project-p2-subsystem-integration / PR-5（子系统容器预置 outbox）

> **状态：已实施**（2026-09-07）。分支 `feat/project-p2-provisioning-outbox`，
> base = `main @ c7abadeb`（P0 #841 已合并；**P1 #846 仍 OPEN，本切片不依赖它**）。
> 与 brief 草图的六处有意偏离及其理由见 [context.yaml](./context.yaml) 的 `deviations`。
> 本文件只写落地结果、验证证据和运维手册；设计论证不重复，都在
> [brief.md](./brief.md) 的 D1 / D2 / D12 里。

---

## 0. 范围：为什么先做 PR-5、而 PR-2 被推到它后面

brief 原来把 PR-2（`POST /v1/internal/projects/status`）和 PR-5 并列成「两个纯 P0、
今天都能开工」。核对代码后 PR-2 不成立，原因写进 brief 的切片表了，这里只留结论：

D9 要求 `unknown` 把「项目不存在」和「不在本 consumer 的 grant 内」折叠成**同一个
字节相同的答案**，而「grant」在 P0 上没有任何数据可以表达 —— 唯一能表达它的就是
`octo_project_provisioning`（一个 consumer 的 grant = 「有它那个 target 的行」的项目
集合）。所以 PR-2 要么带着「任何合法 token 都能读到任何项目的真实状态」这条弱点上线、
并把那条验收标为未满足，要么叠在 PR-5 上。选了后者：反正 PR-5 是先写的那一片，
(a) 换不来任何进度。

**P1 完全不参与本切片。** `group.project_id` / `admitOrRestoreMembersTx` /
`cascade_registry.go` / `removal_worker.go` 在 P0 上都不存在，而本切片一个都没用到 ——
它只在 `createProjectOnce` 事务尾部加一条 INSERT，在 `disbandProjectTx` 里加一条
UPDATE，其余全是新文件。

---

## 1. 落地清单

### 新增

| 文件 | 内容 |
|---|---|
| `modules/project/sql/20260907000001_project_provisioning.sql` | `octo_project_provisioning`：outbox 兼映射表（D12 一张表） |
| `internal/projectprovision/client.go` | **唯一**出网路径：签名、超时、禁代理、拒重定向、限长响应、容器 id 回显校验 |
| `internal/projectprovision/client_test.go` | 单元测试，不碰网络也不碰库 |
| `modules/project/provisioning.go` | 状态枚举、`newContainerID`（唯一生产者）、退避、租约 |
| `modules/project/config_provisioning.go` | 环境变量、per-target 校验、凭据互斥 |
| `modules/project/db_provisioning.go` | 入队 / 认领 / 释放 / 终态 / 扫描 / 清理 / 计数 |
| `modules/project/provisioning_worker.go` | worker、`provisionEnsurer` 接口、指标发布 |
| `modules/project/metrics_provisioning.go` | 5 个指标，标签全是闭集 |
| `modules/project/provisioning_test.go` | 集成测试 + 配置表驱动测试 |
| `modules/project/provisioning_guard_test.go` | 6 个源码守卫 |

### 改动（共 90 行）

| 文件 | 改动 |
|---|---|
| `modules/project/service.go` | `createProjectOnce` 提交前入队（fail-closed）；提交后 nudge |
| `modules/project/db.go` | `disbandProjectTx` 内把本项目所有行推到 `disband_pending` |
| `modules/project/api.go` | `provisionClient` / `nudgeProvisioningFn` 两个字段 + `Route` 启 worker + 入队失败的独立 metric reason |
| `modules/project/config.go` | `Config.Provisioning` |
| `modules/project/metrics.go` | `reasonProvisioningEnqueue` |
| `main.go` | 两个 provisioning secret 进 `ValidateNotifyTokenExclusions` |

**没有新增 pkg/errcode 码，也没有新增 zh-CN 条目** —— 这是核对后的结论而不是漏做：
本切片唯一新增的用户可见失败是「建项目因 outbox 写失败而失败」，客户端对它和对任何
存储失败能做的事完全一样，所以复用 `ErrProjectStoreFailed`（500 / `Internal=true` /
先记 `zap.Error`）。真正需要区分的是运维视角，那走的是 metric label
`write_rejected_total{entry="project_create",reason="provisioning_enqueue"}`。

---

## 2. 验证证据

全部在本机跑过，命令与结果如下（不是推断）。

```
go build ./...                                                        OK
go vet ./...                                                          OK
golangci-lint run ./modules/project/... ./internal/projectprovision/  0 issues
go test ./modules/project/... ./internal/projectprovision/ -race      ok 16.9s / 1.6s
make i18n-extract-check                                               OK
make i18n-lint                                                        OK: no new direct error responses
                                                                      OK: no inline codes
```

`modules/project` 整包 34 个既有测试文件全部仍绿（改动前先跑过同样的基线）。

### 2.1 守卫做过变异验证

守卫最常见的失效方式是「变绿但什么都没查」，所以每一条都注入了它声称能抓的缺陷、
确认它**因为那个原因**失败、再恢复确认变绿：

| 守卫 | 注入的缺陷 | 结果 |
|---|---|---|
| `TestProvisioningClientIsConfinedToTheWorker` | 让 `api.go`（含 handler）import 出网包 | FAIL → 恢复后 PASS |
| `TestNoHandlerReachesTheProvisioningTable` | 在 `createProjectHandler` 里调 provisioning DAO | FAIL → PASS |
| `TestContainerIDHasOneProducerAndItIgnoresTheProjectID` | 让 `newContainerID` 引用 `ProjectID` | FAIL → PASS |
| `TestNoLogFieldCarriesTheContainerID` | 加 `zap.String("containerId", job.ContainerID)` | FAIL → PASS |
| `TestProvisioningMigrationDeclaresTheInvariants` | 删掉 `uk_..._container` 唯一键 | FAIL → PASS |
| `TestProvisioningEnqueueFailureRollsBackTheWholeCreate` | 把入队搬到 `tx.Commit()` **之后** | FAIL → PASS |
| `TestDisbandMoves...`（两条） | 删掉 `markProvisioningDisbandPendingTx` 调用 | FAIL → PASS |

`internal/projectprovision` 侧同样验过两条：去掉 `CheckRedirect` 让
`TestEnsureRefusesRedirects` FAIL，去掉 `io.LimitReader` 让
`TestEnsureBoundsTheResponseBody` FAIL。

前四条守卫的**第一版都是错的**，而且是变异验证抓出来的，不是 review 抓出来的 ——
细节记在 context.yaml 的 `defects_found`。其中两条尤其值得记住：容器 id 守卫按
「下一个 `func `」切取被检函数体，结果把中间那个 `provisioningJob` 结构体一起吞了进去
（它的 db tag 字面就是 `project_id` / `space_id`），于是报了一个并不存在的泄漏；
import 守卫拿 `"internal/projectprovision"`（带前引号）去匹配，而真实 import 行是
全限定路径，所以它在一个**确实** import 了的文件上照样变绿。

### 2.2 验收对照（brief §Acceptance 的 Provisioning 段）

| 验收项 | 覆盖 |
|---|---|
| 任何客户端响应都不含容器 id（验收点名 detail / list / appconfig / verify） | `TestNoClientResponseCarriesAContainerID`：create / detail / list / members / **verify** 五个响应，且在 worker 跑到 ready **之后**再查一遍。**appconfig 没有行为断言**，理由与替代见下方 2.3 |
| 日志与 error details 不含容器 id | `TestNoLogFieldCarriesTheContainerID` + `TestEnsureErrorNeverCarriesTheContainerID`（含 `last_error` 的两条断言） |
| 容器 id 不是 project_id 的函数 | 行为面 `TestContainerIDIsNotAFunctionOfProjectID` + 源码面 `TestContainerIDHasOneProducer...` |
| 回滚不留行 / 提交后每目标恰好一行且 container_id 已就位 | `TestProvisioningEnqueueFailureRollsBackTheWholeCreate` + `TestCreateEnqueuesExactlyOneRowPerEnabledTarget` |
| worker 幂等：重放收敛到 ready 且不造第二个容器 | `TestWorkerReachesReadyAndConvergesOnReplay`（假目标按 container_id 记账，所以能区分「调了两次」和「建了两个」） |
| 无读路径以本表为门 | `TestNoHandlerReachesTheProvisioningTable`（禁用标识符集**从 DAO 源码派生**，改名不会让守卫落空） |
| 两个 secret 进 exclusions、且与任何内部 token 不同 | `TestMainWiresProvisioningSecretsIntoValidateNotifyTokenExclusions` + `TestLoadProvisioningConfig` 的四个 sibling 用例 |
| 解散移到 `disband_pending` 且不发出网请求 | `TestDisbandMovesRowsToDisbandPendingAndSendsNothing`（解散后再跑一轮 worker，断言假目标计数不变） |
| 出网客户端只在 worker 包，`modules/project` / `modules/user` 的 handler 都到不了 | `TestProvisioningClientIsConfinedToTheWorker` |
| 未收窄容器数量有 gauge | `TestUnnarrowedContainerGaugeCountsReadyRowsOnUnnarrowedTargets`（含「条件消失后回落到 0」） |

### 2.3 appconfig：为什么用结构性守卫替代行为断言

验收点名了四个响应，其中 `appconfig` 没有做行为断言，这是一次有意的替换而不是漏做：
`CleanAllTables` 会删掉 `app_config` 行，而重建一条合法的行需要生成 RSA 密钥对并用
master key 加密 —— 等于在测试里重写一遍 `modules/common.insertAppConfigIfNeed`。
不重建就只能对着它的 400 响应断言，那是一条「因为响应里什么都没有所以通过」的测试。

替代的是 `TestNoPackageOutsideTheProvisioningSliceCanReadAContainerID`，而它**比原验收更强**：
行为断言只能覆盖测试想到要调的响应形状，这条走的是前提 —— 容器 id 只有被**读出来**
才可能进响应，而它只存在 `octo_project_provisioning` 一张表里；于是全仓遍历断言
「本切片以外没有任何包点名这张表，也没有任何包能造出容器 id（两个前缀只和唯一生产者
同文件）」。结论覆盖的是**其它模块产出的每一个响应**，不只是 appconfig。

变异验证：往 `modules/user/api.go` 里塞一条读该表的 SQL 常量，守卫 FAIL 并点名该文件；
恢复后 PASS。守卫还带扫描文件数下限（<100 即 Fatal），所以遍历失效不会静默变绿。

---

## 3. 运维手册

### 3.1 默认状态：完全惰性

`OCTO_PROJECT_PROVISION_TARGETS` 为空（默认）时：不入队、不起 worker、不出网，
建项目的行为与合并前逐字节相同。**合并本 PR 不需要任何配置变更，也不改变任何现有
行为。**

### 3.2 打开一个 target（P-2 落地之后再做）

```bash
OCTO_PROJECT_PROVISION_TARGETS=fleet
OCTO_PROJECT_PROVISION_FLEET_URL=https://<fleet>/api/internal/workspaces/ensure
OCTO_PROJECT_PROVISION_FLEET_SECRET=<>=32 字节，且不得等于任何其它内部 token>
# 该子系统声明「已按 Project 收窄鉴权」（fleet 的 R2 / drive 的 R3）之后再置 true：
OCTO_PROJECT_PROVISION_FLEET_NARROWED=false
```

env 走 configmap，两个方向都需要滚动重启（与 P0 的两个开关同一性质）。

**打开之前必须确认 P-2（子系统侧服务身份）已经就绪。** 否则每个新项目都会产出一条
在 ~1 小时后走到 `abandoned` 的行，而 `abandoned` 没有任何自动重驱动。

### 3.3 需要盯的指标

| 指标 | 含义 |
|---|---|
| `project_provisioning_rows{target,status}` | 行普查。`pending` 持续上涨 = 目标不响应；`abandoned` 非零 = 需要人 |
| `project_provisioning_unnarrowed_containers{target}` | **暴露面大小**：该目标尚未声明按 Project 收窄，这些容器只靠 id 不可推导来保护 |
| `project_provisioning_target_misconfigured{target}` | 1 = 这个 target 被要求了但配置被拒（坏 URL / 短 secret / 凭据撞车），它不会预置任何东西 |
| `project_provisioning_attempts_total{target,outcome}` | `target_no_ensure_endpoint`（= 404，P-2 还没到）与 `target_5xx`（= 真故障）在**第一次尝试**就能分开 |
| `write_rejected_total{reason="provisioning_enqueue"}` | 建项目因为 outbox 写失败而失败。非零说明是本切片让 create 挂的 |

### 3.4 P-2 落地后重驱动已放弃的行

`abandoned` 刻意没有自动重驱动 —— 自动重驱动会抹掉「目标坏了」和「目标好着」的区别。
人工重排是一条 UPDATE：

```sql
UPDATE octo_project_provisioning
   SET status = 0, attempts = 0, next_attempt_at = UTC_TIMESTAMP(3),
       lease_owner = '', lease_until = NULL, finished_at = NULL
 WHERE status = 2 AND target = 'fleet'
 LIMIT 500;
```

> `UTC_TIMESTAMP(3)` 只在这条**人工**语句里可以用：应用侧一律用 Go 的 UTC 时钟写入，
> 因为认领是拿 Go 的时间去比 `next_attempt_at`（见 `db_provisioning.go` 头部）。
> 这条语句要求执行者确认 MySQL 会话时区不会把它变成本地时间；分批 500 是为了不让一条
> UPDATE 锁住大范围。

### 3.5 回滚

1. `OCTO_PROJECT_PROVISION_TARGETS=`（清空）+ 滚动重启 → 立刻停止入队与出网；已入队的
   `pending` 行**保留**（认领带 target 过滤），重新打开就继续。
2. 需要连表一起回退时：`DROP TABLE octo_project_provisioning`（migration 的 Down 段）。
   此时必须先确认没有已创建的容器需要回收 —— 表被删掉之后本地就再也答不出
   「哪些项目曾被预置进 drive」。

---

## 4. 明确不在本切片内

- 任何把容器 id 交给客户端的路径，以及项目详情上的 `provisioning:` 字段（D12 禁止读路径以本表为门）。
- 任何解散推送（拉取式，D9）。
- 子系统侧的 `ensure` 端点（P-1）、服务身份（P-2）、按 Project 收窄的鉴权（P-3 / R2 / R3）。
- PR-0 / PR-1 / PR-2 / PR-3 的任何内容。
