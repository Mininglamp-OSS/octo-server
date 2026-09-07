-- +migrate Up

CREATE TABLE `ai_team_agent` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `space_id` VARCHAR(40) NOT NULL,
  `user_uid` VARCHAR(40) NOT NULL,
  `bot_id` VARCHAR(40) NOT NULL,
  `group_no` VARCHAR(40) NULL,
  `is_added` TINYINT NOT NULL DEFAULT 1,
  `container_state` TINYINT NOT NULL DEFAULT 0 COMMENT '0=未分配 1=配置中 2=就绪 3=失败',
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_ai_team_agent_owner` (`space_id`, `user_uid`, `bot_id`),
  UNIQUE KEY `uk_ai_team_agent_group` (`group_no`),
  KEY `idx_ai_team_agent_list` (`space_id`, `user_uid`, `is_added`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE `ai_team_session` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `agent_id` BIGINT UNSIGNED NOT NULL,
  `short_id` VARCHAR(32) NOT NULL,
  `idempotency_key` VARCHAR(128) NOT NULL,
  `request_hash` CHAR(64) NOT NULL,
  `state` TINYINT NOT NULL DEFAULT 1 COMMENT '1=配置中 2=就绪 3=失败',
  `manual_title` TINYINT NOT NULL DEFAULT 0,
  `last_error` VARCHAR(255) NOT NULL DEFAULT '',
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_ai_team_session_idem` (`agent_id`, `idempotency_key`),
  UNIQUE KEY `uk_ai_team_session_short` (`short_id`),
  KEY `idx_ai_team_session_agent` (`agent_id`, `state`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- The AI-session rollout requires global thread auto-archive to be off. A DB
-- row is authoritative over any stale deployment environment variable.
INSERT INTO `system_setting` (`category`, `key_name`, `value`, `value_type`, `description`)
VALUES ('thread', 'auto_archive_enabled', '0', 'bool', '是否开启子区不活跃自动归档')
ON DUPLICATE KEY UPDATE `value`='0', `value_type`='bool';

-- +migrate Down
DROP TABLE IF EXISTS `ai_team_session`;
DROP TABLE IF EXISTS `ai_team_agent`;
