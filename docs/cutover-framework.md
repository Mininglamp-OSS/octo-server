# 单向 cutover 框架（pkg/cutover）

本仓库有一类反复出现的工程问题：**数据面号段/水位的一次性切换**——把一个
"多副本下会重号/乱序"的 legacy 分配器切到一个新的权威分配器，且**切过去就不能
低价回头**（退回低位号段会把新发的 id 落到客户端已持有的游标之下，数据永久不可
见——这正是每次要修的 bug 本身）。

这个模式已经出现了三次：

| | #627 msgextra | #697 botevent | #725/#733 token session |
|---|---|---|---|
| 权威状态 | `octo_message_extra_version_state` | `octo_bot_event_seq_state` | `octo_session_rollout_state` (+ append-only 证据表) |
| 阶段 | 2 态 flip | 2 态 flip | 5 阶阶梯 + reconciler |
| 排空屏障 | 有（写者 FOR SHARE 持锁到 commit） | 无（写入是无事务的 INCR+ZADD，用短暂写暂停替代） | 不适用（fence + lease） |
| guard env | `OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE` | `OCTO_BOTEVENT_EXPECTED_MODE` | （#733 后旧 env 仅作一次性接管种子） |
| 运维入口 | `app cutover msgextra` | `app cutover botevent` | `app session-rollout` |
| runbook | [msgextra-cutover-runbook.md](msgextra-cutover-runbook.md) | [botevent-cutover-runbook.md](botevent-cutover-runbook.md) | [token-session-rollout-runbook.md](token-session-rollout-runbook.md) |
| 回滚 | 维护窗口协调程序（抬 seq 水位 + 全副本重启） | 不可逆，roll forward（迁移 Down 以 3819 自杀保险强制） | 状态依赖的回滚阶梯，pause + roll forward |

前两套的控制面（状态读取、CAS 翻转、guard env、CLI 骨架）曾是逐字重复的手写
拷贝，现已收敛到 `pkg/cutover`。第三套（session rollout）**刻意不在**这个框架
上：它是带证据链与自动 reconciler 的多阶阶梯，不是两态 flip；它遵守本文档的
约定，但不共享这份代码。

## 与 featuregate 的分界

`modules/featuregate` 是 **fail-open、可来回切**的灰度开关，适合行为开关。
cutover 是 **fail-closed、单向、带水位校验**的数据面迁移：切换的先决条件是一个
经过证据校验的水位（floor），切换后退回去会重现原 bug。两者语义不兼容——
不要用 featuregate 做号段切换，也不要用 cutover 做可逆的功能灰度。

## pkg/cutover 提供什么

- `ReadState(ctx, db, table)` — 单例状态行读取。**缺行与缺表（MySQL 1146）都
  返回 `ErrStateMissing`**：两者都意味着"迁移还没在这里播种权威"，运行时读方
  把它当作 pre-cutover 的 legacy 默认值，operator 翻转则拒绝执行。它必须与
  "权威不可达"（其它错误原样上抛）区分开——cutover 之后这两种情况的安全答案
  相反。
- `Flip(ctx, db, FlipSpec)` — 单向 CAS 翻转：`FOR UPDATE` 锁单例行 → 已激活则幂等
  返回 → 锁内重算证据（可选闭包）→ floor 上下界校验 → mode 条件 UPDATE +
  affected==1 不变量 → epoch+1。可选 pinned-connection 的会话级
  `innodb_lock_wait_timeout`（fail-fast 后备，恢复失败则弃连不入池）。
  floor 比较语义按域选择：`FloorMustExceedObserved`（#697 严格大于）或默认
  不小于（#627，首个保留值是 floor+1 所以 floor==observed 安全）。
- `ExpectedMode` — guard env 解析与断言：**unset 不断言；合法值失配 fail
  closed；malformed 同样 fail closed**（typo 不得等价于未设置——那会无声解除
  唯一防止静默降级的防线）。错误文案留在各域：那些消息解释的是域后果
  （"legacy id 会落到活跃游标之下"），是运维在事故中读的文档。
- `app cutover <domain> {preflight,activate,status}` — 统一 CLI 骨架
  （根包 `cutover_cmd.go`），随 `/home/app` 进镜像。约定三条：
  - **动手前打印解析到的端点**（2026-08-11 的教训：一个放错位置的配置键让工具
    连上 127.0.0.1:6379 扫了本地测试残留还报告 complete）。凡是会读某个存储的
    动作，就要先报出那个存储的地址——floor 是从扫描算出来的，扫错实例等于算错
    floor。
  - **端点要脱敏**：MySQL 配置是完整 DSN，含库密码；只打印 `host:port/schema`。
    这行输出会进运维终端回滚记录、`kubectl logs` 和审计抓取。
  - **guard 读的是本进程 env，措辞必须说清楚**：guard 是进程本地配置，控制面无法
    远程观测，而 runbook 明确允许在跳板机上跑这些命令——不加限定词就会在已武装的
    机群上打出 "unset"，把最关键的安全网报成关闭。
  - **中断要两阶段**：装了信号 handler 就等于关闭了默认终止行为，而证据阶段
    （go-redis v6 的 SCAN 没有 per-command context）根本观测不到 ctx。所以第一次
    信号取消能取消的（flip 的 DB 语句），并**立刻恢复默认处理**，让第二次 Ctrl-C
    能终止取消不掉的扫描。只做第一阶段会让命令比它取代的旧工具更难中断——而
    msgextra 的证据扫描是在状态行 `FOR UPDATE` 持锁、所有写入被挡住的窗口里跑的。

