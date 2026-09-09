-- +migrate Up

CREATE TABLE `ai_team_group` (
  `space_id` VARCHAR(40) NOT NULL,
  `user_uid` VARCHAR(40) NOT NULL,
  `group_no` VARCHAR(40) NULL,
  `state` TINYINT NOT NULL DEFAULT 0 COMMENT '0=未分配 1=配置中 2=就绪 3=失败',
  `last_error` VARCHAR(255) NOT NULL DEFAULT '',
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`space_id`, `user_uid`),
  UNIQUE KEY `uk_ai_team_group_no` (`group_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- +migrate Down

DROP TABLE IF EXISTS `ai_team_group`;
