package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// spaceWelcomeStore is the DB access layer for the delivery ledger. Every call
// runs under a caller-supplied context; the service wraps each in swDBCallTimeout.
//
// Time discipline: ALL timestamp writes take an application-computed UTC value
// as a bound parameter (never NOW()), because the DB session TZ is not pinned
// and the SQL would otherwise mix wall-clock with the UTC active_from.
type spaceWelcomeStore struct {
	db         *dbr.Session
	claimOwner string
}

func newSpaceWelcomeStore(db *dbr.Session, claimOwner string) *spaceWelcomeStore {
	return &spaceWelcomeStore{db: db, claimOwner: claimOwner}
}

// upsertPending inserts a pending row for (spaceID, uid) if absent. Returns
// inserted=true only when a new row was actually created; a duplicate hit
// (ON DUPLICATE KEY UPDATE id=id) reports inserted=false so the caller can
// split enqueue_total vs enqueue_dedup_total. INSERT ... ON DUPLICATE KEY
// UPDATE (never INSERT IGNORE, which would swallow charset-truncation errors).
func (s *spaceWelcomeStore) upsertPending(ctx context.Context, spaceID, uid string, now time.Time) (inserted bool, err error) {
	res, err := s.db.InsertBySql(
		"INSERT INTO "+spaceWelcomeTable+" (space_id, uid, status, attempts, created_at, updated_at) "+
			"VALUES (?, ?, ?, 0, ?, ?) ON DUPLICATE KEY UPDATE id = id",
		spaceID, uid, swStatusPending, now, now,
	).ExecContext(ctx)
	if err != nil {
		return false, fmt.Errorf("upsert pending welcome row: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read affected rows: %w", err)
	}
	// MySQL (default, no CLIENT_FOUND_ROWS): 1 = inserted, 0 = duplicate no-op.
	return affected > 0, nil
}

// claimOne atomically claims one due pending row for spaceID via
// SELECT ... FOR UPDATE SKIP LOCKED + UPDATE inside one transaction. Returns
// (nil, nil) when no claimable row exists. attempts is deliberately NOT
// incremented at claim.
func (s *spaceWelcomeStore) claimOne(ctx context.Context, spaceID string, now time.Time) (*spaceWelcomeRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var row spaceWelcomeRow
	err = tx.SelectBySql(
		"SELECT id, space_id, uid, attempts FROM "+spaceWelcomeTable+" "+
			"WHERE space_id=? AND status=? AND (next_retry_at IS NULL OR next_retry_at<=?) "+
			"ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED",
		spaceID, swStatusPending, now,
	).LoadOneContext(ctx, &row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("select claimable welcome row: %w", err)
	}

	if _, err = tx.UpdateBySql(
		"UPDATE "+spaceWelcomeTable+" SET status=?, claim_owner=?, claim_expire_at=?, updated_at=? WHERE id=?",
		swStatusClaimed, s.claimOwner, now.Add(swClaimLease), now, row.ID,
	).ExecContext(ctx); err != nil {
		return nil, fmt.Errorf("mark welcome row claimed: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim tx: %w", err)
	}
	return &row, nil
}

// sweepClaimed recycles claimed rows whose lease expired back to pending. Safe
// because the IM call was not yet started; attempts is not decremented (and was
// not incremented at claim). next_retry_at is set to now so the row is
// immediately re-claimable.
func (s *spaceWelcomeStore) sweepClaimed(ctx context.Context, spaceID string, now time.Time) (int64, error) {
	res, err := s.db.UpdateBySql(
		"UPDATE "+spaceWelcomeTable+" SET status=?, next_retry_at=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE space_id=? AND status=? AND claim_expire_at IS NOT NULL AND claim_expire_at<=?",
		swStatusPending, now, now, spaceID, swStatusClaimed, now,
	).ExecContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("sweep claimed welcome rows: %w", err)
	}
	return res.RowsAffected()
}

// sweepDispatching promotes dispatching rows whose lease expired to unknown
// (we cannot prove whether the IM call happened). Never auto-retried.
func (s *spaceWelcomeStore) sweepDispatching(ctx context.Context, spaceID string, now time.Time) (int64, error) {
	res, err := s.db.UpdateBySql(
		"UPDATE "+spaceWelcomeTable+" SET status=?, error_class=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE space_id=? AND status=? AND claim_expire_at IS NOT NULL AND claim_expire_at<=?",
		swStatusUnknown, swErrClaimExpired, now, spaceID, swStatusDispatching, now,
	).ExecContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("sweep dispatching welcome rows: %w", err)
	}
	return res.RowsAffected()
}

