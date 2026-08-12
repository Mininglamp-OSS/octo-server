-- +migrate Up

-- MySQL is the sole authority for the session rollout. Redis remains the
-- session data plane and supplies run_id-bound scan evidence, but restoring or
-- rolling Redis back cannot lower this state.
CREATE TABLE IF NOT EXISTS `octo_session_rollout_state` (
  `singleton_id` TINYINT UNSIGNED NOT NULL COMMENT '恒为1的单例键',
  `floor` VARCHAR(16) NOT NULL COMMENT 'expand|v3-write|revoke|bounded|enforce',
  `max_per_uid` INT UNSIGNED NOT NULL COMMENT 'v3 session 每 UID 上限',
  `version` BIGINT UNSIGNED NOT NULL DEFAULT 1 COMMENT 'CAS 版本',
  `paused` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '暂停自动推进，不影响轮询应用已有 floor',
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`singleton_id`),
  CONSTRAINT `chk_session_rollout_state_singleton` CHECK (`singleton_id` = 1),
  CONSTRAINT `chk_session_rollout_state_floor` CHECK (`floor` IN ('expand', 'v3-write', 'revoke', 'bounded', 'enforce')),
  CONSTRAINT `chk_session_rollout_state_max_per_uid` CHECK (`max_per_uid` BETWEEN 1 AND 10000)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='session rollout 单一权威状态';

CREATE TABLE IF NOT EXISTS `octo_session_rollout_advance` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `from_floor` VARCHAR(16) NOT NULL DEFAULT '' COMMENT '空串仅表示首次接管',
  `to_floor` VARCHAR(16) NOT NULL,
  `actor` VARCHAR(128) NOT NULL,
  `evidence_json` JSON NULL COMMENT '完整扫描聚合证据，不含 token/UID/key',
  `redis_run_id` VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'Redis run_id 的 SHA-256 指纹',
  `writer_fingerprint` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '推进前后相同的 writer roster 指纹',
  `transition_kind` VARCHAR(32) NOT NULL DEFAULT 'advance' COMMENT 'bootstrap|legacy-takeover|advance|set-cap',
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_session_rollout_advance_created` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='session rollout 事务化推进证据';

-- 不 seed。首次启动在 migration 完成后读取一次 #725 Redis floor 和遗留 MODE，
-- 将两者中更严格的姿态作为接管下限写入单例；之后只读写 MySQL。

-- +migrate Down

DROP TABLE IF EXISTS `octo_session_rollout_advance`;
DROP TABLE IF EXISTS `octo_session_rollout_state`;
