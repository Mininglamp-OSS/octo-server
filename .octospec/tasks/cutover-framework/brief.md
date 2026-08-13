---
type: Task
title: "Task: cutover-framework"
description: Extract the shared control plane of the three one-way cutover mechanisms (#627/#697/#733) into pkg/cutover and fold the two standalone operator tools into the server binary as `app cutover <domain>`
tags: [cutover, operability, refactor, cli, botevent, msgextra, migration]
timestamp: 2026-08-13T00:00:00+08:00
# --- octospec extension fields ---
slug: cutover-framework
upstream: "follow-up to #627 (PR #644/#648), #697 (PR #702), #733"
source: self
---

# Task: cutover-framework

## Conclusion

仓库里有三套"单向数据面 cutover"机制，且是**刻意同形**的（`modules/robot/sql/20260805000001_bot_event_seq_state.sql:14-18` 与 `pkg/botevent/mode.go` 的注释都写明与 #627 同形）。#733 之后三套的权威状态**全部**收敛到了 MySQL 单例行 + CAS，同构程度进一步提高，但控制面代码仍是三份手写：

| 逐字重复的部分 | #627 | #697 | #733 |
|---|---|---|---|
| singleton state 读取（missing 语义） | `internal/msgextraseq/store.go` | `pkg/botevent/state.go` | `pkg/auth/session_rollout_control_store.go` |
| FOR UPDATE CAS flip（幂等 + floor 校验 + affected==1 + epoch/version+1） | `activation.go:150` | `state.go:141` | `control_store.go:227` |
| expected-mode guard env（unset=不断言 / malformed fail-closed） | `store.go:104-117,254-268` | `mode.go:298-355` | （已降级为 seed-only） |
| CLI 骨架（preflight/activate、-yes、-floor 回退推荐值、拒绝条件） | `tools/msgextra-version/main.go` | `tools/botevent-seq/main.go` | `session_rollout_cmd.go` |
| viper+TS_ env 的 loadConfig | `main.go:120-132` | `main.go:508-520` | `session_rollout_cmd.go:282` |

封装差异是最痛的一条：**Dockerfile 只 build 根 package（`Dockerfile:26-34`，只 COPY `/home/app`）**，`tools/msgextra-version` 和 `tools/botevent-seq` 不进镜像 —— 生产执行 cutover 要交叉编译 + `kubectl cp` 43MB 二进制；私有化交付则等于交付不可达的 runbook。#733 已经为 session-rollout 解决了这个问题（折叠为 `app session-rollout` 子命令，`main.go:111-121` 在 flag.Parse 前 dispatch），这就是本任务的先例。

## Goal

1. **新增 leaf 包 `pkg/cutover`**（只依赖 octo-lib / dbr，不 import modules/*），提供单向 cutover 的控制面公共设施：
   - `State{Mode, Epoch, Floor}` + `ReadState`（missing row/table → `ErrStateMissing`，语义由调用方决定）；
   - `Flip(spec)` CAS 原语：FOR UPDATE 锁 → 已激活幂等返回 → 锁内证据重算（可选闭包）→ floor 上下界校验（`ErrFloorTooLow`/`ErrFloorTooHigh`，含"严格大于 vs 不小于"开关）→ mode 条件 UPDATE + affected==1 不变量 → epoch+1；可选 pinned-connection 会话级 `innodb_lock_wait_timeout`（#627 的 activationTransaction 机制通用化，含 ErrBadConn 弃连保护）；
   - `ExpectedMode` guard env 解析/断言：unset=不断言，合法值断言，**malformed fail-closed**（两套现有语义的合并实现）；
   - `Domain` 注册表 + 统一 CLI 骨架：`preflight`（只读证据 + 推荐 floor）/ `activate`（-yes 门 + 先打 preflight + 域前置条件文案）/ `status`（新增轻量动作：只读 state row + guard env 解析，不做全量扫描）；共享 `-config`/`-floor`/`-yes` flag，域可注册附加 flag（如 botevent 的 `-sample`）。
2. **`app cutover <domain> {preflight,activate,status}` 折叠进服务器二进制**（根 package，仿 session-rollout 的 pre-flag.Parse dispatch），domains：`msgextra`、`botevent`。动手前先打印解析到的 MySQL/Redis 端点（采纳 2026-08-11 错连 127.0.0.1 事故的教训，`session_rollout_cmd.go:24-29`）。
3. **#627/#697 控制面迁移到框架上，行为逐条保持**：
   - `internal/msgextraseq/activation.go`：`Activate` 改为调用 `cutover.Flip`，排空屏障语义不变（FOR UPDATE 即 drain barrier、3s lock-wait、锁内重算 MySQL 两源 + Redis cursor 证据）；`Preflight` 不动；guard env 解析换共享实现（env 名不变）；
   - `pkg/botevent/state.go`：`ReadState`/`ReadStateContext`/`Activate` 委托框架原语（表名参数化，floor 严格大于语义保留）；
   - 两个 tools 的 CLI 逻辑（含 botevent 的三源 gather、`judgeMirror` 矩阵、flip 后 mirror 写入失败→非零退出+保持写暂停指引）移到根 package 的 `cutover_*.go`，测试随迁。
4. **删除 `tools/msgextra-version`、`tools/botevent-seq`**（跟随 #733 先例）；文档重整：
   - `tools/msgextra-version/README.md` → `docs/msgextra-cutover-runbook.md`，命令拼写更新为 `app cutover msgextra ...`；
   - 从 `tools/botevent-seq/main.go` 的嵌入文档 + CLI 输出成文 `docs/botevent-cutover-runbook.md`；
   - 新增 `docs/cutover-framework.md` 约定文档：state 表模板 DDL、guard env 命名约定 `OCTO_<DOMAIN>_EXPECTED_MODE`、**"先翻转、验证后才武装 guard、绝不与翻转同一波次发布"的顺序不变量**（两套机制里同一条最高频陷阱）、证据纪律（拒绝 sampled/不完整证据）、Down 自杀保险模式（3819 CHECK 技巧）、"无在线回退，roll forward"原则；交叉引用三份 runbook。

## Background

- #627：`octo_message_extra_version_state`，2 态 flip，有 FOR SHARE 排空屏障（writer 持锁到 commit），回滚=维护窗口抬 seq 水位+全副本重启（README §6）。**prod 尚未执行 cutover**——正因如此现在是统一 CLI 拼写的最佳窗口。
- #697：`octo_bot_event_seq_state`，2 态 flip，无屏障（INCR+ZADD 无事务可持锁），替代品是程序性写暂停；不可逆由迁移 Down 的 CHECK(singleton_id=1) 3819 自杀保险强制。flip 后必须写 Redis mirror `incr:{epoch}`，失败=激活不完整。
- #733：已把 #725 的 Redis floor + marker 状态机简化为 MySQL 单例 + append-only 证据表 + reconciler，5 阶阶梯。**它不是 2 态 flip，不进本框架**；它的价值是先例（app 子命令、端点自报、fail-closed 边界）。
- 灰度框架 `modules/featuregate` 语义不兼容（fail-open、可往返），不是本任务的基础设施。
- 学习条目 `count-entry-points-before-fixing-a-duplicated-block`：重复块要数清入口再抽——本任务即是对三入口重复的正面处置。`characterize-before-you-design`：已部署状态机的行为保持以现有特征化测试为准（activation_test/store_test/lock_order_test/seq*_test/main_test 全量保留且必须保持绿色）。

## 设计边界（收编 vs 留在域内）

**框架收编（控制面，三套里逐字或语义重复）**：singleton 读取、CAS flip、guard env、CLI 骨架、config 加载、端点自报。

**留在域内（领域特定，一行不动或仅搬家）**：
- #627：floor 证据从哪算（MySQL `message_extra.version` max + legacy seq 边界 + Redis cursor 扫描，锁内重算）；`MaxCutoverFloor = 2^53-1-1000` 上界；运行时 `ReserveTx`/`readStateForShare`/`Mode` 热路径。
- #697：三源证据（queue ZSET 扫描 + 两个 seq 命名空间全表扫）+ `observedMax+2000` 推荐值 + `maxSafeFloor=2^50` 上界；`judgeMirror` 判定矩阵与 mirror 写入；`mode.go` 的 belief cache / 非对称缓存全套。
- 差异点显式保留：#627 floor 允许 `>= observed`，#697 要求 `> observedMax`；#627 有排空屏障 + 3s lock-wait，#697 无屏障但有 activate 前置条件文案 + unauthorized-mirror 拒绝。

## Load-bearing list

- 两个 flip 的全部拒绝条件与 floor 校验语义（逐字保持；这是激活与事故的分界线）
- guard env fail-closed 语义（malformed ≠ unset；两套现网部署可能已依赖原 env 名——**env 名不改**）
- `main.go` pre-flag.Parse dispatch（不得影响 `app session-rollout`、`app api`/裸启动路径）
- #697 activate 的"authority 已 commit 但 mirror 写失败 → 非零退出 + 保持写暂停"的操作语义
- 源码守卫测试引用路径（`internal/msgextraseq/source_guard_test.go`、`pkg/botevent/genseq_guard_test.go`、`chokepoint_guard_test.go`——tools 目录删除后注释/清单同步）
- 按 `_index.yaml` paths 注入的规则：rate-limit、space-isolation（本任务不新增 HTTP handler、不触碰用户数据路径，语义上不相交，但按规则全文核对）；testing；commit-style

## Out of scope

- 运行时分配路径零改动：`ReserveTx`/`readStateForShare`/`Mode`（msgextraseq）、`NextEventID`/belief cache/gate/seed（botevent seq.go+mode.go 的分配与缓存逻辑）不改行为。允许的例外：两个包内 guard env 的**解析内部**换成共享解析器（语义与报错文案逐字保持）、state.go 委托框架、注释路径更新
- 不改任何表结构、不新增/修改迁移、不改现有 guard env 名（命名约定只约束未来新域）
- #733/session-rollout 代码零改动；`app session-rollout` 不折叠进 `app cutover`（5 阶阶梯 + reconciler + 7 个子命令，硬塞 3 动词框架会丢语义）；仅文档交叉引用
- 不实现在线 deactivate / 不改变"无在线回退"语义；不动 `tools/genseq-repro` 等无关工具
- 不把框架推广到 octo-lib（跨仓库另议）

## Acceptance

- `pkg/cutover` 单元/集成测试：guard 三态（unset/合法/malformed fail-closed）；Flip 幂等重跑、ErrFloorTooLow（两种比较语义各一）、ErrFloorTooHigh、missing row、affected==1 不变量、lock-wait 超时恢复与 ErrBadConn 弃连（需 MySQL，CI 跑）
- `app cutover msgextra preflight` 输出字段与旧工具等价；`activate` 拒绝条件等价（floor<推荐、>MaxCutoverFloor、缺 -yes、已激活幂等提示）
- `app cutover botevent preflight` 三源证据与推荐值等价；`activate` 拒绝：sampled 证据、failures>0、floor 越界、unauthorized mirror；mirror 写失败非零退出且输出保持写暂停指引；`-yes` 前置条件文案保留
- `app cutover <domain> status`：只读 state row + guard env 解析结果，不发起全量扫描
- 既有特征化测试全绿：`go test ./internal/msgextraseq/... ./pkg/botevent/... .`（根包含 CLI 测试）；`judgeMirror` 矩阵测试随迁不丢
- `tools/msgextra-version`、`tools/botevent-seq` 目录删除；全仓对旧路径的活引用更新（代码注释、guard 测试注释、SQL 注释指向新 runbook 路径）
- `docs/msgextra-cutover-runbook.md`（§1-§6 结构保留、命令更新）、`docs/botevent-cutover-runbook.md`、`docs/cutover-framework.md` 落地；`docs/token-session-rollout-runbook.md` 加交叉引用
- `go build .` 产出的二进制支持 `app cutover`（无 Dockerfile 改动即随镜像发布）；`go vet ./...`、golangci-lint、CI 全绿；i18n gate 不受影响（无错误码变更）

## COMPREHENSION

1. **最 load-bearing 的是什么？** 两个 flip 的拒绝条件与 floor 校验必须逐字等价——floor 校验是"激活"与"id 落在活 cursor 之下（#697 本尊）"的唯一分界；其次是 guard env 的 malformed-fail-closed 与"翻转后才武装"顺序。
2. **迁移为何可信？** 只动控制面（operator-only 路径），运行时热路径零 diff；控制面有完整特征化测试（activation_test 16K、store_test 20K、botevent main_test 的 judgeMirror 矩阵），迁移是"搬家+参数化"而非重写，测试随迁并保持绿色即行为保持的证明。
3. **失败时如何退化？** 框架全部 fail closed：missing row 拒绝激活、malformed env 拒绝断言、证据不完整/采样拒绝翻转、CAS affected!=1 报不变量违例；CLI 失败非零退出并保留域内原有修复指引文案。
