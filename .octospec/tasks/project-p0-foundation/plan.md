# 实施计划：project-p0-foundation

> **状态：已实施**（2026-09-04）。落地结果与偏差见
> [verification.md](./verification.md)。计划里三个 PR 切片在一次实现里全部落地；
> 下面 5.x 的待拍板项按 verification.md 记录的假设先行，均可单点回退。

> 本文件是 `brief.md` 的落地计划，写于 2026-09-04。
> 已按用户指示调整范围：**邀请码 / 自助加入（原 PR-2）整体降级为 P2**。
> 计划中每条"已核实"的结论都在本仓代码或本地 MySQL 8.0.33 上实际验证过；
> 未验证的地方一律标注为"待确认"，不做推断。

---

## 0. 范围调整（相对 brief）

### 0.1 邀请加入 → P2

原 PR-2 的全部内容移出本次交付：

- `octo_project_invitation` 表（**连表一起延后**，不预建空表）
- `GET /v1/projects/invite/:invite_code`（+ `/preview`）匿名预览
- `POST /v1/projects/join` 兑换
- `POST /v1/projects/:project_id/invite`、`GET .../invites`、`DELETE .../invite/:code`
- `StrictIPRateLimitMiddleware("project_invite", 10.0/60, 5)`

**为什么表也一起延后**（与 D6 的 `is_official` 处理方式不同，理由要写清）：
D6 让 `is_official` 列现在就落地，理由是"避免以后对一张已有生产流量的表做第二次
ALTER"。这个理由只适用于**给已有表加列**。`octo_project_invitation` 是一张全新表，
P2 里 `CREATE TABLE` 对既有流量零影响，不存在同样的成本。反过来，现在建一张
没有任何写入方的表，正是 brief 自己在排除 `removing` /
`octo_project_member_removal_im_pending` 时给出的理由——"untestable scaffolding"。

### 0.2 连带影响（必须显式承认，不能悄悄少做）

| brief 里的条目 | P0 调整后 |
|---|---|
| 三张表 | **两张表**：`octo_project`、`octo_project_member` |
| Rollback = drop 三张表 | drop **两张**表 + 去掉 `internal/modules.go` 的 blank import |
| I1 在 direct-add 与 invite-accept 两条路径都被拒 | 只剩 **direct-add**（+ 建项目时的 owner 席位） |
| `join_mode` 默认 1（仅邀请） | 列保留、默认 1，**P0 无消费方**，与 `is_official` 同一处理：只保证列存在且不被业务读写 |
| admission-rejection 指标按 entry point 拆分 | 标签维度**照样落地**（`entry` label），P0 只出现 `member_add` / `role_change` / `create_owner_seat` 三个值；P2 加值不改指标结构 |
| reconcile 扫"孤儿邀请" | 移出 P0 |
| 严格 IP 限流 | P0 **没有任何免登录端点**，`StrictIPRateLimitMiddleware` 不引入 |

### 0.3 P0 结束后的产品能力（下调后的真实边界）

**唯一入组路径 = 项目 owner/admin 从 Space 成员目录里直接加人。**
用户拿不到"发个链接让人自己进项目"的能力。这是一个产品可见的缩减，需要产品侧确认
（brief 原文正是用"`join_mode` 默认 1，没有邀请端点就没有加入路径"来论证邀请属于 P0）。
我的判断：管理员直接加人已经覆盖 brief 的 P0 目标句——"从 Space 目录里挑人组队"——
所以降级是自洽的；但"没有自助加入"这一条要写进 PR 描述，不能靠读者自己发现。

---

## 1. 代码侧可行性核验结果

以下都是我实际查过 / 跑过的，不是推断。

### 1.1 复用点行号核对（brief 引用全部命中）

| brief 引用 | 核实结果 |
|---|---|
| `modules/space/member_removal.go:74-95` 反向注册 | ✅ `RegisterMemberRemovalCleanupStep`，同名覆盖（latest wins） |
| 契约 `:56-64` | ✅ 幂等 / 自行判断无事可做返回 nil / 步骤间互不阻塞（`runMemberRemovalCleanupJob` 逐步 recover，首个错误上报，其余步骤照跑） |
| `pkg/space/membership.go:9-24` `CheckMembership` | ✅ `space_member.status=1 AND space.status=1` |
| `:26-51` `CheckMembershipForCleanup` | ✅ 只差一轴：`space.status <> 0`，封禁（2）算"席位仍在" → 跳过清理 |
| `pkg/space/middleware.go:158-165` 无 space_id 即 `c.Next()` | ✅ 确认 pass-through |
| `:215,220` `SetSpaceID` / `GetSpaceID` 导出 | ✅ 可被新中间件复用同一 context key |
| `:18-19` TTL 60s / 30s | ✅ `cacheTTL` / `negativeCacheTTL` |
| SpaceMiddleware 自身不合规 | ✅ 三个出口都是 `c.AbortWithStatusJSON(..., gin.H{"msg": "请先登录"/"无权访问该 Space"/"校验 Space 成员身份失败"})` |
| `modules/space/member_removal.go:274-285` 10s 轮询 / 10min 租约 / 退避 / abandoned | ✅ 另确认 `removalCleanupMaxAttempts=20`、总窗口约 70 分钟、abandoned **无自动重驱动** |
| `modules/channel/api.go:179-194` 防枚举 | ✅ 群不存在与非成员合并为同一 forbidden |
| `modules/space/member_removal.go:105+` 同步失效缓存 | ✅ 且有 `ErrMembershipCacheNegativeFallback` 两分支语义 |
| `modules/group/db.go:694` `querySameDayCreateCountWitUID` | ✅ `WHERE creator=? AND DATE(created_at)=?` |
| `modules/group/api.go:1033-1039` 日建群上限 | ✅ 阈值来自 `ctx.GetConfig().Group.SameDayCreateMaxCount`（octo-lib config） |
| `modules/space/api.go:157-171` invite/join 形状 | ✅（P2 用） |
| `modules/space/api.go:1366` joinPresetGroups 唯一索引缺陷 | ✅ 确认 D4 的类比成立 |

### 1.2 D4 生成列方案：本地 MySQL 8.0.33 实测通过

在 scratch 库（已 drop）上跑完 5 条：

