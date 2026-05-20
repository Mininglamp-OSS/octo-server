-- +migrate Up

CREATE TABLE IF NOT EXISTS `runtime_latest_version` (
    `id`              bigint       NOT NULL AUTO_INCREMENT,
    `component`       varchar(50)  NOT NULL DEFAULT '' COMMENT 'Component name: octo-daemon, claude, codex, openclaw, hermes, octo',
    `latest_version`  varchar(50)  NOT NULL DEFAULT '',
    `updated_at`      datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_component` (`component`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
