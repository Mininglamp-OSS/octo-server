-- +migrate Up
-- Per-user monotonic event source for durable HTTP-session revocation.
CREATE TABLE IF NOT EXISTS `user_session_revocation_version` (
  `uid` VARCHAR(64) NOT NULL,
  `version` BIGINT UNSIGNED NOT NULL DEFAULT 0,
  `updated_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`uid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Transactional outbox. It intentionally contains no bearer, Redis key,
-- password, generation, or session-index member.
CREATE TABLE IF NOT EXISTS `user_session_revocation_intent` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `event_id` CHAR(36) NOT NULL,
  `uid` VARCHAR(64) NOT NULL,
  `event_version` BIGINT UNSIGNED NOT NULL,
  `reason` VARCHAR(32) NOT NULL,
  `status` TINYINT UNSIGNED NOT NULL DEFAULT 0,
  `attempts` INT UNSIGNED NOT NULL DEFAULT 0,
  `next_attempt_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `lease_owner` VARCHAR(64) NOT NULL DEFAULT '',
  `lease_until` DATETIME(3) NULL,
  `last_error_code` VARCHAR(64) NOT NULL DEFAULT '',
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `applied_at` DATETIME(3) NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_session_revocation_event_id` (`event_id`),
  UNIQUE KEY `uk_user_session_revocation_uid_version` (`uid`, `event_version`),
  KEY `idx_user_session_revocation_pending` (`status`, `next_attempt_at`, `lease_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- +migrate Down
DROP TABLE IF EXISTS `user_session_revocation_intent`;
DROP TABLE IF EXISTS `user_session_revocation_version`;
