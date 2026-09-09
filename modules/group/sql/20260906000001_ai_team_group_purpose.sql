-- +migrate Up

ALTER TABLE `group`
  ADD COLUMN `purpose` VARCHAR(32) NOT NULL DEFAULT ''
  COMMENT '服务端管理的群用途；ai_session_container=我的 AI 团队专用容器';

CREATE INDEX `idx_group_purpose` ON `group` (`purpose`, `status`);

-- +migrate Down

DROP INDEX `idx_group_purpose` ON `group`;
ALTER TABLE `group` DROP COLUMN `purpose`;
