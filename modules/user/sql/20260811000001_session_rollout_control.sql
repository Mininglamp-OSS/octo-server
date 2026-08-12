-- +migrate Up

-- Compatibility tombstone for PR #733 development and pre-release databases
-- that already recorded this migration ID. The authoritative control tables
-- are created by 20260812000001_session_rollout_control.sql. Never remove or
-- repurpose this ID after it has entered a sql-migrate ledger.
SELECT 1;

-- +migrate Down

SELECT 1;
