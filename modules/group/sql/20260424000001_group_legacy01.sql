-- +migrate Up
ALTER TABLE `group` ADD COLUMN `is_external_group` SMALLINT NOT NULL DEFAULT 0 COMMENT 'External group: 0=no, 1=yes (auto-maintained when external members join/leave)';
ALTER TABLE `group_member` ADD COLUMN `is_external` SMALLINT NOT NULL DEFAULT 0 COMMENT 'External member: 0=no, 1=yes';
ALTER TABLE `group_member` ADD COLUMN `source_space_id` VARCHAR(40) NOT NULL DEFAULT '' COMMENT 'Source Space ID for external members';
CREATE INDEX `idx_group_member_external` ON `group_member` (`uid`, `is_external`, `is_deleted`);

-- +migrate Down
DROP INDEX `idx_group_member_external` ON `group_member`;
ALTER TABLE `group_member` DROP COLUMN `source_space_id`;
ALTER TABLE `group_member` DROP COLUMN `is_external`;
ALTER TABLE `group` DROP COLUMN `is_external_group`;
