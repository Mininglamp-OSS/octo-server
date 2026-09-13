-- +migrate Up

-- lifecycle_version — the ordering token for project LIFECYCLE facts.
--
-- Separate from member_epoch, and the separation is the point. member_epoch
-- answers "has the roster changed"; a consumer caches an authorization decision
-- under it and re-checks it to decide whether that decision is still good.
-- lifecycle_version answers "how new is this statement about the project itself"
-- -- its name, its description, whether it is active -- so a consumer receiving
-- these facts out of order can discard the stale one instead of letting an old
-- message resurrect a superseded state.
--
-- Sharing one counter for both would make every rename invalidate every cached
-- authorization decision in every consumer, and every membership change look
-- like a lifecycle statement. They move at different rates for different
-- reasons.
--
-- WRITE DISCIPLINE, identical to member_epoch and enforced the same way: the
-- only statement in this package that writes this column is
-- lifecycle_version = lifecycle_version + 1, which is what makes monotonicity
-- checkable by grep rather than by hoping. A source guard test greps the package
-- for any other shape. Absolute assignment is what lets two writers race a value
-- backwards, and a version that can go backwards is worse than no version at
-- all: a consumer would discard the NEW state as stale.
--
-- STARTING VALUE. New projects reach 1, not 0, because the creation event is
-- itself a lifecycle statement and a consumer has to be able to order it against
-- what follows. Reached by a BUMP right after the insert, not by seeding the
-- column in the insert list -- seeding is an absolute write, which is the one
-- shape the discipline above forbids, and PR #852 already had to redo
-- member_epoch for exactly that reason.
--
-- Rows that predate this migration get the DDL default 0, which is
-- correct for them: they were created before any of this existed and cannot
-- participate in the exchange anyway -- they also carry the older 32-character
-- project_id form, which the peer system cannot match. Nothing needs backfill.
--
-- No index. Nothing filters or sorts by this column; it is read as part of the
-- row a consumer is already fetching by primary key or by project_id.
ALTER TABLE `octo_project`
  ADD COLUMN `lifecycle_version` BIGINT NOT NULL DEFAULT 0
  COMMENT '生命周期版本：创建/改资料/解散等生命周期写入在同一事务内 +1，只允许 +1；与 member_epoch 分离，后者只跟踪成员变化';

-- +migrate Down
--
-- Dropping this column loses the ordering token. A consumer that has already
-- seen version N would then receive version 0 for every subsequent statement and
-- discard all of them as stale, so a rollback has to be paired with disabling
-- the lifecycle event feed rather than done on its own.
--
-- No apostrophes anywhere in this file, in ANY comment, on purpose. The
-- migration test in this module splits statements naively and treats a quote as
-- a string delimiter, so one possessive apostrophe makes it read the next
-- semicolon as being inside a literal. Note the failure PAIRS UP: an even number
-- of them can pass while an odd number fails. Do not add one back and rely on
-- counting.
ALTER TABLE `octo_project` DROP COLUMN `lifecycle_version`;
