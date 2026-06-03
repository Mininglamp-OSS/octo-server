-- +migrate Up
-- 扩展 user_api_key 支持 integration client 维度、撤销状态与使用审计。
-- client_id 默认 'botfather'：存量行自动回填，botfather 现有不带 client_id 的
-- insert/query 经 DEFAULT 兜底无感不回归。
-- api_key_hash / api_key_cipher 本期预留留空（后续 hash 化加固用）。
ALTER TABLE `user_api_key`
  ADD COLUMN `client_id` varchar(100) NOT NULL DEFAULT 'botfather' COMMENT '外部应用ID；botfather 自身为 botfather' AFTER `space_id`,
  ADD COLUMN `status` tinyint(4) NOT NULL DEFAULT 1 COMMENT '1=active 0=revoked' AFTER `client_id`,
  ADD COLUMN `api_key_hash` varchar(64) NOT NULL DEFAULT '' COMMENT '预留：uk_ 明文 SHA-256 hex（鉴权查询用）',
  ADD COLUMN `api_key_cipher` varchar(255) NOT NULL DEFAULT '' COMMENT '预留：uk_ 明文密文（回显用）',
  ADD COLUMN `last_used_at` timestamp NULL DEFAULT NULL COMMENT '最近使用时间',
  ADD COLUMN `last_used_ip` varchar(64) NOT NULL DEFAULT '' COMMENT '最近使用IP',
  ADD COLUMN `last_used_user_agent` varchar(255) NOT NULL DEFAULT '' COMMENT '最近调用方UA',
  ADD COLUMN `revoked_at` timestamp NULL DEFAULT NULL COMMENT '撤销时间';

-- 唯一键 (uid, space_id) -> (uid, space_id, client_id)。存量 client_id 均为
-- 'botfather'，改键安全；否则同一 uid+space 下为不同 client 插第二把 key 会撞旧约束。
ALTER TABLE `user_api_key` DROP INDEX `uk_uid_space`;
ALTER TABLE `user_api_key` ADD UNIQUE KEY `uk_uid_space_client` (`uid`, `space_id`, `client_id`);

-- +migrate Down
ALTER TABLE `user_api_key` DROP INDEX `uk_uid_space_client`;
-- 回滚到 (uid, space_id) 唯一键前，先清掉本特性引入的非 botfather client 行：
-- 一旦特性被使用过，同一 uid+space 下可能存在多个 client 的 key，直接重建旧唯一键
-- 会因重复 (uid, space_id) 撞键失败。删除这些行后 botfather 自有行在 (uid, space_id)
-- 上仍唯一（旧唯一键时代即如此），重建安全。回滚即移除本特性，删其数据符合语义。
DELETE FROM `user_api_key` WHERE `client_id` <> 'botfather';
ALTER TABLE `user_api_key` ADD UNIQUE KEY `uk_uid_space` (`uid`, `space_id`);
ALTER TABLE `user_api_key`
  DROP COLUMN `revoked_at`,
  DROP COLUMN `last_used_user_agent`,
  DROP COLUMN `last_used_ip`,
  DROP COLUMN `last_used_at`,
  DROP COLUMN `api_key_cipher`,
  DROP COLUMN `api_key_hash`,
  DROP COLUMN `status`,
  DROP COLUMN `client_id`;
