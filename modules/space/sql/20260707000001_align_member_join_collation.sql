-- +migrate Up

-- Fix MySQL 1267 (Illegal mix of collations) in GetSpaceMembers.
--
-- queryMembers (modules/space/db.go) LEFT JOINs space_member.uid against three
-- columns: user.uid, user_verification.user_id and robot.robot_id. On a fresh
-- MySQL 8 instance every table created without an explicit COLLATE inherits the
-- server default (utf8mb4_0900_ai_ci), while user_verification pins
-- utf8mb4_general_ci in its own DDL. Comparing two different collations makes
-- MySQL abort the whole statement with error 1267, which surfaces to the client
-- as a 400 (err.server.space.query_failed) and leaves every member name showing
-- as a raw uid.
--
-- Pin every uid-family join key to one explicit collation (utf8mb4_general_ci,
-- the collation already used in production and by user_verification) so the
-- comparison is well-defined in every environment. Fixing the columns at the
-- schema level is the durable fix; a per-query COLLATE clause would only paper
-- over a single call site and leave the rest of the schema drifting.
--
-- MODIFY COLUMN to a fixed collation is idempotent: once a column already is
-- utf8mb4_general_ci the statement is a no-op, so re-running the migration is
-- safe. Column type / nullability / default / comment are kept identical to the
-- original DDL so only the collation changes.

ALTER TABLE `space_member`      MODIFY `uid`      VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL DEFAULT '' COMMENT '成员uid';
ALTER TABLE `user`              MODIFY `uid`      VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL DEFAULT '' COMMENT '用户唯一ID';
ALTER TABLE `robot`             MODIFY `robot_id` VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL DEFAULT '' COMMENT '机器人ID';
ALTER TABLE `user_verification` MODIFY `user_id`  VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL COMMENT 'OCTO 用户 UID';

-- +migrate Down

-- No-op: the pre-alignment collation was environment-dependent (the MySQL server
-- default at table-creation time), so there is no single meaningful value to
-- revert to. Leaving the join keys aligned on utf8mb4_general_ci is safe and is
-- the intended steady state.
