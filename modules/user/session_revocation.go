package user

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

const (
	sessionRevocationPending = 0
	sessionRevocationApplied = 1

	sessionRevocationLease     = 30 * time.Second
	sessionRevocationBatchSize = 20
	postCommitSecurityTimeout  = 3 * time.Second
	// Newly committed intents are reserved briefly for their synchronous
	// by-ID claimant. Shared workers take over after the window if the request
	// process exits or cannot complete the Redis revocation.
	sessionRevocationSyncPriorityWindow = 5 * time.Second
)

var allowedSessionRevocationReasons = map[string]bool{
	"password_change": true,
	"password_reset":  true,
	"account_disable": true,
	"account_destroy": true,
	"admin_delete":    true,
}

type sessionRevocationIntent struct {
	ID           uint64     `db:"id"`
	EventID      string     `db:"event_id"`
	UID          string     `db:"uid"`
	EventVersion uint64     `db:"event_version"`
	Reason       string     `db:"reason"`
	Attempts     uint32     `db:"attempts"`
	LeaseOwner   string     `db:"lease_owner"`
	LeaseUntil   *time.Time `db:"lease_until"`
}

type sessionRevocationMutation func(context.Context, *dbr.Tx) error

func sessionRevocationActive(store userSessionStore) bool {
	v3Store, ok := store.(v3UserSessionStore)
	return ok && v3Store.Mode().RevokesSessions()
}

func (d *DB) updateUserFieldWithSessionRevocation(ctx context.Context, uid, field string, value interface{}, reason string) (*sessionRevocationIntent, error) {
	if !allowedUpdateFields[field] {
		return nil, fmt.Errorf("field %q is not allowed for update", field)
	}
	return d.mutateWithSessionRevocation(ctx, uid, reason, func(ctx context.Context, tx *dbr.Tx) error {
		result, err := tx.Update("user").Set(field, value).Where("uid=?", uid).ExecContext(ctx)
		if err != nil {
			return fmt.Errorf("user: update %s with session revocation: %w", field, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("user: read %s update result: %w", field, err)
		}
		if affected == 0 {
			return errors.New("user: revocation mutation target not found or unchanged")
		}
		return nil
	})
}

func (d *DB) destroyAccountWithSessionRevocation(ctx context.Context, uid, username, phone string, expectedState int) (*sessionRevocationIntent, error) {
	return d.mutateWithSessionRevocation(ctx, uid, "account_destroy", func(ctx context.Context, tx *dbr.Tx) error {
		result, err := tx.Update("user").SetMap(map[string]interface{}{
			"phone":             phone,
			"username":          username,
			"is_destroy":        IsDestroyDone,
			"destroy_apply_at":  nil,
			"destroy_expire_at": nil,
		}).Where("uid=? AND is_destroy=?", uid, expectedState).ExecContext(ctx)
		if err != nil {
			return fmt.Errorf("destroy account with session revocation: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("destroy account with session revocation rows: %w", err)
		}
		if affected == 0 {
			return ErrDestroyStateConflict
		}
		return nil
	})
}

func (d *DB) deleteAdminWithSessionRevocation(ctx context.Context, uid, role string) (*sessionRevocationIntent, error) {
	return d.mutateWithSessionRevocation(ctx, uid, "admin_delete", func(ctx context.Context, tx *dbr.Tx) error {
		result, err := tx.DeleteFrom("user").Where("uid=? AND role=?", uid, role).ExecContext(ctx)
		if err != nil {
			return fmt.Errorf("delete admin with session revocation: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete admin with session revocation rows: %w", err)
		}
		if affected == 0 {
			return errors.New("user: admin revocation target not found")
		}
		return nil
	})
}

func updateUserFieldAndRevokeSessionsWithResult(ctx context.Context, db *DB, store userSessionStore, uid, field string, value interface{}, reason string) (bool, error) {
	if !sessionRevocationActive(store) {
		err := db.UpdateUsersWithField(field, fmt.Sprintf("%v", value), uid)
		return err == nil, err
	}
	intent, err := db.updateUserFieldWithSessionRevocation(ctx, uid, field, value, reason)
	if err != nil {
		return false, err
	}
	return true, applyAndMarkSessionRevocation(ctx, db, store, intent)
}

type quitUserDevicesFunc func(uid string, deviceFlag int) error

func postCommitSecurityContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), postCommitSecurityTimeout)
}

