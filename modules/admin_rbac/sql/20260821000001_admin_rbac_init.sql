-- +migrate Up

CREATE TABLE `admin_rbac_role` (
  `id` BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `role_key` VARCHAR(64) NOT NULL,
  `name` VARCHAR(128) NOT NULL DEFAULT '',
  `description` VARCHAR(255) NOT NULL DEFAULT '',
  `status` TINYINT NOT NULL DEFAULT 1,
  `authorization_version` BIGINT NOT NULL DEFAULT 1,
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_admin_rbac_role_key` (`role_key`),
  KEY `idx_admin_rbac_role_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='octo-admin 全局 RBAC 角色';

CREATE TABLE `admin_rbac_user_role` (
  `id` BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `uid` VARCHAR(40) NOT NULL,
  `role_id` BIGINT NOT NULL,
  `status` TINYINT NOT NULL DEFAULT 1,
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_admin_rbac_user_role` (`uid`, `role_id`),
  KEY `idx_admin_rbac_user_role_uid_status` (`uid`, `status`),
  KEY `idx_admin_rbac_user_role_role_status` (`role_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='octo-admin 用户角色绑定';

CREATE TABLE `admin_rbac_role_permission` (
  `id` BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  `role_id` BIGINT NOT NULL,
  `permission_key` VARCHAR(128) NOT NULL,
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_admin_rbac_role_permission` (`role_id`, `permission_key`),
  KEY `idx_admin_rbac_role_permission_role` (`role_id`),
  KEY `idx_admin_rbac_role_permission_key` (`permission_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='octo-admin 角色权限绑定';

-- +migrate Down

DROP TABLE IF EXISTS `admin_rbac_role_permission`;
DROP TABLE IF EXISTS `admin_rbac_user_role`;
DROP TABLE IF EXISTS `admin_rbac_role`;
