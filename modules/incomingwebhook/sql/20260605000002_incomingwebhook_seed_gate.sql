-- +migrate Up

-- 既有群入站 Webhook 功能（#31）已上线可用。接入 featuregate 后，create 对「无
-- 规则」是 fail-closed —— 若不 seed，已有部署升级后原本可用的创建请求会全部变
-- 403（升级即回归）。这里为两个 key seed 默认 mode=on，保持升级前行为；灰度 / 回滚
-- 由运维后续改规则（whitelist/percent/off）opt-in。
--
-- 幂等：若运维在升级前已手动建规则，ON DUPLICATE 走 no-op，不覆盖其设置。
-- 依赖：feature_gate 表由 featuregate 模块的 20260605000001 迁移先建（按文件时间戳
-- 全局排序，本文件 20260605000002 在其后）。
INSERT INTO feature_gate (feature_key, mode, description)
VALUES ('incoming_webhook_create', 'on', 'seeded on upgrade: keep existing create enabled'),
       ('incoming_webhook_push', 'on', 'seeded on upgrade: keep existing push enabled')
ON DUPLICATE KEY UPDATE feature_key = feature_key;

-- +migrate Down
DELETE FROM feature_gate WHERE feature_key IN ('incoming_webhook_create', 'incoming_webhook_push');
