-- +migrate Up

-- user_conversation_ext 的两个查询级索引（issue #337, PR review Round-3 follow-up）。
--
-- 拆为独立 migration 是因为：sql-migrate 按 version 时间戳追踪是否已执行，
-- 任何在 20260513000001 文件内容更新前就已运行过该 migration 的
-- dev/staging 数据库都不会重跑它。把索引放到一个新的 timestamped 文件，
-- 配合 INFORMATION_SCHEMA + PREPARE 做幂等守卫，既能在干净环境一次性创建，
-- 也能让脏环境再次 migrate 时收敛。
--
-- 不使用 `ALTER TABLE ... DROP INDEX IF EXISTS`：MySQL 8.0.29+ 才识别
-- ALTER 上下文的 IF EXISTS（PR #21 review by Jerry-Xin），早期补丁版会抛
-- 1064 语法错误。改用 INFORMATION_SCHEMA.STATISTICS 显式存在性检测后
-- PREPARE/EXECUTE 动态 DROP，对全部 MySQL 8 系列稳定可用。

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_indexes_repair;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __conv_ext_indexes_repair()
BEGIN
  DECLARE v_exists INT;

  -- idx_unfollowed_group 支持 ListUnfollowedGroups:
  --   WHERE uid=? AND space_id=? AND target_type=2 AND group_unfollowed=1
  SELECT COUNT(*) INTO v_exists
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'user_conversation_ext'
      AND INDEX_NAME = 'idx_unfollowed_group';
  IF v_exists > 0 THEN
    SET @sql = 'ALTER TABLE user_conversation_ext DROP INDEX idx_unfollowed_group';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
  END IF;
  SET @sql = 'CREATE INDEX idx_unfollowed_group ON user_conversation_ext (uid, space_id, target_type, group_unfollowed)';
  PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

  -- idx_thread_sort 支持 ListThreadExts:
  --   WHERE uid=? AND space_id=? AND target_type=5 ORDER BY follow_sort
  SELECT COUNT(*) INTO v_exists
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'user_conversation_ext'
      AND INDEX_NAME = 'idx_thread_sort';
  IF v_exists > 0 THEN
    SET @sql = 'ALTER TABLE user_conversation_ext DROP INDEX idx_thread_sort';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
  END IF;
  SET @sql = 'CREATE INDEX idx_thread_sort ON user_conversation_ext (uid, space_id, target_type, follow_sort)';
  PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
END;
-- +migrate StatementEnd

CALL __conv_ext_indexes_repair();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_indexes_repair;
-- +migrate StatementEnd

-- +migrate Down

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_indexes_drop;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __conv_ext_indexes_drop()
BEGIN
  DECLARE v_exists INT;

  SELECT COUNT(*) INTO v_exists
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'user_conversation_ext'
      AND INDEX_NAME = 'idx_thread_sort';
  IF v_exists > 0 THEN
    SET @sql = 'ALTER TABLE user_conversation_ext DROP INDEX idx_thread_sort';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
  END IF;

  SELECT COUNT(*) INTO v_exists
    FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'user_conversation_ext'
      AND INDEX_NAME = 'idx_unfollowed_group';
  IF v_exists > 0 THEN
    SET @sql = 'ALTER TABLE user_conversation_ext DROP INDEX idx_unfollowed_group';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
  END IF;
END;
-- +migrate StatementEnd

CALL __conv_ext_indexes_drop();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_indexes_drop;
-- +migrate StatementEnd
