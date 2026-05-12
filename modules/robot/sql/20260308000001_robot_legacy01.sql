-- +migrate Up
-- 将所有 Bot 的 auto_approve 默认改为 0（需要 owner 审批）
SET NAMES utf8mb4;
UPDATE `robot` SET `auto_approve` = 0 WHERE `auto_approve` = 1;