// cas runs a CAS UPDATE guarded by id + expected status + claim_owner=self, so a
// lease-expired sweep on another replica cannot be overwritten by this replica.
// Returns ok=false when no row matched (ownership lost / status moved on).
func (s *spaceWelcomeStore) cas(ctx context.Context, query string, args ...interface{}) (bool, error) {
	res, err := s.db.UpdateBySql(query, args...).ExecContext(ctx)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// casToDispatching transitions claimed -> dispatching, persisting lang and
// extending the lease.
func (s *spaceWelcomeStore) casToDispatching(ctx context.Context, id int64, lang string, now time.Time) (bool, error) {
	ok, err := s.cas(ctx,
		"UPDATE "+spaceWelcomeTable+" SET status=?, lang=?, claim_expire_at=?, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusDispatching, lang, now.Add(swClaimLease), now, id, swStatusClaimed, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas to dispatching: %w", err)
	}
	return ok, nil
}

// casToSent transitions dispatching -> sent, persisting the IM identifiers.
func (s *spaceWelcomeStore) casToSent(ctx context.Context, id int64, messageID int64, clientMsgNo string, now time.Time) (bool, error) {
	ok, err := s.cas(ctx,
		"UPDATE "+spaceWelcomeTable+" SET status=?, message_id=?, client_msg_no=?, error_class=NULL, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusSent, messageID, clientMsgNo, now, id, swStatusDispatching, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas to sent: %w", err)
	}
	return ok, nil
}

// casToSkipped transitions claimed -> skipped (recipient ineligible at pre-send
// check). attempts is not consumed. Terminal — never revisited.
func (s *spaceWelcomeStore) casToSkipped(ctx context.Context, id int64, errClass string, now time.Time) (bool, error) {
	ok, err := s.cas(ctx,
		"UPDATE "+spaceWelcomeTable+" SET status=?, error_class=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusSkipped, errClass, now, id, swStatusClaimed, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas to skipped: %w", err)
	}
	return ok, nil
}

// casToUnknown transitions dispatching -> unknown (transport-ambiguous). Never
// auto-retried.
func (s *spaceWelcomeStore) casToUnknown(ctx context.Context, id int64, errClass string, now time.Time) (bool, error) {
	ok, err := s.cas(ctx,
		"UPDATE "+spaceWelcomeTable+" SET status=?, error_class=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusUnknown, errClass, now, id, swStatusDispatching, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas to unknown: %w", err)
	}
	return ok, nil
}

// casPreIMFailure is the ONLY place attempts grows. A definitive pre-IM failure
// (observed while status=claimed) either backs the row off to pending
// (attempts+1, next_retry_at = now + backoff(new_attempts)) or, after the 4th
// consecutive failure, moves it to terminal failed. CAS guarded by claim_owner.
func (s *spaceWelcomeStore) casPreIMFailure(ctx context.Context, id int64, attempts int, errClass string, now time.Time) (bool, error) {
	newAttempts := attempts + 1
	if newAttempts > swMaxPreIMAttempts {
		ok, err := s.cas(ctx,
			"UPDATE "+spaceWelcomeTable+" SET status=?, attempts=?, error_class=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
				"WHERE id=? AND status=? AND claim_owner=?",
			swStatusFailed, newAttempts, errClass, now, id, swStatusClaimed, s.claimOwner,
		)
		if err != nil {
			return false, fmt.Errorf("cas to failed: %w", err)
		}
		return ok, nil
	}
	nextRetry := now.Add(swBackoff[newAttempts-1])
	ok, err := s.cas(ctx,
		"UPDATE "+spaceWelcomeTable+" SET status=?, attempts=?, error_class=?, next_retry_at=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusPending, newAttempts, errClass, nextRetry, now, id, swStatusClaimed, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas pre-IM retry: %w", err)
	}
	return ok, nil
}

// precheckResult is the outcome of the pre-send recipient re-check.
type precheckResult struct {
	eligible bool
	errClass string // set when !eligible: member_left | human_filter | orphan_member
}

// precheckRecipient re-checks recipient eligibility with a direct DB read
// (bypassing the module's stale member cache): the user row must exist (not
// orphan), must not be a robot, must not be a system bot (recipient only), and
// must still be an active member of the target Space. A DB error is returned to
// the caller (treated as a definitive pre-IM failure). systemBot membership is
// scoped strictly to the recipient uid — the sender identity is never touched
// here.
func (s *spaceWelcomeStore) precheckRecipient(ctx context.Context, spaceID, uid string, isSystemBot func(string) bool) (precheckResult, error) {
	// System-bot exclusion is the PRIMARY self-DM guard: the sender uid
	// `notification` lives in pkg/space.SystemBots, so this drops it (and any
	// other system bot) as a recipient. The robot=1 check below is the backstop
	// — keep both; weakening the SystemBots map must not silently open a
	// notification→notification self-DM.
	if isSystemBot != nil && isSystemBot(uid) {
		return precheckResult{eligible: false, errClass: swErrHumanFilter}, nil
	}

	var robot int
	err := s.db.SelectBySql("SELECT robot FROM `user` WHERE uid=?", uid).LoadOneContext(ctx, &robot)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return precheckResult{eligible: false, errClass: swErrOrphanMember}, nil
		}
		return precheckResult{}, fmt.Errorf("precheck user robot: %w", err)
	}
	if robot == 1 {
		return precheckResult{eligible: false, errClass: swErrHumanFilter}, nil
	}

	var member int
	err = s.db.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		spaceID, uid,
	).LoadOneContext(ctx, &member)
	if err != nil {
		return precheckResult{}, fmt.Errorf("precheck membership: %w", err)
	}
	if member == 0 {
		return precheckResult{eligible: false, errClass: swErrMemberLeft}, nil
	}
	return precheckResult{eligible: true}, nil
}

