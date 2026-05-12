-- +migrate Up

-- 用户实名认证记录表（OCTO 实名认证链路 - 与 dmwork-verify-service 对接）
-- verify-service (accounts.example.com) 通过 CAS/企微/飞书 完成实名后，
-- 经 /v1/internal/verification/complete 回调 upsert 到本表。
-- 详见 GH#1300 / YUJ-354。
CREATE TABLE IF NOT EXISTS user_verification (
    user_id       VARCHAR(40)  NOT NULL COMMENT 'OCTO 用户 UID',
    real_name     VARCHAR(128) NOT NULL COMMENT '实名（CAS/企微/飞书 返回）',
    source        VARCHAR(32)  NOT NULL COMMENT '实名来源: cas/wecom/feishu',
    source_sub    VARCHAR(128) NOT NULL COMMENT '来源侧 sub（如 CAS user_id）',
    emp_id        VARCHAR(64)  DEFAULT NULL COMMENT '工号（可空）',
    dept          VARCHAR(255) DEFAULT NULL COMMENT '部门（可空）',
    email         VARCHAR(255) DEFAULT NULL COMMENT '邮箱（可空）',
    mobile        VARCHAR(32)  DEFAULT NULL COMMENT '手机号（可空）',
    verified_at   DATETIME     NOT NULL COMMENT '实名完成时间（UTC）',
    updated_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '记录更新时间',
    PRIMARY KEY (user_id),
    KEY idx_user_verification_source (source, source_sub)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户实名认证（OCTO 实名链路）';

-- +migrate Down

DROP TABLE IF EXISTS user_verification;
