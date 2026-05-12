-- +migrate Up
CREATE TABLE `thread` (
    `id` BIGINT AUTO_INCREMENT PRIMARY KEY COMMENT '主键ID',
    `short_id` VARCHAR(32) NOT NULL COMMENT '子区独立ID (snowflake)',
    `group_no` VARCHAR(40) NOT NULL COMMENT '父群编号',
    `name` VARCHAR(100) NOT NULL COMMENT '子区名称',
    `creator_uid` VARCHAR(40) NOT NULL COMMENT '创建者UID',
    `source_message_id` BIGINT DEFAULT NULL COMMENT '来源消息ID (可选)',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '状态: 1=活跃, 2=已归档, 3=已删除',
    `version` BIGINT NOT NULL DEFAULT 0 COMMENT '版本号',
    `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    UNIQUE KEY `uk_short_id` (`short_id`),
    UNIQUE KEY `uk_group_short` (`group_no`, `short_id`),
    INDEX `idx_group_no` (`group_no`),
    INDEX `idx_creator` (`creator_uid`),
    INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='群组子区表';

-- +migrate Down
DROP TABLE IF EXISTS `thread`;
