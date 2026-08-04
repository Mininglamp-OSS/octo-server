-- +migrate Up

-- system_setting_guard_lock: 跨键系统配置守卫的**串行化锚点**。
--
-- 背景（task per-user-hiding-window / P0-A，结转自 PR #679 review）：
-- 两阶段衰减偏序守卫（thread.auto_archive_days >= sidebar.recent_filter_thread_days）
-- 原先对着进程本地快照校验、且在事务之外，于是两个并发的管理写入各自看到同一份旧快照、
-- 双双通过校验、双双提交，落到守卫本要禁止的状态。
--
-- 为什么需要一张单独的表，而不是直接 `SELECT ... FOR UPDATE` 那三个 system_setting 行：
-- 那三行**可能都不存在**（迁移刻意不写行，取值回落 env / 代码默认，这是 Batch 1 的
-- 上线安全保证）。对不存在的行做 FOR UPDATE 只拿到 gap lock，而 gap-X 锁彼此兼容 ——
-- 并发事务会全部通过校验，随后各自 INSERT 抢 insert-intention lock 互等，退化成
-- InnoDB 死锁(1213)，把一次合法的并发写变成不透明的 500。本仓库在
-- incomingwebhook.insertWithQuota 上已经踩过并记录过同一个坑，结论是「锁一个必然存在的
-- 单行」；card_template_catalog_capacity_guard 用的也是这个形状。
--
-- 单行、无业务字段：它只承担串行化，不存任何状态。CHECK 把主键钉死在 1，杜绝出现第二行
-- 导致两批写入锁到不同锚点。未来其它跨键守卫（如 onboarding.space_welcome_* 复合校验，
-- 它有同一个洞）可以复用本表，届时把 guard_key 换成具名主键即可。
CREATE TABLE `system_setting_guard_lock` (
    `guard_key`  TINYINT UNSIGNED NOT NULL,
    `created_at` TIMESTAMP        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`guard_key`),
    CONSTRAINT `chk_system_setting_guard_lock` CHECK (`guard_key` = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='跨键系统配置守卫的串行化锚点行';

INSERT INTO `system_setting_guard_lock` (`guard_key`) VALUES (1);

-- +migrate Down

DROP TABLE `system_setting_guard_lock`;
