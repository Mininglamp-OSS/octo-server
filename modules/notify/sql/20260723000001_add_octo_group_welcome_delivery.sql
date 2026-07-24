-- +migrate Up

-- Group 入群欢迎语的投递账本（at-most-once）——task group-welcome-message。
-- 唯一去重真源：一个 (group_no, uid) 至多一行、至多投递一次（公开发到群频道）。
-- group_member.created_at 会在重新入群时被重置（不同于 space_member），因此
-- 「首次入群、退群再进不重复」由本账本的 UNIQUE(group_no, uid) 保证，而非时间列。
-- 时间纪律：所有时间列均为应用侧写入的 UTC 值（bound 参数），禁用 NOW()；因此不设
-- DEFAULT CURRENT_TIMESTAMP。COLLATE 显式对齐 group / group_member / user 的 JOIN
-- 键（utf8mb4_general_ci），避免对账 JOIN 触发 MySQL 1267。
-- idx_sweep 直接建在 CREATE 里（新表，无需分步 ALTER）：跨群 sweep 走
-- WHERE status=<claimed|dispatching> AND claim_expire_at<=?。
CREATE TABLE `octo_group_welcome_delivery` (
  `id`              BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `group_no`        VARCHAR(40)  NOT NULL DEFAULT ''         COMMENT '目标群（长度/字符集/COLLATE 对齐 group.group_no）',
  `uid`             VARCHAR(40)  NOT NULL DEFAULT ''         COMMENT '触发首次入群的成员 UID（对齐 user.uid / group_member.uid）',
  `status`          TINYINT      NOT NULL DEFAULT 0          COMMENT '0=pending 1=claimed 2=dispatching 3=sent 4=failed 5=skipped 6=unknown',
  `attempts`        INT          NOT NULL DEFAULT 0          COMMENT '仅统计连续 pre-IM 失败次数；claim/sweep/precheck-skip/sent/unknown 均不消耗',
  `next_retry_at`   DATETIME     NULL                        COMMENT 'UTC；应用侧写入，禁 NOW()；下一次可被 claim 的时间',
  `message_id`      BIGINT       NULL                        COMMENT 'IM 返回的 message_id（成功时）',
  `client_msg_no`   VARCHAR(100) NULL                        COMMENT 'IM 返回的 client_msg_no（对齐 message 表宽度）',
  `claim_owner`     VARCHAR(128) NULL                        COMMENT '<hostname>:<pid>；每次 claim 后 update 的 CAS 断言',
  `claim_expire_at` DATETIME     NULL                        COMMENT 'UTC；应用侧写入；claim 租约到期时间',
  `error_class`     VARCHAR(64)  NULL                        COMMENT '错误分类：human_filter/orphan_member/config_read/bot_not_ready/im_timeout/im_bad_response/im_rejected/sent_persist/claim_expired',
  `created_at`      DATETIME     NOT NULL                    COMMENT 'UTC；应用侧写入，禁 NOW()',
  `updated_at`      DATETIME     NOT NULL                    COMMENT 'UTC；应用侧写入，禁 NOW()',
  UNIQUE KEY `uk_group_uid` (`group_no`, `uid`),
  KEY `idx_claim` (`group_no`, `status`, `next_retry_at`),
  KEY `idx_sweep` (`status`, `claim_expire_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='onboarding group welcome delivery ledger';

-- +migrate Down
DROP TABLE IF EXISTS `octo_group_welcome_delivery`;