**留在各域的**：floor 证据从哪些来源算、翻转前后的域特定步骤（#697 的 mirror
判定与发布）、以及全部运行时读路径（#627 的 FOR SHARE + ReserveTx、#697 的
belief cache）。框架收编的是控制面，不是数据面。

## 状态表模板

新域建表照抄这个形制（参照两张现表的迁移文件）：

```sql
CREATE TABLE IF NOT EXISTS `octo_<domain>_state` (
  `singleton_id`  TINYINT UNSIGNED NOT NULL COMMENT '恒为1的单例键',
  `mode`          TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=<active 语义>',
  `epoch`         BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '换代计数，operator CAS 递增',
  `cutover_floor` BIGINT NOT NULL DEFAULT 0 COMMENT '激活时校验过的号段下界',
  `updated_at`    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`singleton_id`),
  CONSTRAINT `chk_<domain>_singleton` CHECK (`singleton_id` = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- 种子 legacy 行，部署即惰性（幂等写法）：
INSERT INTO `octo_<domain>_state` (`singleton_id`,`mode`,`epoch`,`cutover_floor`)
  VALUES (1, 0, 0, 0)
  ON DUPLICATE KEY UPDATE `singleton_id` = `singleton_id`;
```

要点：

- **权威放 MySQL，不放 Redis。** 生产 Redis 是 `appendonly no`，激活状态跟着
  RDB 快照回退就是静默降级（#697 review 的核心发现；#733 用同一理由收编了
  #725 的 Redis floor）。Redis 只能做镜像或证据。
- **种子 legacy**，让 schema 部署行为中立；翻转永远是独立于部署的运维动作。
- **Down 自杀保险**（可选但推荐，照抄 `20260805000001_bot_event_seq_state.sql`）：
  Down 里先 `INSERT ... SELECT 2, ... WHERE singleton_id=1 AND mode=1`，
  已激活时违反 CHECK 报 3819 中止整个 Down，未激活时 SELECT 无行、照常 DROP。
  这把"激活后不许删权威"从文档约定变成机器强制（需要 MySQL ≥ 8.0.16）。

## guard env 约定

命名：`OCTO_<DOMAIN>_EXPECTED_MODE`（现有两个历史名字保留不改；约定约束新域）。
取值：该域的 mode 拼写（如 `legacy` / `transactional` / `incr`）。

**顺序不变量（两套现网 runbook 各自独立写过同一条，这是本模式最高频的陷阱）：**

1. 翻转前**必须不设**：一个期望 active 而状态行还是 legacy 的副本会把所有
   写入 fail closed——把 guard 和翻转塞进同一个滚动发布波次是自造写入事故。
2. 翻转经 §4 验证之后，才在所有副本上武装 guard。它是持久的读侧防线：状态行
   丢失/被重置时，武装了 guard 的副本 fail closed 而不是静默退回 legacy。
3. 回滚（若该域存在回滚程序）时，guard 必须在同一重启波次里解除或改回。

## 证据纪律

- preflight 只读、随时可跑；activate 前必跑，并对照。
- **采样的证据不是激活证据**（`-sample` 只用于快速查看，activate 拒绝）。
- **任何证据源读取失败，总量就是下界，下界不能用来校验不可逆翻转**（activate
  拒绝，修复读取错误后重跑）。
- floor 校验在锁内、对照翻转时点的证据（有排空屏障的域在锁内重算；没有的域
  用翻转前采集值 + 程序性写暂停）。
- 输出只报聚合计数与最大值，不打印用户/渠道标识。

## 无在线回退

框架刻意没有 `Unflip`。每个域要么书面化一个维护窗口级的协调回滚程序
（msgextra §6：抬水位 + 全副本重启，缺一步就重现 bug），要么明确 roll
forward（botevent）。"再切回去"的诱惑正是这类 bug 的第二次发生方式：新分配器
已经发出的 id 全在 legacy 下一个号段之上，回退后 legacy 发的 id 落在活跃游标
之下——同样的丢失，方向相反。

## 新域接入清单

1. 迁移：照模板建状态表 + 种子 legacy 行（+ 可选 Down 自杀保险）。
2. 运行时：读态用 `cutover.ReadState`（缺行=legacy 默认），写路径按域实现；
   guard 用 `cutover.ParseExpectedMode` + 域内错误文案。
3. 翻转：`cutover.Flip`，证据闭包按域实现；决定 floor 比较语义与上界。
4. CLI：在 `cutover_cmd.go` 的 `cutoverDomainList` 注册域，实现
   preflight/activate/status 三个动作（-yes 门、拒绝条件文案随域）。
5. runbook：docs/ 下成文，含 prepare / drain(或写暂停) / activate / verify /
   guard / rollback 六段，并链接回本文档。