| 场景 | 结果 |
|---|---|
| create "Q3" → disband → 同名重建 | ✅ 成功（`active_name` 变 NULL 后腾出名字） |
| 重复**活跃**同名 | ✅ `ERROR 1062 Duplicate entry 's1-Q3' for key 'uk_space_active_name'` |
| 多行已解散同名共存 | ✅ 三行 `active_name=NULL` 共存 |
| `INSERT` 里点名 `active_name` | ✅ `ERROR 3105` — D4 的实现注意事项确认存在 |
| `UPDATE` 里点名 `active_name` | ✅ `ERROR 3105` |

另核实 dbr v2.7.5：`load.go:98` 对无对应字段的列用 `dummyDest`，所以
`SELECT *` 读到生成列**不会**报错，只有写入侧要防 3105。
`util.AttrToUnderscore` 跳过 struct 类型字段（`db.BaseModel` 因此不进列清单），
所以只要 Go model 不含 `ActiveName` 字段就安全——但**结论是不依赖这个巧合**，见 3.3。

### 1.3 列宽 / COLLATE

本地 `test` 库实测：`space.space_id`、`space_member.space_id`、`space_member.uid`、
`user.uid` 全部 `varchar(40) utf8mb4 utf8mb4_general_ci`（库默认 collation 就是
`utf8mb4_general_ci`）。新表显式写 `VARCHAR(40)` + `COLLATE=utf8mb4_general_ci` 即对齐。

> ⚠️ **生产待确认（不能在这里验证）**：`space` / `space_member` / `user` 这几张老表
> 建表语句里**没有** COLLATE，实际 collation 继承的是**生产库的默认值**。若生产库默认
> 不是 `utf8mb4_general_ci`（MySQL 8 出厂默认是 `utf8mb4_0900_ai_ci`），我们显式写
> `general_ci` 反而会造成 JOIN 1267。上线前必须查一次生产
> `information_schema.COLUMNS`，把结论写进 PR。仓里已有同类前科（`space_member`
> 无 COLLATE vs `user_verification` 强制 `general_ci`）。

### 1.4 路由共存有生产先例，不需要"验证 gin 行为"

brief 说 `/v1/projects/invite/:invite_code` 与 `/v1/projects/:project_id` 可共存。
本仓 `modules/space` **今天就在生产上**这么跑：
`open.GET("/invite/:invite_code")` 与 `auth.GET("/:space_id")` 同挂 `/v1/space`（gin v1.9.1）。
P0 因为不做邀请，这条先例只在 P2 用到；但它同时证明了另一件 P0 要用的事——
**同一前缀下可以由不同模块 / 不同中间件的多个 `r.Group` 注册兄弟路由**
（space 自己就有 `auth` / `joinLimited` / `search` / `open` 四个 `/v1/space` group）。
所以 `modules/project` 注册 `/v1/space/:space_id/projects` 不与 `modules/space` 冲突，
前提是**wildcard 名字必须写成 `:space_id`**（与 space 一致），否则 gin 直接 panic。

### 1.5 迁移文件时间戳

现存最大是 `20260830000001_opanalytics_add_thread_parent.sql`。
本次用 `modules/project/sql/20260904000001_project_core.sql`（唯一一个文件，两张表）。
迁移执行顺序按文件名 VersionInt 全局排序（`internal/modules.go` 头部注释已说明），
blank import 顺序不影响，所以 20260904 一定在 space 之后。

### 1.6 CI 归属

`ci/unit-packages.txt` 是**白名单**，新包默认落 E2E lane。
`ci/run-e2e-shard.sh` 每个包前 `DROP/CREATE DATABASE test CHARACTER SET utf8mb4
COLLATE utf8mb4_general_ci` + `redis-cli FLUSHALL`，所以 CI 里天然是干净库 +
干净 Redis；本地跑要自己保证（见 memory `local_test_infra`）。
`modules/project` 不加进 `unit-packages.txt`（它要 MySQL/Redis）。

### 1.7 ⚠️ 测试包结构：一个会在 P1 才爆的 import cycle 陷阱

P1 里 `modules/group` 会 import `modules/project`。
如果 `modules/project` 的**同包**测试（`package project`）import 了
`octo-server/internal`（进而 group），Go 会把 group 重新编译到"测试增强版
project"上，构成 import cycle —— **同包测试的 cycle 是硬错误**，Go 只对
外部测试包（`package project_test`）放行。

所以测试包结构从第一天就必须定好：

- **`package project`（同包）**：源码守卫、纯函数（权限矩阵）、DAO / 事务 / 级联步骤 /
  reconcile 的集成测试。它通过生产代码传递 import `modules/space`（→ `modules/common`），
  于是拿到 space + common 的迁移，但拿不到 group/robot/user 的建表 →
  **TestMain 需要自建 fixture 表**，照抄 `modules/space/api_test.go:52-75` 的 `depDDLs`
  （`group`、`group_member`、`robot`、`user`、`user_verification`）。
- **`package project_test`（外部包）**：`_ "octo-server/internal"` 拉全量模块，
  用于 C1 启动冒烟 + C2 非回归（需要真的 group 级联步骤在场）。
  先例：`modules/bot_provision/bot_api_test.go`。

这一条如果不提前定，P1 会以"测试编译不过"的形式炸出来，而那时改的是一堆已有测试。

### 1.8 ⚠️ `modules/featuregate` 不在 `main` 上

memory 里写过"功能灰度复用 `modules/featuregate`"。核实：该目录在当前 worktree
**不存在**，只存在于未合并的分支（`c1fc02e7 feat(featuregate): ...` 等一串 commit）。
所以 `project_create_enabled` 不能依赖它，否则 P0 就绑上了一个未合并分支。
可选方案见 5.1（待拍板）。

### 1.9 ⚠️ `pkg/authtree` 的语义与 acceptance 有偏差

`pkg/authtree` 是**非会话认证树**（`uk_*` User API Key 树、bot token 树）的
路由贡献机制 + 租户普查表（包注释里那份逐 input 的清单）。
P0 的 Project 路由是**会话路由**，不挂进任何树，因此没有 `authtree.Add` 可写。
acceptance 那条"每个新路由有 authtree 普查条目"照字面做不到。

