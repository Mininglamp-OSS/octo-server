-- +migrate Up

CREATE TABLE card_template_catalog_capacity_guard (
    guard_key TINYINT UNSIGNED NOT NULL,
    PRIMARY KEY (guard_key),
    CONSTRAINT chk_card_template_catalog_capacity_guard
        CHECK (guard_key = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

INSERT INTO card_template_catalog_capacity_guard (guard_key) VALUES (1);

CREATE TABLE card_template_activation (
    template_id     VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    active_version  VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status          VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT UNSIGNED NOT NULL,
    updated_by      VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    reason          VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    change_ticket   VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    updated_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (template_id),
    KEY idx_card_template_activation_status (status, template_id),
    CONSTRAINT chk_card_template_activation_status
        CHECK (status IN ('active', 'disabled')),
    CONSTRAINT chk_card_template_activation_shape
        CHECK ((status = 'active' AND active_version IS NOT NULL) OR
               (status = 'disabled' AND active_version IS NULL)),
    CONSTRAINT fk_card_template_activation_claim
        FOREIGN KEY (template_id, active_version)
        REFERENCES card_template_version_claim (template_id, version)
        ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

ALTER TABLE card_template_audit
    ADD COLUMN previous_version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER version,
    ADD COLUMN resulting_version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER previous_version,
    ADD COLUMN previous_revision BIGINT UNSIGNED NULL AFTER resulting_version,
    ADD COLUMN resulting_revision BIGINT UNSIGNED NULL AFTER previous_revision,
    ADD KEY idx_card_template_audit_page (template_id, id),
    ADD KEY idx_card_template_audit_previous (template_id, result, operation, previous_version),
    ADD KEY idx_card_template_audit_resulting (template_id, result, operation, resulting_version);

-- +migrate Down
DROP TABLE IF EXISTS card_template_activation;
DROP TABLE IF EXISTS card_template_catalog_capacity_guard;

ALTER TABLE card_template_audit
    DROP INDEX idx_card_template_audit_resulting,
    DROP INDEX idx_card_template_audit_previous,
    DROP INDEX idx_card_template_audit_page,
    DROP COLUMN resulting_revision,
    DROP COLUMN previous_revision,
    DROP COLUMN resulting_version,
    DROP COLUMN previous_version;
