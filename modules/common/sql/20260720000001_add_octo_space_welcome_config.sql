-- +migrate Up

-- Per-Space 新成员欢迎语配置（task space-welcome-per-space-admin-crud）。
-- 每空间至多一行；空间管理员(Role>=1)经 /v1/space/:space_id/welcome 自助增删改。
-- 与平台级全局配置(system_setting onboarding.space_welcome_*)的关系：本表存在该
-- space 的行即「覆盖」全局（无论 enabled——管理员可借 enabled=0 让本空间退出全局
-- 活动），无行则「回落」全局兜底（仅当全局 enabled 且其 space_welcome_space_id
-- 点名该 space）。有效配置每空间恰好一份、解析确定。
-- 时间纪律：active_from 存 RFC3339 UTC 字符串（复用 ParsedActiveFrom /
-- ValidateSpaceWelcomeCombination，与 active_from(RFC3339 UTC) 语义一致）；
-- created_at/updated_at 为应用侧写入的 UTC 值（time.Now().UTC() bound 参数），
-- 禁用 NOW()（DB 会话时区未在 DSN 固定），因此不设 DEFAULT CURRENT_TIMESTAMP。
-- COLLATE 显式对齐 space.space_id（utf8mb4_general_ci），避免与 space/space_member
-- 对账 JOIN 触发 MySQL 1267。
CREATE TABLE `octo_space_welcome_config` (
  `id`          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `space_id`    VARCHAR(40)   NOT NULL DEFAULT ''  COMMENT '目标 Space（长度/字符集/COLLATE 对齐 space.space_id）',
  `enabled`     TINYINT       NOT NULL DEFAULT 0   COMMENT '0=关闭 1=开启；开启需 active_from/message 构成有效组合',
  `active_from` VARCHAR(40)   NOT NULL DEFAULT ''  COMMENT 'RFC3339 UTC 字符串（空=未设置）；仅 created_at>=此刻首次加入的成员会收到',
  `message`     VARCHAR(2000) NOT NULL DEFAULT ''  COMMENT '欢迎语纯文本（trim 后非空，<=2000 code points；支持换行，不渲染 markdown）',
  `updated_by`  VARCHAR(40)   NOT NULL DEFAULT ''  COMMENT '最近修改的管理员 uid（审计）',
  `created_at`  DATETIME      NOT NULL             COMMENT 'UTC；应用侧写入，禁 NOW()',
  `updated_at`  DATETIME      NOT NULL             COMMENT 'UTC；应用侧写入，禁 NOW()',
  UNIQUE KEY `uk_space` (`space_id`),
  KEY `idx_enabled` (`enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='per-space onboarding welcome config';

-- +migrate Down
DROP TABLE IF EXISTS `octo_space_welcome_config`;
