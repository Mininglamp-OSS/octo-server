-- +migrate Up

-- 项目子系统容器编排工单（transactional outbox + 映射表，同一张表）。
--
-- 一个 Project 对应 octo-fleet 的一个 workspace 与 octo-drive 的一个 shared space
-- （D1）。行在**创建项目的同一事务内**写出，所以「项目存在 ⟹ 工单存在」恒真；
-- worker 用 FOR UPDATE SKIP LOCKED + 租约认领，指数退避重试，预算耗尽置为
-- abandoned 并告警。
--
-- 为什么不用 modules/base/event：那是一次性投递器，listener 报错会把事件置 Fail，
-- 而 QueryAllWait 只选 Wait，Fail 行永不重投，承不起 at-least-once。
-- 为什么不用 internal/cardactiondispatch 的 Redis 队列：那不是写项目行的同一个存储，
-- 入队与提交之间会出现丢失或孤儿。形状抄的是
-- modules/space/sql/20260821000001_space_member_removal_cleanup.sql，并带上 P1 记下的
-- 修正：时间列全部由应用侧写 UTC（不设 MySQL 侧默认值）、终态清理有自己的
-- (target, status, finished_at) 索引。
--
-- ⚠️ **没有租约心跳，这是有意的。** 本文件的早期版本（和 brief D2）声称有心跳，而代码里
-- 从来没有 —— lease_until 只在认领时写入、在释放/终态时置 NULL，中间没有任何东西续约。
-- 心跳是给长作业准备的机械结构；本作业的全部成本是一次有界的出网调用（默认 10s），
-- 为它加一个续约循环是拿复杂度换一个不存在的问题。
-- 取而代之的是把那条关系变成**可执行的校验**：OCTO_PROJECT_PROVISION_TIMEOUT 在配置
-- 加载期被限制为不超过租约的四分之一（见 config_provisioning.go 的
-- maxProvisionTimeoutFraction）。既没有心跳又没有这道校验时，一个被配成 10m 的超时会让
-- 租约在调用进行中过期 —— 另一个副本会合法地重新认领并并发跑同一行，而 sweep 还会在
-- 执行者仍在运行时把它写成 abandoned。
--
-- container_id 是**随机不可推导**的（D1），由 octo-server 在入队时生成，随后作为
-- 「要创建的容器 id」发给目标子系统。它不是 project_id 的函数，理由是可推导 id 在
-- 预置（eager）语义下可被利用：listVisibleInSpace 让同 Space 的任何活跃成员都能读到
-- 每个 space_listed 项目的 project_id，而 fleet 的 workspace 门只校验 octo Space 成员
-- 身份并在通过后把调用者 UpsertMember 物化成 workspace 成员——于是「列项目 → 推导
-- 容器 id → 自助入伙」成为一条完整链路（brief P-3）。在 R2/R3 落地前，id 的不可推导
-- 性是这条链路上唯一的阻断，所以它既不进任何客户端响应，也不进日志与错误 details。
--
-- ⚠️ status = ready 记录的是「我们成功调用过一次」，不是「容器现在存在」。容器可能被
-- 子系统侧删除 / 归档 / 迁移而不通知本服务，所以**任何读路径都不得以本表为门**
-- （D12）：Tab 可见性走全局的 per-subsystem 能力开关，容器存在性由目标的 ensure 保证。
-- 一个源码守卫钉住这一点。
--
-- 保留终态行（不在 ready 后删除）的收益是**本地枚举**：「哪些项目曾被预置进 drive」
-- 不必问 drive 就能回答，回收对账需要这个能力。
--
-- 拆解（disband）不推送删除，走拉取（D9）：行转为 disband_pending 作为对账依据，
-- 子系统自己来问 POST /v1/internal/projects/status。
CREATE TABLE `octo_project_provisioning` (
  `id`              BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `project_id`      VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '项目ID（宽度/字符集对齐 octo_project.project_id）',
  `space_id`        VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '冗余 Space；发给目标的 octo_space_id，免得每行回表',
  `target`          VARCHAR(16)      NOT NULL DEFAULT ''  COMMENT '目标子系统：fleet / drive',
  `container_id`    VARCHAR(64)      NOT NULL DEFAULT ''  COMMENT '随机不可推导的容器ID；入队时生成，绝不进客户端响应/日志',
  `status`          TINYINT UNSIGNED NOT NULL DEFAULT 0   COMMENT '0=pending 1=ready 2=abandoned 3=disband_pending',
  `attempts`        INT UNSIGNED     NOT NULL DEFAULT 0   COMMENT '认领即自增；进程被硬杀时也会收敛到 abandoned',
  `next_attempt_at` DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入，不设 MySQL 侧默认值（读写必须同一个时钟）',
  `lease_owner`     VARCHAR(64)      NOT NULL DEFAULT ''  COMMENT '每次认领唯一，不是进程级常量',
  `lease_until`     DATETIME(3)      NULL                 COMMENT 'UTC；租约到期后可被其它副本接管',
  `last_error`      VARCHAR(255)     NOT NULL DEFAULT ''  COMMENT '低基数失败摘要，不含用户内容，也不含 container_id',
  `created_at`      DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入，不设 MySQL 侧默认值',
  `finished_at`     DATETIME(3)      NULL                 COMMENT 'UTC；终态时间',
  PRIMARY KEY (`id`),
  -- 一个项目在一个目标上只有一行。既是「已提交的创建恰好留下每目标一行」的结构保证，
  -- 也让重放不会造出第二个容器。
  UNIQUE KEY `uk_octo_project_provisioning_target` (`project_id`, `target`),
  -- container_id 全局唯一。它是接收方的幂等键（drive 的 drive_space 主键 / fleet 的
  -- slug 唯一索引），本侧重复生成属于生成器缺陷，宁可让 INSERT 失败也不要静默复用。
  UNIQUE KEY `uk_octo_project_provisioning_container` (`container_id`),
  -- 两个扫描索引都以 target 开头，这一列是必需的而不是顺手加的。
  --
  -- 认领、清扫、清理三条语句全都带 `target IN (...)`（认领的 target 过滤是「关掉一个目标
  -- 不破坏已入队的行」的实现方式）。若 target 不在索引里，每个目标的扫描会在索引区间内
  -- 先锁住并检出另一个目标的行、再在 server 层按 target 过滤掉——而 FOR UPDATE SKIP
  -- LOCKED 会让兄弟扫描把这些已被锁住的行整个跳过。实测（本地 MySQL 8.0，两目标同 tick）：
  -- 被抢先的那个目标本 tick 一行都认领不到，静默等到下一个 tick。这正是「每目标一个
  -- goroutine」要消除的互相拖累，所以索引必须让每个目标的扫描只落在自己的行上。
  KEY `idx_octo_project_provisioning_pending` (`target`, `status`, `next_attempt_at`, `lease_until`),
  -- 终态清理按 (target, status, finished_at) 扫。pending 索引前两列虽然相同，但第三列是
  -- next_attempt_at，帮不上 finished_at 的范围条件。首列 target 同时让按 (target, status)
  -- 的行数普查（provisioning_rows 计量）走索引而不是全表。
  KEY `idx_octo_project_provisioning_finished` (`target`, `status`, `finished_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='项目子系统容器预置工单（兼映射表）';

-- +migrate Down
DROP TABLE IF EXISTS `octo_project_provisioning`;
