-- +migrate Up
-- 子区 GROUP.md 支持
-- 为 thread 表添加独立的 GROUP.md 字段
-- 字段命名与 group 表的 group_md 系列字段保持一致模式

ALTER TABLE `thread`
  ADD COLUMN `thread_md` TEXT DEFAULT NULL COMMENT '子区 GROUP.md 内容',
  ADD COLUMN `thread_md_version` BIGINT NOT NULL DEFAULT 0 COMMENT '子区 GROUP.md 版本号（每次更新自增）',
  ADD COLUMN `thread_md_updated_at` TIMESTAMP NULL COMMENT '子区 GROUP.md 最后更新时间',
  ADD COLUMN `thread_md_updated_by` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '子区 GROUP.md 最后更新者 UID';

-- +migrate Down
ALTER TABLE `thread`
  DROP COLUMN `thread_md`,
  DROP COLUMN `thread_md_version`,
  DROP COLUMN `thread_md_updated_at`,
  DROP COLUMN `thread_md_updated_by`;
