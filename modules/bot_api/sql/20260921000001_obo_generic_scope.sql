-- +migrate Up
-- Generic OBO Scopes are opt-in. Existing Channel Grants receive no ALL row.
-- MySQL 8 has no ADD COLUMN IF NOT EXISTS. Guard each column so a partially
-- applied migration can be replayed safely.
SET @policy_version_exists = (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema = DATABASE() AND table_name = 'obo_grants' AND column_name = 'policy_version');
SET @policy_version_sql = IF(@policy_version_exists = 0,
  'ALTER TABLE `obo_grants` ADD COLUMN `policy_version` BIGINT NOT NULL DEFAULT 1',
  'SELECT 1');
PREPARE policy_version_stmt FROM @policy_version_sql;
EXECUTE policy_version_stmt;
DEALLOCATE PREPARE policy_version_stmt;

SET @expires_at_exists = (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema = DATABASE() AND table_name = 'obo_grants' AND column_name = 'expires_at');
SET @expires_at_sql = IF(@expires_at_exists = 0,
  'ALTER TABLE `obo_grants` ADD COLUMN `expires_at` DATETIME(6) NULL COMMENT ''UTC deadline; NULL means no expiry''',
  'SELECT 1');
PREPARE expires_at_stmt FROM @expires_at_sql;
EXECUTE expires_at_stmt;
DEALLOCATE PREPARE expires_at_stmt;

CREATE TABLE IF NOT EXISTS obo_grant_scope_bindings (
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
CREATE TABLE IF NOT EXISTS obo_policy_audits (
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
DROP TABLE IF EXISTS obo_policy_audits;
DROP TABLE IF EXISTS obo_grant_scope_bindings;
ALTER TABLE obo_grants
  DROP COLUMN IF EXISTS expires_at,
  DROP COLUMN IF EXISTS policy_version;
