package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// groupWelcomeStore is the DB access layer for the group welcome delivery ledger.
// It mirrors spaceWelcomeStore (same status machine + CAS discipline) keyed by
// group_no instead of space_id, and drops the per-recipient `lang` (the group
// welcome is a single body posted to the channel, not a per-recipient DM).
//
// Time discipline: ALL timestamp writes take an application-computed UTC value as
// a bound parameter (never NOW()).
type groupWelcomeStore struct {
	db         *dbr.Session
	claimOwner string
}

func newGroupWelcomeStore(db *dbr.Session, claimOwner string) *groupWelcomeStore {
	return &groupWelcomeStore{db: db, claimOwner: claimOwner}
}

// upsertPending inserts a pending row for (groupNo, uid) if absent. inserted=true
// only when a new row was actually created; a duplicate hit reports false so the
// caller can split enqueue_total vs enqueue_dedup_total. The UNIQUE(group_no,uid)
// row is the at-most-once authority: leave→rejoin keeps the existing row, so the
// welcome is posted at most once even though group_member.created_at resets.
//
// dueAt is written to next_retry_at so a freshly-enqueued row is held back from
// the worker until the coalesce window elapses (see enqueue); co-arriving joins
// thereby collect and are claimed together into one post. On a duplicate the
// existing row (and its due time) is left untouched — a rejoin never re-arms the
// window nor resurrects a delivered row.
func (s *groupWelcomeStore) upsertPending(ctx context.Context, groupNo, uid string, now, dueAt time.Time) (inserted bool, err error) {
	res, err := s.db.InsertBySql(
		"INSERT INTO "+groupWelcomeTable+" (group_no, uid, status, attempts, next_retry_at, created_at, updated_at) "+
			"VALUES (?, ?, ?, 0, ?, ?, ?) ON DUPLICATE KEY UPDATE id = id",
		groupNo, uid, swStatusPending, dueAt, now, now,
	).ExecContext(ctx)
	if err != nil {
		return false, fmt.Errorf("upsert pending group welcome row: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read affected rows: %w", err)
	}
	return affected > 0, nil
}

// claimBatch atomically claims up to `limit` due pending rows for groupNo
// (SELECT ... FOR UPDATE SKIP LOCKED + one UPDATE ... WHERE id IN), leasing them
// all to this owner. Returns the claimed rows, or nil when none are due. Claiming
// a batch (not one row) is what lets a burst of joins coalesce into ONE post: the
// worker locks every due row for the group in a single tx and delivers them
// together. Each row still transitions independently afterwards (its own CAS), so
// at-most-once per (group_no, uid) is preserved.
func (s *groupWelcomeStore) claimBatch(ctx context.Context, groupNo string, limit int, now time.Time) ([]*groupWelcomeRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var rows []*groupWelcomeRow
	if _, err = tx.SelectBySql(
		"SELECT id, group_no, uid, attempts FROM "+groupWelcomeTable+" "+
			"WHERE group_no=? AND status=? AND (next_retry_at IS NULL OR next_retry_at<=?) "+
			"ORDER BY id LIMIT ? FOR UPDATE SKIP LOCKED",
		groupNo, swStatusPending, now, limit,
	).LoadContext(ctx, &rows); err != nil {
		return nil, fmt.Errorf("select claimable group welcome rows: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	if _, err = tx.UpdateBySql(
		"UPDATE "+groupWelcomeTable+" SET status=?, claim_owner=?, claim_expire_at=?, updated_at=? WHERE id IN ?",
		swStatusClaimed, s.claimOwner, now.Add(swClaimLease), now, ids,
	).ExecContext(ctx); err != nil {
		return nil, fmt.Errorf("mark group welcome rows claimed: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim tx: %w", err)
	}
	return rows, nil
}

// sweepClaimedAll recycles lease-expired claimed rows across ALL groups back to
// pending (index-backed by idx_sweep). attempts untouched.
func (s *groupWelcomeStore) sweepClaimedAll(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.UpdateBySql(
		"UPDATE "+groupWelcomeTable+" SET status=?, next_retry_at=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE status=? AND claim_expire_at IS NOT NULL AND claim_expire_at<=?",
		swStatusPending, now, now, swStatusClaimed, now,
	).ExecContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("sweep claimed group welcome rows (all groups): %w", err)
	}
	return res.RowsAffected()
}

// sweepDispatchingAll promotes lease-expired dispatching rows across ALL groups
// to unknown (transport-ambiguous; never auto-retried).
func (s *groupWelcomeStore) sweepDispatchingAll(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.UpdateBySql(
		"UPDATE "+groupWelcomeTable+" SET status=?, error_class=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE status=? AND claim_expire_at IS NOT NULL AND claim_expire_at<=?",
		swStatusUnknown, swErrClaimExpired, now, swStatusDispatching, now,
	).ExecContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("sweep dispatching group welcome rows (all groups): %w", err)
	}
	return res.RowsAffected()
}

