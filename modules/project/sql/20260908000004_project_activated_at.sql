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
-- NO INDEX. Every read that consults this column already filters by
-- (space_id, project_id) or by primary key and has the row in hand; the column
-- is a post-filter on a row already selected, not a search key. The one query
-- that scans BY it is the stuck-in-provisioning census, which is a periodic
-- gauge, not a request path.
ALTER TABLE `octo_project`
  ADD COLUMN `activated_at` DATETIME(3) NULL
  COMMENT '两阶段创建：子系统确认容器已就绪的时间（UTC，应用侧写入）。NULL=尚未确认，对端两个入站接口一律按不存在作答。单向闩锁，置位后不再清除';

-- Backfill, deliberately before anything can read the column: rows that predate
-- the gate were confirmed by the absence of the gate.
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
ALTER TABLE `octo_project` DROP COLUMN `activated_at`;
