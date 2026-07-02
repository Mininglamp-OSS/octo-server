-- +migrate Up
-- #394：CreateGroup 在事务提交后才创建 WuKongIM 频道，若 IM 创建失败会跑一段
-- best-effort 补偿删除；补偿删除本身非原子、只记日志，一旦它也失败（或进程在
-- commit 与 IM 创建之间崩溃），group 行就残留为「无 backing IM 频道的孤儿群」，
-- 客户端永远无法收发消息。channel_synced 是持久化的 outbox 标记：
--   channel_synced=1 → 频道已确认创建（稳定态）；
--   channel_synced=0 → 频道尚未确认，等待 reconcile worker 兜底补建。
-- 只有 CreateGroup 路径会在事务内把新群写成 0，并在 IM 创建确认成功后翻成 1；
-- 其余所有插入路径都不触碰该列，落库即 DEFAULT 1（视为已同步），零行为变更。
--
-- 存量行：ADD COLUMN ... NOT NULL DEFAULT 1 会把所有既有 group 行一次性回填为 1
-- （它们本就有频道），因此 reconcile worker 不会把存量群误判成孤儿。无需 NULL 哨兵
-- 三步法——期望的回填值恰是列默认值。整块用存在性守卫包裹，跨迁移重试幂等收敛。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_synced;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_channel_synced()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'channel_synced') THEN
    ALTER TABLE `group`
      ADD COLUMN `channel_synced` TINYINT NOT NULL DEFAULT 1
        COMMENT 'IM频道是否已确认创建 1.已同步(默认/稳定态) 0.待reconcile补建(仅CreateGroup失败窗口)';
  END IF;

  -- reconcile worker 的扫描谓词是 (channel_synced=0 AND created_at < cutoff)，
  -- 复合索引让「找孤儿」永远走 index range 而非全表扫（孤儿是极少数，绝大多数行=1）。
  IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND INDEX_NAME = 'group_channel_synced_created') THEN
    CREATE INDEX `group_channel_synced_created` ON `group` (`channel_synced`, `created_at`);
  END IF;
END;
-- +migrate StatementEnd

CALL __group_channel_synced();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_synced;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_synced_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_channel_synced_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND INDEX_NAME = 'group_channel_synced_created') THEN
    DROP INDEX `group_channel_synced_created` ON `group`;
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'channel_synced') THEN
    ALTER TABLE `group` DROP COLUMN `channel_synced`;
  END IF;
END;
-- +migrate StatementEnd

CALL __group_channel_synced_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_synced_down;
-- +migrate StatementEnd
