-- +migrate Up

-- PR-C D4 (spec: .octospec/tasks/cardtmpl-runtime-catalog-grants-discovery/brief.md):
-- template-ID level producer/space grants. Identity columns use binary
-- collations (no case folding); the global scope is the canonical empty
-- string, kept NOT NULL so the unique primary key cannot admit duplicate
-- "NULL scope" rows. Revoke is a tombstone (status='revoked', permissions
-- zeroed, revision bumped) — rows are never deleted, so revoke→recreate
-- cannot replay a stale revision (ABA). Grants deliberately have no version
-- column: authorization is template-ID scoped (brief invariant 10) and no FK
-- reaches across static/dynamic claims — target existence is enforced by a
-- same-transaction SELECT ... FOR UPDATE on card_template_version_claim.
CREATE TABLE card_template_grant (
    template_id    VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    principal_type VARCHAR(24)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    principal_id   VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    scope_space_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
    status         VARCHAR(16)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    can_discover   TINYINT(1) NOT NULL DEFAULT 0,
    can_send       TINYINT(1) NOT NULL DEFAULT 0,
    can_edit       TINYINT(1) NOT NULL DEFAULT 0,
    revision       BIGINT UNSIGNED NOT NULL,
    updated_by     VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    reason         VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    change_ticket  VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    created_at     DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at     DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (template_id, principal_type, principal_id, scope_space_id),
    KEY idx_card_template_grant_principal (principal_type, principal_id, scope_space_id),
    CONSTRAINT chk_card_template_grant_type
        CHECK (principal_type IN ('bot', 'internal_producer', 'space')),
    CONSTRAINT chk_card_template_grant_status
        CHECK (status IN ('active', 'revoked')),
    -- D2: an active grant always includes discover (send/edit imply it and a
    -- permissionless active row is meaningless); a tombstone carries no
    -- permissions so a reader that forgets the status column stays safe.
    CONSTRAINT chk_card_template_grant_shape
        CHECK ((status = 'active' AND can_discover = 1) OR
               (status = 'revoked' AND can_discover = 0 AND can_send = 0 AND can_edit = 0)),
    -- D2: a space principal is discover-only and its only canonical shape is
    -- principal_id=<space_id> with the global sentinel scope.
    CONSTRAINT chk_card_template_grant_space
        CHECK (principal_type <> 'space' OR (can_send = 0 AND can_edit = 0 AND scope_space_id = ''))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Grant/revoke audit rides the existing append-only card_template_audit in
-- the same transaction as the state write. The additive columns record the
-- principal identity and the before/after permission sets; version stays ''
-- because grants are template-ID scoped.
ALTER TABLE card_template_audit
    ADD COLUMN principal_type VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER resulting_revision,
    ADD COLUMN principal_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' AFTER principal_type,
    ADD COLUMN scope_space_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '' AFTER principal_id,
    ADD COLUMN previous_permissions VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER scope_space_id,
    ADD COLUMN resulting_permissions VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER previous_permissions;

-- +migrate Down
ALTER TABLE card_template_audit
    DROP COLUMN resulting_permissions,
    DROP COLUMN previous_permissions,
    DROP COLUMN scope_space_id,
    DROP COLUMN principal_id,
    DROP COLUMN principal_type;

DROP TABLE IF EXISTS card_template_grant;
