-- +migrate Up
-- 扩展 update_desc 字段长度，支持更长的 release note
ALTER TABLE `app_version` MODIFY COLUMN `update_desc` TEXT NOT NULL COMMENT '更新说明';
