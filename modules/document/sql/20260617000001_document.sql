-- +migrate Up
CREATE TABLE IF NOT EXISTS document_space (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    space_id VARCHAR(64) NOT NULL,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(255) NOT NULL DEFAULT '',
    owner_uid VARCHAR(64) NOT NULL,
    tenant_space_id VARCHAR(64) NOT NULL DEFAULT '',
    status TINYINT NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_document_space_id (space_id),
    KEY idx_document_space_tenant (tenant_space_id, status)
);

CREATE TABLE IF NOT EXISTS document_asset (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    asset_id VARCHAR(64) NOT NULL,
    name VARCHAR(255) NOT NULL,
    kind VARCHAR(32) NOT NULL DEFAULT 'doc',
    extension VARCHAR(32) NOT NULL DEFAULT '',
    size BIGINT NOT NULL DEFAULT 0,
    storage_path TEXT,
    source_type VARCHAR(32) NOT NULL DEFAULT '',
    source_channel_id VARCHAR(128) NOT NULL DEFAULT '',
    source_channel_type TINYINT NOT NULL DEFAULT 0,
    source_message_id VARCHAR(64) NOT NULL DEFAULT '',
    source_name VARCHAR(255) NOT NULL DEFAULT '',
    uploader_uid VARCHAR(64) NOT NULL DEFAULT '',
    uploader_name VARCHAR(128) NOT NULL DEFAULT '',
    owner_uid VARCHAR(64) NOT NULL DEFAULT '',
    owner_name VARCHAR(128) NOT NULL DEFAULT '',
    tenant_space_id VARCHAR(64) NOT NULL DEFAULT '',
    document_space_id VARCHAR(64) NOT NULL DEFAULT '',
    original_space_id VARCHAR(64) NOT NULL DEFAULT '',
    visibility VARCHAR(32) NOT NULL DEFAULT 'space',
    status VARCHAR(32) NOT NULL DEFAULT 'archived',
    downloads INT NOT NULL DEFAULT 0,
    previewable TINYINT NOT NULL DEFAULT 1,
    last_access_at DATETIME NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_document_asset_id (tenant_space_id, asset_id),
    KEY idx_document_asset_tenant_status (tenant_space_id, status, updated_at),
    KEY idx_document_asset_space (tenant_space_id, document_space_id, status),
    KEY idx_document_asset_source (source_channel_id, source_channel_type, source_message_id)
);

CREATE TABLE IF NOT EXISTS document_asset_event (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    event_id VARCHAR(64) NOT NULL,
    asset_id VARCHAR(64) NOT NULL,
    actor_uid VARCHAR(64) NOT NULL,
    action VARCHAR(32) NOT NULL,
    detail VARCHAR(255) NOT NULL DEFAULT '',
    tenant_space_id VARCHAR(64) NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_document_event_id (event_id),
    KEY idx_document_event_asset (asset_id, created_at),
    KEY idx_document_event_tenant (tenant_space_id, created_at)
);

-- +migrate Down
DROP TABLE IF EXISTS document_asset_event;
DROP TABLE IF EXISTS document_asset;
DROP TABLE IF EXISTS document_space;
