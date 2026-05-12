-- +migrate Up
-- no-op migration — YUJ-398 Phase 2e admin endpoint was deferred (PR #1367 reverted).
-- File retained as placeholder to keep migration ledger consistent for any env that
-- already registered this version. PR #1370 revert inadvertently removed this file;
-- PR #1371 restores it so sql-migrate doesn't panic on startup.
SELECT 1;

-- +migrate Down
SELECT 1;
