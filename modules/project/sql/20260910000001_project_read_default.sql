-- +migrate Up

-- Project names are display labels, not tenant identity. Same Space may contain
-- multiple active Projects with the same name, so the old effective-name index
-- is removed while the generated column remains available to older tooling.
ALTER TABLE `octo_project` DROP INDEX `uk_octo_project_space_active_name`;

-- This mixed migration changes only the name index; no additional storage
-- is introduced because reading a list never creates a Project.

-- +migrate Down

-- Rebuild the legacy name index on rollback. If duplicate names were created
-- after this migration, this statement fails without deleting any data.
ALTER TABLE `octo_project`
  ADD UNIQUE KEY `uk_octo_project_space_active_name` (`space_id`, `active_name`);
