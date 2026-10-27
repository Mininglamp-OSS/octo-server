-- +migrate Up
CREATE TABLE IF NOT EXISTS `workspace_capability` (
  `id`              VARCHAR(64)  NOT NULL,
  `space_id`        VARCHAR(64)  NOT NULL,
  `workspace_id`    VARCHAR(64)  NOT NULL,
  `kind`            VARCHAR(16)  NOT NULL,
  `name`            VARCHAR(255) NOT NULL,
  `description`     TEXT         NOT NULL,
  `version`         VARCHAR(64)  NOT NULL DEFAULT '',
  `source`          VARCHAR(16)  NOT NULL,
  `source_id`       VARCHAR(255) NOT NULL,
  `content`         MEDIUMTEXT   NOT NULL,
  `files_json`      MEDIUMTEXT   NOT NULL,
  `definition_json` MEDIUMTEXT   NOT NULL,
  `auth_json`       MEDIUMTEXT   NOT NULL,
  `snapshot`        VARCHAR(80)  NOT NULL,
  `created_by`      VARCHAR(64)  NOT NULL,
  `created_at`      DATETIME(3)  NOT NULL,
  `updated_at`      DATETIME(3)  NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_workspace_capability_source` (`space_id`, `workspace_id`, `kind`, `source`, `source_id`),
  KEY `idx_workspace_capability_list` (`space_id`, `workspace_id`, `kind`, `updated_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Workspace-scoped Skill and connector library';

CREATE TABLE IF NOT EXISTS `workspace_capability_binding` (
  `space_id`      VARCHAR(64) NOT NULL,
  `workspace_id`  VARCHAR(64) NOT NULL,
  `expert_id`     VARCHAR(64) NOT NULL,
  `capability_id` VARCHAR(64) NOT NULL,
  `created_at`    DATETIME(3) NOT NULL,
  PRIMARY KEY (`space_id`, `workspace_id`, `expert_id`, `capability_id`),
  KEY `idx_workspace_capability_binding_cap` (`space_id`, `workspace_id`, `capability_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Expert to workspace capability bindings';

-- +migrate Down
DROP TABLE IF EXISTS `workspace_capability_binding`;
DROP TABLE IF EXISTS `workspace_capability`;
