-- +migrate Up

CREATE TABLE card_template_version_claim (
    template_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version     VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source      VARCHAR(8)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at  DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (template_id, version),
    CONSTRAINT chk_card_template_claim_source CHECK (source IN ('static', 'dynamic'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE card_template_artifact (
    template_id      VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version          VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    owner             VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    visibility        VARCHAR(16)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    engine_contract   VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    protocol          VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    contract_version  VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    canonical_bundle  MEDIUMBLOB NOT NULL,
    content_sha256    CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_by        VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    created_at        DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    blocked_at        DATETIME(6) NULL,
    blocked_by        VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    block_reason      VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
    PRIMARY KEY (template_id, version),
    KEY idx_card_template_artifact_hash (content_sha256),
    CONSTRAINT chk_card_template_artifact_visibility CHECK (visibility IN ('public', 'private')),
    CONSTRAINT fk_card_template_artifact_claim
        FOREIGN KEY (template_id, version)
        REFERENCES card_template_version_claim (template_id, version)
        ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE card_template_audit (
    id             BIGINT NOT NULL AUTO_INCREMENT,
    actor_uid      VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    operation      VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    template_id    VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version        VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    content_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    result         VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    reason         VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    change_ticket  VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    created_at     DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY idx_card_template_audit_identity (template_id, version, created_at),
    KEY idx_card_template_audit_actor (actor_uid, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- +migrate Down
DROP TABLE IF EXISTS card_template_audit;
DROP TABLE IF EXISTS card_template_artifact;
DROP TABLE IF EXISTS card_template_version_claim;
