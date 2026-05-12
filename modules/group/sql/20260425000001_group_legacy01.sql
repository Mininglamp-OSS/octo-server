-- +migrate Up
ALTER TABLE `group` ADD COLUMN `allow_external` SMALLINT NOT NULL DEFAULT 1 COMMENT 'Allow external members: 1=yes (default, backward-compat), 0=block external scan-join and invite';

-- +migrate Down
ALTER TABLE `group` DROP COLUMN `allow_external`;
