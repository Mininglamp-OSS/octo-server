-- +migrate Up
ALTER TABLE `group` ADD COLUMN `allow_no_mention` TINYINT NOT NULL DEFAULT 1 COMMENT 'Group-level allow no-@: 1=yes (default, backward-compat, existing no-@ bots unaffected), 0=bot must be @mentioned in this group';

-- +migrate Down
ALTER TABLE `group` DROP COLUMN `allow_no_mention`;
