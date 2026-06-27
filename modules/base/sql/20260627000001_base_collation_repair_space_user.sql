-- +migrate Up

-- Follow-up to 20260512000001_base_oss_compat_repair.sql.
--
-- The original repair converted 17 tables (including user_verification) to
-- utf8mb4_general_ci, but missed space_member and user — both created without
-- an explicit COLLATE clause, inheriting the DB default.
--
-- On deployments where the DB default is utf8mb4_0900_ai_ci (MySQL 8 default),
-- any JOIN from space_member to user_verification triggers:
--   Error 1267: Illegal mix of collations
-- This was first hit by #420 (queryMembers LEFT JOIN user_verification).
--
-- This migration converts space_member and user to utf8mb4_general_ci,
-- eliminating the collation asymmetry for all current and future JOINs.
-- Idempotent: skips tables already at the target collation.

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_compat_repair_space_user;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __oss_compat_repair_space_user()
BEGIN
  DECLARE v_collation VARCHAR(64);

  -- Repair space_member
  SELECT MAX(TABLE_COLLATION) INTO v_collation
    FROM information_schema.TABLES
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'space_member';
  IF v_collation IS NOT NULL AND v_collation <> 'utf8mb4_general_ci' THEN
    ALTER TABLE `space_member` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
  END IF;

  -- Repair user
  SELECT MAX(TABLE_COLLATION) INTO v_collation
    FROM information_schema.TABLES
    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user';
  IF v_collation IS NOT NULL AND v_collation <> 'utf8mb4_general_ci' THEN
    ALTER TABLE `user` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
  END IF;
END;
-- +migrate StatementEnd

CALL __oss_compat_repair_space_user();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __oss_compat_repair_space_user;
-- +migrate StatementEnd

-- +migrate Down
-- No-op: see 20260512000001_base_oss_compat_repair.sql for rationale.
SELECT 1;
