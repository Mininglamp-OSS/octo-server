-- +migrate Up

CREATE TABLE IF NOT EXISTS octo_resource_share_intent (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  nonce_hash BINARY(32) NOT NULL COMMENT 'SHA-256 of the provider nonce',
  fingerprint BINARY(32) NOT NULL COMMENT 'JCS intent fingerprint',
  idempotency_hash BINARY(32) NOT NULL COMMENT 'Hashed intent metadata; delivery dedup uses delivery_id',
  actor_uid VARCHAR(128) NOT NULL,
  space_id VARCHAR(128) NOT NULL,
  provider_id VARCHAR(64) NOT NULL,
  resource_type VARCHAR(64) NOT NULL,
  resource_id VARCHAR(256) NOT NULL,
  resource_revision VARCHAR(128) NOT NULL,
  expires_at BIGINT NOT NULL COMMENT 'intent expiry as unix seconds',
  created_at BIGINT NOT NULL COMMENT 'claim time as unix seconds',
  PRIMARY KEY (id),
  UNIQUE KEY uk_resource_share_nonce (nonce_hash),
  KEY idx_resource_share_intent_actor (actor_uid, created_at),
  KEY idx_resource_share_intent_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Durable resource-share intent claims';

CREATE TABLE IF NOT EXISTS octo_resource_share_delivery (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  intent_id BIGINT UNSIGNED NOT NULL,
  delivery_id CHAR(64) NOT NULL COMMENT 'per-resource-revision target identity',
  target_kind VARCHAR(16) NOT NULL,
  target_ref VARCHAR(512) NOT NULL COMMENT 'bounded canonical target, never a card payload',
  state VARCHAR(24) NOT NULL,
  retry_at BIGINT NOT NULL DEFAULT 0,
  message_id VARCHAR(32) NOT NULL DEFAULT '',
  message_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
  client_msg_no VARCHAR(128) NOT NULL DEFAULT '',
  outcome_code VARCHAR(64) NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_resource_share_delivery (delivery_id),
  KEY idx_resource_share_delivery_intent (intent_id),
  KEY idx_resource_share_delivery_state_retry (state, retry_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Per-target resource-share delivery state';

CREATE TABLE IF NOT EXISTS octo_resource_share_audit (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  intent_id BIGINT UNSIGNED NOT NULL,
  delivery_id CHAR(64) NOT NULL,
  actor_uid VARCHAR(128) NOT NULL,
  space_id VARCHAR(128) NOT NULL,
  provider_id VARCHAR(64) NOT NULL,
  resource_type VARCHAR(64) NOT NULL,
  resource_id VARCHAR(256) NOT NULL,
  resource_revision VARCHAR(128) NOT NULL,
  target_kind VARCHAR(16) NOT NULL,
  target_ref VARCHAR(512) NOT NULL,
  request_id VARCHAR(128) NOT NULL DEFAULT '',
  outcome VARCHAR(24) NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (id),
  KEY idx_resource_share_audit_delivery (delivery_id, id),
  KEY idx_resource_share_audit_actor (actor_uid, created_at),
  KEY idx_resource_share_audit_provider (provider_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Bounded resource-share security audit';

-- +migrate Down

DROP TABLE IF EXISTS octo_resource_share_audit;
DROP TABLE IF EXISTS octo_resource_share_delivery;
DROP TABLE IF EXISTS octo_resource_share_intent;
