-- +migrate Up
ALTER TABLE `user_api_key` ADD COLUMN `space_id` varchar(40) NOT NULL DEFAULT '' COMMENT '绑定的Space ID' AFTER `api_key`;
ALTER TABLE `user_api_key` ADD UNIQUE KEY `uk_uid_space` (`uid`, `space_id`);

-- +migrate Down
ALTER TABLE `user_api_key` DROP INDEX `uk_uid_space`;
ALTER TABLE `user_api_key` DROP COLUMN `space_id`;