// firstJoinHumanMember reports whether uid is an active human member of spaceID
// whose first-ever join (space_member.created_at, never reset on rejoin) is
// at/after activeFrom. It excludes robots and orphan members (JOIN user). The
// system-bot exclusion is applied by the caller (recipient-only scope). Used by
// the low-latency event path to decide whether to enqueue.
//
// Time-zone correctness: space_member.created_at is written by NOW() in the DB
// session wall clock (e.g. +08:00 in this deployment), while activeFrom is a UTC
// instant. Comparing them directly would be off by the session offset, so we
// compare epoch-to-epoch: UNIX_TIMESTAMP(created_at) interprets the column in the
// session TZ and yields a UTC epoch, matched against activeFrom.Unix(). Correct
// regardless of the DB session TZ or the driver loc setting.
func (s *spaceWelcomeStore) firstJoinHumanMember(ctx context.Context, spaceID, uid string, activeFrom time.Time) (bool, error) {
	var n int
	err := s.db.SelectBySql(
		"SELECT COUNT(*) FROM space_member sm "+
			"JOIN `user` u ON u.uid = sm.uid AND u.robot = 0 "+
			"WHERE sm.space_id = ? AND sm.uid = ? AND sm.status = 1 AND UNIX_TIMESTAMP(sm.created_at) >= ?",
		spaceID, uid, activeFrom.Unix(),
	).LoadOneContext(ctx, &n)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return false, fmt.Errorf("enqueue eligibility check: %w", err)
	}
	return n > 0, nil
}

// reconcileScan returns up to `limit` uids that are active human members of
// spaceID whose first join is at/after activeFrom and that still lack a ledger
// row. This is the periodic catch-up for dropped events, restarts and the
// manager addMembers path (which bypasses the SpaceMemberJoin event).
//
// The scan JOINs the ledger against space / space_member / user; the ledger's
// space_id/uid COLLATE matches those columns (utf8mb4_general_ci) so the JOIN
// cannot hit MySQL 1267.
//
// activeFrom (a UTC instant) is compared epoch-to-epoch against
// space_member.created_at via UNIX_TIMESTAMP — see firstJoinHumanMember for why
// this is TZ-correct regardless of the DB session zone.
func (s *spaceWelcomeStore) reconcileScan(ctx context.Context, spaceID string, activeFrom time.Time, systemBots []string, limit int) ([]string, error) {
	builder := s.db.SelectBySql(
		"SELECT sm.uid FROM space_member sm "+
			"JOIN `user` u ON u.uid = sm.uid AND u.robot = 0 "+
			"LEFT JOIN "+spaceWelcomeTable+" d ON d.space_id = sm.space_id AND d.uid = sm.uid "+
			"WHERE sm.space_id = ? AND sm.status = 1 AND UNIX_TIMESTAMP(sm.created_at) >= ? AND d.id IS NULL "+
			"AND sm.uid NOT IN ? "+
			"ORDER BY sm.created_at, sm.uid LIMIT ?",
		spaceID, activeFrom.Unix(), systemBots, limit,
	)
	var uids []string
	if _, err := builder.LoadContext(ctx, &uids); err != nil {
		return nil, fmt.Errorf("reconcile scan: %w", err)
	}
	return uids, nil
}

// spaceActive reports whether spaceID refers to an existing, non-dissolved
// Space (status=1). Used by the runtime re-validation, bounded by the caller's
// context.
func (s *spaceWelcomeStore) spaceActive(ctx context.Context, spaceID string) (bool, error) {
	if spaceID == "" {
		return false, nil
	}
	var n int
	err := s.db.SelectBySql("SELECT COUNT(*) FROM space WHERE space_id=? AND status=1", spaceID).LoadOneContext(ctx, &n)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return false, fmt.Errorf("space active check: %w", err)
	}
	return n > 0, nil
}

// countByStatus returns the number of ledger rows for spaceID in the given
// status. Used by the drive-loop / disable tests to assert claimed vs pending
// counts (and available for a future pending_backlog gauge).
func (s *spaceWelcomeStore) countByStatus(ctx context.Context, spaceID string, status int) (int64, error) {
	var n int64
	err := s.db.SelectBySql(
		"SELECT COUNT(*) FROM "+spaceWelcomeTable+" WHERE space_id=? AND status=?",
		spaceID, status,
	).LoadOneContext(ctx, &n)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return 0, err
	}
	return n, nil
}
