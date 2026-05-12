-- +migrate Up

-- OSS-compat one-shot repair for databases that were initialised against
-- pre-rename schemas (PR #7 history). Two structural drifts existed:
--
--   1) Sixteen tables shipped with CHARSET=utf8mb4 but no explicit COLLATE.
--      MySQL 8 resolves that to utf8mb4_0900_ai_ci (the charset default),
--      not the server default — so any JOIN crossing into utf8mb4_general_ci
--      tables raised Error 1267. Fixed in the source migrations by adding
--      explicit COLLATE clauses, but sql-migrate skips already-applied
--      migrations on existing installs, so the fix never reaches them
--      without a forward migration.
--
--   2) robot.bot_token was NOT NULL DEFAULT '' with a UNIQUE index, which
--      collided across system-bot init paths the moment two empty-token
--      rows tried to coexist (Error 1062 on every cold start).
--
-- Every action below is idempotent: it guards on INFORMATION_SCHEMA and
-- becomes a no-op when the schema is already in the target state, so the
-- migration is safe to apply against a clean install (where the source
-- migrations already produced the right schema) and against partially
-- upgraded internal databases alike. Tables that simply don't exist on a
-- given deployment (e.g. thread_* without DM_THREAD_ON=true) are silently
-- skipped rather than failing the migration.

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_compat_repair;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __oss_compat_repair()
BEGIN
  DECLARE v_db VARCHAR(64) DEFAULT DATABASE();

  -- (1) Normalise table collation to utf8mb4_general_ci where it drifted.
  -- We list every table that historically shipped a non-general_ci default,
  -- including thread_* (skipped if absent) and app_bot (guarded against the
  -- 20260509/10/10 triple-flip leaving it on the wrong rail).
  --
  -- Note: ALTER TABLE ... CONVERT TO CHARACTER SET on utf8mb4 → utf8mb4 is
  -- metadata-only at the row level (no byte rewrite), but MySQL still does
  -- a COPY to rebuild every index. That means each table briefly takes a
  -- table-level metadata lock and reads block on the new table during the
  -- rename. Internal operators should review the row counts of oidc_audit_log
  -- and (if DM_THREAD_ON=true) thread_member before applying off-hours.

  CALL __oss_repair_table_collation(v_db, 'app_bot');
  CALL __oss_repair_table_collation(v_db, 'backup_config');
  CALL __oss_repair_table_collation(v_db, 'backup_history');
  CALL __oss_repair_table_collation(v_db, 'login_log');
  CALL __oss_repair_table_collation(v_db, 'oidc_audit_log');
  CALL __oss_repair_table_collation(v_db, 'robot_apply');
  CALL __oss_repair_table_collation(v_db, 'space_email_invite');
  CALL __oss_repair_table_collation(v_db, 'space_join_apply');
  CALL __oss_repair_table_collation(v_db, 'thread');
  CALL __oss_repair_table_collation(v_db, 'thread_member');
  CALL __oss_repair_table_collation(v_db, 'thread_setting');
  CALL __oss_repair_table_collation(v_db, 'user_api_key');
  CALL __oss_repair_table_collation(v_db, 'user_oidc_identity');
  CALL __oss_repair_table_collation(v_db, 'user_oidc_refresh');
  CALL __oss_repair_table_collation(v_db, 'user_pinned_channel');
  CALL __oss_repair_table_collation(v_db, 'user_verification');
  CALL __oss_repair_table_collation(v_db, 'user_voice_context');

  -- (2) Repair robot.bot_token nullability + index uniqueness.
  CALL __oss_repair_robot_bot_token(v_db);
END;
-- +migrate StatementEnd

-- Helper: if `table_name` exists in current DB and its collation isn't
-- utf8mb4_general_ci, CONVERT it. Skips otherwise.
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_repair_table_collation;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __oss_repair_table_collation(IN p_db VARCHAR(64), IN p_table VARCHAR(64))
BEGIN
  DECLARE v_collation VARCHAR(64);
  SELECT TABLE_COLLATION INTO v_collation
    FROM information_schema.TABLES
    WHERE TABLE_SCHEMA = p_db AND TABLE_NAME = p_table
    LIMIT 1;
  IF v_collation IS NOT NULL AND v_collation <> 'utf8mb4_general_ci' THEN
    SET @sql = CONCAT('ALTER TABLE `', p_table, '` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci');
    PREPARE stmt FROM @sql;
    EXECUTE stmt;
    DEALLOCATE PREPARE stmt;
  END IF;
END;
-- +migrate StatementEnd

-- Helper: ensure robot.bot_token is NULL DEFAULT NULL and the supporting
-- index is non-unique. Application-side uniqueness is already enforced by
-- crypto/rand-generated bf_* tokens (collision probability <1e-18); the
-- DB-level UNIQUE only ever blocked system-bot init.
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_repair_robot_bot_token;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __oss_repair_robot_bot_token(IN p_db VARCHAR(64))
BEGIN
  DECLARE v_table_exists INT;
  DECLARE v_is_nullable VARCHAR(3);
  DECLARE v_non_unique INT;

  SELECT COUNT(*) INTO v_table_exists
    FROM information_schema.TABLES
    WHERE TABLE_SCHEMA = p_db AND TABLE_NAME = 'robot';

  IF v_table_exists > 0 THEN
    -- (a) Loosen bot_token to NULL DEFAULT NULL if currently NOT NULL.
    SELECT IS_NULLABLE INTO v_is_nullable
      FROM information_schema.COLUMNS
      WHERE TABLE_SCHEMA = p_db AND TABLE_NAME = 'robot' AND COLUMN_NAME = 'bot_token'
      LIMIT 1;
    IF v_is_nullable = 'NO' THEN
      ALTER TABLE `robot`
        MODIFY COLUMN `bot_token` VARCHAR(100) NULL DEFAULT NULL
          COMMENT 'Bot认证Token(bf_前缀)';
    END IF;

    -- (b) If the bot_token index is still UNIQUE, drop and recreate as plain.
    -- information_schema.STATISTICS.NON_UNIQUE = 0 means UNIQUE, 1 means plain.
    SELECT MIN(NON_UNIQUE) INTO v_non_unique
      FROM information_schema.STATISTICS
      WHERE TABLE_SCHEMA = p_db
        AND TABLE_NAME = 'robot'
        AND INDEX_NAME = 'idx_robot_bot_token';
    IF v_non_unique = 0 THEN
      ALTER TABLE `robot` DROP INDEX `idx_robot_bot_token`;
      CREATE INDEX `idx_robot_bot_token` ON `robot` (`bot_token`);
    END IF;
  END IF;
END;
-- +migrate StatementEnd

CALL __oss_compat_repair();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_compat_repair;
-- +migrate StatementEnd

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_repair_table_collation;
-- +migrate StatementEnd

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_repair_robot_bot_token;
-- +migrate StatementEnd

-- +migrate Down
-- No-op: the repair brings drifted schemas into alignment with the canonical
-- one. Reverting it would require knowing each table's pre-repair collation
-- per deployment, which we do not record. Operators who want to roll back
-- should restore from snapshot.
SELECT 1;
