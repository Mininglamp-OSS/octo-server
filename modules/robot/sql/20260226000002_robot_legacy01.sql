-- +migrate Up

ALTER TABLE `robot` ADD COLUMN `creator_uid` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '创建者UID';
ALTER TABLE `robot` ADD COLUMN `description` VARCHAR(500) NOT NULL DEFAULT '' COMMENT '机器人描述';
ALTER TABLE `robot` ADD COLUMN `bot_token` VARCHAR(100) NOT NULL DEFAULT '' COMMENT 'Bot认证Token(bf_前缀)';
ALTER TABLE `robot` ADD COLUMN `im_token_cache` VARCHAR(200) NOT NULL DEFAULT '' COMMENT '缓存的IM Token';
ALTER TABLE `robot` ADD COLUMN `bot_commands` VARCHAR(1000) NOT NULL DEFAULT '' COMMENT '机器人命令列表JSON';
-- Functional UNIQUE index on NULLIF(bot_token, ''): MySQL 8 treats each NULL
-- as distinct in a UNIQUE index, so the expression `(NULLIF(bot_token, ''))`
-- lets every robot whose token is still the empty-string default (BotFather
-- init, Notify bot init, insertSystemRobot — all share that case at boot
-- time) coexist while still rejecting duplicate *real* bf_* tokens at the
-- storage layer. A plain UNIQUE here would 1062 on the second empty-token
-- insert; dropping uniqueness altogether would let manual or buggy writes
-- create authentication-token collisions (auth treats `WHERE bot_token = ?`
-- as a single-row lookup).
CREATE UNIQUE INDEX `idx_robot_bot_token` ON `robot` ((NULLIF(`bot_token`, '')));
CREATE INDEX `idx_robot_creator_uid` ON `robot` (`creator_uid`);
