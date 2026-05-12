-- +migrate Up

ALTER TABLE `robot` ADD COLUMN `creator_uid` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '创建者UID';
ALTER TABLE `robot` ADD COLUMN `description` VARCHAR(500) NOT NULL DEFAULT '' COMMENT '机器人描述';
ALTER TABLE `robot` ADD COLUMN `bot_token` VARCHAR(100) NOT NULL DEFAULT '' COMMENT 'Bot认证Token(bf_前缀)';
ALTER TABLE `robot` ADD COLUMN `im_token_cache` VARCHAR(200) NOT NULL DEFAULT '' COMMENT '缓存的IM Token';
ALTER TABLE `robot` ADD COLUMN `bot_commands` VARCHAR(1000) NOT NULL DEFAULT '' COMMENT '机器人命令列表JSON';
CREATE UNIQUE INDEX `idx_robot_bot_token` ON `robot` (`bot_token`);
CREATE INDEX `idx_robot_creator_uid` ON `robot` (`creator_uid`);
