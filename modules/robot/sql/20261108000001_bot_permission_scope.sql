-- +migrate Up
-- Additive source migration only. No runtime seed, backfill, cascade or reset.
CREATE TABLE bot_permission_scope (
  bot_uid VARBINARY(160) NOT NULL,
  space_id VARBINARY(160) NOT NULL,
  scope_incarnation BINARY(16) NOT NULL,
  revision BIGINT UNSIGNED NOT NULL,
  context_epoch BIGINT UNSIGNED NOT NULL,
  schema_version SMALLINT UNSIGNED NOT NULL,
  catalog_version VARBINARY(64) NOT NULL,
  catalog_digest BINARY(32) NOT NULL,
  bundle_json JSON NOT NULL,
  bundle_digest BINARY(32) NOT NULL,
  native_anchor_json JSON NOT NULL,
  native_anchor_digest BINARY(32) NOT NULL,
  retired TINYINT UNSIGNED NOT NULL DEFAULT 0,
  updated_by VARBINARY(160) NULL,
  updated_at DATETIME(6) NOT NULL,
  PRIMARY KEY (bot_uid,space_id),
  UNIQUE KEY scope_nonce (scope_incarnation)
) ENGINE=InnoDB;

-- +migrate Down
-- Intentionally preserve permanent incarnations, erased grants and fences.
-- Destructive rollback requires a separate retirement/recovery decision.
SELECT 1;
