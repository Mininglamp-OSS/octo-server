-- +migrate Up

-- MySQL DDL auto-commits per-statement and is NOT covered by sql-migrate's
-- surrounding transaction, so a mid-migration failure (or two processes
-- racing the same migration during a rolling deploy) can leave some of
-- these four columns already applied while gorp_migrations never got the
-- ledger row for this migration ID. On the next run sql-migrate treats the
-- whole file as pending again and re-issues every ADD COLUMN, tripping
-- MySQL Error 1060 on whichever column landed last time. Guard each ADD
-- COLUMN with an INFORMATION_SCHEMA existence check (see #252) so the
-- migration is safe to replay from any partially-applied state.

SET @mc_exists = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'thread' AND COLUMN_NAME = 'message_count');
SET @mc_sql = IF(@mc_exists = 0, 'ALTER TABLE `thread` ADD COLUMN `message_count` BIGINT NOT NULL DEFAULT 0 COMMENT ''消息数量''', 'SELECT 1');
PREPARE mc_stmt FROM @mc_sql;
EXECUTE mc_stmt;
DEALLOCATE PREPARE mc_stmt;

SET @lma_exists = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'thread' AND COLUMN_NAME = 'last_message_at');
SET @lma_sql = IF(@lma_exists = 0, 'ALTER TABLE `thread` ADD COLUMN `last_message_at` TIMESTAMP NULL DEFAULT NULL COMMENT ''最后一条消息时间''', 'SELECT 1');
PREPARE lma_stmt FROM @lma_sql;
EXECUTE lma_stmt;
DEALLOCATE PREPARE lma_stmt;

SET @lmc_exists = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'thread' AND COLUMN_NAME = 'last_message_content');
SET @lmc_sql = IF(@lmc_exists = 0, 'ALTER TABLE `thread` ADD COLUMN `last_message_content` VARCHAR(500) NOT NULL DEFAULT '''' COMMENT ''最后一条消息内容''', 'SELECT 1');
PREPARE lmc_stmt FROM @lmc_sql;
EXECUTE lmc_stmt;
DEALLOCATE PREPARE lmc_stmt;

SET @lmsu_exists = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'thread' AND COLUMN_NAME = 'last_message_sender_uid');
SET @lmsu_sql = IF(@lmsu_exists = 0, 'ALTER TABLE `thread` ADD COLUMN `last_message_sender_uid` VARCHAR(40) NOT NULL DEFAULT '''' COMMENT ''最后一条消息发送者UID''', 'SELECT 1');
PREPARE lmsu_stmt FROM @lmsu_sql;
EXECUTE lmsu_stmt;
DEALLOCATE PREPARE lmsu_stmt;

-- +migrate Down
ALTER TABLE `thread`
  DROP COLUMN `message_count`,
  DROP COLUMN `last_message_at`,
  DROP COLUMN `last_message_content`,
  DROP COLUMN `last_message_sender_uid`;
