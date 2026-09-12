# 实施计划：project-p2-subsystem-integration / PR-5（子系统容器预置 outbox）

> **状态：已实施**（2026-09-07）。分支 `feat/project-p2-provisioning-outbox`，
> base = `main @ d5b091a6`（P0 #841 与 **P1 #846 均已合并**；本分支已于 2026-09-07 rebase 到 P1 之上）。
> 本片最初是在 `main @ c7abadeb`、P1 还 OPEN 时写就并验证的，那种独立性当时是真的、也正是两片
> 能并行推进的原因 —— 但 P1 合并、本分支 rebase 之后它就不再是**已交付这棵树**的事实了，而记录
> 还照旧断言了三个 head。现在恰好相反：P1 让 `modules/group` 依赖 `modules/project`，闭合了一条
> 经由 `internal/cardactiondispatch` 的导入环，`pkg/octosign` 的抽取就是被它逼出来的。
> 与 brief 草图的有意偏离及其理由，逐条见 [context.yaml](./context.yaml) 的 `deviations`
> （刻意不在这里写条数 —— 早期版本写「六处」而 context.yaml 里是七条，数字本身就是一类漂移）。
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

**本切片不使用 P1 的任何符号。** `group.project_id` / `admitOrRestoreMembersTx` /
`cascade_registry.go` / `removal_worker.go` 一个都没用到 —— 它只在 `createProjectOnce`
事务尾部加一条 INSERT，在 `disbandProjectTx` 里加一条 UPDATE，其余全是新文件。这是它能
在 P1 还 OPEN 时独立写完并验证的原因。

> **但「不使用它的符号」不等于「与它无关」，这一条更新于 2026-09-07。** P1 合并后本分支
> rebase 到它上面，而 P1 让 `modules/group` 依赖 `modules/project` —— 这闭合了一条
> `modules/project → internal/projectprovision → internal/cardactiondispatch → … →
> modules/group` 的导入环，于是 v1 签名原语必须被抽到叶子包 `pkg/octosign`。所以本片**在
> 构建层面确实依赖 P1 已落地**，`internal/cardactiondispatch/signature.go` 与
> `pkg/octosign/octosign.go` 的包注释正是这么写的。本文件早期版本写的「P1 完全不参与本切片」
> 与那两处注释直接矛盾。

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
| `modules/project/provisioning_guard_test.go` | 10 个源码守卫（7 个不变量守卫 + 3 个文档失真守卫） |

### 改动（共 +129 / −24 行，8 个既有文件）

`git diff --numstat origin/main` 实测；早期版本写的「共 90 行」既低估了行数，也漏掉了
`internal/cardactiondispatch` 的两个文件 —— 那两个恰恰是本片唯一改到**已上线路径**的地方。

| 文件 | 改动 |
|---|---|
| `modules/project/service.go` | `createProjectOnce` 提交前入队（fail-closed）；提交后 nudge |
| `modules/project/db.go` | `disbandProjectTx` 内把本项目所有行推到 `disband_pending` |
| `modules/project/api.go` | `provisionClient` / `nudgeProvisioningFn` 两个字段 + `Route` 启 worker + 入队失败的独立 metric reason |
| `modules/project/config.go` | `Config.Provisioning` |
| `modules/project/metrics.go` | `reasonProvisioningEnqueue` |
| `main.go` | 两个 provisioning secret 进 `ValidateNotifyTokenExclusions` |
| `internal/cardactiondispatch/signature.go` | v1 原语搬到 `pkg/octosign` 后改为纯委托（唯一改到已上线路径的改动之一） |
| `internal/cardactiondispatch/http.go` | 三个 header 常量改为别名 `pkg/octosign` 的 |

**没有新增 pkg/errcode 码；专属全员群 guard 复用既有码并已补齐 bot_api/group 的 zh-CN 条目**：
这是与本切片并行收口的 Group/Bot guard 变更，不引入新的错误码。Provisioning
本切片唯一新增的用户可见失败是「建项目因 outbox 写失败而失败」，客户端对它和对任何
存储失败能做的事完全一样，所以复用 `ErrProjectStoreFailed`（500 / `Internal=true` /
先记 `zap.Error`）。真正需要区分的是运维视角，那走的是 metric label
`project_write_rejected_total{entry="project_create",reason="provisioning_enqueue"}`。

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
| 两个 secret 进 exclusions、且与任何内部 token 不同 | `TestMainWiresProvisioningCredentialsIntoValidateNotifyTokenExclusions` + `TestLoadProvisioningConfig` 的四个 sibling 用例 |
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

### 2.4 自审（/code-review）后修掉的 9 项

复审在自己的代码上找出 2 个 HIGH、3 个 MEDIUM、4 个 LOW，全部已修并各自补了变异验证。
两个 HIGH 都是「文档/指标说的和代码做的不一致」这一类 —— 不是崩溃，但会把运维引向错误结论：

