-- +migrate Up
-- 将 auto_approve 列的 DDL 默认值从 1 改为 0（新建 Bot 默认需要审批）
SET NAMES utf8mb4;
ALTER TABLE `robot` ALTER COLUMN `auto_approve` SET DEFAULT 0;
