-- +migrate Up

-- group.project_id — the attribution column invariant I2 is written against.
--
-- WHY THIS LIVES IN modules/group/sql AND NOT modules/project/sql
--
-- The task brief placed it in modules/project/sql on the principle that "the
-- module that owns the concept owns the column", citing modules/space adding
-- group.space_id from its own directory
-- (20260307000004_space_legacy03.sql).
--
-- That precedent does not transfer, and the difference is load-bearing rather
-- than stylistic: modules/group IMPORTS modules/space, so space's migrations are
-- present in every binary that contains group — including group's test binary.
-- modules/project is not a dependency of modules/group at all (the cascade runs
-- the other way, reverse-registered at runtime), so its migrations are absent
-- from modules/group's test binary.
--
-- Found empirically, not reasoned about: with the column in modules/project/sql,
-- every modules/group test that inserts a group failed with
-- `Error 1054 (42S22): Unknown column 'project_id' in 'field list'`, because
-- Model now carries ProjectID and the DAO builds its column list by reflection.
--
-- The rule this leaves behind: a column belongs to the migration directory of a
-- module whose code READS OR WRITES it, and modules/group does both. The
-- Project-side schema (octo_project_member.removing, the cascade outbox) stays
-- in modules/project/sql, where only modules/project touches it.

-- ---------------------------------------------------------------------------
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
