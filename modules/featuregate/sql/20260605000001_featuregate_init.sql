-- +migrate Up

-- 通用功能灰度规则表：每个功能 key 一行
CREATE TABLE `feature_gate` (
  `id`          BIGINT       AUTO_INCREMENT PRIMARY KEY,
  `feature_key` VARCHAR(64)  NOT NULL DEFAULT ''       COMMENT '功能键，如 incoming_webhook_create / incoming_webhook_push',
  `mode`        VARCHAR(16)  NOT NULL DEFAULT 'off'    COMMENT 'off/on/whitelist/percent',
  `percent`     SMALLINT     NOT NULL DEFAULT 0        COMMENT 'percent 模式阈值 [0,100]，<percent 命中',
  `description` VARCHAR(255) NOT NULL DEFAULT ''       COMMENT '说明',
  `created_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_feature_gate_key` (`feature_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通用功能灰度规则';

-- 灰度白名单条目表：逐条加删，scope_type 兼容 group/space，当前代码仅 group 生效
CREATE TABLE `feature_gate_scope` (
  `id`          BIGINT       AUTO_INCREMENT PRIMARY KEY,
  `feature_key` VARCHAR(64)  NOT NULL DEFAULT ''      COMMENT '关联 feature_gate.feature_key',
  `scope_type`  VARCHAR(16)  NOT NULL DEFAULT 'group' COMMENT 'group/space；当前仅 group 生效',
  `scope_id`    VARCHAR(64)  NOT NULL DEFAULT ''      COMMENT '群编号 group_no 或 space_id',
  `created_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_fgs_key_type_id` (`feature_key`, `scope_type`, `scope_id`),
  INDEX `idx_fgs_key` (`feature_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通用功能灰度白名单条目';

-- +migrate Down
DROP TABLE IF EXISTS `feature_gate_scope`;
DROP TABLE IF EXISTS `feature_gate`;
