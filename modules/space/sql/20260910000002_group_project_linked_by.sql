-- +migrate Up
-- The Project relation actor is paired with group.project_id. Existing rows are
-- intentionally NULL: no historical actor can be inferred safely.
ALTER TABLE `group`
  ADD COLUMN `project_linked_by` VARCHAR(40) NULL DEFAULT NULL
  COMMENT '绑定当前 Project 的操作者；NULL 表示存量关系没有历史操作者';

-- +migrate Down
ALTER TABLE `group` DROP COLUMN `project_linked_by`;