我的处理（需确认）：**不调 `authtree.Add`**，改为在
`pkg/authtree/authtree.go` 的包注释里追加一段显式记录——
"Project 路由刻意不进任何非会话树；`uk_*` 保持 Space 作用域，**不**变成
Project 作用域，这是有意的"。这正好满足 brief 那句"把这个决定显式登记为
intentional 而不是留白"。代价：**要动一个已有文件**（只加注释）。

### 1.10 ⚠️ `member_epoch` 单调性在"只读、每个 pod 都能跑"的扫描里做不到

D7 说 reconcile 只读、可在每个 pod 跑。但"epoch 单调递增"是一个需要**记住上一次
观测值**的性质，无状态只读扫描拿不到（跨 pod 更拿不到）。
brief 把它列进 reconcile 项是个真实缺口。

我的处理（需确认）：把保证下沉到**写纪律 + 源码守卫**，扫描只做兜底：

1. 真正的机制：SQL 里**只允许** `member_epoch = member_epoch + 1`，禁止任何绝对赋值；
   加一条源码守卫测试，扫 `modules/project/*.go` 里出现的 `member_epoch` 写入，
   凡不是 `member_epoch + 1` 形状的一律失败。单调性由构造保证，不靠观测。
2. 扫描侧只做两件廉价的事：`COUNT(*) WHERE member_epoch < 0`（哨兵）；
   进程内一个有界 LRU 记录 last-seen epoch，发现下降就告警（**best-effort，
   明确写成 best-effort**，不作为正确性依据）。

### 1.11 "audit" 在本仓是结构化日志，不是表

唯一先例 `modules/messages_search/audit.go` = `zap` 结构化日志行（低基数字段 +
keyword 哈希），仓里没有审计表。所以 acceptance 的"审计条目携带 actor / target /
reason"按结构化日志实现。若产品要可查询的审计流水，那是一张新表 + 一个独立任务，
不塞进 P0——这一点要在 PR 里说明，避免读者以为已经有审计查询能力。

### 1.12 `app_config` 短路分支：P0 不加字段

要给客户端下发"能否创建项目"，就得改 `modules/common/api.go`
（两个分支都发，见 `:389`、`:815-822`、`:904-908`）——那是一个**已有文件**，
和 acceptance 的"只碰新文件"直接冲突。

处理：**P0 不加 appconfig 字段**。能力位放在 Project 列表 / 详情响应里
（brief 本来就要求 `member_epoch` + `my_role` + capability bits 一起下发），
客户端从那里读。如果产品要"直接隐藏入口"，那是一个独立小 PR，
改 `modules/common/api.go` 时**必须两个分支都发**。

### 1.13 时区：新表用应用侧 UTC，不用 `CURRENT_TIMESTAMP`

老表（`space` / `group`）用 `timeStamp DEFAULT CURRENT_TIMESTAMP`，走 MySQL 会话时区。
仓里已经因此吃过一次亏（`member_removal_metrics.go` 那段注释：拿 Go 的 UTC 去减
会话时区的 `created_at`，在 `TZ=Asia/Shanghai` 部署下得到 −28799 秒，
而那个 gauge 恰恰是要在事故**发生前**告警的）。

新表照**最新**先例 `octo_group_welcome_delivery` 办：`DATETIME(3) NOT NULL`，
**无 DEFAULT / 无 ON UPDATE**，全部由 Go 写 UTC。
配套：DAO 一律显式列清单（不用 `AttrToUnderscore`），所以 `created_at` /
`updated_at` 必须显式带上——这同时解决了 D4 的 3105 与 D6 的"`is_official`
不被写入"。日建项目上限的"天"边界在 Go 里按业务时区（默认 `Asia/Shanghai`）算，
存的仍是 UTC，避免 group 那种 `DATE(created_at)` 既走不上索引、又依赖两个时钟同源的写法。

---

## 2. PR 切片（三个，各自可独立 revert）

原 brief 的 PR-1 太大（模块 + schema + CRUD + 成员面 + 中间件 + 级联 + flag +
reconcile + 指标 + 审计）。拆成三个，切点按"接触面风险"而不是按"代码量"：

### PR-A 地基：模块 + schema + CRUD + flag + 级联步骤
含 C1 / C2 两个非回归验证。**级联步骤在这一版就注册**——因为建项目会写 owner 席位，
`octo_project_member` 从 PR-A 起就有活跃行，I1 的反向恢复必须同时到位；
"级联先落地、此时几乎空转"是可测的（用 SQL 直接造行），不是 untestable scaffolding。

### PR-B 成员面：list / add / remove / leave / role + 权限矩阵 + 配额 + 缓存同步失效
`member_epoch` 在 PR-A 已有（建项目 / 解散两条路径），PR-B 把同一条纪律铺到全部成员写路径。

### PR-C 对账与观测：reconcile job + 分布/队列指标 + abandoned 独立告警

### P2（本次不做）
邀请码表 + 匿名预览 + `POST /v1/projects/join` + 邀请管理 + 严格 IP 限流。

---

## 3. 详细设计

### 3.1 文件清单

```
modules/project/
  1module.go                    register.AddModule + //go:embed sql + registerSpaceMemberRemovalCleanup
  api.go                        Route() + handlers（CRUD）
  api_member.go                 handlers（成员面，PR-B）
  api_i18n.go                   respondProject* helpers + mustLookupSharedCode
  middleware.go                 spaceIDParamMiddleware / projectMiddleware / 项目成员缓存
  service.go                    业务编排（事务边界、epoch 纪律、权限矩阵）
  db.go                         DAO（显式列清单）
  model.go                      DB model + 响应 struct
  config.go                     配额 / flag / reconcile 节奏
  space_member_removal.go       级联步骤（PR-A）
  reconcile.go                  对账 job（PR-C）
  metrics.go                    promauto 指标
  audit.go                      结构化审计日志
  sql/20260904000001_project_core.sql
pkg/errcode/project.go          新文件
pkg/space/member_role.go        新文件：MemberRole(session, spaceID, uid)（Space admin 判定）
internal/modules.go             + blank import（已有文件，acceptance 允许）
pkg/i18n/locales/active.zh-CN.toml   + zh-CN 条目（已有文件，acceptance 允许）
tools/i18nmarkers/server/active.en-US.toml  ← make i18n-extract 生成，diff 里会出现（acceptance 那条要补上这一项）
pkg/authtree/authtree.go        仅追加注释（见 1.9，需确认）
```

