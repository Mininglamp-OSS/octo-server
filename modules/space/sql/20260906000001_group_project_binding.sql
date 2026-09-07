-- +migrate Up

-- group.project_id — the attribution column invariant I2 is written against.
--
-- WHY THIS LIVES IN modules/space/sql
--
-- Not because modules/space owns the concept — it does not — but because a
-- column must ship from a migration directory present in EVERY binary that
-- reads it, and modules/space is the only directory that qualifies here.
--
-- Measured with `go list -deps`:
--
--   binary            has modules/space migrations   has modules/group migrations
--   modules/group     yes                            yes
--   modules/space     yes                            NO
--   modules/project   yes                            NO
--   modules/channel   yes                            yes
--
-- Three modules read this column: modules/group (the admission gate and the
-- detach step), modules/space (joinPresetGroups skips project groups, and the
-- Space settings endpoint refuses one as a preset group), and modules/project
-- (the I2/I3 reconcile scans). Only modules/space/sql is in all three binaries.
--
-- This is also, finally, the real reason group.space_id was added from
-- modules/space's own directory in 20260307000004_space_legacy03.sql. The task
-- brief cited that as a precedent for "the module that owns the concept owns the
-- column" and placed this column in modules/project/sql. Both readings of the
-- precedent were wrong, and each was caught the same way — by a whole test
-- package failing with `Error 1054: Unknown column`:
--
--   * in modules/project/sql: modules/group's binary has no project migrations,
--     so every group and channel test that inserted a group failed (82 of them);
--   * in modules/group/sql:   modules/space's binary has no group migrations,
--     so the Space settings endpoint failed the moment it read the column.
--
-- The rule this leaves behind is about dependency direction, not ownership: a
-- column belongs to the migration directory of a module that every reader
-- imports. The Project-side schema (octo_project_member.removing, the cascade
-- outbox) correctly stays in modules/project/sql, because only modules/project
-- touches it.

-- group.project_id + (space_id, project_id)
-- ---------------------------------------------------------------------------
--
-- NOT NULL DEFAULT '' — '' is the sentinel for "Space-direct", never NULL.
--
-- This deliberately diverges from `group.space_id`, which the space migration
-- added as a nullable `VARCHAR(40) DEFAULT ''`. Every predicate in this feature
-- is written `project_id != ''` / `project_id = ''` — in the admission gate, in
-- the source guard, and in both reconcile scans. A three-valued column turns
-- each of those into a bug waiting for the first NULL row: `NULL != ''` is NULL,
-- not TRUE, so a NULL-attributed group would silently fall out of the I2 scan
-- while still being a project group. NOT NULL makes that unrepresentable.
--
-- ADD COLUMN ... NOT NULL DEFAULT '' is INSTANT on MySQL 8.0 (no table rebuild,
-- no row rewrite), so this statement is O(1) regardless of `group`'s size.
ALTER TABLE `group`
  ADD COLUMN `project_id` VARCHAR(40) NOT NULL DEFAULT ''
  COMMENT '所属项目ID；'' = 直属 Space（哨兵值，永不为 NULL）。I2/I3 的全部谓词都写作 project_id != '''' / = ''''';

-- The I2 reconcile scan is driven group-first — find project groups by
-- (space_id, project_id), then their members by group_no — because no index on
-- group_member leads with is_deleted, so a member-first scan would walk the
-- whole table.
--
-- ⚠️ NOT a no-op for existing queries. `group` has had exactly two indexes since
-- 2019 (group_groupNo UNIQUE(group_no), group_creator(creator)) and space_id is
-- in neither, so queries that already filter on space_id have had no index
-- behind them: db.go queryGroupsWithMemberUIDAndSpaceID and
-- modules/bot_api/groups.go:51. This index is the first to serve space_id, so
-- BOTH of those get new query plans. Expected to be faster; verified with
-- EXPLAIN before/after (brief C4).
--
-- Unlike the ADD COLUMN above this is a real index build: ALGORITHM=INPLACE,
-- online (concurrent DML permitted), but its DURATION is proportional to row
-- count and must be measured against production before rollout rather than
-- assumed from CI, where `group` is empty.
CREATE INDEX `group_space_project` ON `group` (`space_id`, `project_id`);


-- +migrate Down
DROP INDEX `group_space_project` ON `group`;
ALTER TABLE `group` DROP COLUMN `project_id`;
