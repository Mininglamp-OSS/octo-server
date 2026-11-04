-- +migrate Up

-- Reversible Project archive state and per-member archive notice dismissal.
-- Existing rows remain active and start with archive_version zero.
ALTER TABLE `octo_project`
  ADD COLUMN `archived_at` DATETIME(3) NULL COMMENT 'UTC；归档时间，NULL 表示活跃',
  ADD COLUMN `archive_version` BIGINT NOT NULL DEFAULT 0 COMMENT '最近一次归档的 lifecycle_version；恢复时保留';

-- pinned 变成三态：1 是置顶，0 是用户显式取消，NULL 是从未表达过置顶偏好。
-- 归档提醒的关闭必须写一行偏好，但它不是取消置顶；三态让侧边栏的 pinned=0
-- 过滤只命中显式取消，而不是把仅仅关闭提醒的用户从关注里删掉。
ALTER TABLE `octo_project_user_setting`
  ADD COLUMN `archive_notice_dismissed_version` BIGINT NOT NULL DEFAULT 0 COMMENT '该用户已关闭的归档轮次；0 表示未关闭',
  MODIFY COLUMN `pinned` TINYINT UNSIGNED NULL DEFAULT NULL COMMENT '1=置顶；0=显式取消置顶；NULL=未设置';

-- +migrate Down
-- WARNING: rolling this back after any archive discards lifecycle state. Check
-- that archived_at is NULL and archive_version is zero for every Project first.
-- NULL rows only hold archive-notice state, so discard them on rollback. Explicit
-- pin=0 and pin=1 rows remain intact before pinned returns to NOT NULL.
DELETE FROM `octo_project_user_setting` WHERE `pinned` IS NULL;

ALTER TABLE `octo_project_user_setting`
  MODIFY COLUMN `pinned` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '1=该用户置顶了这个项目',
  DROP COLUMN `archive_notice_dismissed_version`;

ALTER TABLE `octo_project`
  DROP COLUMN `archive_version`,
  DROP COLUMN `archived_at`;