| 编号 | 问题 | 修法 | 变异验证 |
|---|---|---|---|
| **H-1** | `provisioning_attempts_total` 同时漏计与重复计：`panic` / `target_disabled` 两条路径 0 次增量，而耗尽预算的那次增量记两次（真实 outcome + `"abandoned"`）。后果是 `sum by(outcome)` ≠ 尝试次数，且一个每次都 panic 的 job 在这个 counter 里直到放弃前完全不可见 | 增量点收敛到 `releaseOrAbandon` 唯一一处，label 用真实 outcome；删掉 `"abandoned"` outcome（该问题由 `provisioning_rows{status="abandoned"}` gauge 回答） | 退回旧形态 → 三条测试 FAIL |
| **H-2** | 退避窗口文档写「约 1 小时」，实际 **23.5 分钟**（代码注释 + context.yaml + plan.md 三处） | 三处改成实算值，并把算式写进注释 | 算术已实算复核 |
| **M-1** | 容器 id 走在 `X-Octo-Event-ID` header 里。body 几乎不会被记录，header 经常会（反代日志格式、APM 默认抓 header），而在 R2/R3 前它是 capability | event-id 槽改成 `sha256(container_id)`；保留重放稳定性与资源绑定，接收方从 body 读真 id | 退回原始 id → 签名测试 FAIL |
| **M-2** | 接收方契约只写了「怎么签」，没写「验什么」。缺时间戳新鲜度窗口 → 抓到的 ensure 请求可无限重放，**把子系统已回收的容器复活** | 包注释 + brief D2 补三条 MUST（验签 / 拒绝过期时间戳 / 以 `container_id` 为幂等键） | 契约文档，无代码可变异 |
| **M-3** | `target_disabled` 与 `panic` 两条路径既无测试也无指标 —— 正是 H-1 漏计的那两条 | 两个用例，同时断言行状态、租约已释放与指标增量；panic 用例通过 `provisionEnsurer` 注入 | 见 H-1 |
| **L-1** | `uk_..._container` 撞键的 MySQL 消息形如 `Duplicate entry 'octows-...' for key ...`，经 `errors.Join` → `zap.Error` **把容器 id 带进日志**；zap 字段守卫看不见从错误串来的泄漏 | 入队的 duplicate-key 分支换成固定文案 | 删掉分支 → 新测试 FAIL |
| **L-2** | `ValidateTarget` 不挡私网/link-local（元数据端点） | **保持不挡**，但把「已接受」的理由写进注释：目标是部署期 operator 值而非用户输入，两个真实目标都在集群内私网上，私网黑名单会拒掉所有合法配置；与 cardactiondispatch 同姿态 | n/a |
| **L-3** | DELETE 走 `UpdateBySql` | 改 `DeleteBySql`（仓内 `modules/conversation_ext` 已在用） | 既有 purge 测试覆盖 |
| **L-4** | 所有容器在子系统侧显示名都是同一个 `octo-project`，fleet UI 里会一片同名 | 契约里写明「接收方拿 `project_id` 自建标签」，并说清这是 D3 的后果而不是 bug | 契约文档 |

另有若干项复审确认**不是**问题，理由记在报告里，其中最值得留档的一条：
`errors.Join` 不会打断 `retryOnLockConflict` 的 `errors.As(*mysql.MySQLError)` 识别，
也不打断 `respondCreateError` 的 sentinel 分支 —— 用一个独立最小测试实测过，
不是靠读文档推断。

---

### 2.5 上游 review round（yujiawei, CHANGES_REQUESTED）修掉的 15 项

自动评审在本仓之外、只读 diff 的情况下，把「文档声称 vs 代码行为」这类问题几乎全部
抓了出来 —— 包括我们自己变异验证漏掉的。16 项论断逐条核实**无一错报**。同样分两类记录：

**Spec 缺口（2）**

| | 问题 | 处置 |
|---|---|---|
| S-1 | D2 与迁移头声称「租约有心跳」，代码从未实现过 | **撤回声称而不是实现**（评审给的 B 选项）：心跳是长作业的机械结构，本作业是一次有界调用；代之以 `Timeout <= 租约/4` 的配置加载期校验（这同时是 P1-2 的修法）。迁移头与 brief 同步改写 |
| S-2 | target 关闭/配错期间创建的项目**永远**拿不到容器，且未声明；首次启用时这是 operator 遇到的**第一个**状态 | Out of scope 声明 + runbook §3.4.2 补建 SQL（实测 `RANDOM_BYTES(16)` 与 `newContainerID` 输出形状一致）+ brief 新增 **PR-6** 切片（补建/reconcile），D1 的不变量由此才可达 |

**代码（P1×2 + P2×7 + nit×4）**

