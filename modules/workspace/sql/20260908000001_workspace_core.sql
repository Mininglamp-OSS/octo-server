-- +migrate Up
-- Workspace owns metadata and the single owner projection. Member rows retain
-- history by flipping status; owner_uid is the only owner authority and the
-- member role column intentionally stores only admin/member.
CREATE TABLE IF NOT EXISTS `octo_workspace` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `workspace_id` VARCHAR(40)     NOT NULL DEFAULT '',
  `space_id`     VARCHAR(40)     NOT NULL DEFAULT '',
  `name`         VARCHAR(100)    NOT NULL DEFAULT '',
  `description`  VARCHAR(500)    NOT NULL DEFAULT '',
  `logo`         TEXT           NOT NULL,
  `owner_uid`    VARCHAR(40)     NOT NULL DEFAULT '',
  `status`       TINYINT        NOT NULL DEFAULT 1,
  `created_at`   DATETIME(3)    NOT NULL,
  `updated_at`   DATETIME(3)    NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_octo_workspace_workspace_id` (`workspace_id`),
  KEY `idx_octo_workspace_space_status` (`space_id`, `status`),
  KEY `idx_octo_workspace_owner_status` (`owner_uid`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='协作 Workspace 元数据';

CREATE TABLE IF NOT EXISTS `octo_workspace_member` (
  `workspace_id` VARCHAR(40) NOT NULL DEFAULT '',
  `uid`         VARCHAR(40) NOT NULL DEFAULT '',
  `role`        TINYINT     NOT NULL DEFAULT 0 COMMENT '0=member 1=admin; owner由workspace.owner_uid投影',
  `status`      TINYINT     NOT NULL DEFAULT 1 COMMENT '1=active 0=inactive',
  `granted_by`  VARCHAR(40) NOT NULL DEFAULT '',
  `created_at`  DATETIME(3) NOT NULL,
  `updated_at`  DATETIME(3) NOT NULL,
  PRIMARY KEY (`workspace_id`, `uid`),
  KEY `idx_octo_workspace_member_uid_status` (`uid`, `status`),
  KEY `idx_octo_workspace_member_workspace_status` (`workspace_id`, `status`, `role`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Workspace 成员关系';

-- +migrate Down
DROP TABLE IF EXISTS `octo_workspace_member`;
DROP TABLE IF EXISTS `octo_workspace`;