### 3.2 迁移草案

```sql
-- +migrate Up

-- Space 内的「项目」协作层（P0：成员池；P1 才有项目群）。
-- 时间列全部由应用侧写 UTC，无 DEFAULT / 无 ON UPDATE —— 与 octo_group_welcome_delivery
-- 同一约定。老表（space/group）的 CURRENT_TIMESTAMP 走 MySQL 会话时区，仓里已因此
-- 出过一次"gauge 在最需要它的时刻是坏的"（modules/space/member_removal_metrics.go）。
-- 宽度/字符集/COLLATE 显式对齐 space.space_id / user.uid / space_member.uid，
-- 否则对账与级联的 JOIN 在 MySQL 8.0 上报 1267。
CREATE TABLE `octo_project` (
  `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `project_id`      VARCHAR(40)  NOT NULL DEFAULT ''  COMMENT '项目唯一ID（32 hex，util.GenerUUID）',
  `space_id`        VARCHAR(40)  NOT NULL DEFAULT ''  COMMENT '所属 Space（对齐 space.space_id）',
  `name`            VARCHAR(64)  NOT NULL DEFAULT ''  COMMENT '项目名称',
  `description`     VARCHAR(500) NOT NULL DEFAULT ''  COMMENT '项目描述',
  `logo`            VARCHAR(200) NOT NULL DEFAULT ''  COMMENT '项目图标',
  `creator`         VARCHAR(40)  NOT NULL DEFAULT ''  COMMENT '创建者 uid（对齐 user.uid）',
  `discoverability` TINYINT      NOT NULL DEFAULT 0   COMMENT '0=space_listed 1=unlisted；只过滤列表/搜索，不是安全边界（D8）',
  `join_mode`       TINYINT      NOT NULL DEFAULT 1   COMMENT '0=开放加入 1=仅邀请；自助加入是 P2，本列在 P0 无消费方',
  `max_members`     INT          NOT NULL DEFAULT 0   COMMENT '成员上限；0=取全局配置',
  `is_official`     TINYINT      NOT NULL DEFAULT 0   COMMENT '官方徽标；P0 无任何写入方（D6）',
  `member_epoch`    BIGINT       NOT NULL DEFAULT 0   COMMENT '成员纪元；每次成员/角色写入在同一事务内 +1，只允许 +1（D2）',
  `status`          TINYINT      NOT NULL DEFAULT 1   COMMENT '1=正常 0=已解散',
  `active_name`     VARCHAR(64)  GENERATED ALWAYS AS (IF(`status` = 1, `name`, NULL)) STORED
                                 COMMENT '生成列：只有活跃项目参与重名判定，解散即腾出名字（D4）。INSERT/UPDATE 禁止点名本列，否则 3105',
  `created_at`      DATETIME(3)  NOT NULL             COMMENT 'UTC，应用侧写入',
  `updated_at`      DATETIME(3)  NOT NULL             COMMENT 'UTC，应用侧写入',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_octo_project_project_id` (`project_id`),
  UNIQUE KEY `uk_octo_project_space_active_name` (`space_id`, `active_name`),
  KEY `idx_octo_project_space_status` (`space_id`, `status`),
  KEY `idx_octo_project_space_creator` (`space_id`, `creator`, `status`),
  KEY `idx_octo_project_creator_created` (`creator`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Space 内项目（协作单元）';

-- (project_id, uid) 主键 + status 翻转，行永不删除：
-- 这正是 joinPresetGroups 那个「退了就再也加不回来」缺陷的反面
-- （modules/space/api.go:1366），重新加入是一次 UPDATE。
CREATE TABLE `octo_project_member` (
  `project_id`  VARCHAR(40) NOT NULL DEFAULT ''  COMMENT '项目ID',
  `uid`         VARCHAR(40) NOT NULL DEFAULT ''  COMMENT '成员 uid（对齐 user.uid / space_member.uid）',
  `space_id`    VARCHAR(40) NOT NULL DEFAULT ''  COMMENT '冗余 Space：级联与对账按 (space_id, uid) 走索引',
  `role`        TINYINT     NOT NULL DEFAULT 0   COMMENT '0=普通 1=管理员 2=拥有者',
  `status`      TINYINT     NOT NULL DEFAULT 1   COMMENT '1=正常 0=已移除',
  `invite_uid`  VARCHAR(40) NOT NULL DEFAULT ''  COMMENT '把他加进来的人；自助加入时等于 uid',
  `created_at`  DATETIME(3) NOT NULL             COMMENT 'UTC，应用侧写入',
  `updated_at`  DATETIME(3) NOT NULL             COMMENT 'UTC，应用侧写入',
  PRIMARY KEY (`project_id`, `uid`),
  KEY `idx_octo_project_member_space_uid` (`space_id`, `uid`, `status`),
  KEY `idx_octo_project_member_uid_status` (`uid`, `status`),
  KEY `idx_octo_project_member_project_status` (`project_id`, `status`, `role`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='项目成员（I1：活跃行 ⊆ 同 Space 活跃成员）';

-- +migrate Down
DROP TABLE IF EXISTS `octo_project_member`;
DROP TABLE IF EXISTS `octo_project`;
```

刻意**不加**外键：仓里没有一张表用 FK，级联语义靠 cleanup 工单 + 对账。

### 3.3 DAO 约定（三个坑一次性关掉）

1. **显式列清单，不用 `util.AttrToUnderscore`**（`octo_project` 上）：
   同时挡住 D4 的 3105（`active_name` 永远不出现在 INSERT/UPDATE 列里）和
   D6 的"`is_official` 不被写入"（列清单里就没有它）。
   `octo_project_member` 无生成列，可以用 `AttrToUnderscore`，但为一致性也用显式清单。
2. **dbr 反引号**：`Update` / `InsertInto` / `DeleteFrom` 传裸名（dbr 自己 quote）；
   `From` / `Select` 手动加反引号。`octo_*` 不是保留字，但 reconcile 会 JOIN
   `` `space` ``（保留字），那里必须手写反引号。
3. **零值时间**：读侧用 `db.Time` / `time.Time`（DSN 有 `parseTime=true`），
   写侧一律显式 `time.Now().UTC()`。

