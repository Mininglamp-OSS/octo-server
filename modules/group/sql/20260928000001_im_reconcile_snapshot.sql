-- +migrate Up
ALTER TABLE im_group_reconcile ADD COLUMN active_revision BIGINT UNSIGNED NOT NULL DEFAULT 0;
ALTER TABLE im_reconcile_deleted_channel ADD COLUMN completed_revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
    ADD KEY idx_group_pending (group_no, completed_revision, thread_id);

-- Deploy with old reconciliation workers drained. Their cursor did not retain
-- the corresponding data, so resume pending candidate work under a new revision.
UPDATE im_group_reconcile SET revision=revision+1, child_cursor=0, member_page=0,
    lease_owner='', lease_until=NULL, attempts=0, next_attempt_at=UTC_TIMESTAMP(6)
    WHERE pending=1;

-- One shared immutable member page per active group revision. Children reuse
-- these pages; storing a full member list for every child would multiply space.
CREATE TABLE im_reconcile_snapshot_page (
    group_no VARCHAR(40) NOT NULL,
    revision BIGINT UNSIGNED NOT NULL,
    page_index INT UNSIGNED NOT NULL,
    payload MEDIUMBLOB NOT NULL,
    PRIMARY KEY (group_no, page_index)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Each channel has its own durable acknowledgement cursor. A failed channel
-- does not discard successful sibling pages. Rows are removed on completion.
CREATE TABLE im_reconcile_snapshot_channel (
    group_no VARCHAR(40) NOT NULL,
    revision BIGINT UNSIGNED NOT NULL,
    child_id BIGINT NOT NULL,
    member_page INT UNSIGNED NOT NULL DEFAULT 0,
    page_count INT UNSIGNED NOT NULL,
    deleted TINYINT UNSIGNED NOT NULL DEFAULT 0,
    complete TINYINT UNSIGNED NOT NULL DEFAULT 0,
    payload MEDIUMBLOB NOT NULL,
    PRIMARY KEY (group_no, child_id),
    KEY idx_group_incomplete (group_no, complete, child_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +migrate Down
-- Active snapshots, authority and deletion acknowledgements are recovery data.
-- A binary rollback is not supported while these workers/data are active.
