-- +migrate Up

-- PoC4: collapse agent + bot into a single first-class `bot` table.
-- PoC1's `managed_runtime_agent` is dropped — all rows were
-- `created_by='poc'` test data, accepted loss documented in spec.
--
-- Lifecycle (PoC4):
--   provisioning   -> row inserted, before daemon does anything
--   bot_minted     -> server minted bot identity (status set by handler)
--   dispatched     -> heartbeat handed bot.provision to daemon
--   active         -> daemon ack'd: openclaw workspace + bind done
--                     (or for non-openclaw: set straight after mint)
--   failed         -> openclaw side-effect failed; see error_msg
--   archived       -> soft-delete

DROP TABLE IF EXISTS `managed_runtime_agent`;

CREATE TABLE IF NOT EXISTS `bot` (
    `id`             bigint       NOT NULL AUTO_INCREMENT,
    `space_id`       varchar(40)  NOT NULL DEFAULT '',
    `owner_uid`      varchar(40)  NOT NULL DEFAULT '' COMMENT 'real user uid',
    `runtime_id`     bigint       NOT NULL DEFAULT 0  COMMENT 'FK agent_runtime.id',
    `runtime_kind`   varchar(32)  NOT NULL DEFAULT '' COMMENT 'openclaw|claude|codex|hermes',
    `daemon_id`      varchar(100) NOT NULL DEFAULT '',
    `name`           varchar(120) NOT NULL DEFAULT '' COMMENT 'user-chosen display name',
    `bot_uid`        varchar(64)  NOT NULL DEFAULT '' COMMENT 'minted bot uid',
    `bot_token`      varchar(120) NOT NULL DEFAULT '' COMMENT 'bf_xxx',
    `workspace_id`   varchar(64)  NOT NULL DEFAULT '' COMMENT 'openclaw agent slug; empty for non-openclaw',
    `status`         varchar(32)  NOT NULL DEFAULT 'provisioning',
    `claim_token`    varchar(64)  NOT NULL DEFAULT '',
    `error_msg`      text         NOT NULL DEFAULT (''),
    `created_by`     varchar(32)  NOT NULL DEFAULT 'poc',
    `created_at`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    KEY `idx_space_owner` (`space_id`, `owner_uid`),
    KEY `idx_runtime_status` (`runtime_id`, `status`),
    KEY `idx_daemon_status` (`daemon_id`, `status`),
    KEY `idx_bot_uid` (`bot_uid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
