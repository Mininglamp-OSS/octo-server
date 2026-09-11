-- +migrate Up

-- joined_at is the start of the current membership round. Keep the first-ever
-- row timestamp in created_at: existing rows cannot reveal when their current
-- round began, so the only honest historical backfill is created_at.
--
-- Add nullable first so the migration is safe against existing rows, backfill
-- before enforcing the same no-default application-written timestamp policy as
-- created_at and updated_at.
ALTER TABLE `octo_project_member`
  ADD COLUMN `joined_at` DATETIME(3) NULL
    COMMENT 'UTC；当前成员加入本轮的时间；历史数据由 created_at 回填；应用侧写入，禁 NOW()'
    AFTER `created_at`;

UPDATE `octo_project_member`
SET `joined_at` = `created_at`
WHERE `joined_at` IS NULL;

ALTER TABLE `octo_project_member`
  MODIFY COLUMN `joined_at` DATETIME(3) NOT NULL
    COMMENT 'UTC；当前成员加入本轮的时间；历史数据由 created_at 回填；应用侧写入，禁 NOW()';

-- +migrate Down
ALTER TABLE `octo_project_member`
  DROP COLUMN `joined_at`;
