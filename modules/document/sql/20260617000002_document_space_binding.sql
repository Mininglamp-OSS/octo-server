-- +migrate Up
CREATE TABLE IF NOT EXISTS document_space_binding (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    binding_id VARCHAR(64) NOT NULL,
    document_space_id VARCHAR(64) NOT NULL,
    source_channel_id VARCHAR(128) NOT NULL,
    source_channel_type TINYINT NOT NULL DEFAULT 0,
    source_name VARCHAR(255) NOT NULL DEFAULT '',
    created_by VARCHAR(64) NOT NULL DEFAULT '',
    tenant_space_id VARCHAR(64) NOT NULL DEFAULT '',
    status TINYINT NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_document_space_binding_id (binding_id),
    UNIQUE KEY uk_document_space_binding_source (tenant_space_id, document_space_id, source_channel_id, source_channel_type),
    KEY idx_document_space_binding_tenant (tenant_space_id, status),
    KEY idx_document_space_binding_space (tenant_space_id, document_space_id, status)
);

-- +migrate Down
DROP TABLE IF EXISTS document_space_binding;
