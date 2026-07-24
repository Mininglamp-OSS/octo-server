-- +migrate Up

-- Per-group 入群欢迎语配置（task group-welcome-message）。
-- 每群至多一行；群主/管理员(role∈{creator,manager})经 /v1/groups/:group_no/welcome
-- 自助增删改。与 Space 版不同：本表【没有】平台级全局兜底 —— 一个 group 有该行且
-- enabled 才有欢迎语，无行或未启用即关闭。
-- 时间纪律：active_from 存 RFC3339 UTC 字符串（复用 ParsedActiveFrom /
-- ValidateGroupWelcomeCombination）；created_at/updated_at 为应用侧写入的 UTC 值
-- （time.Now().UTC() bound 参数），禁用 NOW()（DB 会话时区未在 DSN 固定），因此不设
-- DEFAULT CURRENT_TIMESTAMP。
-- COLLATE 显式对齐 group.group_no（legacy 群表用 DB 默认 utf8mb4_general_ci），避免
-- 与 group / group_member 对账 JOIN 触发 MySQL 1267。
CREATE TABLE `octo_group_welcome_config` (
  `id`          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `group_no`    VARCHAR(40)   NOT NULL DEFAULT ''  COMMENT '目标群（长度/字符集/COLLATE 对齐 group.group_no）',
  `enabled`     TINYINT       NOT NULL DEFAULT 0   COMMENT '0=关闭 1=开启；开启需 active_from/message 构成有效组合',
  `active_from` VARCHAR(40)   NOT NULL DEFAULT ''  COMMENT 'RFC3339 UTC 字符串（空=未设置）；仅 created_at>=此刻首次入群的成员会收到',
  `message`     VARCHAR(2000) NOT NULL DEFAULT ''  COMMENT '欢迎语纯文本（trim 后非空，<=2000 code points；支持 {member} 占位符点名新成员；不渲染 markdown）',
  `updated_by`  VARCHAR(40)   NOT NULL DEFAULT ''  COMMENT '最近修改的管理员 uid（审计）',
  `created_at`  DATETIME      NOT NULL             COMMENT 'UTC；应用侧写入，禁 NOW()',
  `updated_at`  DATETIME      NOT NULL             COMMENT 'UTC；应用侧写入，禁 NOW()',
  UNIQUE KEY `uk_group` (`group_no`),
  KEY `idx_enabled` (`enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='per-group onboarding welcome config';

-- +migrate Down
DROP TABLE IF EXISTS `octo_group_welcome_config`;
