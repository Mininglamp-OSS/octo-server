-- +migrate Up

CREATE TABLE IF NOT EXISTS `runtime_ping` (
    `id`         varchar(64)  NOT NULL DEFAULT '' COMMENT 'Ping ID',
    `space_id`   varchar(40)  NOT NULL DEFAULT '',
    `daemon_id`  varchar(100) NOT NULL DEFAULT '',
    `server_ts`  bigint       NOT NULL DEFAULT 0 COMMENT 'Server timestamp ms',
    `daemon_ts`  bigint       NOT NULL DEFAULT 0 COMMENT 'Daemon timestamp ms',
    `rtt_ms`     bigint       NOT NULL DEFAULT 0,
    `status`     varchar(20)  NOT NULL DEFAULT 'pending' COMMENT 'pending/done/timeout',
    `created_at` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    KEY `idx_space_daemon_status` (`space_id`, `daemon_id`, `status`, `created_at`),
    KEY `idx_created` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
