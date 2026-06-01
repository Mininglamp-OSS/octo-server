-- +migrate Up

-- managed_runtime_agent: agents created via the runtime Web UI (PoC)
-- A "managed agent" is the pair (openclaw agent workspace, bot minted by runtime)
-- created together so that the user has a chat-ready agent in one click.
--
-- Lifecycle (poc, simplified vs AGREED_RUNTIME_PLAN.zh.md §7):
--   provisioning   -> create row, before any side effects
--   bot_minted     -> bot row + space_member + friend done in octo-server
--   dispatched     -> heartbeat picked up the row and pushed the command to daemon
--   active         -> daemon ack'd: openclaw agents add + channels.octo bind done
--   failed_*       -> see error_msg
--
-- created_by='poc' tags PoC rows so the v1.1 cutover migration to octo-runtime
-- can identify them.
CREATE TABLE IF NOT EXISTS `managed_runtime_agent` (
    `id`             bigint       NOT NULL AUTO_INCREMENT,
    `agent_id`       varchar(64)  NOT NULL DEFAULT '' COMMENT 'openclaw agent id (slug, user-supplied or derived from display_name)',
    `space_id`       varchar(40)  NOT NULL DEFAULT '',
    `owner_uid`      varchar(40)  NOT NULL DEFAULT '' COMMENT 'real user uid (not space-prefixed)',
    `runtime_id`     bigint       NOT NULL DEFAULT 0 COMMENT 'FK agent_runtime.id (openclaw runtime row this agent lives in)',
    `daemon_id`      varchar(100) NOT NULL DEFAULT '',
    `display_name`   varchar(120) NOT NULL DEFAULT '',
    `provider`       varchar(32)  NOT NULL DEFAULT 'openclaw',
    `bot_uid`        varchar(64)  NOT NULL DEFAULT '' COMMENT 'minted bot robot_id',
    `bot_token`      varchar(120) NOT NULL DEFAULT '' COMMENT 'bf_xxx token for IM auth',
    `status`         varchar(32)  NOT NULL DEFAULT 'provisioning',
    `command_kind`   varchar(32)  NOT NULL DEFAULT 'agent.create' COMMENT 'agent.create: mint bot + openclaw agents add; bot.add: mint bot + openclaw agents bind to existing agent',
    `dispatched_at`  datetime     DEFAULT NULL COMMENT 'heartbeat last pushed the command at',
    `claim_token`    varchar(64)  NOT NULL DEFAULT '' COMMENT 'guards ack idempotency vs replay',
    `error_msg`      text         NOT NULL DEFAULT (''),
    `created_by`     varchar(32)  NOT NULL DEFAULT 'poc' COMMENT 'cutover marker; PoC rows = poc',
    `created_at`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    KEY `idx_space_agent_id` (`space_id`, `agent_id`),
    KEY `idx_daemon_status` (`daemon_id`, `status`),
    KEY `idx_runtime` (`runtime_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
