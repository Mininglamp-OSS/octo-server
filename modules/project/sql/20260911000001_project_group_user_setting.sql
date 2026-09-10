-- +migrate Up

-- A Project member's group pin is personal to one Space, Project, user, and
-- currently associated group.  Keep the scope columns on the preference row so
-- an old preference cannot leak into another Space or Project after rebinding.
CREATE TABLE `octo_project_group_user_setting` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `space_id`   VARCHAR(40)     NOT NULL DEFAULT '' COMMENT '所属 Space ID',
  `project_id` VARCHAR(40)     NOT NULL DEFAULT '' COMMENT '所属 Project ID',
  `group_no`   VARCHAR(40)     NOT NULL DEFAULT '' COMMENT '关联群编号',
  `uid`        VARCHAR(40)     NOT NULL DEFAULT '' COMMENT '个人偏好所属用户',
  `pinned`     TINYINT        NOT NULL DEFAULT 0 COMMENT '0=未置顶 1=已置顶',
  `pinned_at`  DATETIME(6)     NULL                 COMMENT 'UTC；应用侧写入，取消置顶时置 NULL',
  `created_at` DATETIME(6)     NOT NULL             COMMENT 'UTC；应用侧写入，禁 CURRENT_TIMESTAMP',
  `updated_at` DATETIME(6)     NOT NULL             COMMENT 'UTC；应用侧写入，禁 ON UPDATE',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_octo_project_group_user_setting` (`space_id`, `project_id`, `group_no`, `uid`),
  KEY `idx_octo_project_group_user_setting_project_uid` (`space_id`, `project_id`, `uid`, `pinned`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- +migrate Down
DROP TABLE IF EXISTS `octo_project_group_user_setting`;
