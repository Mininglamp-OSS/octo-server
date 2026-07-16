-- +migrate Up

-- Space 新成员欢迎语的投递账本（at-most-once）。
-- 唯一去重真源：一个 (space_id, uid) 至多一行、至多投递一次。
-- 时间纪律：本表所有时间列均为应用侧写入的 UTC 值（time.Now().UTC() 作为 bound
-- 参数），禁止使用 NOW()——DB 会话时区未在 DSN 固定，NOW() 会把服务器墙钟与
-- active_from(RFC3339 UTC) 混用。因此不给任何时间列设 DEFAULT CURRENT_TIMESTAMP。
-- COLLATE 显式对齐 space / space_member / user 的 JOIN 键（utf8mb4_general_ci），
-- 避免对账 JOIN 触发 MySQL 1267（见 issue_344 collation 混排事故）。
CREATE TABLE `octo_space_welcome_delivery` (
  `id`              BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `space_id`        VARCHAR(40)  NOT NULL DEFAULT ''         COMMENT '目标 Space（长度/字符集/COLLATE 对齐 space.space_id）',
  `uid`             VARCHAR(40)  NOT NULL DEFAULT ''         COMMENT '收件人 UID（对齐 user.uid / space_member.uid）',
  `status`          TINYINT      NOT NULL DEFAULT 0          COMMENT '0=pending 1=claimed 2=dispatching 3=sent 4=failed 5=skipped 6=unknown',
  `attempts`        INT          NOT NULL DEFAULT 0          COMMENT '仅统计连续 pre-IM 失败次数；claim/sweep/precheck-skip/sent/unknown 均不消耗',
  `next_retry_at`   DATETIME     NULL                        COMMENT 'UTC；应用侧写入，禁 NOW()；下一次可被 claim 的时间',
  `lang`            VARCHAR(16)  NULL                        COMMENT '发送时解析出的收件人语言（zh-CN/en-US）',
  `message_id`      BIGINT       NULL                        COMMENT 'IM 返回的 message_id（成功时）',
  `client_msg_no`   VARCHAR(100) NULL                        COMMENT 'IM 返回的 client_msg_no（对齐 message 表宽度）',
  `claim_owner`     VARCHAR(128) NULL                        COMMENT '<hostname>:<pid>；每次 claim 后 update 的 CAS 断言',
  `claim_expire_at` DATETIME     NULL                        COMMENT 'UTC；应用侧写入；claim 租约到期时间',
  `error_class`     VARCHAR(64)  NULL                        COMMENT '错误分类：bot_not_ready/member_left/human_filter/orphan_member/config_read/im_timeout/im_bad_response/sent_persist/claim_expired',
  `created_at`      DATETIME     NOT NULL                    COMMENT 'UTC；应用侧写入，禁 NOW()',
  `updated_at`      DATETIME     NOT NULL                    COMMENT 'UTC；应用侧写入，禁 NOW()',
  UNIQUE KEY `uk_space_uid` (`space_id`, `uid`),
  KEY `idx_claim` (`space_id`, `status`, `next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='onboarding space welcome delivery ledger';

-- +migrate Down
DROP TABLE IF EXISTS `octo_space_welcome_delivery`;