### 3.4 `member_epoch` 纪律（D2 的落地形式）

```
BEGIN
  SELECT project_id, space_id, status, member_epoch
    FROM `octo_project` WHERE project_id = ? AND status = 1 FOR UPDATE   -- ① 先锁项目行
  <成员写入：INSERT ... ON DUPLICATE KEY UPDATE / UPDATE octo_project_member ...>  -- ②
  if affectedRows > 0 {
    UPDATE octo_project SET member_epoch = member_epoch + 1, updated_at = ?
      WHERE project_id = ?                                              -- ③ 同事务，只允许 +1
  }
COMMIT
```

- **顺序**符合 brief 要求的锁序（space → project → group → group_member → `octo_project_member`）：
  项目行先锁、成员行后写。锁序写进 `service.go` 头部注释。
- **`affectedRows > 0` 才 bump** 是三条 acceptance 的共同实现：
  no-op 不改 epoch；级联步骤重跑不重复 bump；"再加一个已存在的活跃成员"不改 epoch。
  注意 `INSERT ... ON DUPLICATE KEY UPDATE` 的 affectedRows 语义（新增=1、
  实际改动=2、无变化=0），要按 0 判定 no-op，不能按 `!= 1`。
- **只允许 `+1`** → 单调性由构造保证（见 1.10），配一条源码守卫。
- 回滚测试：在 ② 之后注入错误使事务回滚，断言成员行与 epoch **都**没变。

### 3.5 两个中间件

`modules/project/middleware.go`。**新增文件，不改 `pkg/space/middleware.go`**
（用它导出的 `SetSpaceID` / `CheckMembership` / `NewRedisMembershipCache`）。

**`spaceIDParamMiddleware(ctx)`** — 挂在 `/v1/space/:space_id/projects`：
1. `spaceID := c.Param("space_id")`；空 → shared `param.invalid` + `c.Abort()`
2. `uid := c.GetLoginUID()`；空 → shared `auth.required` + Abort
   （brief 明确要求：无 `X-Space-ID`、无 `space_id` query 时也必须 401/403，
   不能继承 SpaceMiddleware 的 pass-through——路径参数就是我们的租户锚点）
3. Space 成员校验 → 通过则 `spacepkg.SetSpaceID(c, spaceID)`；否则 shared `auth.forbidden`
4. 全部出口走 `httperr.ResponseErrorL` + `c.Abort()`，**不得**照抄
   SpaceMiddleware 的 `AbortWithStatusJSON`

**`projectMiddleware(ctx)`** — 挂在 `/v1/projects/:project_id`：
1. 按 `uk_octo_project_project_id` 点读项目行（`project_id, space_id, status, discoverability`）
2. 不存在 / `status=0` → `err.server.project.not_found`
3. `spacepkg.SetSpaceID(c, row.SpaceID)`；Space 成员校验失败 → **同一个
   `not_found`**（不是 403），否则跨 Space 探测能区分"项目不存在"与"存在但你不在那个 Space"
4. 解析项目角色（成员=0/1/2，非成员=-1）与 Space 角色（`pkg/space.MemberRole`）
5. `unlisted` 且非项目成员且非 Space admin → 同一个 `not_found`
6. 角色写入 context，供各 handler 做权限判定

**缓存分两层，这一点 brief 的 acceptance 容易被读反：**

| 缓存 | key | 归属 |
|---|---|---|
| Space 成员身份 | `space:member:{spaceID}:{uid}`（**复用 `spacepkg.NewRedisMembershipCache`**） | space 模块。**必须复用**，不能自建 `project:spacemember:*`——Space 移除时是 space 在请求内同步删这个 key，我们自建一份就没人失效它，被移除的人还能带 Space 身份进项目接口最长 60s |
| 项目成员身份 | `project:member:{projectID}:{uid}`，60s 正 / 30s 负 | project 模块自有，每次成员写入**请求内同步**失效 |

所以 acceptance 那条"Project 缓存键命名空间 `project:*`，不与既有前缀冲突"只约束
**第二层**；第一层刻意复用 `space:*`，这是正确性要求而非命名疏漏，要写进代码注释与 PR。

DEL 失败的处理照 space 的两分支纪律（`ErrMembershipCacheNegativeFallback`）：
DEL 失败就写一条更短 TTL 的否定条目盖住它，日志按 Warn；
DEL 与兜底双双失败才按"隔离可能失效"报 Error。`spacepkg` 那个 helper 绑死了
它自己的 key 格式（`invalidateMembershipCacheIn` 未导出），所以在 project 里写一份等价实现。

### 3.6 路由与中间件链

```go
// 认证 + UID 限流必须这个顺序：SharedUIDRateLimiter 放在 AuthMiddleware 之前
// 读不到 uid，会静默 fail-open。
spaceScoped := r.Group("/v1/space",
    p.ctx.AuthMiddleware(r),
    appwkhttp.SharedUIDRateLimiter(r, p.ctx),
    p.spaceIDParamMiddleware(),
)
{
    spaceScoped.POST("/:space_id/projects", p.createProject)   // flag 门 + 配额
    spaceScoped.GET ("/:space_id/projects", p.listProjects)
}

projectScoped := r.Group("/v1/projects",
    p.ctx.AuthMiddleware(r),
    appwkhttp.SharedUIDRateLimiter(r, p.ctx),
    p.projectMiddleware(),
)
{
    projectScoped.GET   ("/:project_id", p.getProject)
    projectScoped.PUT   ("/:project_id", p.updateProject)      // flag 门
    projectScoped.DELETE("/:project_id", p.disbandProject)     // flag 门

    projectScoped.GET   ("/:project_id/members",           p.listMembers)      // PR-B
    projectScoped.POST  ("/:project_id/members/add",       p.addMembers)       // PR-B, flag 门
    projectScoped.POST  ("/:project_id/members/remove",    p.removeMembers)    // PR-B, flag 门
    projectScoped.POST  ("/:project_id/leave",             p.leaveProject)     // PR-B, flag 门
    projectScoped.PUT   ("/:project_id/members/:uid/role", p.updateMemberRole) // PR-B, flag 门
}
```

