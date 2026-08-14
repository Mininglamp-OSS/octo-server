-- +migrate Up

-- MySQL 8 has no ADD COLUMN IF NOT EXISTS. The migration runner normally
-- applies this once, while this guard also handles a reused database whose
-- schema already contains the column but whose migration ledger does not.
SET @mode_col_exists = (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema = DATABASE() AND table_name = 'user_notification_pause' AND column_name = 'mode');
SET @mode_add_sql = IF(@mode_col_exists = 0,
  'ALTER TABLE `user_notification_pause` ADD COLUMN `mode` VARCHAR(16) NULL AFTER `uid`',
  'SELECT 1');
PREPARE mode_add_stmt FROM @mode_add_sql;
EXECUTE mode_add_stmt;
DEALLOCATE PREPARE mode_add_stmt;

-- +migrate Down

-- Dropping this column irreversibly loses active manual pauses. Roll back only
-- with a state backup and an explicit customer-impact decision.
ALTER TABLE `user_notification_pause`
  DROP COLUMN `mode`;
