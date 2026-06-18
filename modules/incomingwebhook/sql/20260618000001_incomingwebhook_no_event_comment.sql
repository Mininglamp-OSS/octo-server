-- +migrate Up

-- #297 Phase 3 review 跟进：缺 X-GitHub-Event 头的 400 改用独立 reason `no_event`，
-- 与「事件在渲染子集之外」的 200 跳过(reason=event) 区分开（前者是配置错误，后者正常
-- 但不渲染）。仅刷新 reason 列 COMMENT 让 schema 自文档化（同 20260610000001 的做法）。
-- 目标库 MySQL 8.0：仅改 COMMENT 的 MODIFY COLUMN 走 INSTANT 算法、瞬时无锁。
ALTER TABLE `incoming_webhook_audit`
  MODIFY COLUMN `reason` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '失败/跳过原因码（body/json/content/blocks/msg_type/no_event/too_large/delivery_failed/event/ping）；成功为空。限流429不入审计';

-- +migrate Down
ALTER TABLE `incoming_webhook_audit`
  MODIFY COLUMN `reason` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '失败/跳过原因码（body/json/content/blocks/msg_type/too_large/delivery_failed/event/ping）；成功为空。限流429不入审计';