链序断言不能靠"压测出 429"（挂错也照样过）。`wkhttp.WKHttp` 没有导出 `Routes()`
（`r *gin.Engine` 私有），所以照仓内做法用**源码守卫**：扫 `api.go`，断言每个
`r.Group("/v1/...")` 的前两个中间件恰好是 `AuthMiddleware` 后紧跟
`SharedUIDRateLimiter`，且模块内不存在别的 `r.Group("/v1/project` / `r.Group("/v1/space`。
先例：`modules/messages_search/route_chain_test.go` 用结构性断言守住"没有端点绕过链"。

### 3.7 权限矩阵

角色：`0=普通 1=管理员 2=拥有者`。

| 操作 | 允许 | 传递性保护 |
|---|---|---|
| create | 任意 Space 成员（flag + 配额） | 创建者成为 owner（role=2） |
| get 详情 | 项目成员；`space_listed` 项目对任意 Space 成员可见元数据；Space admin 可见 | `unlisted` 且非成员非 admin → 与"不存在"同一响应 |
| update | owner + admin | — |
| disband | **owner only** | — |
| members list | 项目成员 + Space admin | 非成员（即使 `space_listed`）看不到名单 |
| members/add | owner + admin | 目标必须是同 Space 活跃成员（I1，事务内校验） |
| members/remove | owner + admin | **admin 不能移除 admin/owner**；不能通过它移除自己（走 leave） |
| leave | 任意成员 | 最后一个 owner 必须先交接：请求带 `transfer_to`，交接 + 退出在**同一事务** |
| role change | **owner only** | 见下 |

> **待确认（brief 未明说）**：role change 我按 **owner only** 实现。
> brief 只写了"admin 不能移除或降级 admin/owner"，没说 admin 能否**提升**普通成员。
> 若允许 admin 提升，admin 就能造一个同级 admin 从而绕过"不能降级 admin"这条
> ——那是一条提权路径。owner only 是保守解；如果产品要 admin 能加 admin，
> 需要额外规则（例如 admin 只能提升到 role<自身）。

`pkg/space/member_role.go`（新文件）提供 Space admin 判定：
```go
// MemberRole 返回 uid 在 spaceID 的角色，ok=false 表示不是活跃成员或 Space 非活跃。
// 语义与 CheckMembership 对齐（space.status=1 + space_member.status=1）——
// 鉴权判定绝不能用 CheckMembershipForCleanup 的放宽版本。
func MemberRole(session *dbr.Session, spaceID, uid string) (role int, ok bool, err error)
```

### 3.8 配额（全部可配，不写字面量）

`modules/project/config.go`：

| 项 | 默认 | 来源 |
|---|---|---|
| `project_create_enabled` | **false（fail-closed）** | 见 5.1 待拍板 |
| 每 Space 项目数 | 1000 | 配置 |
| 每 Space 每创建者项目数 | 100 | 配置 |
| 每项目成员数 | 500 | 配置（`octo_project.max_members>0` 时按行覆盖） |
| 每人每天创建数 | 20 | 配置 |
| 日边界时区 | `Asia/Shanghai` | 配置 |
| reconcile 间隔 / 单轮 LIMIT | 5min（带抖动）/ 500 | 配置 |

日建上限查询**改进** group 的写法（brief 说"抄的是 pattern，不是包"）：
```sql
SELECT COUNT(*) FROM `octo_project`
 WHERE creator = ? AND created_at >= ? AND created_at < ?   -- 半开区间，走 idx_octo_project_creator_created
```
不用 `DATE(created_at)=?`：那是列上的函数调用，索引失效，且依赖 Go 与 MySQL 会话时区同源。

配额检查必须在**写事务内**做（`FOR UPDATE` 锁项目行 / 计数后立刻插入），
否则并发创建能越过上限。每条配额一个独立错误码。

### 3.9 级联步骤（PR-A）

`modules/project/space_member_removal.go`，步骤名 `project_member`，在模块构造时
`spacemod.RegisterMemberRemovalCleanupStep` 反向注册（照 group 的
`registerSpaceMemberRemovalCleanup`）。

```
cleanupSpaceMemberProjects(ctx, removal):
  1. stillMember, err := spacepkg.CheckMembershipForCleanup(ctx.DB(), removal.SpaceID, removal.UID)
     err  → return wrapped err（可重试）
     true → return nil          // 封禁 Space 席位仍在 / 人已重新加入 → 跳过
  2. 分页查 (space_id, uid, status=1) 的 project_id（LIMIT + 游标，走 idx_octo_project_member_space_uid）
  3. 逐项目一个短事务：锁项目行 → UPDATE 成员行 status=0 → affected>0 才 epoch+1
  4. 每成功一个项目：同步删 project:member:{projectID}:{uid}
  5. 单项目失败不中断其余；返回首个错误让整条工单重试
  6. 没有活跃行 → return nil（"无事可做自己判断"）
```

关键性质（对应 C2）：
- **幂等**：`status=1` 过滤让重跑天然空转，`affected=0` 不 bump epoch
  → "跑两次同一最终状态、无错误、**无第二次 epoch bump**"
- **不返错表示无事可做**：只有真 DB 错误才返错
- **快**：分页 + 短事务，不长时间占租约（工单与 group/conversation 步骤共享租约周期）
- **不假设别的步骤成功**：只看 `octo_project_member` 当前状态
- **语义必须是 `CheckMembershipForCleanup`**：用 `CheckMembership` 会在 Space 被
  封禁的瞬间把该 Space 全部项目成员清掉，解封也不恢复

### 3.10 对账 job（PR-C）

`modules/project/reconcile.go`，`ctx.Schedule(interval, ...)` 包在 `sync.Once` 里
——**这一条不是可选的**：`space` 模块吃过亏（`removalCleanupWorkerOnce` 的注释：
`modules/user` 一个包建 196 个 testServer，堆起近 400 个永不停止的定时器）。
间隔加抖动（D7）。只读，可在每个 pod 跑。

