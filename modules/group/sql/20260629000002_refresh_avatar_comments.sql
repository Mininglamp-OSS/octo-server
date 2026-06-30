-- +migrate Up
-- 仅刷新列 COMMENT，使其与 2026-06-29 改版后的语义一致（comment-only，不改类型/约束/默认值，
-- 不改任何行数据）。背景：改版后群名不再作为新群头像文字，is_named 含义从「用户显式起名」
-- 重定义为「改版前的存量老群」标记（新群恒为 0、仅迁移回填老群为 1；渲染:老群群名文字、新群
-- 双人图标）。原迁移 20260629000001 / 20260625000001 的列 COMMENT 仍是旧措辞，schema 自省会
-- 携带过期运营说明（评审 Octo-Q / yujiawei / OctoBoooot / Jerry-Xin 一致指出）。已应用的历史
-- 迁移不可改，故用本 no-op COMMENT 迁移刷新。INFORMATION_SCHEMA 守卫 + MODIFY 本身幂等，可重入。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_refresh_avatar_comments;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_refresh_avatar_comments()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'is_named') THEN
    ALTER TABLE `group`
      MODIFY COLUMN `is_named` TINYINT NOT NULL DEFAULT 0
        COMMENT '1=改版前老群/0=新群(2026-06-29改版);老群渲染群名前2字、新群双人图标;新建群恒为0,is_named=1仅由迁移回填存量老群';
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'avatar_text') THEN
    ALTER TABLE `group`
      MODIFY COLUMN `avatar_text` VARCHAR(16) NOT NULL DEFAULT ''
        COMMENT '自定义群头像文字;空=按 is_named 回退(老群群名前2字/新群双人图标)';
  END IF;
END;
-- +migrate StatementEnd

CALL __group_refresh_avatar_comments();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_refresh_avatar_comments;
-- +migrate StatementEnd

-- +migrate Down
-- 还原为改版前的列 COMMENT（comment-only 逆操作；类型/约束不变）。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_refresh_avatar_comments_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_refresh_avatar_comments_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'is_named') THEN
    ALTER TABLE `group`
      MODIFY COLUMN `is_named` TINYINT NOT NULL DEFAULT 0
        COMMENT '群名是否用户显式起名(1)/成员拼接自动名(0);默认头像取字仅对1生效';
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'avatar_text') THEN
    ALTER TABLE `group`
      MODIFY COLUMN `avatar_text` VARCHAR(16) NOT NULL DEFAULT ''
        COMMENT '自定义群头像文字，空表示用群名派生';
  END IF;
END;
-- +migrate StatementEnd

CALL __group_refresh_avatar_comments_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_refresh_avatar_comments_down;
-- +migrate StatementEnd
