-- +migrate Up

-- #426 / PR #427：新增 multica 出站 webhook 适配器（adapter_multica.go）。
-- adapter 列新增取值 multica；status / reason 列无变化。仅刷新 COMMENT 让 schema
-- 自文档化（同 20260610000001 / 20260618000001 的做法）。目标库 MySQL 8.0：仅改
-- COMMENT 的 MODIFY COLUMN 走 INSTANT 算法、瞬时无锁。
ALTER TABLE `incoming_webhook_audit`
  MODIFY COLUMN `adapter` VARCHAR(16) NOT NULL DEFAULT 'native' COMMENT '消息来源/适配器：native/test/github/wecom/multica（后续扩展 gitlab/feishu）';

-- +migrate Down
ALTER TABLE `incoming_webhook_audit`
  MODIFY COLUMN `adapter` VARCHAR(16) NOT NULL DEFAULT 'native' COMMENT '消息来源/适配器：native/test/github/wecom（后续扩展 gitlab/feishu）';
