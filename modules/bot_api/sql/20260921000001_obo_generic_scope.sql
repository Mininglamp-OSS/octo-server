-- +migrate Up
-- Generic OBO Scopes are opt-in. Existing Channel Grants receive no ALL row.
ALTER TABLE obo_grants
  ADD COLUMN policy_version BIGINT NOT NULL DEFAULT 1,
  ADD COLUMN expires_at DATETIME(6) NULL COMMENT 'UTC deadline; NULL means no expiry';

CREATE TABLE obo_grant_scope_bindings (
  grant_id BIGINT NOT NULL,
  scope_code VARCHAR(32) NOT NULL,
  assigned_by VARCHAR(64) NOT NULL,
  assigned_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (grant_id, scope_code),
  CONSTRAINT fk_obo_scope_binding_grant FOREIGN KEY (grant_id)
    REFERENCES obo_grants(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Management audit is committed with the policy mutation, so delivery failure
-- cannot erase the fact that an authorization change occurred.
CREATE TABLE obo_policy_audits (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  grant_id BIGINT NOT NULL,
  actor_uid VARCHAR(64) NOT NULL,
  operation VARCHAR(32) NOT NULL,
  previous_json TEXT NULL,
  current_json TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  KEY idx_obo_policy_audits_grant (grant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- +migrate Down
DROP TABLE obo_policy_audits;
DROP TABLE obo_grant_scope_bindings;
ALTER TABLE obo_grants
  DROP COLUMN expires_at,
  DROP COLUMN policy_version;
