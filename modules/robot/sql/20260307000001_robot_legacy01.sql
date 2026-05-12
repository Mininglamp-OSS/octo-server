-- +migrate Up

SET NAMES utf8mb4;

ALTER TABLE `robot` ADD COLUMN `auto_approve` tinyint NOT NULL DEFAULT 0 COMMENT '是否自动通过好友申请 0:否 1:是';
