-- +migrate Up

-- Cross-space sweep support (task space-welcome-per-space-admin-crud).
-- The per-space idx_claim (space_id, status, next_retry_at) leads with space_id,
-- so a space-less sweep of lease-expired claimed/dispatching rows across ALL
-- Spaces would degrade to a full scan on a monotonically-growing ledger. This
-- (status, claim_expire_at) index keeps the multi-space worker's sweep
-- index-backed: WHERE status=<claimed|dispatching> AND claim_expire_at<=?.
ALTER TABLE `octo_space_welcome_delivery`
  ADD INDEX `idx_sweep` (`status`, `claim_expire_at`);

-- +migrate Down
ALTER TABLE `octo_space_welcome_delivery` DROP INDEX `idx_sweep`;