| | 问题 | 修法 | 变异验证 |
|---|---|---|---|
| P1-1 | 干净释放过、`attempts` ≥ 被下调的 `MaxAttempts` 的行：认领要 `attempts<max`、sweep 要 `lease_until IS NOT NULL` → 永久 pending 僵尸，且 runbook 把 `pending` 上涨读成「目标不响应」，主动误导诊断 | sweep 谓词放宽为 `IS NULL OR <= grace`（NULL 臂的安全性依赖 `MaxAttempts ∈ [1,50]` 的配置期保证，一并落地） | 还原旧谓词 → 新复现测试 FAIL |
| P1-2 | `TIMEOUT` 无上界（`10m` 被原样接受）而租约固定 2 分钟、又无心跳：租约在调用中过期 → 并发重跑 + 被运行中写 abandoned | 同 S-1 的配置期校验（`maxProvisionTimeoutFraction=4`） | 越界用例表 |
| P2-1 | sweep 的 UPDATE 只复查 `id+status`：滚动变更 `MAX_ATTEMPTS` 时两副本对 max 不一致，旧副本会在新副本认领后清掉新租约、把运行中的行写成 abandoned | `AND attempts >= ? AND (lease_until IS NULL OR lease_until <= ?)` 带进 UPDATE | 代码走查（滚动窗口竞态无法本地复现时序） |
| P2-2 | query string 在 MAC 之外（canonical 只签 path，请求发完整 URL）：`?tenant=A` 未认证，改成 `?tenant=B` 照样验过 | `ValidateTarget` 拒绝 `RawQuery`/`ForceQuery`/`Fragment`/`Opaque`，`Hostname()` 替代 `Host`（`http://:8080/` 会骗过后者）；对齐 `cardactiondispatch.validateCallbackURL` | `TestValidateTarget` 四个新负例 |
| P2-3 | 未收窄 gauge 只算 `ready` 且 target 被移出配置时归零 —— 少报 + 与本仓自己的另一段注释矛盾 | 改 `ready+pending+abandoned`；未配置的 target 按**未收窄**计；`disband_pending` 刻意排除并写明理由 | gauge 测试三种状态 + rollback 分支 |
| P2-4 | `Retryable` 被计算、从未被消费；`container_id_mismatch` 文档写「永久」实际重试 12 次共 ~23 分钟 | **删除**该抽象（它编码了错误的问题——worker 故意重试 404/401）；worker 改按**类别**短路三个永久 outcome（`container_id_mismatch`/`invalid_request`/`encode_failed`），其余照旧重试 | 还原 → 立即放弃测试 FAIL；另加「404 必须继续重试」的反向测试 |
| P2-5 | 两处注释声称「Space 级联与无主路径都经过 disbandProjectTx」——本 base 上它只有 1 个调用方 | 改写为前瞻式（这是未来级联会落地的位置），并把「Space 解散不触发项目解散 → ready 行永不转 reclaim」这个真实 gap 记录在案 | 文档 |
| P2-6 | 三个守卫洞：`zap.Any("job", job)` 反射序列化绕过按名守卫；披露测试只遍历第一个项目的行；main-wiring 守卫子串匹配常量名而非 `os.Getenv` 值 | 禁 `zap.Any/Reflect` 整体记录 job；披露断言遍历**两个**项目的行；匹配 `os.Getenv(...)` 包裹 | `zap.Any` 注入 FAIL；把常量名换进 args FAIL；自检注入第二项目 id FAIL |
| P2-7 | 单循环 + `target IN (...)`：一个不健康的 target 用 20×10s 占满批次，`provisioningRunning` 让 15s 的 tick 全部让位 → 健康的 drive 在 fleet 的超时后面排几分钟 | 每 target 一个 goroutine + 各自批次配额（`BatchSize/len(targets)`）；等待是**被移除**而不只是被限制 | 结构性，测试覆盖两 target 各自推进 |
| nit | `main.go` 指向不存在的 `main_wiring_test.go`；偏离条数 6 vs 7；`space_id` 不在 D2 列清单；接收方拿不到 `internal/` 包的 canonical string | 全部更正；条数改为不写死的引用；`space_id` 记入 context.yaml；**canonical string 逐字节写进契约**（见下） | — |

**一致性检查包（评审「需人工确认」第 1 条的落地）**

`internal/projectprovision/conformance.go`：4 条固定向量（valid / stale_timestamp /
tampered_body / wrong_secret，含期望签名与固定时钟）+ canonical string 六字段逐字节说明
+ 一个防漂移测试（每条签名用 `Sign` 重算）。**运行时决策：接收方被刻意**不**要求对已见
请求去重** —— octo-server 的重试可能落在同一秒，时间戳与签名完全相同，要求拒绝重复会
把合法重试拒掉烧掉一次尝试。时间戳窗口就是那条边界；残余风险（窗口内重放重建那**一个**
容器）写死在注释里，不粉饰。**把 target 加进 `TARGETS` 的前置条件 = 该子系统的端点跑通
这组向量**（runbook §3.2）。

