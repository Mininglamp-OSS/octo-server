-- +migrate Up
CREATE TABLE im_group_reconcile (
    group_no VARCHAR(40) NOT NULL,
    revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
    completed_revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
    child_cursor BIGINT NOT NULL DEFAULT 0,
    member_page INT UNSIGNED NOT NULL DEFAULT 0,
    pending TINYINT UNSIGNED NOT NULL DEFAULT 1,
    attempts INT UNSIGNED NOT NULL DEFAULT 0,
    next_attempt_at DATETIME(6) NOT NULL,
    lease_owner VARCHAR(64) NOT NULL DEFAULT '',
    lease_until DATETIME(6) NULL,
    last_error VARCHAR(512) NOT NULL DEFAULT '',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (group_no),
    KEY idx_pending (pending, next_attempt_at, group_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Live and soft-deleted children remain in thread. Only a physical deletion
-- needs this ownership tombstone, captured in the deletion's transaction.
CREATE TABLE im_reconcile_deleted_channel (
    thread_id BIGINT NOT NULL,
    group_no VARCHAR(40) NOT NULL,
    channel_id VARCHAR(128) NOT NULL,
    channel_type TINYINT UNSIGNED NOT NULL DEFAULT 5,
    PRIMARY KEY (thread_id),
    KEY idx_group_cursor (group_no, thread_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +migrate Down
-- Application rollback must not remove unreconciled intent or authority
-- tombstones. Reverting these tables requires an explicit offline migration.
