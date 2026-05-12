-- +migrate Up
CREATE TABLE IF NOT EXISTS `space_email_invite` (
  `id`                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `token_hash`          VARCHAR(64)     NOT NULL COMMENT 'SHA-256 十六进制',
  `invite_type`         TINYINT         NOT NULL COMMENT '1=owner 2=member',
  `email`               VARCHAR(200)    NOT NULL COMMENT '收件邮箱',
  `space_id`            VARCHAR(40)     NOT NULL DEFAULT '' COMMENT 'member 类型关联的空间ID',
  `role`                TINYINT         NOT NULL DEFAULT 0 COMMENT 'member 类型角色 0=成员 1=管理员',
  `planned_name`        VARCHAR(100)    NOT NULL DEFAULT '' COMMENT 'owner 类型计划空间名',
  `planned_description` VARCHAR(500)    NOT NULL DEFAULT '',
  `planned_logo`        VARCHAR(200)    NOT NULL DEFAULT '',
  `planned_max_users`   INT             NOT NULL DEFAULT 0,
  `planned_join_mode`   TINYINT         NOT NULL DEFAULT 0,
  `status`              TINYINT         NOT NULL DEFAULT 0 COMMENT '0=pending 1=consumed 2=expired 3=revoked',
  `expires_at`          TIMESTAMP       NULL     DEFAULT NULL,
  `created_by`          VARCHAR(40)     NOT NULL COMMENT '发起人UID',
  `consumed_by`         VARCHAR(40)     NOT NULL DEFAULT '' COMMENT '接受人UID',
  `consumed_at`         TIMESTAMP       NULL     DEFAULT NULL,
  `created_at`          TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`          TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_token_hash` (`token_hash`),
  KEY `idx_email_status` (`email`, `status`),
  KEY `idx_space_status` (`space_id`, `status`),
  KEY `idx_created_by` (`created_by`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='空间邮件邀请（owner/member）';

-- +migrate Down
DROP TABLE IF EXISTS `space_email_invite`;
