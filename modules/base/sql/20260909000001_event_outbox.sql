-- +migrate Up
CREATE TABLE IF NOT EXISTS `event_outbox` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `event_id` CHAR(36) NOT NULL,
  `domain` VARCHAR(64) NOT NULL,
  `resource_id` VARCHAR(64) NOT NULL,
  `event_type` VARCHAR(128) NOT NULL,
  `target_service` VARCHAR(64) NOT NULL,
  `payload` MEDIUMTEXT NOT NULL,
  `status` TINYINT UNSIGNED NOT NULL DEFAULT 0,
  `attempts` INT UNSIGNED NOT NULL DEFAULT 0,
  `next_attempt_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `last_error` VARCHAR(255) NULL,
  `lease_owner` VARCHAR(128) NULL,
  `lease_until` DATETIME(3) NULL,
  `created_at` DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `delivered_at` DATETIME(3) NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_event_outbox_event_target` (`event_id`, `target_service`),
  KEY `idx_event_outbox_pending` (`status`, `next_attempt_at`, `lease_until`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- +migrate Down
DROP TABLE IF EXISTS `event_outbox`;
