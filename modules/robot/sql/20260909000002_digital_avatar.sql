-- +migrate Up
ALTER TABLE robot
  ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT 'user',
  ADD COLUMN management_scope VARCHAR(16) NOT NULL DEFAULT '',
  ADD COLUMN management_space_id VARCHAR(40) NOT NULL DEFAULT '',
  ADD COLUMN created_by VARCHAR(40) NOT NULL DEFAULT '',
  ADD COLUMN publication_state VARCHAR(16) NOT NULL DEFAULT 'draft',
  ADD COLUMN lifecycle_pending TINYINT NOT NULL DEFAULT 0,
  ADD KEY idx_robot_avatar_scope (kind, management_scope, management_space_id, status),
  ADD CONSTRAINT chk_robot_avatar_shape CHECK (
    kind = 'user' OR (kind = 'avatar' AND creator_uid = '' AND auto_approve = 1
      AND ((management_scope = 'platform' AND management_space_id = '')
        OR (management_scope = 'space' AND management_space_id <> ''))
      AND publication_state IN ('draft', 'published', 'unpublished', 'deleted')
      AND ((publication_state = 'published' AND status = 1)
        OR (publication_state <> 'published' AND status = 0)))
  );

-- Serializes platform publication and new-Space enrollment. Both transactions
-- take this row before Space/robot writes, so neither can miss the other's row.
CREATE TABLE avatar_enrollment_lock (
  id TINYINT NOT NULL PRIMARY KEY
) ENGINE=InnoDB;
INSERT INTO avatar_enrollment_lock (id) VALUES (1);

-- Authorization is revoked in the local transaction before best-effort IM and
-- cache cleanup. A durable job makes those external side effects retryable.
CREATE TABLE avatar_cleanup_job (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  robot_id VARCHAR(40) NOT NULL,
  operator_uid VARCHAR(40) NOT NULL DEFAULT '',
  operation VARCHAR(16) NOT NULL,
  state VARCHAR(16) NOT NULL DEFAULT 'pending',
  attempts INT NOT NULL DEFAULT 0,
  last_error VARCHAR(500) NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  KEY idx_avatar_cleanup_pending (state, updated_at),
  KEY idx_avatar_cleanup_robot (robot_id, state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- +migrate Down
DROP TABLE avatar_cleanup_job;
DROP TABLE avatar_enrollment_lock;
ALTER TABLE robot
  DROP CHECK chk_robot_avatar_shape,
  DROP KEY idx_robot_avatar_scope,
  DROP COLUMN lifecycle_pending,
  DROP COLUMN publication_state,
  DROP COLUMN created_by,
  DROP COLUMN management_space_id,
  DROP COLUMN management_scope,
  DROP COLUMN kind;
