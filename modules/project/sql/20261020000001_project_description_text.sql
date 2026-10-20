-- +migrate Up
-- Fleet Project descriptions allow up to 32 KiB of UTF-8 text. Keep the empty
-- default for callers that omit description when inserting a Project.
ALTER TABLE `octo_project`
  MODIFY COLUMN `description` TEXT NOT NULL DEFAULT ('') COMMENT '项目描述';

-- +migrate Down
-- Keep the widened column on rollback: descriptions written by this version
-- may exceed VARCHAR(500), and a schema rollback must not truncate user text.
