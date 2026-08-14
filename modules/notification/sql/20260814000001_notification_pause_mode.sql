-- +migrate Up

ALTER TABLE `user_notification_pause`
  ADD COLUMN `mode` VARCHAR(16) NULL AFTER `uid`;

-- +migrate Down

ALTER TABLE `user_notification_pause`
  DROP COLUMN `mode`;
