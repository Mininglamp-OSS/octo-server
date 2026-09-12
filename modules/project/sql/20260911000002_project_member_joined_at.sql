-- +migrate Up

-- joined_at is the start of the current membership round.
--
-- This is the expand step of the rolling deployment. Keep the column nullable:
-- binaries from before this migration omit it from their explicit INSERT list,
-- and existing rows have no reliable current-round timestamp. Readers use
-- COALESCE(joined_at, created_at) until every writer has been upgraded. A later
-- contract migration may backfill and enforce NOT NULL after that window.
ALTER TABLE `octo_project_member`
  ADD COLUMN `joined_at` DATETIME(3) NULL
    COMMENT 'UTC；当前成员加入本轮的时间；历史数据读取时回退到 created_at；应用侧写入，禁 NOW()'
    AFTER `created_at`;

-- +migrate Down
ALTER TABLE `octo_project_member`
  DROP COLUMN `joined_at`;
