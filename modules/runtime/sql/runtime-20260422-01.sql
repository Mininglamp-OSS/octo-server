-- +migrate Up

CREATE TABLE IF NOT EXISTS `agent_runtime` (
    `id`           bigint       NOT NULL AUTO_INCREMENT,
    `space_id`     varchar(40)  NOT NULL DEFAULT '' COMMENT 'Space ID',
    `daemon_id`    varchar(100) NOT NULL DEFAULT '' COMMENT 'Daemon unique ID (UUID per machine)',
    `name`         varchar(100) NOT NULL DEFAULT '' COMMENT 'Display name',
    `provider`     varchar(50)  NOT NULL DEFAULT '' COMMENT 'Agent provider: claude, codex, openclaw, hermes',
    `runtime_mode` varchar(20)  NOT NULL DEFAULT 'local' COMMENT 'local or cloud',
    `status`       varchar(20)  NOT NULL DEFAULT 'offline' COMMENT 'online or offline',
    `version`      varchar(50)  NOT NULL DEFAULT '' COMMENT 'Agent CLI version',
    `device_name`  varchar(200) NOT NULL DEFAULT '' COMMENT 'Machine hostname',
    `device_info`  text         COMMENT 'Device metadata JSON',
    `metadata`     text         COMMENT 'Additional metadata JSON',
    `owner_uid`    varchar(40)  NOT NULL DEFAULT '' COMMENT 'Robot UID that registered this runtime',
    `last_seen_at` datetime     DEFAULT NULL COMMENT 'Last heartbeat timestamp',
    `created_at`   datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`   datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_space_daemon_provider` (`space_id`, `daemon_id`, `provider`),
    KEY `idx_space_status` (`space_id`, `status`),
    KEY `idx_last_seen` (`last_seen_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
