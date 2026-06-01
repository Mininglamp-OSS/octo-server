-- +migrate Up

-- bot_task: external services (octo-matter) hand a "make this bot do something"
-- task to the runtime. The server resolves bot_uid → openclaw agent_id
-- (via managed_runtime_agent), queues the row, and daemon pulls it through
-- the heartbeat pull pattern shared with managed_runtime_agent.
--
-- Lifecycle (PoC):
--   queued      -> row inserted; agent_id+daemon_id resolved
--   dispatched  -> heartbeat picked it up, gave daemon a claim_token
--   succeeded   -> daemon ack'd success with result_summary
--   failed      -> daemon ack'd failure with error_msg, OR resolve failed at insert
--
-- created_by='poc' so the v1.1 cutover to a dedicated octo-runtime service
-- can detect PoC rows.
CREATE TABLE IF NOT EXISTS `bot_task` (
    `id`                bigint       NOT NULL AUTO_INCREMENT,
    `matter_id`         varchar(64)  NOT NULL DEFAULT '' COMMENT 'octo-matter matter uuid',
    `matter_base_url`   varchar(255) NOT NULL DEFAULT '' COMMENT 'callback base url to write timeline back',
    `space_id`          varchar(40)  NOT NULL DEFAULT '',
    `bot_uid`           varchar(64)  NOT NULL DEFAULT '' COMMENT 'assignee bot uid that triggered this task',
    `agent_id`          varchar(64)  NOT NULL DEFAULT '' COMMENT 'resolved openclaw agent id this bot is bound to',
    `daemon_id`         varchar(100) NOT NULL DEFAULT '' COMMENT 'resolved daemon hosting that agent',
    `runtime_id`        bigint       NOT NULL DEFAULT 0  COMMENT 'resolved openclaw runtime row id',
    `requester_uid`     varchar(64)  NOT NULL DEFAULT '' COMMENT 'user who created the matter / added the bot assignee',
    `title`             varchar(500) NOT NULL DEFAULT '',
    `description`       text         NOT NULL DEFAULT (''),
    `prompt`            text         NOT NULL DEFAULT ('') COMMENT 'final composed prompt passed to openclaw agent',
    `status`            varchar(32)  NOT NULL DEFAULT 'queued' COMMENT 'queued | dispatched | succeeded | failed',
    `claim_token`       varchar(64)  NOT NULL DEFAULT '' COMMENT 'guards ack idempotency vs replay',
    `result_summary`    text         NOT NULL DEFAULT (''),
    `error_msg`         text         NOT NULL DEFAULT (''),
    `created_by`        varchar(32)  NOT NULL DEFAULT 'poc',
    `created_at`        datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`        datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    KEY `idx_daemon_status` (`daemon_id`, `status`),
    KEY `idx_matter` (`matter_id`),
    KEY `idx_bot` (`bot_uid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