**评审确认正确、无需改动的**（原文照录要点）：`errors.Join` 与 1213 重试兼容；应用侧
UTC 时钟两侧一致；`attempts` 恰好一次自增；`truncateProvisioningError` 的字节/字符处理
正确；无 secret 进日志；锁序如文档；退避级数算术正确。

**未修（经用户决策）**：不强制 https —— 有意允许，理由与代价（明文链路上 id 暴露 + 请求
可被抓取）已写进 `ValidateTarget` 注释，并说明这正是新鲜度校验为 MUST、向量必须跑通的
原因。

---

### 2.6 第二 / 第三份 review 追加修掉的 6 项

同一个 head（`4587b788`）上另外来了两份 review，其中 Jerry-Xin 跑了**真引擎**（MySQL 8.0.46
生产 collation 形状、8 线程锁交错探针、六项守卫变异、对 #846 活头做 merge-tree）。它确认了
第一份的所有阻塞项，并且**抓出我上一轮漏掉的东西**：

| | 问题 | 处置 | 变异验证 |
|---|---|---|---|
| **P1-A(2)** | 「P1 的三处修正」里我只修了 heartbeat 那一条声称，**漏了 purge 不 drain** —— 代码是每小时一条固定 `DELETE ... LIMIT 1000`，正是 #797 标记过的形状；#846 的 `purgeRemovalJobs` 有现成 drain 循环 | 改成 drain（循环到短批为止）+ 每 tick 总量上限，撞上限打 Warn（那才是「保留策略跑不过 churn」的信号，固定上限会把它藏起来） | 退回单批 → 新测试 FAIL |
| **retention × PR-2** | 90 天 retention 的理由是「子系统靠轮询 D9 端点得知解散」，而**那个端点在本仓不存在**（就是 PR-2）。开启后时钟就开始走，一旦 purge 掉 `disband_pending`，状态答案永久变 `unknown` —— 而 D9 让它与「不在你的 grant 内」不可区分，容器**永远无法回收** | 新增 **per-target** 的 `OCTO_PROJECT_PROVISION_{FLEET,DRIVE}_RECLAIM_CONSUMER_LIVE`（均默认 off）**门住 purge**，且开关集合作为 `target IN (...)` 谓词穿进 DELETE。删行是本片唯一不可逆操作，不能跑在假设上；留行只花存储，漏容器是泄漏。**曾经是进程级布尔**，那样先交付消费方的子系统会顺带授权删掉另一个的回收账（上游复审判为阻塞项） | 门控测试双向 + fleet/drive 混合断言未声明 target 的行存活 |
| **census 随 rollback 消失** | `refreshProvisioningMetrics` 原本在 `startProvisioningWorker` 里调度，共享同一个 early return —— 于是 runbook 的 rollback（清空 `TARGETS`）会在下次部署把**所有** provisioning gauge 一起带走，包括 runbook 承诺「保留」的 `pending` 积压 | 拆出 `startProvisioningMetrics`，**无条件调度**。rollback 恰恰是有人在盯这些数字的时候，而消失的 series 读起来就是 0 | gauge 测试加 rollback 后 census 断言 |
| **sweep 覆盖 last_error** | sweep 用常量覆写 `last_error`，毁掉唯一的按行失败证据 —— 而且恰好在这条路径上（pod 被打死）连日志都没有，`plan.md §3.4` 还让人照着这个字段去判断要不要重排 | 改成**追加**（`LEFT(CONCAT(...), 255)` 防列宽超限 —— 超限会让 UPDATE 挂掉、行回到不可 sweep 状态，等于把僵尸换个层次复现） | 断言 sweep 后仍含 release 期的原因 |
| **两个测试空洞** | reviewer 变异发现：删掉 `finishProvisioningJob` 的 `AND lease_owner = ?` **整包仍绿**（没有任何测试驱动过租约易主）；撤掉 sweep 的整租约宽限**也仍绿**（两个 fixture 都在窗口外，一个远过期一个未来） | 两个新测试：租约易主后陈旧执行者的终态写入必须落空；租约刚过期 30s（宽限窗口**内**）必须不被 sweep | 两个变异 → 两条 FAIL |
| **gauge label 基数** | `provisioning_target_misconfigured` 把 `TARGETS` 里的**任意 operator 文本**当 label 值，违反本文件自己「label 全是闭集」的声明；Prometheus 永不遗忘 label 值，打错一次留一条永久 series | 未识别的名字折叠成 `unknown`；真实名字仍在构造期的 Error 日志里 | — |

