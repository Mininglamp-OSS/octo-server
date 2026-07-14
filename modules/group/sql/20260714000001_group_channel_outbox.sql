-- +migrate Up
-- 群 IM 频道创建「事务发件箱」(transactional outbox) —— 收口 CreateGroup 的孤儿群窗口 (#394)。
--
-- 背景：CreateGroup 在 tx.Commit() 后才调 IMCreateOrUpdateChannel；IM 失败时做 best-effort
-- 补偿删除，但补偿删除自身失败(或 commit 与 IM 之间进程崩溃)会留下「有 group 行、无 IM 频道」
-- 的孤儿群 —— 客户端永远无法进入。IM 频道在 WuKongIM 而非本库，无法入 tx，点修无法根除。
--
-- 方案：建群同一事务内往本表写一行 outbox（group_no 唯一），随 group/group_member 一起原子提交；
-- IM 频道确认创建成功后删除该行。于是「本表存在某 group_no 的行」== 「该群的 IM 频道尚未确认」。
-- 后台 reconcile 定时任务扫描超过宽限期(grace)仍存在的行，用当前成员幂等重建频道并删行；
-- group 行已不存在的陈旧 outbox 行直接清理。存量群不写本表 → 永不被触碰。
--
-- 恢复安全：整段用 INFORMATION_SCHEMA 守卫保证部分执行后重试可重入。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_outbox;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_channel_outbox()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.TABLES
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group_channel_outbox') THEN
    CREATE TABLE `group_channel_outbox` (
      `id`           BIGINT       NOT NULL AUTO_INCREMENT,
      `group_no`     VARCHAR(40)  NOT NULL COMMENT '待确认 IM 频道的群编号',
      `channel_type` SMALLINT     NOT NULL DEFAULT 2 COMMENT 'WuKongIM 频道类型(群=2)',
      `attempts`     INT          NOT NULL DEFAULT 0 COMMENT 'reconcile 重试次数(观测/退避用)',
      `last_error`   VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次重建失败原因(截断)',
      `created_at`   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '写入时间(建群时刻)',
      `updated_at`   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最近一次 reconcile 尝试时间(due 判定与退避基准)',
      PRIMARY KEY (`id`),
      UNIQUE KEY `uk_group_no` (`group_no`),
      KEY `idx_updated_at` (`updated_at`)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='群 IM 频道创建事务发件箱(#394 孤儿群 reconcile)';
  END IF;
END;
-- +migrate StatementEnd

CALL __group_channel_outbox();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_outbox;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_outbox_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_channel_outbox_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.TABLES
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group_channel_outbox') THEN
    DROP TABLE `group_channel_outbox`;
  END IF;
END;
-- +migrate StatementEnd

CALL __group_channel_outbox_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_outbox_down;
-- +migrate StatementEnd
