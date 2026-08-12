-- +migrate Up

CREATE TABLE IF NOT EXISTS `user_notification_pause` (
  `uid` VARCHAR(40) NOT NULL,
  `paused_until` DATETIME(3) NULL,
  `revision` BIGINT UNSIGNED NOT NULL DEFAULT 0,
  `updated_at` DATETIME(3) NOT NULL,
  PRIMARY KEY (`uid`),
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- +migrate Down

DROP TABLE IF EXISTS `user_notification_pause`;