// cas runs a CAS UPDATE guarded by id + expected status + claim_owner=self.
func (s *groupWelcomeStore) cas(ctx context.Context, query string, args ...interface{}) (bool, error) {
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

// casToDispatching transitions claimed -> dispatching, extending the lease.
func (s *groupWelcomeStore) casToDispatching(ctx context.Context, id int64, now time.Time) (bool, error) {
	ok, err := s.cas(ctx,
		"UPDATE "+groupWelcomeTable+" SET status=?, claim_expire_at=?, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusDispatching, now.Add(swClaimLease), now, id, swStatusClaimed, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas to dispatching: %w", err)
	}
	return ok, nil
}

// casToSent transitions dispatching -> sent, persisting the IM identifiers.
func (s *groupWelcomeStore) casToSent(ctx context.Context, id int64, messageID int64, clientMsgNo string, now time.Time) (bool, error) {
	ok, err := s.cas(ctx,
		"UPDATE "+groupWelcomeTable+" SET status=?, message_id=?, client_msg_no=?, error_class=NULL, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusSent, messageID, clientMsgNo, now, id, swStatusDispatching, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas to sent: %w", err)
	}
	return ok, nil
}

// casToSkipped transitions claimed -> skipped (joiner ineligible at pre-send).
func (s *groupWelcomeStore) casToSkipped(ctx context.Context, id int64, errClass string, now time.Time) (bool, error) {
	ok, err := s.cas(ctx,
		"UPDATE "+groupWelcomeTable+" SET status=?, error_class=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusSkipped, errClass, now, id, swStatusClaimed, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas to skipped: %w", err)
	}
	return ok, nil
}

// casToUnknown transitions dispatching -> unknown (transport-ambiguous).
func (s *groupWelcomeStore) casToUnknown(ctx context.Context, id int64, errClass string, now time.Time) (bool, error) {
	ok, err := s.cas(ctx,
		"UPDATE "+groupWelcomeTable+" SET status=?, error_class=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusUnknown, errClass, now, id, swStatusDispatching, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas to unknown: %w", err)
	}
	return ok, nil
}

// casPreIMFailure is the ONLY place attempts grows: back off to pending, or fail
// terminally after the 4th consecutive pre-IM failure. CAS guarded by claim_owner.
func (s *groupWelcomeStore) casPreIMFailure(ctx context.Context, id int64, attempts int, errClass string, now time.Time) (bool, error) {
	newAttempts := attempts + 1
	if newAttempts > swMaxPreIMAttempts {
		ok, err := s.cas(ctx,
			"UPDATE "+groupWelcomeTable+" SET status=?, attempts=?, error_class=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
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
		"UPDATE "+groupWelcomeTable+" SET status=?, attempts=?, error_class=?, next_retry_at=?, claim_owner=NULL, claim_expire_at=NULL, updated_at=? "+
			"WHERE id=? AND status=? AND claim_owner=?",
		swStatusPending, newAttempts, errClass, nextRetry, now, id, swStatusClaimed, s.claimOwner,
	)
	if err != nil {
		return false, fmt.Errorf("cas pre-IM retry: %w", err)
	}
	return ok, nil
}

// groupPrecheckResult is the outcome of the pre-send joiner re-check.
type groupPrecheckResult struct {
	eligible bool
	errClass string // set when !eligible: human_filter | orphan_member
	name     string // joiner display name for {member} rendering (set when eligible)
}

// userNameRow projects the joiner's robot flag + display name.
type userNameRow struct {
	Robot int    `db:"robot"`
	Name  string `db:"name"`
}

// precheckRecipient re-checks the joiner with a direct DB read (bypassing any
// cache): the user row must exist (not orphan), must not be a robot, and must not
// be a system bot. It resolves the joiner's display name for the {member}
// placeholder. It does NOT require current membership — the public group post is
// keyed to the first-join event and fires even if the member has since left
// (task group-welcome-message decision "post-then-leave still fires").
func (s *groupWelcomeStore) precheckRecipient(ctx context.Context, groupNo, uid string, activeFrom time.Time, isSystemBot func(string) bool) (groupPrecheckResult, error) {
	if isSystemBot != nil && isSystemBot(uid) {
		return groupPrecheckResult{eligible: false, errClass: swErrHumanFilter}, nil
	}
	var row userNameRow
	err := s.db.SelectBySql("SELECT robot, name FROM `user` WHERE uid=?", uid).LoadOneContext(ctx, &row)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return groupPrecheckResult{eligible: false, errClass: swErrOrphanMember}, nil
		}
		return groupPrecheckResult{}, fmt.Errorf("precheck user: %w", err)
	}
	if row.Robot == 1 {
		return groupPrecheckResult{eligible: false, errClass: swErrHumanFilter}, nil
	}

	// Re-validate the joiner against the CURRENT config's active_from window: a row
	// enqueued under an earlier config must not be delivered after an admin moved
	// active_from forward, or deleted+recreated the config with a later window
	// (a DELETE does not discard pending rows). We check only the join-time window
	// (`created_at >= active_from`), NOT current membership — leaving is intentional
	// ("post-then-leave still fires"), and rejoin dedup stays keyed on the ledger
	// UNIQUE, not this timestamp. created_at is a session-wall-clock DATETIME while
	// activeFrom is a UTC instant, so compare epoch-to-epoch via UNIX_TIMESTAMP (see
	// firstJoinHumanMember).
	var inWindow int
	err = s.db.SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND UNIX_TIMESTAMP(created_at) >= ?",
		groupNo, uid, activeFrom.Unix(),
	).LoadOneContext(ctx, &inWindow)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return groupPrecheckResult{}, fmt.Errorf("precheck active_from window: %w", err)
	}
	if inWindow == 0 {
		return groupPrecheckResult{eligible: false, errClass: swErrActiveFromMoved}, nil
	}

	name := row.Name
	if name == "" {
		name = uid // fall back to the uid if the display name is empty
	}
	return groupPrecheckResult{eligible: true, name: name}, nil
}

