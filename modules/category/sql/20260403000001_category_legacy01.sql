-- +migrate Up

CREATE TABLE `group_category` (
    `id` BIGINT AUTO_INCREMENT PRIMARY KEY,
    `category_id` VARCHAR(32) NOT NULL COMMENT '类别ID',
    `space_id` VARCHAR(40) NOT NULL COMMENT '所属空间',
    `uid` VARCHAR(40) NOT NULL COMMENT '拥有者',
    `name` VARCHAR(100) NOT NULL COMMENT '类别名称',
    `sort` INT NOT NULL DEFAULT 0 COMMENT '排序权重（越小越靠前）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '1=正常 2=已删除',
    `created_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    `updated_at` TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY `uk_category_id` (`category_id`),
    INDEX `idx_uid_space_sort` (`uid`, `space_id`, `sort`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='群组类别表（用户个人视图）';

ALTER TABLE `group_setting` ADD COLUMN `category_id` VARCHAR(32) DEFAULT NULL COMMENT '用户自定义类别ID';
ALTER TABLE `group_setting` ADD COLUMN `category_sort` INT NOT NULL DEFAULT 0 COMMENT '类别内排序';
ALTER TABLE `group_setting` ADD INDEX `idx_uid_category` (`uid`, `category_id`, `category_sort`);