// finishCommittedUserSecurityMutation preserves the database commit boundary:
// a rolled-back mutation has no external effects, while a committed mutation
// always attempts HTTP bearer revocation and the independent WuKongIM logout.
// In revoke/enforce modes the durable intent already owns bearer revocation;
// earlier rollout modes use the compatibility indexes through Session Store.
func finishCommittedUserSecurityMutation(ctx context.Context, store userSessionStore, uid string, committed bool, mutationErr error, quit quitUserDevicesFunc) error {
	if !committed {
		return mutationErr
	}
	securityCtx, cancel := postCommitSecurityContext(ctx)
	defer cancel()
	result := mutationErr
	if !sessionRevocationActive(store) {
		for _, failure := range revokeDeviceTokens(securityCtx, store, uid) {
			result = errors.Join(result, fmt.Errorf("user: revoke device %d session: %w", failure.flag, failure.err))
		}
	}
	if quit == nil {
		return errors.Join(result, errors.New("user: quit user devices callback is required"))
	}
	if err := quit(uid, -1); err != nil {
		result = errors.Join(result, fmt.Errorf("user: quit all IM devices: %w", err))
	}
	return result
}

func updatePasswordAndRevokeSessions(ctx context.Context, db *DB, store userSessionStore, quit quitUserDevicesFunc, uid string, password interface{}, reason string) error {
	committed, err := updateUserFieldAndRevokeSessionsWithResult(ctx, db, store, uid, "password", password, reason)
	return finishCommittedUserSecurityMutation(ctx, store, uid, committed, err, quit)
}

func (d *DB) mutateWithSessionRevocation(ctx context.Context, uid, reason string, mutation sessionRevocationMutation) (*sessionRevocationIntent, error) {
	if uid == "" || !allowedSessionRevocationReasons[reason] || mutation == nil {
		return nil, errors.New("user: invalid session revocation mutation")
	}
	tx, err := d.session.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("user: begin revocation transaction: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := mutation(ctx, tx); err != nil {
		return nil, err
	}
	if _, err := tx.InsertBySql(
		"INSERT INTO user_session_revocation_version (uid, version) VALUES (?, 0) ON DUPLICATE KEY UPDATE uid=VALUES(uid)",
		uid,
	).ExecContext(ctx); err != nil {
		return nil, fmt.Errorf("user: initialize revocation version: %w", err)
	}
	var current uint64
	if _, err := tx.Select("version").From("user_session_revocation_version").Where("uid=?", uid).Suffix("FOR UPDATE").LoadContext(ctx, &current); err != nil {
		return nil, fmt.Errorf("user: lock revocation version: %w", err)
	}
	next := current + 1
	if next == 0 {
		return nil, errors.New("user: session revocation version exhausted")
	}
	if _, err := tx.UpdateBySql(
		"UPDATE user_session_revocation_version SET version=?, updated_at=? WHERE uid=? AND version=?",
		next, time.Now().UTC(), uid, current,
	).ExecContext(ctx); err != nil {
		return nil, fmt.Errorf("user: advance revocation version: %w", err)
	}
	intent := &sessionRevocationIntent{
		EventID:      util.GenerUUID(),
		UID:          uid,
		EventVersion: next,
		Reason:       reason,
	}
	workerVisibleAt := time.Now().UTC().Add(sessionRevocationSyncPriorityWindow)
	result, err := tx.InsertInto("user_session_revocation_intent").
		Columns("event_id", "uid", "event_version", "reason", "status", "next_attempt_at").
		Values(intent.EventID, uid, next, reason, sessionRevocationPending, workerVisibleAt).
		ExecContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("user: insert session revocation intent: %w", err)
	}
	lastID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("user: read session revocation intent id: %w", err)
	}
	intent.ID = uint64(lastID)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("user: commit revocation transaction: %w", err)
	}
	return intent, nil
}

