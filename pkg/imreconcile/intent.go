// Package imreconcile owns the durable business-to-IM membership boundary.
// A mutation and its dirty revision commit together. Delivery happens only
// after the SQL transaction is released and is fenced by the IM receiver.
package imreconcile

import (
	"database/sql"
	"errors"
	"os"
	"strings"

	"github.com/gocraft/dbr/v2"
)

// Enable only after all IM nodes support subscriber protocol 4. Once used,
// disabling is an unsupported rollback: Start refuses existing authority rows.
func Enabled() bool {
	v := strings.ToLower(os.Getenv("DM_IM_RECONCILE_ENABLED"))
	return v == "true" || v == "1"
}

// LockGroupTx precedes locking reads of members in transactions that may later
// write an intent. A new group's INSERT already owns this lock; do not call
// this before inserting a new group, since locking an absent key takes a gap.
func LockGroupTx(tx *dbr.Tx, groupNo string) error {
	if !Enabled() {
		return nil
	}
	var group string
	err := tx.QueryRow("SELECT group_no FROM `group` WHERE group_no=? FOR UPDATE", groupNo).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// TouchGroupTx precedes member/child writes and their locking reads, in the
// same transaction. Take the parent group lock before the intent row to match
// the existing create/disband lock order. Callers with Space seat locks must
// acquire those first. Recording intent first also protects callers which log
// and continue after a failed mutation: they cannot commit a write whose
// intent failed, and a harmless extra intent reconciles unchanged SQL state.
// A new mutation invalidates old leases; the receiver's revision, rather than
// that lease, prevents a delayed old network request from overwriting it.
func TouchGroupTx(tx *dbr.Tx, groupNo string) error {
	if !Enabled() {
		return nil
	}
	if tx == nil || groupNo == "" || len(groupNo) > 40 {
		return errors.New("invalid IM reconciliation group transaction")
	}
	// Store the canonical SQL identity; business group keys are case
	// insensitive whereas IM channel identities are byte strings.
	var canonical string
	err := tx.QueryRow("SELECT group_no FROM `group` WHERE group_no=? FOR UPDATE", groupNo).Scan(&canonical)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		groupNo = canonical
	}
	_, err = tx.Exec(`INSERT INTO im_group_reconcile
        (group_no, revision, pending, next_attempt_at)
        VALUES (?, 1, 1, UTC_TIMESTAMP(6))
        ON DUPLICATE KEY UPDATE revision=revision+1, pending=1, child_cursor=0, member_page=0,
        attempts=0, next_attempt_at=UTC_TIMESTAMP(6), lease_owner='',
        lease_until=NULL, last_error='', updated_at=UTC_TIMESTAMP(6)`, groupNo)
	return err
}

// RetireChildTx records the channel identity before its last business row is
// physically deleted. Soft deletion needs only TouchGroupTx: thread retains it.
func RetireChildTx(tx *dbr.Tx, threadID int64, groupNo, shortID string) error {
	if !Enabled() {
		return nil
	}
	if threadID <= 0 || shortID == "" {
		return errors.New("invalid child reconciliation tombstone")
	}
	if err := TouchGroupTx(tx, groupNo); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO im_reconcile_deleted_channel
        (thread_id, group_no, channel_id, channel_type) VALUES (?, ?, ?, 5)
        ON DUPLICATE KEY UPDATE thread_id=thread_id`, threadID, groupNo, groupNo+"____"+shortID)
	if err != nil {
		return err
	}
	return nil
}

// Mutate wraps a formerly standalone write when it needs atomic intent capture.
func Mutate(session *dbr.Session, groupNo string, write func(*dbr.Tx) error) error {
	tx, err := session.Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()
	if err := TouchGroupTx(tx, groupNo); err != nil {
		return err
	}
	if err := write(tx); err != nil {
		return err
	}
	return tx.Commit()
}
