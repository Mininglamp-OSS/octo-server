-- +migrate Up
CREATE TABLE IF NOT EXISTS document_space_member (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    member_id VARCHAR(64) NOT NULL,
    document_space_id VARCHAR(64) NOT NULL,
    uid VARCHAR(64) NOT NULL,
    name VARCHAR(128) NOT NULL DEFAULT '',
    role VARCHAR(32) NOT NULL DEFAULT 'viewer',
    source VARCHAR(32) NOT NULL DEFAULT '手动添加',
    created_by VARCHAR(64) NOT NULL DEFAULT '',
    tenant_space_id VARCHAR(64) NOT NULL DEFAULT '',
    status TINYINT NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_document_space_member_id (member_id),
    UNIQUE KEY uk_document_space_member_uid (tenant_space_id, document_space_id, uid),
    KEY idx_document_space_member_space (tenant_space_id, document_space_id, status),
    KEY idx_document_space_member_uid_lookup (tenant_space_id, uid, status)
);

-- +migrate Down
DROP TABLE IF EXISTS document_space_member;
