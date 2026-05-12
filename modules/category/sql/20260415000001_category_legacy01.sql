-- +migrate Up

SET @col_exists = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'group_category' AND COLUMN_NAME = 'is_default');
SET @sql = IF(@col_exists = 0, 'ALTER TABLE `group_category` ADD COLUMN `is_default` TINYINT NOT NULL DEFAULT 0 COMMENT ''1=默认未分类分组（不可删除/重命名）''', 'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- +migrate Down

ALTER TABLE `group_category` DROP COLUMN IF EXISTS `is_default`;
