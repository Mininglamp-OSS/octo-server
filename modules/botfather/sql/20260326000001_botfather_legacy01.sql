-- +migrate Up
-- robot_apply: 若表不存在则创建（dev/test），若已存在则加 space_id 列（prod）
CREATE TABLE IF NOT EXISTS `robot_apply` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `uid`        VARCHAR(40)     NOT NULL COMMENT '申请人 UID',
  `robot_uid`  VARCHAR(40)     NOT NULL COMMENT 'Bot UID',
  `owner_uid`  VARCHAR(40)     NOT NULL COMMENT 'Bot Owner UID',
  `remark`     VARCHAR(200)    NOT NULL DEFAULT '' COMMENT '申请备注',
  `space_id`   VARCHAR(100)    NOT NULL DEFAULT '' COMMENT '申请来源 Space',
  `status`     TINYINT         NOT NULL DEFAULT 0  COMMENT '0=待处理 1=通过 2=拒绝',
  `created_at` TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_uid_robot_pending` (`uid`, `robot_uid`, `status`),
  KEY `idx_owner_status` (`owner_uid`, `status`),
  KEY `idx_robot_status` (`robot_uid`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Bot 好友申请记录';

-- 已有表补加 space_id 列（IF NOT EXISTS 需要存储过程包装）
SET @col_exists = (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema = DATABASE() AND table_name = 'robot_apply' AND column_name = 'space_id');
SET @sql = IF(@col_exists = 0,
  'ALTER TABLE `robot_apply` ADD COLUMN `space_id` VARCHAR(100) NOT NULL DEFAULT \'\' COMMENT \'申请来源 Space\' AFTER `remark`',
  'SELECT 1');
PREPARE stmt FROM @sql;
EXECUTE stmt;
DEALLOCATE PREPARE stmt;

-- +migrate Down
ALTER TABLE `robot_apply` DROP COLUMN IF EXISTS `space_id`;
