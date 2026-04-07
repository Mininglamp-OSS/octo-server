-- +migrate Up
-- 子区成员表
CREATE TABLE IF NOT EXISTS `thread_member` (
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `thread_id` BIGINT UNSIGNED NOT NULL COMMENT '子区ID',
    `uid` VARCHAR(40) NOT NULL COMMENT '用户UID',
    `role` TINYINT NOT NULL DEFAULT 0 COMMENT '角色: 0=普通成员, 1=创建者',
    `version` BIGINT NOT NULL DEFAULT 0 COMMENT '版本号',
    `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_thread_uid` (`thread_id`, `uid`),
    KEY `idx_uid` (`uid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='子区成员表';
