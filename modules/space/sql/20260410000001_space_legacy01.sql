-- +migrate Up
ALTER TABLE `space` ADD COLUMN `join_mode` TINYINT NOT NULL DEFAULT 0 COMMENT '加入模式 0=直接加入 1=需要审批' AFTER `preset_group_ids`;
