-- +migrate Up
ALTER TABLE `space` ADD COLUMN `preset_group_ids` TEXT DEFAULT NULL COMMENT '预设群组ID列表(JSON数组)，新成员加入Space时自动加入这些群';
