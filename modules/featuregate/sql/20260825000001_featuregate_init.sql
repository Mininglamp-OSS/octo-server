-- +migrate Up

-- 通用功能灰度规则表：每个功能 key 一行。
--
-- 表名带 octo_ 前缀（本仓新表约定）。本框架初版（未合并的 PR #280）建的是无前缀
-- 的 feature_gate / feature_gate_scope；因为那套表从未上线，此处直接按现行约定命名，
-- 不存在改名成本。
--
-- 注意 feature_key / scope_id 刻意**不设** DEFAULT ''：空串永远无法满足下面的 CHECK，
-- 留一个不可能成立的默认值只会让人误以为空 key 是合法状态。
--
-- CHECK 约束是纵深防御：管理端写路径已校验 key/mode/percent/bucket_by，这里再挡一层
-- 直接改库的旁路。少了它，一条手写的 mode='rollout' 只会在读侧静默 fail-safe 拒绝，
-- 运维看不出配错在哪。
--
-- feature_key 的字符集约束尤其重要：它要推导 env 杀开关名（OCTO_FEATUREGATE_<大写>_KILL）。
-- 一个含空格的 key 推出的环境变量名设不进去，这条 gate 就永久失去 env 级紧急停止 ——
-- 而那是「DB/Redis 全挂时仍能一键停」的最后一条路径。限定在 [a-z0-9_] 同时保证
-- key → env 名是单射，不会出现两条 gate 共用一个杀开关。
CREATE TABLE `octo_feature_gate` (
  `id`          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `feature_key` VARCHAR(64)  NOT NULL                 COMMENT '功能键（运维面标识；OCTO_FEATUREGATE_<大写>_KILL 环境变量名由它推导）',
  `mode`        VARCHAR(16)  NOT NULL DEFAULT 'off'   COMMENT 'off/on/whitelist/percent',
  `percent`     SMALLINT     NOT NULL DEFAULT 0       COMMENT 'percent 模式阈值 [0,100]，bucket < percent 即命中',
  `bucket_by`   VARCHAR(16)  NOT NULL DEFAULT 'group' COMMENT 'percent 分桶维度 group/space/user；选了调用点提供不了的维度会在读侧 fail-closed',
  `description` VARCHAR(255) NOT NULL DEFAULT ''      COMMENT '说明（按字符计，非字节）',
  `updated_by`  VARCHAR(40)  NOT NULL DEFAULT ''      COMMENT '最近修改的管理员 uid（审计）',
  `created_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_octo_feature_gate_key` (`feature_key`),
  CONSTRAINT `chk_octo_feature_gate_key` CHECK (REGEXP_LIKE(`feature_key`, '^[a-z][a-z0-9_]*$')),
  CONSTRAINT `chk_octo_feature_gate_mode` CHECK (`mode` IN ('off', 'on', 'whitelist', 'percent')),
  CONSTRAINT `chk_octo_feature_gate_percent` CHECK (`percent` BETWEEN 0 AND 100),
  CONSTRAINT `chk_octo_feature_gate_bucket_by` CHECK (`bucket_by` IN ('group', 'space', 'user'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通用功能灰度规则';

-- 灰度白名单条目表：逐条加删。
--
-- scope_type 三个维度在读侧【都】参与命中判定（见 pkg/featuregate.whitelistHit）。
-- 初版曾出现 space 写得进去、读侧却忽略的状态——那种「写成功但不生效」比不支持更难
-- 排查，故此处三者对等。
--
-- 白名单在 whitelist 与 percent 两个 mode 下均生效（percent 下优先于分桶），
-- 在 off 下无条件失效。
CREATE TABLE `octo_feature_gate_scope` (
  `id`          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `feature_key` VARCHAR(64)  NOT NULL                 COMMENT '关联 octo_feature_gate.feature_key',
  `scope_type`  VARCHAR(16)  NOT NULL DEFAULT 'group' COMMENT 'group/space/user',
  `scope_id`    VARCHAR(64)  NOT NULL                 COMMENT 'group_no / space_id / uid（禁空白与 /，见 delScope 的路径段取值）',
  `updated_by`  VARCHAR(40)  NOT NULL DEFAULT ''      COMMENT '添加该条目的管理员 uid（审计）',
  `created_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_octo_fgs_key_type_id` (`feature_key`, `scope_type`, `scope_id`),
  KEY `idx_octo_fgs_key` (`feature_key`),
  CONSTRAINT `chk_octo_fgs_feature_key` CHECK (REGEXP_LIKE(`feature_key`, '^[a-z][a-z0-9_]*$')),
  CONSTRAINT `chk_octo_fgs_scope_type` CHECK (`scope_type` IN ('group', 'space', 'user')),
  CONSTRAINT `chk_octo_fgs_scope_id` CHECK (`scope_id` <> '')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='通用功能灰度白名单条目';

-- +migrate Down
DROP TABLE IF EXISTS `octo_feature_gate_scope`;
DROP TABLE IF EXISTS `octo_feature_gate`;