// firstJoinHumanMember reports whether uid is an active human member of groupNo
// whose current join (group_member.created_at) is at/after activeFrom. Excludes
// robots and orphan members (JOIN user). Used by the low-latency event path.
//
// NOTE: group_member.created_at is reset on rejoin (unlike space_member), so this
// gate identifies "currently qualifies as a first-join-window human member"; the
// ledger UNIQUE(group_no,uid) is what guarantees at-most-once across rejoins.
//
// TZ correctness: created_at is a session-wall-clock DATETIME while activeFrom is
// a UTC instant; compare epoch-to-epoch via UNIX_TIMESTAMP (see the Space
// firstJoinHumanMember for the full rationale).
func (s *groupWelcomeStore) firstJoinHumanMember(ctx context.Context, groupNo, uid string, activeFrom time.Time) (bool, error) {
	var n int
	err := s.db.SelectBySql(
		"SELECT COUNT(*) FROM group_member gm "+
			"JOIN `user` u ON u.uid = gm.uid AND u.robot = 0 "+
			"WHERE gm.group_no = ? AND gm.uid = ? AND gm.is_deleted = 0 AND gm.status = 1 AND UNIX_TIMESTAMP(gm.created_at) >= ?",
		groupNo, uid, activeFrom.Unix(),
	).LoadOneContext(ctx, &n)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return false, fmt.Errorf("group enqueue eligibility check: %w", err)
	}
	return n > 0, nil
}

// reconcileScan returns up to `limit` uids that are active human members of
// groupNo whose join is at/after activeFrom and still lack a ledger row. This is
// the periodic catch-up for dropped events, restarts and org/dept auto-add paths
// that bypass the GroupMemberAdd event.
func (s *groupWelcomeStore) reconcileScan(ctx context.Context, groupNo string, activeFrom time.Time, systemBots []string, limit int) ([]string, error) {
	builder := s.db.SelectBySql(
		"SELECT gm.uid FROM group_member gm "+
			"JOIN `user` u ON u.uid = gm.uid AND u.robot = 0 "+
			"LEFT JOIN "+groupWelcomeTable+" d ON d.group_no = gm.group_no AND d.uid = gm.uid "+
			"WHERE gm.group_no = ? AND gm.is_deleted = 0 AND gm.status = 1 AND UNIX_TIMESTAMP(gm.created_at) >= ? AND d.id IS NULL "+
			"AND gm.uid NOT IN ? "+
			"ORDER BY gm.created_at, gm.uid LIMIT ?",
		groupNo, activeFrom.Unix(), systemBots, limit,
	)
	var uids []string
	if _, err := builder.LoadContext(ctx, &uids); err != nil {
		return nil, fmt.Errorf("group reconcile scan: %w", err)
	}
	return uids, nil
}

// groupActive reports whether groupNo refers to an existing, non-disbanded group
// (status = GroupStatusNormal = 1). `group` is a reserved word (backticked).
func (s *groupWelcomeStore) groupActive(ctx context.Context, groupNo string) (bool, error) {
	if groupNo == "" {
		return false, nil
	}
	var n int
	err := s.db.SelectBySql("SELECT COUNT(*) FROM `group` WHERE group_no=? AND status=1", groupNo).LoadOneContext(ctx, &n)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return false, fmt.Errorf("group active check: %w", err)
	}
	return n > 0, nil
}

// countByStatus returns the number of ledger rows for groupNo in the given status.
func (s *groupWelcomeStore) countByStatus(ctx context.Context, groupNo string, status int) (int64, error) {
	var n int64
	err := s.db.SelectBySql(
		"SELECT COUNT(*) FROM "+groupWelcomeTable+" WHERE group_no=? AND status=?",
		groupNo, status,
	).LoadOneContext(ctx, &n)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return 0, err
	}
	return n, nil
}
