-- +migrate Up
-- 区分「用户显式起的群名」(is_named=1) 与「成员名拼接的自动默认名」(is_named=0)。
-- 默认头像取字仅对 is_named=1 生效：命名群取群名前 2 字（script 感知），自动名群回退
-- 双人图标（避免把「张三、李四、王五」这种拼接名渲成头像文字）。
-- 存量群事后无法区分两类名（都存在 name 里），按产品决策**保守回填为 1**——一律视为
-- 命名群、保留现状（按群名取字），不改变任何既有头像；仅新建群按建群是否传入 name 计算。
--
-- 恢复安全（recovery-safe）三步法：MySQL 的 ALTER TABLE 隐式提交、不可回滚，因此不能把
-- 回填放进「列是否存在」的守卫里——一旦 ADD COLUMN 已提交、回填 UPDATE 中途失败
-- （崩溃 / 锁超时 / 死锁），重试时列已存在、整块被跳过，存量行将停留在 DEFAULT 0
-- （= 自动名 → 双人图标），与「不改变既有头像」相悖。改用可空哨兵：
--   1) 先以 NULL（无默认）建列：存量行得到 NULL = 「尚未回填」；
--   2) 无条件回填 `WHERE is_named IS NULL`：幂等、只动未回填行，任意中断后重试都能补完；
--   3) 再 MODIFY 为 NOT NULL DEFAULT 0：新建群默认 0，由建群逻辑显式写 0/1。
-- 每步各自带幂等守卫，跨「隐式提交边界」的部分失败重试都能正确收敛。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_is_named;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_is_named()
BEGIN
  -- Step 1: 以可空（无默认）建列——存量行 = NULL「尚未回填」。守卫保证可重入。
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'is_named') THEN
    ALTER TABLE `group`
      ADD COLUMN `is_named` TINYINT NULL COMMENT '群名是否用户显式起名(1)/成员拼接自动名(0);默认头像取字仅对1生效';
  END IF;

  -- Step 2: 回填存量行为 1（保留现状、不改既有头像）。无条件执行但仅命中 NULL 行——
  -- 幂等、可重复，任意中断后重试都能补完剩余未回填行，且不会覆盖已落定的 0/1。
  -- 注：单条全表 UPDATE 仅命中存量 NULL 行，一次性回填；迁移在服务对外前运行，无并发写。
  UPDATE `group` SET `is_named` = 1 WHERE `is_named` IS NULL;

  -- Step 3: 收紧为 NOT NULL DEFAULT 0（新建群默认 0）。守卫在「当前可空」时才执行，
  -- 使 Step 2 完成后的重试安全收敛。
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'is_named' AND IS_NULLABLE = 'YES') THEN
    ALTER TABLE `group`
      MODIFY COLUMN `is_named` TINYINT NOT NULL DEFAULT 0 COMMENT '群名是否用户显式起名(1)/成员拼接自动名(0);默认头像取字仅对1生效';
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
