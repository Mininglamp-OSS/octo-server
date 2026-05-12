-- +migrate Up
-- conversation 表在 WuKongIM 侧管理，dmworkim 只有 conversation_extra
-- space_id 通过 channel_id 前缀 s{spaceId}_ 在运行时解析，无需额外列
ALTER TABLE `group` ADD COLUMN `space_id` VARCHAR(40) DEFAULT '' COMMENT 'Space ID';