另外修掉两处陈旧注释（`main.go` 指向不存在的守卫文件 —— **三份 review 都提了**；
`TestEnsureSignsTheCanonicalRequest` 的注释还写着「container id 在 eventID 槽」而测试
断言的已是 sha256）。

**引擎侧被确认正确的**（照录要点，免得复审重跑）：8 线程 ~500 事务的
create/disband/claim/sweep/purge 交错**零 1213/1205**；`(project_id,target)` 并发插入恰好
一个赢家 + 一个 1062；两个并发 `SKIP LOCKED` 认领取到不同行且 `attempts` 各自 +1；
迁移在**生产形状**（默认 collation `utf8mb4_0900_ai_ci`）的服务器上干净应用，且本片每条
语句都是单表、不存在跨表表达式面（#842 判定过的 1267/1270 类根本不会出现）；
EXPLAIN 实测 disband 标记走 `uk_..._target`；claim / sweep / purge 三条各自走
`idx_..._pending` 或 `idx_..._finished` —— **两个索引现在共享 `(target, status)` 前缀，所以
任一个都能服务这三条语句，优化器具体挑哪个在不同数据量下会互换，功能等价**（这条记录写于索引
改成 target 前缀之前，当时的观测是 claim/sweep 走 pending、purge 走 finished；之后复审实测两侧
互换）。普查 `GROUP BY target, status` 现在是覆盖索引读（`Using index`）；对 #846 活头 merge-tree **恰好两处文本冲突**（与 PR body 声称一致）、
迁移 id 不撞、两种合并顺序都无锁环。

---

## 3. 运维手册

### 3.1 默认状态：完全惰性

`OCTO_PROJECT_PROVISION_TARGETS` 为空（默认）时：不入队、不起 worker、**不出网**，
建项目的行为与合并前逐字节相同。**合并本 PR 不需要任何配置变更。**

默认状态下仍在跑的只有两件事，都是有意的、且都不产生任何出网或用户可见行为：

- **解散事务里对本表的 UPDATE 是无条件的**（`markProvisioningDisbandPendingTx`）。加
  `Enabled()` 门会让「启用过 → 产出过行 → 后来关掉」的 target 不再标记可回收，那是真泄漏。
  空表上这条 UPDATE 影响 0 行。参见 §3.5 步骤 2 的方框 —— 这也正是回滚必须先退二进制的原因。
- **行数普查按 `MetricsInterval` 无条件调度**（`startProvisioningMetrics`）。放在启用门内
  会让文档化的回滚把所有 provisioning 计量一起带走，而回滚恰恰是有人在盯这些数字的时候。
  空表上是一次覆盖索引的 `GROUP BY`。

（早期版本这里写的是「也不改变任何现有行为」—— 上面两条使那句话略微过头，虽然两者都不产生
外部可观察的行为变化。）

### 3.2 打开一个 target（P-2 落地之后再做）

```bash
OCTO_PROJECT_PROVISION_TARGETS=fleet
OCTO_PROJECT_PROVISION_FLEET_URL=https://<fleet>/api/internal/workspaces/ensure
OCTO_PROJECT_PROVISION_FLEET_SECRET=<>=32 字节，且不得等于任何其它内部 token>
# 该子系统声明「已按 Project 收窄鉴权」（fleet 的 R2 / drive 的 R3）之后再置 true：
OCTO_PROJECT_PROVISION_FLEET_NARROWED=false
```

env 走 configmap，两个方向都需要滚动重启（与 P0 的两个开关同一性质）。

**打开之前必须确认两件事：**

1. **P-2（子系统侧服务身份）已经就绪。** 否则每个新项目都会产出一条在 **~23.5 分钟**后
   走到 `abandoned` 的行，而 `abandoned` 没有任何自动重驱动。
2. **该子系统的 ensure 端点已经跑通 4 条一致性向量，且成功请求精确返回
   `200` 和回显的 `container_id`。**
   （`internal/projectprovision/conformance.go`：valid / stale_timestamp / tampered_body /
   wrong_secret）。这不是形式主义 —— 三条 MUST 里，canonical string 写错会 fail closed
   会自己暴露，而**时间戳校验写松了 fail open 且完全静默**，本仓没有任何东西能发现它。
   向量就是把「已评审」变成一个可执行动作；成功响应形状另做一次真实
   ensure 探针，因为签名向量本身不覆盖响应。

**retention purge 由 per-target 开关门住，两个都默认 off：**

```bash
OCTO_PROJECT_PROVISION_FLEET_RECLAIM_CONSUMER_LIVE=false
OCTO_PROJECT_PROVISION_DRIVE_RECLAIM_CONSUMER_LIVE=false
```

