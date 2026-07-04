-- +migrate Up
-- channel_synced 标记群的 IM 频道是否已确认创建（1=已同步，0=待对账）。
-- 背景：CreateGroup 在事务提交后才创建 WuKongIM 频道；频道创建失败时做 best-effort
-- 补偿删除，若补偿删除自身也失败（DB 抖动）或进程在「commit 后、建频道前」崩溃，
-- group 行会残留成没有 IM 频道的孤儿。channel_synced=0 让后台对账 worker
-- （ChannelReconciler）能发现这些孤儿并重建频道 / 硬删除，实现最终一致。#394
--
-- 语义：只有 CreateGroup 主路径在插入群行后、commit 前显式写 0，建频道确认后写 1；
-- 其它建群路径（系统群 event.go 在事务内建频道、AddGroup 管理接口）无需参与对账，
-- 一律取默认值 1。故列默认值取 1，存量行也回填 1——零行为回归、无误报。
--
-- 恢复安全（recovery-safe）三步法：ALTER TABLE 隐式提交、不可回滚，回填不能塞进
-- 「列是否存在」守卫里（一旦 ADD 已提交、回填中途失败，重试会跳过整块，存量行停在
-- 建列默认值）。改用可空哨兵：
--   1) 先以 NULL（无默认）建列：存量行 = NULL「尚未回填」；
--   2) 无条件回填 `WHERE channel_synced IS NULL`：幂等、只动未回填行；
--   3) 再 MODIFY 为 NOT NULL DEFAULT 1：新建群默认已同步，CreateGroup 显式写 0。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_synced;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_channel_synced()
BEGIN
  -- Step 1: 以可空（无默认）建列——存量行 = NULL「尚未回填」。守卫保证可重入。
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'channel_synced') THEN
    ALTER TABLE `group`
      ADD COLUMN `channel_synced` TINYINT NULL COMMENT 'IM频道是否已确认创建(1)/待对账(0);孤儿群对账用 #394';
  END IF;

  -- Step 2: 回填存量行为 1（视为已同步、零行为回归）。仅命中 NULL 行，幂等可重试。
  UPDATE `group` SET `channel_synced` = 1 WHERE `channel_synced` IS NULL;

  -- Step 3: 收紧为 NOT NULL DEFAULT 1（新建群默认已同步）。守卫在「当前可空」时才执行。
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'channel_synced' AND IS_NULLABLE = 'YES') THEN
    ALTER TABLE `group`
      MODIFY COLUMN `channel_synced` TINYINT NOT NULL DEFAULT 1 COMMENT 'IM频道是否已确认创建(1)/待对账(0);孤儿群对账用 #394';
  END IF;
END;
-- +migrate StatementEnd

CALL __group_channel_synced();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_synced;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_synced_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __group_channel_synced_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group'
         AND COLUMN_NAME = 'channel_synced') THEN
    ALTER TABLE `group` DROP COLUMN `channel_synced`;
  END IF;
END;
-- +migrate StatementEnd

CALL __group_channel_synced_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __group_channel_synced_down;
-- +migrate StatementEnd
