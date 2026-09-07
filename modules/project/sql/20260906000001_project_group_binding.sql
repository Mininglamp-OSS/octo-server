-- +migrate Up

-- P1 — bind groups to projects, and give the removal cascade somewhere to live.
--
-- Two things land here, both Project-owned tables:
--   1. `octo_project_member.removing` — the "seat closing" flag (D4)
--   2. `octo_project_member_removal_cleanup` — the project-side cascade outbox (D5)
--
-- `group.project_id` deliberately does NOT live here. It is in
-- modules/space/sql/20260906000001_group_project_binding.sql, and that file
-- explains why: a column ships from a migration directory present in EVERY
-- binary that reads it. Three modules read this one (group, space, project) and
-- modules/space/sql is the only directory in all three -- which is also why
-- group.space_id was added from there.

-- ---------------------------------------------------------------------------
-- 1. octo_project_member.removing
-- ---------------------------------------------------------------------------
--
-- D4 — the cascade closes the seat FIRST and detaches groups after.
--
-- `removing` is set to 1 in the same transaction that begins removal, while
-- `status` stays 1. Every authorization read — the project member list, the
-- group admission gate, the role resolution in the middleware — treats removing = 1
-- as a non-member. Keeping status = 1 until the detach finishes is what makes
-- I2 never LITERALLY violated by the removal itself: the group_member rows that
-- have not been cleaned up yet still belong to a member of record. The worker
-- flips status to 0 and clears removing in its final transaction.
--
-- Re-admission during the window CANCELS rather than being rejected: adding the
-- uid back inside a transaction holding the project row lock clears `removing`,
-- leaves status = 1, and marks the outstanding job cancelled. Rejecting instead
-- would make an unrelated admin action fail for as long as the cascade takes,
-- and the cascade can legitimately run long.
ALTER TABLE `octo_project_member`
  ADD COLUMN `removing` TINYINT NOT NULL DEFAULT 0
  COMMENT '1=席位正在关闭（D4）。status 仍为 1，但所有授权读一律按非成员处理；worker 完成后置 status=0 并清零';

-- The "removing stalled" reconcile scan looks for rows stuck at removing = 1
-- past a configured age, and it is a SEPARATE alert from I2 with the opposite
-- meaning: I2 says the invariant broke, this says the machinery stopped.
--
-- `updated_at` is the stall anchor rather than a dedicated removing_at column,
-- and that is only valid because of a property this schema has to keep: while
-- removing = 1, NOTHING else writes the row. The worker re-reads under lock but
-- does not write until its final transaction, and the only other write that can
-- land is re-admission, which clears removing and takes the row out of this
-- index range anyway. If a future change starts touching member rows mid-cascade
-- the stall alert goes silent — add removing_at then rather than debugging why
-- on-call stopped being paged.
--
-- A low-cardinality leading column is the right shape here precisely because the
-- distribution is skewed: removing = 1 is a handful of rows out of the table, so
-- the scan reads only what it needs and stays bounded on rows EXAMINED, which
-- TestReconcileQueriesAreBounded requires.
CREATE INDEX `idx_octo_project_member_removing` ON `octo_project_member` (`removing`, `updated_at`);

