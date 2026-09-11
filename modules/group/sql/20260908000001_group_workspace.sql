-- +migrate Up
-- Existing groups remain unbound: both relation columns are nullable and no
-- membership/ACL rows are created. One Workspace may point to many groups, so
-- the index is intentionally non-unique.
ALTER TABLE `group`
  ADD COLUMN `workspace_id` VARCHAR(40) NULL DEFAULT NULL,
  ADD COLUMN `workspace_linked_by` VARCHAR(40) NULL DEFAULT NULL,
  ADD INDEX `idx_group_workspace_id` (`workspace_id`);

-- +migrate Down
ALTER TABLE `group`
  DROP INDEX `idx_group_workspace_id`,
  DROP COLUMN `workspace_linked_by`,
  DROP COLUMN `workspace_id`;
