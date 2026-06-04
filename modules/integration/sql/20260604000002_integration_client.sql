-- +migrate Up
-- OIDC integration clients. PR2 option A treats this as a registration table
-- plus global exchange switch; client_secret_hash is reserved for the future
-- actor-authenticated option B.
CREATE TABLE IF NOT EXISTS `integration_client` (
  `id` bigint NOT NULL AUTO_INCREMENT,
  `client_id` varchar(100) NOT NULL DEFAULT '' COMMENT '外部应用 ID / 审计标签',
  `name` varchar(100) NOT NULL DEFAULT '' COMMENT '展示名称',
  `client_secret_hash` varchar(128) NULL DEFAULT NULL COMMENT '预留：client credentials secret hash',
  `status` tinyint NOT NULL DEFAULT 0 COMMENT '1=enabled 0=disabled',
  `created_at` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_integration_client_id` (`client_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='OIDC integration client registry';

-- +migrate Down
DROP TABLE IF EXISTS `integration_client`;