-- ---------------------------------------------------------------------------
-- 2. octo_project_member_removal_cleanup
-- ---------------------------------------------------------------------------
--
-- D5 — the project-side cascade is its OWN outbox, not another step on the
-- Space job. Three reasons, all structural:
--
--   (a) keying. The Space job is keyed (space_id, uid). Project removal is
--       keyed (project_id, uid) and one uid can be removed from one project
--       while staying in others in the same Space.
--   (b) lease sizing. The Space job shares ONE 10-minute lease across its group,
--       conversation and project steps, and issue #797 documents that a single
--       job on a large Space can already outrun it. Project removal fans out
--       over every group in the project — project-sized work inside a lease
--       sized for Space cleanup.
--   (c) blast radius. modules/space/member_removal.go:56-64 states the step
--       contract: a returned error reruns the WHOLE job including the steps
--       that already succeeded. Hanging project cleanup off that job makes a
--       project-side failure re-drive the Space-side steps.
--
-- Structurally copied from space_member_removal_cleanup
-- (modules/space/sql/20260821000001_*.sql) — lease owner + lease_until,
-- attempts, next_attempt_at, backoff, a terminal abandoned state, the same two
-- index shapes, retention purge — with the corrections that the history of that table
-- earned:
--
--   * TIME IS APPLICATION-WRITTEN UTC, with no DEFAULT and no ON UPDATE. The
--     space table uses DEFAULT CURRENT_TIMESTAMP(3), which is the MySQL SESSION
--     timezone, and this repo has already shipped a metric broken by exactly
--     that: the member-removal age gauge compared a Go UTC clock against a
--     session-timezone column and read -28799 seconds under TZ=Asia/Shanghai
--     (modules/space/member_removal_metrics.go). One clock, in Go, in UTC.
--   * status carries a CANCELLED terminal state (3), which the space table has
--     no equivalent of. The re-admission path in D4 needs to retire an in-flight job
--     without it looking like either success or exhaustion, and without deleting
--     the row — a deleted job leaves no evidence that a cascade was cancelled.
--
-- Deliberately NOT unique on (project_id, uid): remove → re-add → remove must
-- enqueue a second job. The worker re-reads the member row under lock before
-- each batch, so a stale job for a member who has since been re-added resolves
-- to a no-op rather than to damage.
CREATE TABLE IF NOT EXISTS `octo_project_member_removal_cleanup` (
  `id`              BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `project_id`      VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '成员被移出的项目',
  `uid`             VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '被移除的成员（对齐 octo_project_member.uid）',
  `space_id`        VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '冗余 Space；避免 worker 每行回表查 octo_project',
  `operator_uid`    VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '操作者；自助退出时等于 uid',
  `reason`          VARCHAR(32)      NOT NULL DEFAULT ''  COMMENT 'kicked / left / space_removed / project_disbanded',
  `status`          TINYINT UNSIGNED NOT NULL DEFAULT 0   COMMENT '0=pending 1=done 2=abandoned 3=cancelled（D4 重新加入）',
  `attempts`        INT UNSIGNED     NOT NULL DEFAULT 0,
  `next_attempt_at` DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入，禁 CURRENT_TIMESTAMP',
  `lease_owner`     VARCHAR(64)      NOT NULL DEFAULT '',
  `lease_until`     DATETIME(3)      NULL                 COMMENT 'UTC；worker 在作业体内心跳续租（#797 的未决 P1）',
  `last_error`      VARCHAR(255)     NOT NULL DEFAULT ''  COMMENT '低基数失败摘要，不含用户内容',
  `created_at`      DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入，禁 CURRENT_TIMESTAMP',
  `finished_at`     DATETIME(3)      NULL                 COMMENT 'UTC；应用侧写入',
  PRIMARY KEY (`id`),
  KEY `idx_octo_project_removal_pending` (`status`, `next_attempt_at`, `lease_until`),
  KEY `idx_octo_project_removal_target` (`project_id`, `uid`),
  -- 保留期清理按 (status, finished_at) 扫。pending 索引首列虽然也是 status，
  -- 但第二列是 next_attempt_at，帮不上 finished_at 的范围条件；没有这条索引，
  -- purge 会全表扫一张随每次移除增长的表。
  KEY `idx_octo_project_removal_finished` (`status`, `finished_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='项目成员移除后的群面清理工单（D5）';

-- +migrate Down
--
-- WARNING: rolling this back DISCARDS PENDING CASCADE JOBS. Check the pending
-- count first, the same warning the space_member_removal_cleanup migration
-- carries: count rows in octo_project_member_removal_cleanup with status = 0.
-- A discarded job means a member whose project seat is closed keeps their rows
-- in that project group set, which the I2 reconcile scan will then report.
--
-- No apostrophes anywhere in this file, in ANY comment, on purpose. The
-- migration test in this module splits statements naively and treats a quote as
-- a string delimiter, so a possessive apostrophe makes it read the next
-- semicolon as being inside a literal.
--
-- Note the shape of the failure: apostrophes PAIR UP, so a file with an even
-- number of them can pass while a file with an odd number fails, and the test
-- then points at whichever statement happens to sit between the last pair. It
-- has now caught this twice, the second time because an unrelated edit removed
-- one apostrophe from a comment forty lines away and flipped the parity. Do not
-- add one back and rely on counting.
DROP TABLE IF EXISTS `octo_project_member_removal_cleanup`;
DROP INDEX `idx_octo_project_member_removing` ON `octo_project_member`;
ALTER TABLE `octo_project_member` DROP COLUMN `removing`;