**扫描 1 — I1 违约**（有界；谓词与 `CheckMembership` 严格互为否定，两层不许漂移）：
```sql
SELECT pm.project_id, pm.uid, pm.space_id
  FROM `octo_project_member` pm
 WHERE pm.status = 1
   AND (pm.project_id, pm.uid) > (?, ?)                     -- 主键游标
   AND NOT EXISTS (SELECT 1 FROM space_member_removal_cleanup c
                    WHERE c.space_id = pm.space_id AND c.uid = pm.uid
                      AND c.status = 0)                     -- 在途清理工单 → 豁免
   AND NOT EXISTS (SELECT 1 FROM `space` s
                    WHERE s.space_id = pm.space_id AND s.status = 2)  -- 封禁 Space 不算违约
   AND NOT EXISTS (SELECT 1 FROM space_member sm
                    INNER JOIN `space` s2 ON s2.space_id = sm.space_id AND s2.status = 1
                    WHERE sm.space_id = pm.space_id AND sm.uid = pm.uid
                      AND sm.status = 1)                    -- 即 CheckMembership 取非
 ORDER BY pm.project_id, pm.uid
 LIMIT ?;
```
用 `NOT EXISTS` 而不是 `LEFT JOIN`：`(space_id, uid)` 在 cleanup 表上**不唯一**
（remove → rejoin → remove 会有多行，那张表的注释明确说了这是有意的），
`LEFT JOIN` 会放大行数。
走的索引：`space_member` 的 `(space_id, uid)` 唯一索引、`space` 的 `space_id` 唯一索引、
`space_member_removal_cleanup` 的 `idx_..._target(space_id, uid)`。
`space_member_removal_cleanup.uid` 是 `VARCHAR(64)` 而项目侧是 `VARCHAR(40)`——
宽度不同不影响 JOIN，charset/collation 一致（那张表显式写了 `general_ci`）才是关键。

**扫描 2 — abandoned 泄漏（独立告警，语义与扫描 1 相反）**：
```sql
SELECT COUNT(*) FROM space_member_removal_cleanup c
 WHERE c.status = 2
   AND EXISTS (SELECT 1 FROM `octo_project_member` pm
                WHERE pm.space_id = c.space_id AND pm.uid = c.uid AND pm.status = 1);
```
在途窗口是**正常**的、被豁免；abandoned 是**真泄漏**、无自动重驱动、要人介入。
两者混一个告警，等于在功能还没有第一个用户时就把告警训练成噪音。

**扫描 3 — 孤儿项目（space_id 已不存在）**：
```sql
SELECT project_id, space_id FROM `octo_project` p
 WHERE p.status = 1 AND p.id > ?
   AND NOT EXISTS (SELECT 1 FROM `space` s WHERE s.space_id = p.space_id)
 ORDER BY p.id LIMIT ?;
```

**扫描 4 — epoch 哨兵**：`COUNT(*) WHERE member_epoch < 0` + 进程内有界 LRU
（best-effort，见 1.10）。

**分布指标**放更稀疏的节奏（照 `removalMetricsInterval` 的理由：全表聚合在它最
有价值的时刻——积压之后——恰好最慢）。

### 3.11 指标（`promauto`，namespace `project`，无高基数标签）

| 指标 | 类型 | 标签 |
|---|---|---|
| `project_admission_rejected_total` | Counter | `entry`{`member_add`,`role_change`,`create_owner_seat`}（P2 加 `invite_accept`,`join_open`,`join_code`）× `reason`{`not_space_member`,`quota_members`,`project_disbanded`,`flag_off`,`permission_denied`} |
| `project_total` / `project_member_total` | Gauge | — |
| `project_member_count_bucket` | Histogram | — |
| `project_i1_violations` | Gauge | — |
| `project_i1_abandoned_cleanup_leak` | Gauge | — |
| `project_orphan_total` | Gauge | — |
| `project_reconcile_duration_seconds` | Histogram | `scan` |

绝不放 `space_id` / `project_id` / `uid`（会撑爆 Prometheus）。
`/metrics` 端点已在服务中（`pkg/metrics/http.go`，main.go 启动），落地即被抓取。
**`entry` 标签从第一天就要有**——它是 P1 发现"漏了一条写路径"的唯一信号，
单个不分维度的计数器不满足 acceptance。

### 3.12 错误码（`pkg/errcode/project.go`）

`err.server.project.*`，D5 用 `ResponseErrorL`（线上固定 400，真实 status 在
`error.http_status`）。5xx ⟺ `Internal=true`，4xx 绝不 Internal。

```
request_invalid              400  SafeDetailKeys: field
name_invalid                 400  field, max_chars
name_duplicated              409
not_found                    404   ← 不存在 / 跨 Space / unlisted 非成员，全部同一码同一 body（防枚举，不带 details）
permission_denied            403
not_member                   403
disabled                     403   ← project_create_enabled 关闭时的写路径统一码
quota_projects_per_space     403  max
quota_projects_per_creator   403  max
quota_members                403  max
quota_daily_create           403  max
member_not_space_member      403   ← I1 拒绝
member_not_found             404
role_invalid                 400  field
last_owner_must_transfer     409
batch_too_large              400  max
query_failed                 500  Internal=true
store_failed                 500  Internal=true
```
（P2 再加一个 `invite_invalid`，把 invalid / expired / revoked / exhausted 合并成一个码。）

shared 码经 `mustLookupSharedCode` 在 init 解析：`err.shared.auth.required`、
`err.shared.auth.forbidden`、`err.shared.param.invalid`。
⚠️ **这是 C1 的引爆点**：`mustLookupSharedCode` 设计上就是 init 期 panic，
一个没注册的 shared 码 = 整个 IM 服务起不来。所以 PR-A 必须有启动冒烟测试。

流程：写 `pkg/errcode/project.go` → `make i18n-extract` → 补
`pkg/i18n/locales/active.zh-CN.toml` → `make i18n-extract-check` + `make i18n-lint`。
`tools/lint-direct-error-response/baseline.txt` **不新增条目**。

### 3.13 审计（结构化日志）

`modules/project/audit.go`，字段：`action`（`create`/`disband`/`member_add`/
`member_remove`/`role_change`）、`actor`、`target`、`project_id`、`space_id`、
`reason`、`space_role`/`project_role`。低基数枚举，不含用户内容。

---

## 4. 测试计划（映射 acceptance）

### 4.1 `package project`（同包，TestMain 自建 fixture 表）