func (d *DB) claimSessionRevocation(ctx context.Context, owner string, now time.Time) (*sessionRevocationIntent, error) {
	if owner == "" {
		return nil, errors.New("user: revocation claim owner required")
	}
	tx, err := d.session.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("user: begin revocation claim: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var intent sessionRevocationIntent
	err = tx.SelectBySql(
		"SELECT id,event_id,uid,event_version,reason,attempts,lease_owner,lease_until "+
			"FROM user_session_revocation_intent "+
			"WHERE status=? AND next_attempt_at<=? AND (lease_until IS NULL OR lease_until<=?) "+
			"ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED",
		sessionRevocationPending, now, now,
	).LoadOneContext(ctx, &intent)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("user: select session revocation intent: %w", err)
	}
	result, err := tx.UpdateBySql(
		"UPDATE user_session_revocation_intent SET lease_owner=?,lease_until=? WHERE id=? AND status=?",
		owner, now.Add(sessionRevocationLease), intent.ID, sessionRevocationPending,
	).ExecContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("user: claim session revocation intent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return nil, fmt.Errorf("user: invalid session revocation claim result")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("user: commit session revocation claim: %w", err)
	}
	intent.LeaseOwner = owner
	leaseUntil := now.Add(sessionRevocationLease)
	intent.LeaseUntil = &leaseUntil
	return &intent, nil
}

func (d *DB) pendingSessionRevocation(ctx context.Context, uid, reason string) (*sessionRevocationIntent, error) {
	if uid == "" || !allowedSessionRevocationReasons[reason] {
		return nil, errors.New("user: invalid pending session revocation query")
	}
	var intent sessionRevocationIntent
	err := d.session.SelectBySql(
		"SELECT id,event_id,uid,event_version,reason,attempts,lease_owner,lease_until "+
			"FROM user_session_revocation_intent WHERE uid=? AND reason=? AND status=? "+
			"ORDER BY event_version DESC LIMIT 1",
		uid, reason, sessionRevocationPending,
	).LoadOneContext(ctx, &intent)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("user: load pending session revocation intent: %w", err)
	}
	return &intent, nil
}