**每个开关只授权删除它自己那个 target 的行** —— 开关集合是直接作为 `target IN (...)` 谓词
穿进 DELETE 的，不是只在调用点做一次布尔判断。**只有当该子系统自己确实在轮询**
`POST /v1/internal/projects/status`（= PR-2 已上线，且**这个** target 有消费方）之后，才置
它自己那一个为 true。

> **为什么必须 per-target。** 消费方是**按子系统**交付的：fleet 与 drive 归不同团队、排期
> 不同，本切片里关于一个 target 的其它一切（启用、URL、secret、收窄声明）本来就都是
> per-target 的。早期版本这里是一个进程级布尔，配一条不带 target 谓词的 DELETE ——
> 于是**先交付消费方的那个子系统会顺带授权删掉另一个子系统的回收账**：90 天后那些行消失，
> 状态答案永久变 `unknown`，D9 让它与「不在你的 grant 内」不可区分，容器永远回收不了；
> 而删行是本切片**唯一不可逆**的操作，也没有「被删掉但未回收」的计量。上游复审把这条判为
> 阻塞项，已改成 per-target（回归测试：`TestPurgeSparesRowsOfTargetsWithNoDeclaredReclaimConsumer`
> 用 fleet/drive 混合行断言「未声明的那个 target 的行必须存活」）。

在开关置 true 之前，该 target 的 `disband_pending` 行只增不删 —— 这是有意的：留行只花存储，
漏容器是泄漏。

> 旧的进程级 `OCTO_PROJECT_PROVISION_RECLAIM_CONSUMER_LIVE` **已退役**。它没有被静默忽略：
> 配置加载期检测到它被设置就记一条 Error 级 problem（`resolveReclaimTargets`），因为
> 「静默 off」正是操作者最可能误以为自己已经打开的状态。

**两个 knob 有硬边界，越界会被拒绝并回落默认值**（配置加载期报 Error 日志）：

| env | 允许范围 | 越界的后果（如果不拒） |
|---|---|---|
| `OCTO_PROJECT_PROVISION_MAX_ATTEMPTS` | `[1, 50]` | `4294967296` 会被 `int` 解析通过再 cast 成 `uint32(0)`，于是 `attempts < 0` 恒假 —— 整片从第一个 tick 起静默变 no-op |
| `OCTO_PROJECT_PROVISION_TIMEOUT` | `> 0` 且 `<= 30s`（租约 2 分钟的四分之一） | 没有心跳，超时超过租约就会让另一副本并发跑同一行，且 sweep 会在执行者还在跑时写 `abandoned` |

> 这个数字是算出来的，不是估的：`provisioningRetryDelay` 是 `min(2^attempt, 300s)`，
> `release` 收到的 attempts 是 1..11（第 12 次放弃），
> 即 `2+4+8+16+32+64+128+256+300+300+300 = 1410s`。改 `MaxAttempts` 或改封顶值时重算。
> 本文件早期版本写的「约 1 小时」是错的（复审 H-2 抓到）。

### 3.3 需要盯的指标

| 指标 | 含义 |
|---|---|
| `project_provisioning_rows{target,status}` | 行普查。`pending` 持续上涨 = 目标不响应；`abandoned` 非零 = 需要人 |
| `project_provisioning_unnarrowed_containers{target}` | **暴露面大小**（多报）：`ready + pending + abandoned`，因为响应丢失会留下「容器存在而行不承认」的状态；target 被移出配置**不会**让它归零（收窄是子系统的属性，不是配置的属性）。`disband_pending` 刻意不算 —— 那些已明确列入回收清单，看 `provisioning_rows` |
| `project_provisioning_target_misconfigured{target}` | 1 = 这个 target 被要求了但配置被拒（坏 URL / 短 secret / 凭据撞车），它不会预置任何东西 |
| `project_provisioning_attempts_total{target,outcome}` | **一次尝试恰好一个增量**，所以 `sum by(outcome)` 就是尝试次数。`target_no_ensure_endpoint`（= 404，P-2 还没到）与 `target_5xx`（= 真故障）在**第一次尝试**就能分开；`panic` / `target_disabled` 也各有自己的 outcome。刻意**没有** `abandoned` 这个 outcome —— 「多少行放弃了」由上面那个 gauge 回答 |
| `project_write_rejected_total{reason="provisioning_enqueue"}` | 建项目因为 outbox 写失败而失败。非零说明是本切片让 create 挂的 |

### 3.4 P-2 落地后重驱动已放弃的行

这一节有**两种**要处理的形状，别混：一种是「行在、但放弃了」（`status = 2`），另一种是
「压根没有行」—— 后者是**首次启用时的默认状态**（所有先于启用创建的项目都没有行），
以及某个 target 配错期间创建的项目。

### 3.4.1 行在但已放弃（`status = 2`）

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
> `UTC_TIMESTAMP` 与会话时区无关（不同于 `NOW()`），所以这里是安全的；分批 500 是为了
> 不让一条 UPDATE 锁住大范围。

