-- +migrate Up
-- Add autocomplete metadata for user custom stickers. The migration is
-- idempotent/retry-safe because sql-migrate records a migration only after all
-- statements finish; if a pod dies after ADD COLUMN but before ADD INDEX, a
-- blind rerun would otherwise fail with Duplicate column name.

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __sticker_metadata;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __sticker_metadata()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'shortcode') THEN
    ALTER TABLE `sticker`
      ADD COLUMN `shortcode` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '客户端联想 shortcode（同 uid live 非空唯一）' AFTER `sort`;
  END IF;

  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'keywords') THEN
    ALTER TABLE `sticker`
      ADD COLUMN `keywords` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '客户端联想关键词 JSON 数组' AFTER `shortcode`;
  END IF;

  IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND INDEX_NAME = 'idx_uid_status_shortcode') THEN
    ALTER TABLE `sticker`
      ADD INDEX `idx_uid_status_shortcode` (`uid`, `status`, `shortcode`);
  END IF;
END;
-- +migrate StatementEnd

CALL __sticker_metadata();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __sticker_metadata;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __sticker_metadata_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __sticker_metadata_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND INDEX_NAME = 'idx_uid_status_shortcode') THEN
    ALTER TABLE `sticker` DROP INDEX `idx_uid_status_shortcode`;
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'keywords') THEN
    ALTER TABLE `sticker` DROP COLUMN `keywords`;
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'shortcode') THEN
    ALTER TABLE `sticker` DROP COLUMN `shortcode`;
  END IF;
END;
-- +migrate StatementEnd

CALL __sticker_metadata_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __sticker_metadata_down;
-- +migrate StatementEnd
