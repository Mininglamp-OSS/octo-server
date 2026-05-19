-- +migrate Up
-- YUJ-1166 / Mininglamp-OSS/octo-server#81 — Persona Clone (OBO) v0.
-- Two new tables backing the On-Behalf-Of authorization layer used by
-- /v1/obo/* CRUD endpoints, the on_behalf_of branch in /v1/bot/sendMessage,
-- and the fan-out hook that copies inbound messages to a grantee bot.
--
-- See RFC §4.1 / §4.2 / §11 (Redis cache `obo:grantor:{uid}` is application
-- layer, not declared here). messages-table ALTER intentionally skipped:
-- v0 stores audit metadata in message_extra (RFC §4.3 / out-of-scope row in
-- YUJ-1166).

CREATE TABLE IF NOT EXISTS obo_grants (
  id              BIGINT AUTO_INCREMENT PRIMARY KEY,
  grantor_uid     VARCHAR(64)  NOT NULL COMMENT 'Real-user uid being represented (e.g. yu_uid).',
  grantee_bot_uid VARCHAR(64)  NOT NULL COMMENT 'Bot uid acting on behalf of grantor (e.g. yu_clone_bot).',
  mode            VARCHAR(16)  NOT NULL DEFAULT 'auto' COMMENT 'auto | draft (v0 only auto is honored).',
  global_enabled  TINYINT      NOT NULL DEFAULT 0 COMMENT 'Master switch: 0 disables ALL scopes for this grant.',
  active          TINYINT      NOT NULL DEFAULT 1 COMMENT 'Soft-delete flag (0 = revoked, kept for audit).',
  created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  revoked_at      DATETIME     DEFAULT NULL,
  UNIQUE KEY uk_grantor_grantee (grantor_uid, grantee_bot_uid),
  KEY idx_grantor (grantor_uid, active),
  KEY idx_grantee (grantee_bot_uid, active)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS obo_scopes (
  id           BIGINT AUTO_INCREMENT PRIMARY KEY,
  grant_id     BIGINT       NOT NULL,
  channel_id   VARCHAR(128) NOT NULL,
  channel_type TINYINT      NOT NULL,
  enabled      TINYINT      NOT NULL DEFAULT 1,
  created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_grant_channel (grant_id, channel_id, channel_type),
  KEY idx_channel (channel_id, channel_type, enabled),
  CONSTRAINT fk_obo_scopes_grant FOREIGN KEY (grant_id) REFERENCES obo_grants(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- +migrate Down
DROP TABLE IF EXISTS obo_scopes;
DROP TABLE IF EXISTS obo_grants;
