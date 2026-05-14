-- +migrate Up

-- 删除 user_conversation_ext.version 列。PR #21 Round-3 起 per-row 乐观锁
-- 已被用户级 user_follow_version 表取代，UpdateSort 不再读写该列。Go struct
-- Model.Version 也已删除（PR #21 Round-6 P1 by yujiawei）。

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_drop_version_column;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __conv_ext_drop_version_column()
BEGIN
  DECLARE v_exists INT;
  SELECT COUNT(*) INTO v_exists
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'user_conversation_ext'
      AND COLUMN_NAME = 'version';
  IF v_exists > 0 THEN
    SET @sql = 'ALTER TABLE user_conversation_ext DROP COLUMN version';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
  END IF;
END;
-- +migrate StatementEnd

CALL __conv_ext_drop_version_column();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_drop_version_column;
-- +migrate StatementEnd

-- +migrate Down

-- 反向迁移：重新加回 version 列（NOT NULL DEFAULT 0），与原 schema 一致。

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_readd_version_column;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __conv_ext_readd_version_column()
BEGIN
  DECLARE v_exists INT;
  SELECT COUNT(*) INTO v_exists
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'user_conversation_ext'
      AND COLUMN_NAME = 'version';
  IF v_exists = 0 THEN
    SET @sql = 'ALTER TABLE user_conversation_ext ADD COLUMN version INT NOT NULL DEFAULT 0 COMMENT ''乐观锁版本号''';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
  END IF;
END;
-- +migrate StatementEnd

CALL __conv_ext_readd_version_column();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_readd_version_column;
-- +migrate StatementEnd
