-- +migrate Up
-- 子区用户设置表：支持子区免打扰等个人偏好
CREATE TABLE `thread_setting` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `group_no` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '父群编号',
  `short_id` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '子区 shortID',
  `uid` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '用户 UID',
  `mute` TINYINT NOT NULL DEFAULT 0 COMMENT '免打扰: 0=关闭, 1=开启',
  `version` BIGINT NOT NULL DEFAULT 0 COMMENT '版本号',
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_thread_uid` (`group_no`, `short_id`, `uid`),
  KEY `idx_uid` (`uid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='子区用户设置表';

-- +migrate Down
DROP TABLE IF EXISTS `thread_setting`;
