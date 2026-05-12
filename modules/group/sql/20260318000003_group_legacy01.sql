-- +migrate Up
ALTER TABLE `group`
  ADD COLUMN `group_md` TEXT DEFAULT NULL COMMENT 'GROUP.md content',
  ADD COLUMN `group_md_version` BIGINT NOT NULL DEFAULT 0 COMMENT 'GROUP.md version (auto-increment on update)',
  ADD COLUMN `group_md_updated_at` TIMESTAMP NULL COMMENT 'GROUP.md last update time',
  ADD COLUMN `group_md_updated_by` VARCHAR(40) NOT NULL DEFAULT '' COMMENT 'GROUP.md last updater UID';
ALTER TABLE `group_member` ADD COLUMN `bot_admin` SMALLINT NOT NULL DEFAULT 0 COMMENT 'Bot admin: 0=no, 1=yes';

-- +migrate Down
ALTER TABLE `group`
  DROP COLUMN `group_md`,
  DROP COLUMN `group_md_version`,
  DROP COLUMN `group_md_updated_at`,
  DROP COLUMN `group_md_updated_by`;
ALTER TABLE `group_member` DROP COLUMN `bot_admin`;
