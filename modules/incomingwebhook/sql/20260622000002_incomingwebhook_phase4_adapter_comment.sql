-- +migrate Up

-- #297 Phase 4 平台适配器（gitlab/feishu）引入：
--   - adapter 列新增取值 gitlab / feishu（在 20260622000001 的 multica 之后）；
--   - reason 列新增取值 token（GitLab X-Gitlab-Token 与 URL token 不匹配的 401）。
-- 复用既有列、无结构变更，仅刷新 COMMENT 让 schema 自文档化（同 20260610000001 的
-- 做法）。本迁移刻意排在 multica 注释迁移（20260622000001）之后，以便 adapter 注释
-- 收敛为 native/test/github/wecom/multica/gitlab/feishu 全集；reason 在 20260618000001
-- 的 no_event 基础上再加 token。目标库 MySQL 8.0：仅改 COMMENT 的 MODIFY COLUMN 走
-- INSTANT 算法、瞬时无锁。
ALTER TABLE `incoming_webhook_audit`
  MODIFY COLUMN `reason`  VARCHAR(32) NOT NULL DEFAULT ''       COMMENT '失败/跳过原因码（body/json/content/blocks/msg_type/no_event/token/too_large/delivery_failed/event/ping）；成功为空。限流429不入审计',
  MODIFY COLUMN `adapter` VARCHAR(16) NOT NULL DEFAULT 'native' COMMENT '消息来源/适配器：native/test/github/wecom/multica/gitlab/feishu';

-- +migrate Down
ALTER TABLE `incoming_webhook_audit`
  MODIFY COLUMN `reason`  VARCHAR(32) NOT NULL DEFAULT ''       COMMENT '失败/跳过原因码（body/json/content/blocks/msg_type/no_event/too_large/delivery_failed/event/ping）；成功为空。限流429不入审计',
  MODIFY COLUMN `adapter` VARCHAR(16) NOT NULL DEFAULT 'native' COMMENT '消息来源/适配器：native/test/github/wecom/multica（后续扩展 gitlab/feishu）';
