-- +migrate Up

CREATE TABLE IF NOT EXISTS `sticker` (
    `id` BIGINT AUTO_INCREMENT PRIMARY KEY,
    `sticker_id` VARCHAR(32) NOT NULL COMMENT '贴纸ID',
    `uid` VARCHAR(40) NOT NULL COMMENT '拥有者UID',
    `path` VARCHAR(512) NOT NULL COMMENT '文件路径（来自 /v1/file/upload?type=sticker，可能是对象存储绝对 URL）',
    `placeholder` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '占位文案（会话摘要/通知用）',
    `format` VARCHAR(16) NOT NULL DEFAULT '' COMMENT '格式：gif/png/jpg/jpeg/webp',
    `sort` INT NOT NULL DEFAULT 0 COMMENT '排序权重（预留，当前按 id 倒序）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '1=正常 2=已删除',
    `created_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    `updated_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY `uk_sticker_id` (`sticker_id`),
    INDEX `idx_uid_status` (`uid`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户自定义贴纸表（个人维度，扁平不分包）';
