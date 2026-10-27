-- +migrate Up
-- Skill names are unique per workspace and version. Keep the newest row for
-- each (name, version), move bindings to it, then delete the older duplicates.
CREATE TEMPORARY TABLE `workspace_capability_skill_keep` AS
SELECT `id`, `keep_id` FROM (
  SELECT `id`,
         FIRST_VALUE(`id`) OVER (
           PARTITION BY `space_id`, `workspace_id`, `name`, `version`
           ORDER BY `updated_at` DESC, `id` DESC
         ) AS `keep_id`,
         ROW_NUMBER() OVER (
           PARTITION BY `space_id`, `workspace_id`, `name`, `version`
           ORDER BY `updated_at` DESC, `id` DESC
         ) AS `rn`
  FROM `workspace_capability`
  WHERE `kind` = 'skill'
) ranked
WHERE `rn` > 1;

UPDATE IGNORE `workspace_capability_binding` b
JOIN `workspace_capability_skill_keep` k ON k.`id` = b.`capability_id`
SET b.`capability_id` = k.`keep_id`;

DELETE c FROM `workspace_capability` c
JOIN `workspace_capability_skill_keep` k ON k.`id` = c.`id`;

DELETE b FROM `workspace_capability_binding` b
LEFT JOIN `workspace_capability` c ON c.`id` = b.`capability_id`
WHERE c.`id` IS NULL;

DROP TEMPORARY TABLE `workspace_capability_skill_keep`;

ALTER TABLE `workspace_capability`
  ADD COLUMN `skill_key` VARCHAR(320)
    GENERATED ALWAYS AS (IF(`kind` = 'skill', CONCAT(`name`, '\n', `version`), NULL)) STORED,
  ADD UNIQUE KEY `uk_workspace_capability_skill_name` (`space_id`, `workspace_id`, `skill_key`);

-- +migrate Down
ALTER TABLE `workspace_capability`
  DROP INDEX `uk_workspace_capability_skill_name`,
  DROP COLUMN `skill_key`;
