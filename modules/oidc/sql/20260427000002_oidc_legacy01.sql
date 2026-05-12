
-- +migrate Up

-- OIDC 身份绑定表 (Aegis 等真 OIDC IdP 通用)
create table `user_oidc_identity`
(
    id              bigint         not null primary key AUTO_INCREMENT,
    uid             VARCHAR(40)    not null default '',                              -- DMWork 本地 UID
    issuer          VARCHAR(255)   not null default '',                              -- OIDC iss
    subject         VARCHAR(255)   not null default '',                              -- OIDC sub
    email           VARCHAR(255)   not null default '',
    email_verified  smallint       not null default 0,
    phone           VARCHAR(32)    not null default '',
    phone_verified  smallint       not null default 0,
    linked_at       timestamp      not null default CURRENT_TIMESTAMP,
    last_login_at   timestamp      null     default null,
    created_at      timestamp      not null default CURRENT_TIMESTAMP,
    updated_at      timestamp      not null default CURRENT_TIMESTAMP on update CURRENT_TIMESTAMP,
    unique key uk_issuer_subject (issuer, subject),
    key idx_uid (uid),
    key idx_email (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- OIDC Refresh Token 加密存储 (支撑后台状态同步 Worker)
-- token_ciphertext 容量按 AES-GCM(plaintext)+ 12B nonce + 16B tag 估算,
-- 保留 4096 余量以兼容包含 JWT 的较长 refresh_token。
-- identity_id 应用层保证一致性,刻意不加 FK 约束:与项目其他表风格一致,
-- 同时避免 ON DELETE CASCADE 误删审计相关 RT 记录。
-- token_hash / token_ciphertext / expires_at 均无默认值,强制调用方显式赋值。
-- token_hash 尤其关键:default '' + UNIQUE 会让应用层遗漏赋值时第一行误成,
-- 第二行才报 duplicate key,无默认值能让首次错误就在 INSERT 失败。
create table `user_oidc_refresh`
(
    id                bigint         not null primary key AUTO_INCREMENT,
    identity_id       bigint         not null,                                       -- 关联 user_oidc_identity.id
    token_hash        char(64)       not null,                                       -- HMAC-SHA256(refresh_token, derived_key)
    token_ciphertext  varbinary(4096) not null,                                      -- AES-256-GCM 密文
    expires_at        timestamp      not null,
    last_refreshed_at timestamp      null     default null,
    revoked_at        timestamp      null     default null,
    created_at        timestamp      not null default CURRENT_TIMESTAMP,
    updated_at        timestamp      not null default CURRENT_TIMESTAMP on update CURRENT_TIMESTAMP,
    unique key uk_token_hash (token_hash),
    key idx_identity (identity_id),
    key idx_expires (expires_at, revoked_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- OIDC 登录 / 状态同步审计日志
create table `oidc_audit_log`
(
    id          bigint         not null primary key AUTO_INCREMENT,
    uid         VARCHAR(40)    not null default '',
    event       VARCHAR(32)    not null default '',                                  -- authorize/callback_ok/callback_fail/refresh_ok/refresh_fail/logout
    ip          VARCHAR(45)    not null default '',
    user_agent  VARCHAR(512)   not null default '',
    reason      VARCHAR(255)   not null default '',
    trace_id    VARCHAR(64)    not null default '',
    created_at  timestamp      not null default CURRENT_TIMESTAMP,
    updated_at  timestamp      not null default CURRENT_TIMESTAMP on update CURRENT_TIMESTAMP,
    key idx_uid_time (uid, created_at),
    key idx_event_time (event, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- +migrate Down

-- 回滚顺序:先删依赖 user_oidc_identity.id 的子表,再删父表
DROP TABLE IF EXISTS `oidc_audit_log`;
DROP TABLE IF EXISTS `user_oidc_refresh`;
DROP TABLE IF EXISTS `user_oidc_identity`;
