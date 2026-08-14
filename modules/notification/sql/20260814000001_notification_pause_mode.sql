-- +migrate Up

ALTER TABLE `user_notification_pause`
  ADD COLUMN IF NOT EXISTS `mode` VARCHAR(16) NULL AFTER `uid`;

-- +migrate Down

-- Dropping this column irreversibly loses active manual pauses. Roll back only
-- with a state backup and an explicit customer-impact decision.
ALTER TABLE `user_notification_pause`
  DROP COLUMN `mode`;
