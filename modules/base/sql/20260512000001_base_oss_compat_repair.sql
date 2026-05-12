-- +migrate Up

-- OSS-compat one-shot repair. Two structural drifts existed in pre-rename
-- schemas (PR #7 history):
--
--   (a) Sixteen tables shipped with CHARSET=utf8mb4 but no explicit COLLATE.
--       MySQL 8 resolves that to utf8mb4_0900_ai_ci (the charset default),
--       not the server default — so any JOIN crossing into utf8mb4_general_ci
--       tables raised Error 1267. Source migrations now pin general_ci
--       explicitly, but sql-migrate skips already-applied migrations on
--       existing installs, so the fix only reaches them via this forward
--       migration.
--
--   (b) robot.idx_robot_bot_token was a plain UNIQUE on a NOT NULL DEFAULT
--       '' column, so the second system-bot whose token wasn't issued yet
--       tripped Error 1062 on every cold start. Replaced with a functional
--       UNIQUE on NULLIF(bot_token, '') — MySQL 8 treats each NULL
--       distinctly, so empty-token rows coexist while duplicate real bf_*
--       tokens are still rejected at the storage layer (auth assumes
--       `WHERE bot_token = ?` matches at most one row).
--
-- Why a stored procedure (vs flat ALTERs): sql-migrate's MySQL driver runs
-- one statement per query (no multiStatements), and ALTER TABLE has no
-- IF-EXISTS modifier. The two requirements together force a procedural
-- guard: check INFORMATION_SCHEMA, dispatch dynamic SQL with PREPARE.
-- Done outside a procedure each guard would need PREPARE/EXECUTE/DEALLOCATE
-- as separate statements, which the driver rejects (Error 1064). Packaging
-- the body as a procedure makes it a single MySQL statement and lets us
-- use plain BEGIN/END, IF, and a small dispatch loop instead of repeating
-- the guard 17 times.
--
-- Idempotent: each ALTER is gated by an INFORMATION_SCHEMA check. Safe to
-- apply on a clean install (no-op — source migrations already produced the
-- right schema) and on partially-upgraded internal databases (executes
-- only the still-drifted deltas). Tables that don't exist on a deployment
-- (e.g. thread_* without DM_THREAD_ON=true) skip silently.
--
-- Data impact: CONVERT TO CHARACTER SET on utf8mb4 → utf8mb4 is metadata-
-- only at the row level (no byte rewrite), but MySQL still does a COPY
-- rebuild and holds a table-level metadata lock for the duration. Reads
-- block on the new table during rename. Operators with large
-- oidc_audit_log / thread_member tables should apply off-hours.

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_compat_repair;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __oss_compat_repair()
BEGIN
  DECLARE v_collation VARCHAR(64);
  DECLARE v_expression LONGTEXT;
  DECLARE v_index_exists INT;
  DECLARE v_table VARCHAR(64);
  DECLARE v_done INT DEFAULT 0;
  DECLARE v_cur CURSOR FOR
    SELECT t FROM (
      SELECT 'app_bot' AS t UNION ALL SELECT 'backup_config' UNION ALL
      SELECT 'backup_history'      UNION ALL SELECT 'login_log' UNION ALL
      SELECT 'oidc_audit_log'      UNION ALL SELECT 'robot_apply' UNION ALL
      SELECT 'space_email_invite'  UNION ALL SELECT 'space_join_apply' UNION ALL
      SELECT 'thread'              UNION ALL SELECT 'thread_member' UNION ALL
      SELECT 'thread_setting'      UNION ALL SELECT 'user_api_key' UNION ALL
      SELECT 'user_oidc_identity'  UNION ALL SELECT 'user_oidc_refresh' UNION ALL
      SELECT 'user_pinned_channel' UNION ALL SELECT 'user_verification' UNION ALL
      SELECT 'user_voice_context'
    ) AS tables_to_repair;
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET v_done = 1;

  -- (a) Normalise collation for each listed table (skip if absent or already correct).
  --
  -- Note on the lookup form: we use MAX(TABLE_COLLATION) rather than plain
  -- SELECT … LIMIT 1, and *not* SELECT INTO with a guard. The CONTINUE
  -- HANDLER FOR NOT FOUND below is shared with the cursor's FETCH, so a
  -- SELECT INTO that returns zero rows (e.g. when DM_THREAD_ON=false and
  -- the thread table is absent) would also fire the handler and set
  -- v_done=1, causing the next FETCH iteration to terminate the loop and
  -- silently skip every remaining table after the missing one. Aggregating
  -- with MAX() always returns one row whose value is NULL when the table
  -- is absent, so the NOT FOUND handler stays exclusive to cursor
  -- exhaustion.
  OPEN v_cur;
  read_loop: LOOP
    FETCH v_cur INTO v_table;
    IF v_done = 1 THEN LEAVE read_loop; END IF;
    SELECT MAX(TABLE_COLLATION) INTO v_collation
      FROM information_schema.TABLES
      WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = v_table;
    IF v_collation IS NOT NULL AND v_collation <> 'utf8mb4_general_ci' THEN
      SET @sql = CONCAT('ALTER TABLE `', v_table, '` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci');
      PREPARE stmt FROM @sql;
      EXECUTE stmt;
      DEALLOCATE PREPARE stmt;
    END IF;
  END LOOP;
  CLOSE v_cur;

  -- (b) Repair robot.idx_robot_bot_token to functional UNIQUE on NULLIF(bot_token,'').
  --     Detect current shape: STATISTICS.EXPRESSION is NULL for plain-column
  --     indexes and non-NULL (containing "nullif") for the target form.
  IF (SELECT COUNT(*) FROM information_schema.TABLES
        WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'robot') > 0 THEN
    SELECT COUNT(*), MAX(EXPRESSION) INTO v_index_exists, v_expression
      FROM information_schema.STATISTICS
      WHERE TABLE_SCHEMA = DATABASE()
        AND TABLE_NAME = 'robot'
        AND INDEX_NAME = 'idx_robot_bot_token';

    IF v_index_exists = 0
       OR v_expression IS NULL
       OR LOWER(v_expression) NOT LIKE '%nullif%bot_token%' THEN
      IF v_index_exists > 0 THEN
        ALTER TABLE `robot` DROP INDEX `idx_robot_bot_token`;
      END IF;
      ALTER TABLE `robot` ADD UNIQUE KEY `idx_robot_bot_token` ((NULLIF(`bot_token`, '')));
    END IF;
  END IF;
END;
-- +migrate StatementEnd

CALL __oss_compat_repair();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_compat_repair;
-- +migrate StatementEnd

-- +migrate Down
-- No-op: the repair brings drifted schemas into alignment with the canonical
-- one. Reverting would require per-deployment knowledge of the pre-repair
-- state, which we do not record. Operators who want to roll back should
-- restore from snapshot.
SELECT 1;