### 3.4.2 压根没有行（首次启用 / 配错窗口）

PR-5 只为「创建那一刻已启用且配置合法」的 target 入队，没有任何补建路径（见 brief 的
Out of scope 与 PR-6）。所以**开一个 target 之后必须为已有项目补建一次**，每个 target
各跑一遍（注意换前缀）：

```sql
-- fleet：前缀必须是 octows-；drive 是 octods-（见 containerIDPrefix）
INSERT INTO octo_project_provisioning
  (project_id, space_id, target, container_id, status, next_attempt_at, created_at)
SELECT p.project_id, p.space_id, 'fleet',
       CONCAT('octows-', LOWER(HEX(RANDOM_BYTES(16)))),
       0, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)
  FROM octo_project p
  LEFT JOIN octo_project_provisioning pp
    ON pp.project_id = p.project_id AND pp.target = 'fleet'
 WHERE p.status = 1 AND pp.project_id IS NULL
 LIMIT 500;
```

> **必须是 `RANDOM_BYTES(16)`，不能用 `RAND()` 也不能用 `UUID()`。** 这是整个系统里唯一
> 由 SQL 生成容器 id 的地方，而容器 id 的不可推导性是 R2/R3 落地前唯一的阻断（D1）。
> `RAND()` 不是密码学随机；`UUID()` 是 v1，含时间戳与 MAC，可推导。`RANDOM_BYTES` 走的是
> OpenSSL 的 CSPRNG。
>
> 输出形状与 `newContainerID` 一致 —— **在 MySQL 8.0.33 上实测**：
> `octows-` + 32 位小写 hex = 39 字符，落在 `container_id VARCHAR(64)` 内。
>
> `LIMIT 500` 分批重复跑到 0 行为止。`(project_id, target)` 上有唯一键，所以重复执行
> 不会造出第二行；`status = 1` 保证已解散的项目不会被补建。

### 3.5 回滚

**顺序是强制的：先退代码，再退表。** 中间那一步不能跳，理由见步骤 2 的方框。

1. **停功能（不动表）**：`OCTO_PROJECT_PROVISION_TARGETS=`（清空）+ 滚动重启 → 立刻停止
   入队与出网。已入队的 `pending` 行**保留**：认领和清扫都带 target 过滤，所以这些行既
   不会被认领、也不会被清扫成终态 `abandoned`，重新打开开关就从原处继续。
   `provisioning_rows` 计量与本表无关地继续发布，回滚期间照样能看到积压。

   > 绝大多数回滚**到这里就该停**。这一步完全可逆、不丢任何回收账，也不需要任何 DBA 操作。