TestMain：`OCTO_MASTER_KEY` 兜底 → 建 `group`/`group_member`/`robot`/`user`/
`user_verification` fixture（抄 `modules/space/api_test.go`）→ `NewTestServer` →
**`SetErrorRenderer`**（不注入就断言不到 `error.code`）→ 清 `ratelimit:uid:*`。

| 用例 | acceptance 覆盖 |
|---|---|
| up/down/up 无残留 | Schema |
| JOIN `space_member` / `user` 无 1267 | Schema |
| HTTP create → update 走真 DAO（含生成列） | Schema（3105） |
| create → disband → 同名重建成功；重复活跃同名报 `name_duplicated` | Schema |
| 加非 Space 成员被拒；Space A 成员加不进 Space B 项目 | I1 |
| SQL 造 I1 违约 → 被 flag；有 pending 工单的 (space,uid) **不**被 flag；封禁 Space 成员**不**被 flag | I1 / reconcile |
| 封禁 Space（status=2）不清任何成员行，解封无需修复 | I1 |
| 跑两次清理工单：同一最终状态、无错误、**无第二次 epoch bump** | I1 / epoch |
| abandoned + 项目仍有活跃行 → 独立告警计数 | I1 |
| epoch 在 add/remove/leave/role/级联/disband 各自严格递增 | epoch |
| 重复加已有活跃成员、设置同角色 → epoch 不变 | epoch |
| 事务回滚后成员行与 epoch 都不变 | epoch |
| 列表/详情响应含 `member_epoch` + `my_role` + capability bits | epoch / D3 |
| 无 `X-Space-ID` 无 `space_id` query 时全部路由 401/403 | 授权 |
| `unlisted` 项目对非成员与不存在项目**同响应**；成员与 Space admin 拿真 payload | 防枚举 |
| admin 不能移除/降级 admin 或 owner；最后 owner 不交接不能退出/降级；交接原子 | 授权 |
| 被移除成员**下一个请求**即被拒（证明同步失效，不是等 TTL） | 授权 |
| 四条配额各自在边界报各自的码；阈值来自配置而非字面量 | 配额 |
| create/disband/add/remove/role 各写一条审计 | 审计 |
| `is_official` 走完整 CRUD 后仍为 0 + 源码级检查 | D6 |
| flag 关闭：写路径 403，list/detail 仍 200 | D1 |
| 每条 reconcile 查询都有 LIMIT + 游标（源码守卫 / 计划检查） | C3 |
| `TestProjectNoLegacyResponseError` 覆盖 **api*.go + middleware.go + service.go** | 规范 |
| 路由链源码守卫：`SharedUIDRateLimiter` 紧跟 `AuthMiddleware` | 规范 |
| `member_epoch` 只允许 `+1` 的源码守卫 | 1.10 |
| 缓存 key 前缀守卫：`project:*`，且 Space 成员缓存复用 `space:member:*` | C3 |

### 4.2 `package project_test`（外部包，`_ "octo-server/internal"`）

| 用例 | acceptance 覆盖 |
|---|---|
| 全量模块下 `NewTestServer` 不 panic，项目路由可达（端到端打一发） | **C1** |
| 注册一个**故意持续返错**的 `project_member` 步骤，Space 移除成员后**断言 group 步骤的结果**（人确实退群了），不看日志 | **C2** |

### 4.3 门禁

```
go vet ./...
golangci-lint run ./modules/project/... ./pkg/errcode/... ./pkg/space/...
make i18n-extract-check && make i18n-lint
go test ./modules/project/...
go test ./modules/space/... ./modules/group/... ./modules/thread/... ./modules/message/...   # 不改任何既有测试文件
git diff --stat origin/main...   # 人工核对触及文件清单
```
`go build` 不算门禁——它不编译 `_test.go`。
既有测试若需要改，那就是行为变了，必须在 PR 里解释而不是顺手改掉。

---

## 5. 待拍板（会影响实现，我不替你决定）

### 5.1 `project_create_enabled` 放哪里

| 方案 | 优点 | 代价 |
|---|---|---|
| **A. env**（`OCTO_PROJECT_CREATE_ENABLED`，默认 false） | P0 diff 严格只碰新文件；rollback 最干净 | 开关要改 configmap + 滚动重启（生产 env 注入走部署侧 configmap（envFrom），需重启）；**出事时关闭也要重启** |
| **B. `system_setting`**（推荐） | 管理台可切、60s 内多副本收敛、双向即时 | 要动两个既有文件：`modules/common/system_setting_schema.go`（schema 白名单，不加就写不进去）+ `modules/common/system_settings.go`（getter）。与 acceptance 的"只碰新文件"冲突，需要在 brief 里显式豁免这两个文件 |
| C. `modules/featuregate` | 秒级 | **不在 `main` 上**（1.8），会把 P0 绑到未合并分支。不建议 |

我的倾向是 **B**：这是一个上线期要来回切的门，两次 additive 编辑换来"不重启即可关闭"
是值得的；对消息链路零风险。但它需要你同意修改 acceptance 那一条。

### 5.2 role change 是否 owner only（3.7 的待确认）

### 5.3 `pkg/authtree` 那段注释是否加（1.9）
加 = 动一个既有文件（仅注释）；不加 = acceptance 那条无法满足，需改 brief 措辞。

### 5.4 `join_mode` 列是否保留

邀请降 P2 后 `join_mode` 在 P0 完全无消费方。
保留（与 `is_official` 同处理）= P2 不用改表；删掉 = P2 加一列。
我倾向**保留**，因为它是 `octo_project` 上的列，D6 的论证对它成立。

### 5.5 产品确认：P0 没有自助加入路径（0.3）

### 5.6 上线前必须做的一次生产核查

- 生产 `space` / `space_member` / `user` 三张表的实际 collation（1.3 的 1267 风险）
- 结论写进 PR；若不是 `utf8mb4_general_ci`，新表的 COLLATE 要跟着改

---

## 6. 回滚

- **软**：`project_create_enabled` 关 → 新建停止、成员写冻结，读仍可用（既有数据可观察）
- **硬**：`DROP TABLE octo_project_member, octo_project` + 去掉 `internal/modules.go`
  的 blank import。没有任何既有表加列、没有任何既有行被改动——这是 P0 这样划范围的全部理由。
- 注意：级联步骤按名字注册（`project_member`），模块不加载就不注册，
  space 的清理工单不受影响。
