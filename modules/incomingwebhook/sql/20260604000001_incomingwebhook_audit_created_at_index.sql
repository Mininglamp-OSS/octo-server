-- +migrate Up

-- 审计表 TTL 清理（计划中的 DELETE ... WHERE created_at < ?）需要 created_at 单列
-- 前导索引。原 idx_iwa_webhook_time(webhook_id, created_at) 的前导列是 webhook_id，
-- TTL 删除没有 webhook_id 等值条件，命中不了该索引，会退化成全表扫。补一条 created_at
-- 单列索引专供按时间批量清理使用。
-- 目标库为 MySQL 8.0：ADD INDEX 默认即 INPLACE 在线建索引、不锁表，无需显式 pin。
ALTER TABLE `incoming_webhook_audit`
  ADD INDEX `idx_iwa_created` (`created_at`);

-- +migrate Down
ALTER TABLE `incoming_webhook_audit`
  DROP INDEX `idx_iwa_created`;
