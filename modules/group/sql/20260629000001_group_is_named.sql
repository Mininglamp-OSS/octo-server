-- +migrate Up
-- 区分「用户显式起的群名」(is_named=1) 与「成员名拼接的自动默认名」(is_named=0)。
-- 默认头像取字仅对 is_named=1 生效：命名群取群名前 2 字（script 感知），自动名群回退
-- 双人图标（避免把「张三、李四、王五」这种拼接名渲成头像文字）。
-- 存量群事后无法区分两类名（都存在 name 里），按产品决策**保守回填为 1**——一律视为
-- 命名群、保留现状（按群名取字），不改变任何既有头像；仅新建群按建群是否传入 name 计算。
-- 使用 INFORMATION_SCHEMA 守卫保证迁移在部分执行后重试时仍可重入。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_is_named;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_is_named()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'is_named') THEN
    ALTER TABLE `group`
      ADD COLUMN `is_named` TINYINT NOT NULL DEFAULT 0 COMMENT '群名是否用户显式起名(1)/成员拼接自动名(0);默认头像取字仅对1生效';
    -- 存量群保守回填为 1：保留现状（按群名取字），不改变既有头像。
    UPDATE `group` SET `is_named` = 1;
  END IF;
END;
-- +migrate StatementEnd

CALL __group_is_named();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_is_named;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_is_named_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_is_named_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'is_named') THEN
    ALTER TABLE `group` DROP COLUMN `is_named`;
  END IF;
END;
-- +migrate StatementEnd

CALL __group_is_named_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_is_named_down;
-- +migrate StatementEnd