2. **只有确实要连表一起回退时，才继续；而且必须按下面三步走。**

   > ⚠️ **不要手工 `DROP TABLE`，也不要在旧二进制还在服务时动表。**
   >
   > 两个原因，都实测于本切片的代码：
   >
   > - **步骤 1 并不会停掉解散路径对本表的写入。** `disbandProjectTx` 里
   >   `markProvisioningDisbandPendingTx` 是**无条件**调用的，而这个位置是**设计上正确
   >   的、不该改**：若给它加 `Enabled()` 门，一个「曾启用 → 产出过行 → 后来关掉」的
   >   target 就再也不会把自己的容器标成可回收，那是真泄漏。代价是：只要还有一个旧二进制
   >   在服务，表一旦不存在，**每一次项目解散**都会拿到 `Error 1146` →
   >   `disbandProjectOnce` 报错 → **500**，而且是在一条与被回滚功能毫无关系的路径上。
   > - **手工 drop 不会自愈。** 模块迁移在启动时由 `module.Setup` 应用（octo-lib
   >   `module/module.go:88` 的 `migrate.Exec(..., migrate.Up)`），手工 drop 会把 `gorp_migrations` 里
   >   `20260907000001` 那条账本行留在原地 —— sql-migrate 认为它已应用，**重启不会重建
   >   表**。解散会一直坏着，直到有人再手工删账本行。
   >
   > 三步：
   >
   > 1. **先把二进制回退**到不含本迁移的版本（或至少不含 `modules/project` 本切片的版本），
   >    滚动重启完成、确认没有旧 Pod 还在服务。此时已经没有任何代码路径引用本表。
   > 2. 再退表。**两条语句必须都执行，但它们无法放进同一个事务** —— 顺序如下：
   >    ```sql
   >    -- 注意：DROP TABLE 会触发隐式提交，把它包进事务不会得到原子性。
   >    DROP TABLE IF EXISTS `octo_project_provisioning`;
   >    DELETE FROM `gorp_migrations` WHERE id = '20260907000001_project_provisioning.sql';
   >    ```
   >    账本行才是关键的那一半，也是「手工 drop」之所以被禁止的全部理由 —— 只 drop 不删
   >    账本，sql-migrate 认为这条迁移已应用，**重启不会重建表**；两条都做，则以后重新
   >    部署新二进制时会干净地重建。
   >
   >    > ⚠️ **本文件早先把这两条包在 `START TRANSACTION; … COMMIT;` 里，并称「必须放进
   >    > 同一个事务」。那是错的**，而且错在一个会误导操作者的方向上：MySQL 的 DDL 会
   >    > **隐式提交**，所以 `DROP TABLE` 一执行事务就已经结束了，后面的 `DELETE` 属于另一个
   >    > 事务。实测（本地 MySQL 8.0.46，`START TRANSACTION; DROP TABLE t; DELETE …; ROLLBACK;`）：
   >    > 回滚**两样都没恢复** —— 表没回来，账本行也没回来。
   >    >
   >    > 早先那句「已实测走通」本身不假：两条**按顺序执行到底**确实到达预期终态，这也是
   >    > 它被评为 P2 而不是阻塞项的原因。假的是它暗示的**原子性**。真实风险是：`DROP` 成功
   >    > 而 `DELETE` 失败（连接断开、权限、手误）时，你恰好落在本段自己警告的那个状态 ——
   >    > 表没了、账本行还在、重启不会重建。所以第二条语句失败时必须**立刻重试**，不要靠
   >    > 事务回滚，因为没有可回滚的东西。
   >
   >    > ⚠️ 本仓**没有** `dbconfig.yml`，也没有 Makefile 的迁移目标：迁移是由
   >    > `module.Setup` → octo-lib `module/module.go:88` 的 `migrate.Exec(..., migrate.Up)` 在**进程启动时**对各模块
   >    > `go:embed` 的 SQL 目录施加的，没有可用的 `sql-migrate` 命令行入口。
   >    > （本文件早先指向 `pkg/db/mysql.go`。那个文件**确实**有 `migrate.Exec`，但它在
   >    > `Migration()` 里，而这条链是死的 —— 只是死在比早先说法更上一层：`Migration()`
   >    > **确实有**一个非测试调用方（`pkg/db/mysql.go:32`，在 `NewMySQL` 内），但
   >    > `pkg/db.NewMySQL` 自己**零调用方**：全仓唯一写着 `db.NewMySQL(` 的地方
   >    > （`session_rollout_cmd.go:276`）按它第 46 行的 import 解析到的是 **octo-lib 的
   >    > 同名函数**，签名都不同（4 个参数 vs 3 个）。所以那不是引用不精确，是指向了一条根本
   >    > 不执行的路径。（早先还引用了 `testutil` 显式设 `cfg.DB.Migration = false` 作为佐证：
   >    > 这句字面为真，但那个开关在本仓和锁定版 octo-lib 里都没有非测试消费方，它什么也没门。）
   >    > 回滚步骤本身是对的，错的是它让人去看哪段代码。）本文件早期
   >    > 版本写的 `sql-migrate down -limit=1 -env=<env>` **在本仓根本跑不起来** —— 而一个
   >    > 跑不通的补救步骤，恰恰会把操作者推回同一段落禁止的手工 DDL。如果将来引入了
   >    > dbconfig，再换回 Down 段调用；在那之前，上面的**两条顺序语句**就是等价物，第二条失败
   >    > 时仍须立刻重试，不能依赖事务回滚。
   >
   >    （对照：migration 的 Down 段本身写的是 `DROP TABLE IF EXISTS
   >    octo_project_provisioning`，与上面第一条语句一致。）
   > 3. 退表**之前**必须确认没有已创建的容器还需要回收：表一旦删掉，本地就再也答不出
   >    「哪些项目曾被预置进 fleet/drive」。若还有，先按 §3.4.2 的口径把
   >    `container_id` 导出留档。

3. **回收账的时序约束**（与上面同一件事的另一面）：`disband_pending` 行的 90 天保留期是
   一个**回收窗口**，而消费它的 `POST /v1/internal/projects/status` 在 PR-2 才落地。所以
   在 PR-2 上线前就启用某个 target 的话，这些行会在 90 天后被删（如果那时该 target 的
   per-target reclaim 开关已置 true），而中间从未有任何消费方能读到它们。参见 §3.3 的
   per-target 开关说明 —— 默认 off 就是为了让这条时序约束**不需要靠人记住**。

---

## 4. 明确不在本切片内

- 任何把容器 id 交给客户端的路径，以及项目详情上的 `provisioning:` 字段（D12 禁止读路径以本表为门）。
- 任何解散推送（拉取式，D9）。
- 子系统侧的 `ensure` 端点（P-1）、服务身份（P-2）、按 Project 收窄的鉴权（P-3 / R2 / R3）。
- PR-0 / PR-1 / PR-2 / PR-3 的任何内容。
