-- +migrate Up

-- #297 Phase 3 平台适配器（github/wecom）引入：
--   - adapter 新增取值 github / wecom；
--   - status 新增取值 3=跳过（已接收、刻意不投递：GitHub ping / 渲染子集之外的事件，
--     响应 200 但没有消息进群）；
--   - reason 新增取值 event（不支持/不渲染的平台事件）、ping（GitHub 连通性测试）。
-- 复用既有列、无结构变更，仅刷新 COMMENT 让 schema 自文档化（同 20260604000002 的
-- 做法）。目标库 MySQL 8.0：仅改 COMMENT 的 MODIFY COLUMN 走 INSTANT 算法、瞬时无锁，
-- 无需显式 pin ALGORITHM/LOCK。
ALTER TABLE `incoming_webhook_audit`
  MODIFY COLUMN `status`  SMALLINT    NOT NULL DEFAULT 1        COMMENT '投递结果：1=成功,2=失败,3=跳过(已接收、刻意不投递：ping/未渲染事件)',
  MODIFY COLUMN `reason`  VARCHAR(32) NOT NULL DEFAULT ''       COMMENT '失败/跳过原因码（body/json/content/blocks/msg_type/too_large/delivery_failed/event/ping）；成功为空。限流429不入审计',
  MODIFY COLUMN `adapter` VARCHAR(16) NOT NULL DEFAULT 'native' COMMENT '消息来源/适配器：native/test/github/wecom（后续扩展 gitlab/feishu）';

-- +migrate Down
ALTER TABLE `incoming_webhook_audit`
  MODIFY COLUMN `status`  SMALLINT    NOT NULL DEFAULT 1        COMMENT '投递结果：1=成功,2=失败',
  MODIFY COLUMN `reason`  VARCHAR(32) NOT NULL DEFAULT ''       COMMENT '失败原因码（body/json/content/blocks/msg_type/too_large/delivery_failed）；成功为空。限流429不入审计',
  MODIFY COLUMN `adapter` VARCHAR(16) NOT NULL DEFAULT 'native' COMMENT '消息来源/适配器：native/test（Phase 3/4 扩展 github/gitlab/wecom/feishu）';
