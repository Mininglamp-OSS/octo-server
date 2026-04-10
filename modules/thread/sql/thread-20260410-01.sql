-- +migrate Up

ALTER TABLE `thread` ADD COLUMN `message_count` BIGINT NOT NULL DEFAULT 0 COMMENT '消息数量';
ALTER TABLE `thread` ADD COLUMN `last_message_at` TIMESTAMP NULL DEFAULT NULL COMMENT '最后一条消息时间';
ALTER TABLE `thread` ADD COLUMN `last_message_content` VARCHAR(500) NOT NULL DEFAULT '' COMMENT '最后一条消息内容';
ALTER TABLE `thread` ADD COLUMN `last_message_sender_uid` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '最后一条消息发送者UID';
