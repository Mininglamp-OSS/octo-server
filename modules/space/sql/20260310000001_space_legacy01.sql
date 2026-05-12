-- +migrate Up
ALTER TABLE `space` ADD COLUMN `max_users` INT NOT NULL DEFAULT 0 COMMENT '最大成员数 0表示不限制';
