-- +migrate Up

-- 自动归档 cron 的扫描索引：WHERE status=? AND last_message_at < ?
--   ORDER BY last_message_at, id LIMIT ?
-- 三列覆盖让 MySQL 走纯索引 range scan，避免对命中行集做 filesort。
ALTER TABLE `thread` ADD INDEX `idx_status_last_msg_id` (`status`, `last_message_at`, `id`);

-- +migrate Down

ALTER TABLE `thread` DROP INDEX `idx_status_last_msg_id`;
