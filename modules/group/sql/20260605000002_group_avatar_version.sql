-- +migrate Up
-- 群头像对象版本。0 表示沿用旧版稳定对象 key，正数写入对象 path 用于 CDN-safe cache busting。
-- 使用 INFORMATION_SCHEMA 守卫保证迁移在部分执行后重试时仍可重入。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_avatar_version;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_avatar_version()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'avatar_version') THEN
    ALTER TABLE `group`
      ADD COLUMN `avatar_version` BIGINT NOT NULL DEFAULT 0 COMMENT '群头像对象版本，0 表示旧版稳定路径';
  END IF;
END;
-- +migrate StatementEnd

CALL __group_avatar_version();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_avatar_version;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_avatar_version_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_avatar_version_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'avatar_version') THEN
    ALTER TABLE `group` DROP COLUMN `avatar_version`;
  END IF;
END;
-- +migrate StatementEnd

CALL __group_avatar_version_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_avatar_version_down;
-- +migrate StatementEnd
