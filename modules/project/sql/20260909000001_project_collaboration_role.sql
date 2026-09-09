-- +migrate Up

ALTER TABLE `octo_project`
  ADD COLUMN `collaboration_role_epoch` BIGINT NOT NULL DEFAULT 0
    COMMENT '协作角色目录或成员绑定每次真实变更 +1；与 member_epoch 权限/席位纪元分离'
    AFTER `member_epoch`;

CREATE TABLE `octo_project_collaboration_role` (
  `role_id`         VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '协作角色稳定 ID',
  `project_id`      VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '所属项目',
  `builtin_key`     VARCHAR(40)  NULL                 COMMENT '内置角色稳定键；自定义角色为 NULL',
  `name`            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '展示名称',
  `normalized_name` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin
                                  NOT NULL DEFAULT '' COMMENT 'trim+小写后的项目内唯一名称；保留重音差异',
  `source`          VARCHAR(16)  NOT NULL DEFAULT '' COMMENT 'builtin/custom；不承载权限',
  `creator_uid`     VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '自定义角色创建者；内置为空',
  `created_at`      DATETIME(3)  NOT NULL,
  `updated_at`      DATETIME(3)  NOT NULL,
  PRIMARY KEY (`role_id`),
  UNIQUE KEY `uk_octo_project_collab_role_name` (`project_id`, `normalized_name`),
  UNIQUE KEY `uk_octo_project_collab_role_builtin` (`project_id`, `builtin_key`),
  KEY `idx_octo_project_collab_role_project` (`project_id`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE `octo_project_member_collaboration_role` (
  `project_id` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '项目 ID',
  `uid`        VARCHAR(40) NOT NULL DEFAULT '' COMMENT '真人项目成员 uid',
  `role_id`    VARCHAR(40) NOT NULL DEFAULT '' COMMENT '协作角色 ID',
  `created_at` DATETIME(3) NOT NULL,
  PRIMARY KEY (`project_id`, `uid`, `role_id`),
  KEY `idx_octo_project_member_collab_role_role` (`project_id`, `role_id`, `uid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- 存量项目不在 DDL 事务中做无界 INSERT ... SELECT。应用启动后的有界维护任务
-- 每轮最多处理 OCTO_PROJECT_RECONCILE_LIMIT 个项目，并依靠
-- (project_id, builtin_key) 唯一键保证多实例并发和重试幂等。

-- +migrate Down

DROP TABLE IF EXISTS `octo_project_member_collaboration_role`;
DROP TABLE IF EXISTS `octo_project_collaboration_role`;
ALTER TABLE `octo_project` DROP COLUMN `collaboration_role_epoch`;
