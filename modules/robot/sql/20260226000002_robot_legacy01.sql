-- +migrate Up

ALTER TABLE `robot` ADD COLUMN `creator_uid` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '创建者UID';
ALTER TABLE `robot` ADD COLUMN `description` VARCHAR(500) NOT NULL DEFAULT '' COMMENT '机器人描述';
ALTER TABLE `robot` ADD COLUMN `bot_token` VARCHAR(100) NULL DEFAULT NULL COMMENT 'Bot认证Token(bf_前缀)';
ALTER TABLE `robot` ADD COLUMN `im_token_cache` VARCHAR(200) NOT NULL DEFAULT '' COMMENT '缓存的IM Token';
ALTER TABLE `robot` ADD COLUMN `bot_commands` VARCHAR(1000) NOT NULL DEFAULT '' COMMENT '机器人命令列表JSON';
-- Non-unique index: uniqueness of bot_token is enforced application-side
-- (crypto/rand-generated bf_* token; collision probability < 1e-18). A
-- DB-level UNIQUE here would block insertion of any second robot whose
-- bot_token is the default value (NULL or "") — bots issued by BotFather
-- init, Notify bot init, and insertSystemRobot all share that case and
-- would otherwise collide on first boot. The lookup-by-token path uses
-- `WHERE bot_token = ? AND bot_token != ''` so duplicates of empty/NULL
-- entries are harmless.
CREATE INDEX `idx_robot_bot_token` ON `robot` (`bot_token`);
CREATE INDEX `idx_robot_creator_uid` ON `robot` (`creator_uid`);
