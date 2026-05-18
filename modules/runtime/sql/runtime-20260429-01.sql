-- +migrate Up

CREATE TABLE IF NOT EXISTS `runtime_upgrade_task` (
    `id` VARCHAR(64) PRIMARY KEY,
    `space_id` VARCHAR(40) NOT NULL,
    `daemon_id` VARCHAR(100) NOT NULL,
    `owner_uid` VARCHAR(40) NOT NULL,
    `component` VARCHAR(50) NOT NULL DEFAULT 'octo-daemon',
    `from_version` VARCHAR(50) NOT NULL,
    `to_version` VARCHAR(50) NOT NULL,
    `download_url` VARCHAR(500) NOT NULL,
    `checksum` VARCHAR(128) NOT NULL DEFAULT '',
    `metadata` TEXT,
    `status` VARCHAR(20) NOT NULL DEFAULT 'pending',
    `error_msg` TEXT,
    `created_at` DATETIME DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    INDEX `idx_daemon_component_status` (`daemon_id`, `component`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

ALTER TABLE `runtime_latest_version` ADD COLUMN `release_meta` TEXT AFTER `latest_version`;
