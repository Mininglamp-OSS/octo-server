-- +migrate Up

-- activated_at — the two-phase create gate (O6).
--
-- WHAT IT ANSWERS: has the subsystem side confirmed, at least once, that this
-- project has a container. Until it has, the peer control plane must answer
-- questions about the project exactly as it answers about one that does not
-- exist -- epoch 0, member false -- so that nothing can be authorized into a
-- project whose workspace is not there yet.
--
-- A ONE-WAY LATCH, not a status. Once set it is never cleared: it records that a
-- confirmation happened, not that the container exists right now. Container
-- existence is the subsystem side truth and octo_project_provisioning is
-- explicitly forbidden from gating any read path (D12) for exactly that reason.
--
-- WHY A COLUMN AND NOT A THIRD status VALUE. About twenty predicates in this
-- repository read status = 1, and they mean different things by it: the seat
-- exists / the name is taken / it counts against quota / it is visible / it is
-- writable. A third status value changes all of them at once, and the two ways
-- of getting it wrong are not symmetric -- a missed predicate on the status
-- route silently turns members into non-members, which the I2 reconcile then
-- reads as a violation and tears down the project group. A missed predicate here
-- only means the peer sees a project slightly early, which is the same window
-- that exists today. The archive design (contract section 8) chose a column for
-- the same reason; this is that decision applied to the same table.
--
-- WHY NULLABLE RATHER THAN A ZERO TIME. NULL is the only value that cannot also
-- be a real timestamp. A sentinel inside the value domain is what
-- member_epoch = 0 was, and PR #852 spent a review round removing it.
--
-- BACKFILL. Existing rows are set to created_at, not left NULL. They were
-- created before this gate existed, and leaving them NULL would silently remove
-- every existing project from the two peer-facing endpoints the moment this
-- deploys -- a mass revocation dressed up as a migration.
--
-- WHEN IT IS SET ON A NEW ROW. At insert, unless the fleet provisioning target
-- is enabled. That default is the important half: with no target enabled nothing
-- would ever run the confirmation step, so a NULL default would make every new
-- project permanently invisible to the peer on the deployments that have not
-- turned provisioning on -- which is all of them today.
--
-- ONE INDEX, and it is not for the read paths. Every read that consults this
-- column already filters by (space_id, project_id) or by primary key and has the
-- row in hand; there the column is a post-filter on a row already selected.
--
-- The index exists for the three statements that scan BY it: the awaiting count,
-- the oldest-age gauge and the reconcile repair. An earlier version of this file
-- argued no index was needed because those are periodic rather than request
-- paths. That argument had the cost backwards. The gauges run on every metrics
-- tick on every pod, and their STEADY STATE is the worst case: with nothing
-- awaiting activation, activated_at IS NULL matches no row, so an unindexed
-- query reads the whole table to return nothing, forever, and gets slower as the
-- table grows. NULLs are indexed in InnoDB, so the leading column makes exactly
-- that case an empty index range.
--
-- Column order follows the predicates: activated_at first because it is the
-- selective one (almost always zero rows), then status, then created_at so the
-- oldest-age query gets its ORDER BY from the index instead of a filesort.
ALTER TABLE `octo_project`
  ADD COLUMN `activated_at` DATETIME(3) NULL
  COMMENT '两阶段创建：子系统确认容器已就绪的时间（UTC，应用侧写入）。NULL=尚未确认，对端两个入站接口一律按不存在作答。单向闩锁，置位后不再清除',
  ADD KEY `idx_octo_project_awaiting_activation` (`activated_at`, `status`, `created_at`);

-- Backfill, deliberately before anything can read the column: rows that predate
-- the gate were confirmed by the absence of the gate.
--
-- ONE UNBOUNDED UPDATE, and that is a decision rather than an oversight — it was
-- raised twice in review, so the reasoning belongs here instead of in a thread.
--
-- Every other bulk operation in this module is batched, including this feature
-- own purge, and the argument there is about a table that grows without bound
-- from traffic. This one is different in the way that matters: it runs ONCE, in
-- the migration, against a table whose row count is the number of projects that
-- exist. That is bounded by the per-Space and per-creator quotas rather than by
-- traffic, and octo_project was created days before this column.
--
-- The real limit is what a migration can express: sql-migrate applies each file
-- as one unit with no loop construct, so chunking means either a stored
-- procedure or moving the backfill out of the migration into application code
-- that has to be idempotent and observable on its own. Both are more machinery
-- than a single UPDATE over a table this size deserves, and both add a way for
-- the backfill to be half-done — which is the state the reconcile repair exists
-- to clean up, so it would be repairing a mess this file created.
--
-- What to check before deploying to an installation where octo_project is
-- large: SELECT COUNT(*) FROM octo_project. This statement holds row locks on
-- every matching row for its duration and runs inside the startup path, so a
-- count in the millions wants a maintenance window and the chunked rewrite; at
-- the thousands this repository actually has, it is milliseconds.
UPDATE `octo_project` SET `activated_at` = `created_at` WHERE `activated_at` IS NULL;

-- +migrate Down
--
-- Dropping this column removes the gate: projects still awaiting confirmation
-- become visible to the peer immediately. That is the pre-O6 behaviour, so a
-- rollback is safe in the availability direction and only in that direction.
--
-- No apostrophes anywhere in this file, in ANY comment, on purpose. The
-- migration test in this module splits statements naively and treats a quote as
-- a string delimiter, so one possessive apostrophe makes it read the next
-- semicolon as being inside a literal. The failure PAIRS UP: an even number of
-- them can pass while an odd number fails. Do not add one back and rely on
-- counting.
ALTER TABLE `octo_project`
  DROP KEY `idx_octo_project_awaiting_activation`,
  DROP COLUMN `activated_at`;
