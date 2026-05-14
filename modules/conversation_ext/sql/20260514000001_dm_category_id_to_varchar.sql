-- +migrate Up

-- PR #21 Round-6 (Jerry-Xin / 原型 image-v1.png)：DM category 必须与 group_category
-- 共用 namespace，让客户端能把 DM 归类到既有的 group_category UUID 下。
-- 原 BIGINT 类型客户端拿不到真实 category_id（group_category.category_id 是 VARCHAR(32)
-- UUID），无法实现"项目协作"分类下既有群又有 DM 的产品形态。
--
-- 安全性：本 migration 把 dm_category_id 从 BIGINT NULL 改成 VARCHAR(32) NULL。
-- 由于到此为止 dm_category_id 没有任何客户端真正写入有意义值（只是 echo back），
-- 直接 DROP 旧字段 + ADD 新字段是合法的。但为了在脏环境（已经测试过有数据）安全，
-- 用 INFORMATION_SCHEMA.COLUMNS 检测当前类型决定是否需要改：
--   - 当前 BIGINT → DROP + ADD VARCHAR(32)
--   - 当前 VARCHAR(32) → 跳过（migration 重跑幂等）
-- 旧 idx_followed (uid, space_id, followed_dm, dm_category_id, follow_sort) 在 DROP
-- column 时会自动失效，需要重建。

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_dm_category_id_migrate;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __conv_ext_dm_category_id_migrate()
BEGIN
  DECLARE v_type VARCHAR(64);
  DECLARE v_idx_exists INT;

  SELECT DATA_TYPE INTO v_type
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'user_conversation_ext'
      AND COLUMN_NAME = 'dm_category_id';

  IF v_type IS NOT NULL AND LOWER(v_type) != 'varchar' THEN
    -- 先 DROP 依赖 dm_category_id 的索引（如果存在）
    SELECT COUNT(*) INTO v_idx_exists
      FROM information_schema.STATISTICS
      WHERE TABLE_SCHEMA = DATABASE()
        AND TABLE_NAME = 'user_conversation_ext'
        AND INDEX_NAME = 'idx_followed';
    IF v_idx_exists > 0 THEN
      SET @sql = 'ALTER TABLE user_conversation_ext DROP INDEX idx_followed';
      PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
    END IF;

    -- 改字段类型（dm_category_id 没有外部约束，直接 MODIFY）。
    -- 旧 BIGINT 值无法表达 VARCHAR(32) UUID，所以丢弃数据：用 ALTER 直接转换
    -- 会让 BIGINT 变成字符串数字（如 "12345"），不符合 UUID 形态。改用 DROP + ADD。
    SET @sql = 'ALTER TABLE user_conversation_ext DROP COLUMN dm_category_id';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

    SET @sql = 'ALTER TABLE user_conversation_ext ADD COLUMN dm_category_id VARCHAR(32) NULL COMMENT ''私聊所属分类ID（group_category.category_id UUID，NULL 表示未分类）'' AFTER followed_dm';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

    -- 重建 idx_followed。
    SET @sql = 'CREATE INDEX idx_followed ON user_conversation_ext (uid, space_id, followed_dm, dm_category_id, follow_sort)';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
  END IF;
END;
-- +migrate StatementEnd

CALL __conv_ext_dm_category_id_migrate();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_dm_category_id_migrate;
-- +migrate StatementEnd

-- +migrate Down

-- 反向迁移：把 VARCHAR(32) 转回 BIGINT NULL。
-- 不保留数据（UUID 无法塞回 BIGINT），与 Up 对称。

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_dm_category_id_revert;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __conv_ext_dm_category_id_revert()
BEGIN
  DECLARE v_type VARCHAR(64);
  DECLARE v_idx_exists INT;

  SELECT DATA_TYPE INTO v_type
    FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA = DATABASE()
      AND TABLE_NAME = 'user_conversation_ext'
      AND COLUMN_NAME = 'dm_category_id';

  IF v_type IS NOT NULL AND LOWER(v_type) != 'bigint' THEN
    SELECT COUNT(*) INTO v_idx_exists
      FROM information_schema.STATISTICS
      WHERE TABLE_SCHEMA = DATABASE()
        AND TABLE_NAME = 'user_conversation_ext'
        AND INDEX_NAME = 'idx_followed';
    IF v_idx_exists > 0 THEN
      SET @sql = 'ALTER TABLE user_conversation_ext DROP INDEX idx_followed';
      PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
    END IF;

    SET @sql = 'ALTER TABLE user_conversation_ext DROP COLUMN dm_category_id';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

    SET @sql = 'ALTER TABLE user_conversation_ext ADD COLUMN dm_category_id BIGINT NULL COMMENT ''私聊所属分类ID，NULL表示未分类'' AFTER followed_dm';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

    SET @sql = 'CREATE INDEX idx_followed ON user_conversation_ext (uid, space_id, followed_dm, dm_category_id, follow_sort)';
    PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
  END IF;
END;
-- +migrate StatementEnd

CALL __conv_ext_dm_category_id_revert();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __conv_ext_dm_category_id_revert;
-- +migrate StatementEnd
