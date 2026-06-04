-- +migrate Up
-- 群级「允许免@生效」总开关 allow_no_mention（YUJ-2996）。默认 1=允许，零回归，
-- 存量已设免@的群不受影响。
--
-- 幂等/可重入写法（与 botfather/20260603000001、base/20260512000001 同范式：
-- INFORMATION_SCHEMA 守卫 + 存储过程）。原因：MySQL DDL 隐式提交，sql-migrate
-- 仅在「一个迁移的所有语句都成功」后才往 gorp_migrations 记账；多 pod 滚动发布
-- 或部分迁移重试时，裸 ADD COLUMN 重跑会因列已存在报 Duplicate column name 而
-- CrashLoopBackOff（见 #239/#253 部署事故）。存在性守卫后本迁移可安全重入。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_allow_no_mention;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_allow_no_mention()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'allow_no_mention') THEN
    ALTER TABLE `group`
      ADD COLUMN `allow_no_mention` TINYINT NOT NULL DEFAULT 1 COMMENT 'Group-level allow no-@: 1=yes (default, backward-compat, existing no-@ bots unaffected), 0=bot must be @mentioned in this group';
  END IF;
END;
-- +migrate StatementEnd

CALL __group_allow_no_mention();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_allow_no_mention;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_allow_no_mention_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_allow_no_mention_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'allow_no_mention') THEN
    ALTER TABLE `group` DROP COLUMN `allow_no_mention`;
  END IF;
END;
-- +migrate StatementEnd

CALL __group_allow_no_mention_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_allow_no_mention_down;
-- +migrate StatementEnd
