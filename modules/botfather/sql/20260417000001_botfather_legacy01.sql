-- +migrate Up
ALTER TABLE `robot` ADD COLUMN `agent_platform` VARCHAR(50) DEFAULT '' COMMENT 'AI Agent 平台名称（如 OpenClaw）';
ALTER TABLE `robot` ADD COLUMN `agent_version` VARCHAR(50) DEFAULT '' COMMENT 'Agent 平台版本号（最后一次注册时上报）';
ALTER TABLE `robot` ADD COLUMN `plugin_version` VARCHAR(50) DEFAULT '' COMMENT 'DMWork 插件版本号（最后一次注册时上报）';
