-- +migrate Up
-- Track the source object for sticker-message collection. Direct uploads keep
-- source_path_hash empty; only live collected rows participate in the unique
-- key via the generated column, so existing multi-upload behavior is unchanged.

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __sticker_collect;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __sticker_collect()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'source_path') THEN
    ALTER TABLE `sticker`
      ADD COLUMN `source_path` VARCHAR(512) NOT NULL DEFAULT '' COMMENT '收藏来源贴纸对象 key（sticker/{uid}/{file}）' AFTER `keywords`;
  END IF;

  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'source_path_hash') THEN
    ALTER TABLE `sticker`
      ADD COLUMN `source_path_hash` CHAR(64) NOT NULL DEFAULT '' COMMENT '收藏来源贴纸对象 key 的 sha256 hex' AFTER `source_path`;
  END IF;

  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'source_path_hash_live') THEN
    ALTER TABLE `sticker`
      ADD COLUMN `source_path_hash_live` CHAR(64)
        GENERATED ALWAYS AS (
          CASE
            WHEN `status` = 1 AND `source_path_hash` <> '' THEN `source_path_hash`
            ELSE NULL
          END
        ) STORED COMMENT 'live 收藏幂等唯一键辅助列' AFTER `source_path_hash`;
  END IF;

  IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND INDEX_NAME = 'uk_uid_source_path_hash_live') THEN
    ALTER TABLE `sticker`
      ADD UNIQUE KEY `uk_uid_source_path_hash_live` (`uid`, `source_path_hash_live`);
  END IF;
END;
-- +migrate StatementEnd

CALL __sticker_collect();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __sticker_collect;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __sticker_collect_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __sticker_collect_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND INDEX_NAME = 'uk_uid_source_path_hash_live') THEN
    ALTER TABLE `sticker` DROP INDEX `uk_uid_source_path_hash_live`;
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'source_path_hash_live') THEN
    ALTER TABLE `sticker` DROP COLUMN `source_path_hash_live`;
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'source_path_hash') THEN
    ALTER TABLE `sticker` DROP COLUMN `source_path_hash`;
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sticker'
         AND COLUMN_NAME = 'source_path') THEN
    ALTER TABLE `sticker` DROP COLUMN `source_path`;
  END IF;
END;
-- +migrate StatementEnd

CALL __sticker_collect_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __sticker_collect_down;
-- +migrate StatementEnd
