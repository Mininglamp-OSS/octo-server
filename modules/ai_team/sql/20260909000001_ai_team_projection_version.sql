-- +migrate Up

ALTER TABLE `ai_team_group`
  ADD COLUMN `roster_version` BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER `state`,
  ADD COLUMN `retry_after` TIMESTAMP NULL DEFAULT NULL AFTER `last_error`;

-- +migrate Down

ALTER TABLE `ai_team_group`
  DROP COLUMN `retry_after`,
  DROP COLUMN `roster_version`;
