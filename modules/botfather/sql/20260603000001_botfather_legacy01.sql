-- +migrate Up
-- 扩展 user_api_key 支持 integration client 维度、撤销状态与使用审计。
-- client_id 默认 'botfather'：存量行自动回填，botfather 现有不带 client_id 的
-- insert/query 经 DEFAULT 兜底无感不回归。
-- api_key_hash / api_key_cipher 本期预留留空（后续 hash 化加固用）。
--
-- 幂等/可重入写法（与 base/20260512000001_base_oss_compat_repair.sql 同范式：
-- INFORMATION_SCHEMA 守卫 + 存储过程）。原因：MySQL 的 DDL 隐式提交，而
-- sql-migrate 仅在「一个迁移的所有语句都成功」后才往 gorp_migrations 记账
-- （rubenv/sql-migrate@v1.5.2 applyMigrations：失败即 Rollback，但已提交的
-- DDL 回不掉）。一旦首条 ADD COLUMN 成功、后续 DROP INDEX / ADD UNIQUE 任一
-- 失败，就残留「列已加但迁移未记账」的中间态——pod 重启再次执行首条 ADD COLUMN
-- 即报 Duplicate column name 'client_id'，陷入 CrashLoopBackOff（见 #239 部署
-- 事故）。逐项存在性守卫后本迁移可安全重入，并能自动修复已处于该中间态的环境。

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_user_api_key_clientdim;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __botfather_user_api_key_clientdim()
BEGIN
  -- 列：仅当 client_id 不存在时整体添加。首条 ADD COLUMN 是单条原子 ALTER，
  -- 要么 8 列全在、要么全不在，故以 client_id 为存在性哨兵即可覆盖整组。
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key'
         AND COLUMN_NAME = 'client_id') THEN
    ALTER TABLE `user_api_key`
      ADD COLUMN `client_id` varchar(100) NOT NULL DEFAULT 'botfather' COMMENT '外部应用ID；botfather 自身为 botfather' AFTER `space_id`,
      ADD COLUMN `status` tinyint(4) NOT NULL DEFAULT 1 COMMENT '1=active 0=revoked' AFTER `client_id`,
      ADD COLUMN `api_key_hash` varchar(64) NOT NULL DEFAULT '' COMMENT '预留：uk_ 明文 SHA-256 hex（鉴权查询用）',
      ADD COLUMN `api_key_cipher` varchar(255) NOT NULL DEFAULT '' COMMENT '预留：uk_ 明文密文（回显用）',
      ADD COLUMN `last_used_at` timestamp NULL DEFAULT NULL COMMENT '最近使用时间',
      ADD COLUMN `last_used_ip` varchar(64) NOT NULL DEFAULT '' COMMENT '最近使用IP',
      ADD COLUMN `last_used_user_agent` varchar(255) NOT NULL DEFAULT '' COMMENT '最近调用方UA',
      ADD COLUMN `revoked_at` timestamp NULL DEFAULT NULL COMMENT '撤销时间';
  END IF;

  -- 唯一键 (uid, space_id) -> (uid, space_id, client_id)。存量 client_id 均为
  -- 'botfather'，旧键 (uid, space_id) 唯一即蕴含新三元组唯一，改键安全。两步均
  -- 加存在性守卫，重入不报 1091（DROP 不存在的索引）/ 1061（重复建索引）。
  IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key'
         AND INDEX_NAME = 'uk_uid_space') THEN
    ALTER TABLE `user_api_key` DROP INDEX `uk_uid_space`;
  END IF;

  IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key'
         AND INDEX_NAME = 'uk_uid_space_client') THEN
    ALTER TABLE `user_api_key`
      ADD UNIQUE KEY `uk_uid_space_client` (`uid`, `space_id`, `client_id`);
  END IF;
END;
-- +migrate StatementEnd

CALL __botfather_user_api_key_clientdim();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_user_api_key_clientdim;
-- +migrate StatementEnd

-- +migrate Down
-- 同样做存在性守卫，使回滚可重入。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_user_api_key_clientdim_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __botfather_user_api_key_clientdim_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key'
         AND INDEX_NAME = 'uk_uid_space_client') THEN
    ALTER TABLE `user_api_key` DROP INDEX `uk_uid_space_client`;
  END IF;

  -- 回滚到 (uid, space_id) 唯一键前，先清掉本特性引入的非 botfather client 行：
  -- 一旦特性被使用过，同一 uid+space 下可能存在多个 client 的 key，直接重建旧
  -- 唯一键会因重复 (uid, space_id) 撞键失败。删除这些行后 botfather 自有行在
  -- (uid, space_id) 上仍唯一（旧唯一键时代即如此），重建安全。回滚即移除本特性，
  -- 删其数据符合语义。client_id 列仍在时才执行（列已不在说明已回滚过）。
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key'
         AND COLUMN_NAME = 'client_id') THEN
    DELETE FROM `user_api_key` WHERE `client_id` <> 'botfather';
  END IF;

  IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key'
         AND INDEX_NAME = 'uk_uid_space') THEN
    ALTER TABLE `user_api_key` ADD UNIQUE KEY `uk_uid_space` (`uid`, `space_id`);
  END IF;

  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key'
         AND COLUMN_NAME = 'client_id') THEN
    ALTER TABLE `user_api_key`
      DROP COLUMN `revoked_at`,
      DROP COLUMN `last_used_user_agent`,
      DROP COLUMN `last_used_ip`,
      DROP COLUMN `last_used_at`,
      DROP COLUMN `api_key_cipher`,
      DROP COLUMN `api_key_hash`,
      DROP COLUMN `status`,
      DROP COLUMN `client_id`;
  END IF;
END;
-- +migrate StatementEnd

CALL __botfather_user_api_key_clientdim_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_user_api_key_clientdim_down;
-- +migrate StatementEnd