func (d *DB) claimSessionRevocationByID(ctx context.Context, intent *sessionRevocationIntent, owner string, now time.Time) (bool, error) {
	if intent == nil || intent.ID == 0 || owner == "" {
		return false, errors.New("user: synchronous revocation claim requires intent and owner")
	}
	result, err := d.session.UpdateBySql(
		"UPDATE user_session_revocation_intent SET lease_owner=?,lease_until=? "+
			"WHERE id=? AND status=? AND (lease_until IS NULL OR lease_until<=?)",
		owner, now.Add(sessionRevocationLease), intent.ID, sessionRevocationPending, now,
	).ExecContext(ctx)
	if err != nil {
		return false, fmt.Errorf("user: claim synchronous session revocation intent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("user: read synchronous revocation claim result: %w", err)
	}
	if affected == 1 {
		intent.LeaseOwner = owner
		leaseUntil := now.Add(sessionRevocationLease)
		intent.LeaseUntil = &leaseUntil
		return true, nil
	}
	var status uint8
	_, loadErr := d.session.Select("status").From("user_session_revocation_intent").Where("id=?", intent.ID).LoadContext(ctx, &status)
	if loadErr == nil && status == sessionRevocationApplied {
		return false, nil
	}
	return false, errors.New("user: session revocation lease ownership lost")
}

func (d *DB) markSessionRevocationApplied(ctx context.Context, intent *sessionRevocationIntent, owner string) error {
	query := "UPDATE user_session_revocation_intent SET status=?,applied_at=?,lease_owner='',lease_until=NULL,last_error_code='' WHERE id=? AND status=?"
	args := []interface{}{sessionRevocationApplied, time.Now().UTC(), intent.ID, sessionRevocationPending}
	if owner != "" {
		query += " AND lease_owner=?"
		args = append(args, owner)
	}
	result, err := d.session.UpdateBySql(query, args...).ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("user: mark session revocation applied: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("user: read revocation applied result: %w", err)
	}
	if affected == 0 {
		var status uint8
		_, loadErr := d.session.Select("status").From("user_session_revocation_intent").Where("id=?", intent.ID).LoadContext(ctx, &status)
		if loadErr == nil && status == sessionRevocationApplied {
			return nil
		}
		return errors.New("user: session revocation lease ownership lost")
	}
	return nil
}

func (d *DB) releaseSessionRevocation(ctx context.Context, intent *sessionRevocationIntent, owner string) error {
	delay := sessionRevocationRetryDelay(intent.Attempts + 1)
	result, err := d.session.UpdateBySql(
		"UPDATE user_session_revocation_intent SET attempts=attempts+1,next_attempt_at=?,lease_owner='',lease_until=NULL,last_error_code=? "+
			"WHERE id=? AND status=? AND lease_owner=?",
		time.Now().UTC().Add(delay), "redis_apply_failed", intent.ID, sessionRevocationPending, owner,
	).ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("user: release session revocation intent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return errors.New("user: session revocation lease ownership lost on release")
	}
	return nil
}

func sessionRevocationRetryDelay(attempt uint32) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	delay := time.Second * time.Duration(1<<attempt)
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func applySessionRevocation(ctx context.Context, store userSessionStore, intent *sessionRevocationIntent) error {
	v3Store, ok := store.(v3UserSessionStore)
	if !ok || !v3Store.Mode().RevokesSessions() {
		return errors.New("user: session revocation rollout mode is not active")
	}
	err := v3Store.RevokeAll(ctx, intent.UID, auth.RevocationEvent{Version: intent.EventVersion, ID: intent.EventID})
	if errors.Is(err, auth.ErrRevocationAlreadyApplied) {
		return nil
	}
	return err
}

func (u *User) applySessionRevocationIntent(ctx context.Context, intent *sessionRevocationIntent) error {
	return applyAndMarkSessionRevocation(ctx, u.db, u.sessionStore, intent)
}

func applyAndMarkSessionRevocation(ctx context.Context, db *DB, store userSessionStore, intent *sessionRevocationIntent) error {
	securityCtx, cancel := postCommitSecurityContext(ctx)
	defer cancel()
	owner := "sync-" + util.GenerUUID()
	claimed, err := db.claimSessionRevocationByID(securityCtx, intent, owner, time.Now().UTC())
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	if err := applySessionRevocation(securityCtx, store, intent); err != nil {
		if releaseErr := db.releaseSessionRevocation(securityCtx, intent, owner); releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
		return err
	}
	return db.markSessionRevocationApplied(securityCtx, intent, owner)
}

func (u *User) processPendingSessionRevocations() {
	v3Store, ok := u.sessionStore.(v3UserSessionStore)
	if !ok || !v3Store.Mode().RevokesSessions() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	u.observeSessionRevocationBacklog(ctx)
	defer func() {
		observeCtx, observeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer observeCancel()
		u.observeSessionRevocationBacklog(observeCtx)
	}()
	for processed := 0; processed < sessionRevocationBatchSize; processed++ {
		intent, err := u.db.claimSessionRevocation(ctx, u.revocationWorkerOwner, time.Now().UTC())
		if err != nil {
			metrics.ObserveSessionRevocationRetry("claim")
			u.Error("领取会话撤销任务失败", zap.Error(err))
			return
		}
		if intent == nil {
			return
		}
		if err := applySessionRevocation(ctx, u.sessionStore, intent); err != nil {
			metrics.ObserveSessionRevocationRetry("redis_apply")
			if releaseErr := u.db.releaseSessionRevocation(ctx, intent, u.revocationWorkerOwner); releaseErr != nil {
				metrics.ObserveSessionRevocationRetry("release")
				u.Error("释放会话撤销任务失败", zap.String("subject", sessionRevocationSubject(intent.UID)), zap.Error(releaseErr))
			}
			u.Warn("应用会话撤销任务失败", zap.String("subject", sessionRevocationSubject(intent.UID)), zap.String("reason", intent.Reason))
			continue
		}
		if err := u.db.markSessionRevocationApplied(ctx, intent, u.revocationWorkerOwner); err != nil {
			metrics.ObserveSessionRevocationRetry("mark_applied")
			u.Error("完成会话撤销任务失败", zap.String("subject", sessionRevocationSubject(intent.UID)), zap.Error(err))
			return
		}
	}
}

func (u *User) observeSessionRevocationBacklog(ctx context.Context) {
	var backlog int64
	_, err := u.db.session.Select("COUNT(*)").From("user_session_revocation_intent").Where("status=?", sessionRevocationPending).LoadContext(ctx, &backlog)
	if err != nil {
		u.Warn("查询会话撤销积压失败", zap.Error(err))
		return
	}
	metrics.SetSessionRevocationBacklog(backlog)
}

func sessionRevocationSubject(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return fmt.Sprintf("%x", sum[:8])
}
