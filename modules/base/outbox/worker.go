package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

var errOutboxNotInitialized = errors.New("outbox: component is not initialized")

// ScanIntervalFromEnv resolves the supported fallback scan intervals. Values
// outside the explicit 3/5/10 minute set fail closed to the three-minute
// default rather than silently creating an unbounded or overly frequent loop.
func ScanIntervalFromEnv() time.Duration {
	raw := strings.TrimSpace(getenv(ScanIntervalEnv))
	minutes, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultScanInterval
	}
	switch minutes {
	case 3, 5, 10:
		return time.Duration(minutes) * time.Minute
	default:
		return DefaultScanInterval
	}
}

// getenv is a variable so focused tests can replace environment access without
// changing the public API. Production uses os.Getenv through init assignment.
var getenv = func(key string) string {
	return os.Getenv(key)
}

// RunWorker runs the durable fallback loop until ctx is canceled. The first
// pass is immediate; subsequent passes are separated by interval. Redis and DB
// failures are logged and retried on the next pass rather than terminating the
// process.
func RunWorker(ctx context.Context, interval time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	interval = normalizeInterval(interval)
	for {
		if ctx.Err() != nil {
			return
		}
		if err := runWorkerOnce(ctx, interval); err != nil {
			outboxLog.Error("event outbox worker pass failed", zap.Error(err))
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

// DeliverNow performs the post-commit fast path for one event. A Redis failure
// is deliberately log-only: pending rows and their attempt counters remain
// untouched for the fallback worker. A successful XADD is followed by a
// one-way delivered update; if that update fails, the worker may legitimately
// redeliver the same event after the crash window.
func DeliverNow(eventID string) {
	if strings.TrimSpace(eventID) == "" {
		return
	}
	db, client := snapshotState()
	if db == nil || client == nil {
		outboxLog.Warn("event delivery skipped because outbox is not initialized", zap.String("event_id", eventID))
		return
	}

	var rows []outboxRow
	if _, err := db.SelectBySql(
		"SELECT id,event_id,domain,resource_id,event_type,target_service,payload,status,attempts,next_attempt_at,lease_owner,lease_until,created_at,delivered_at FROM event_outbox WHERE event_id=? AND status=? ORDER BY id",
		eventID,
		outboxStatusPending,
	).Load(&rows); err != nil {
		outboxLog.Error("load event outbox rows for immediate delivery failed", zap.String("event_id", eventID), zap.Error(err))
		return
	}
	for _, row := range rows {
		if !targetEnabled(row.Domain, row.TargetService) {
			continue
		}
		if err := addToStream(client, row); err != nil {
			logDeliveryError(row, err)
			continue
		}
		if err := markDeliveredWithSession(context.Background(), db, row.ID, ""); err != nil {
			outboxLog.Error("mark event outbox row delivered failed",
				zap.String("event_id", row.EventID),
				zap.String("target_service", row.TargetService),
				zap.Error(err))
		}
	}
}

func runWorkerOnce(ctx context.Context, interval time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	db, client := snapshotState()
	if db == nil || client == nil {
		return errOutboxNotInitialized
	}
	interval = normalizeInterval(interval)
	now := time.Now().UTC()
	owner := workerOwner()
	rows, err := claimBatchWithSession(ctx, db, owner, now)
	if err != nil {
		return err
	}

	var firstErr error
	for _, row := range rows {
		if !targetEnabled(row.Domain, row.TargetService) {
			if releaseErr := releaseUnavailableWithSession(ctx, db, row, owner, interval, time.Now().UTC()); releaseErr != nil && firstErr == nil {
				firstErr = releaseErr
			}
			continue
		}
		if err := addToStream(client, row); err != nil {
			logDeliveryError(row, err)
			if releaseErr := releaseFailedWithSession(ctx, db, row, owner, err, interval, time.Now().UTC()); releaseErr != nil {
				outboxLog.Error("release failed event outbox row failed",
					zap.String("event_id", row.EventID),
					zap.String("target_service", row.TargetService),
					zap.Error(releaseErr))
				if firstErr == nil {
					firstErr = releaseErr
				}
			} else if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if markErr := markDeliveredWithSession(ctx, db, row.ID, owner); markErr != nil {
			outboxLog.Error("mark event outbox row delivered failed",
				zap.String("event_id", row.EventID),
				zap.String("target_service", row.TargetService),
				zap.Error(markErr))
			if firstErr == nil {
				firstErr = markErr
			}
		}
	}
	if err := cleanupDeliveredWithSession(ctx, db, time.Now().UTC()); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func claimBatch(ctx context.Context, owner string, now time.Time) ([]outboxRow, error) {
	db, _ := snapshotState()
	return claimBatchWithSession(ctx, db, owner, now)
}

func claimBatchWithSession(ctx context.Context, db *dbr.Session, owner string, now time.Time) ([]outboxRow, error) {
	if db == nil {
		return nil, errOutboxNotInitialized
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if owner == "" {
		return nil, errors.New("outbox: claim owner is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	batchSize := effectiveClaimBatchSize()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("outbox: begin claim transaction: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	rows := make([]outboxRow, 0, batchSize)
	if _, err := tx.SelectBySql(
		"SELECT id,event_id,domain,resource_id,event_type,target_service,payload,status,attempts,next_attempt_at,last_error,lease_owner,lease_until,created_at,delivered_at FROM event_outbox WHERE status=? AND next_attempt_at<=? AND (lease_until IS NULL OR lease_until<=?) ORDER BY next_attempt_at,id LIMIT ? FOR UPDATE SKIP LOCKED",
		outboxStatusPending,
		now,
		now,
		batchSize,
	).LoadContext(ctx, &rows); err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("outbox: select claimable rows: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	leaseUntil := now.Add(effectiveLeaseDuration())
	ids := make([]uint64, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	result, err := tx.UpdateBySql(
		"UPDATE event_outbox SET lease_owner=?,lease_until=? WHERE id IN ? AND status=? AND (lease_until IS NULL OR lease_until<=?)",
		owner,
		leaseUntil,
		ids,
		outboxStatusPending,
		now,
	).ExecContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("outbox: claim rows: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("outbox: read claim result: %w", err)
	}
	if affected != int64(len(rows)) {
		return nil, fmt.Errorf("outbox: claim affected %d rows, selected %d", affected, len(rows))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("outbox: commit claim transaction: %w", err)
	}
	for i := range rows {
		rows[i].LeaseOwner = sql.NullString{String: owner, Valid: true}
		lease := leaseUntil
		rows[i].LeaseUntil = &lease
	}
	return rows, nil
}

func addToStream(client *redis.Client, row outboxRow) error {
	if client == nil {
		return errOutboxNotInitialized
	}
	if row.Domain == "" || row.TargetService == "" {
		return errors.New("outbox: row has no stream target")
	}
	if row.Payload == "" {
		return errors.New("outbox: row has empty payload")
	}
	stream := streamKey(row.Domain, row.TargetService)
	if _, err := client.XAdd(&redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{"event_json": row.Payload},
	}).Result(); err != nil {
		return fmt.Errorf("xadd %s: %w", stream, err)
	}
	return nil
}

func markDelivered(ctx context.Context, id uint64, owner string) error {
	db, _ := snapshotState()
	return markDeliveredWithSession(ctx, db, id, owner)
}

func markDeliveredWithSession(ctx context.Context, db *dbr.Session, id uint64, _ string) error {
	if db == nil {
		return errOutboxNotInitialized
	}
	if id == 0 {
		return errors.New("outbox: row ID is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := db.UpdateBySql(
		"UPDATE event_outbox SET status=?,delivered_at=?,last_error=NULL,lease_owner=NULL,lease_until=NULL WHERE id=? AND status=?",
		outboxStatusDelivered,
		time.Now().UTC(),
		id,
		outboxStatusPending,
	).ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("outbox: mark delivered: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("outbox: read mark delivered result: %w", err)
	}
	if affected == 0 {
		// A late worker is allowed to observe a row already marked delivered. The
		// status predicate makes this transition one-way and prevents any stale
		// failure from moving it back to pending.
		return nil
	}
	return nil
}

func releaseUnavailableWithSession(ctx context.Context, db *dbr.Session, row outboxRow, owner string, interval time.Duration, now time.Time) error {
	if db == nil {
		return errOutboxNotInitialized
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := db.UpdateBySql(
		"UPDATE event_outbox SET next_attempt_at=?,lease_owner=NULL,lease_until=NULL WHERE id=? AND status=? AND lease_owner=?",
		now.Add(normalizeInterval(interval)), row.ID, outboxStatusPending, owner,
	).ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("outbox: release unavailable target row: %w", err)
	}
	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("outbox: read unavailable target release result: %w", err)
	}
	return nil
}

func releaseFailed(ctx context.Context, row outboxRow, owner string, cause error, interval time.Duration) error {
	db, _ := snapshotState()
	return releaseFailedWithSession(ctx, db, row, owner, cause, interval, time.Now().UTC())
}

func releaseFailedWithSession(ctx context.Context, db *dbr.Session, row outboxRow, owner string, cause error, interval time.Duration, now time.Time) error {
	if db == nil {
		return errOutboxNotInitialized
	}
	if row.ID == 0 || owner == "" {
		return errors.New("outbox: failed delivery requires row and owner")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	attempts := row.Attempts
	if attempts < math.MaxUint32 {
		attempts++
	}
	nextAttemptAt := now.Add(retryDelay(attempts, interval))
	result, err := db.UpdateBySql(
		"UPDATE event_outbox SET attempts=?,last_error=?,next_attempt_at=?,lease_owner=NULL,lease_until=NULL WHERE id=? AND status=? AND lease_owner=?",
		attempts,
		errorSummary(cause),
		nextAttemptAt,
		row.ID,
		outboxStatusPending,
		owner,
	).ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("outbox: release failed row: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("outbox: read release failed result: %w", err)
	}
	if affected == 0 {
		// Another worker may have reclaimed an expired lease or marked the row
		// delivered. Never overwrite that owner's state.
		return nil
	}
	return nil
}

func retryDelay(attempts uint32, interval time.Duration) time.Duration {
	interval = normalizeInterval(interval)
	if attempts == 0 {
		return interval
	}
	shift := attempts - 1
	if shift > 5 {
		shift = 5
	}
	multiplier := time.Duration(1 << shift)
	if interval > time.Hour/multiplier {
		return time.Hour
	}
	return interval * multiplier
}

func cleanupDelivered(ctx context.Context, now time.Time) error {
	db, _ := snapshotState()
	return cleanupDeliveredWithSession(ctx, db, now)
}

func cleanupDeliveredWithSession(ctx context.Context, db *dbr.Session, now time.Time) error {
	if db == nil {
		return errOutboxNotInitialized
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-effectiveDeliveredRetention())
	batchSize := effectiveClaimBatchSize()
	for {
		result, err := db.UpdateBySql(
			"DELETE FROM event_outbox WHERE status=? AND delivered_at IS NOT NULL AND delivered_at<=? ORDER BY id LIMIT ?",
			outboxStatusDelivered,
			cutoff,
			batchSize,
		).ExecContext(ctx)
		if err != nil {
			return fmt.Errorf("outbox: cleanup delivered rows: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("outbox: read cleanup result: %w", err)
		}
		if deleted < int64(batchSize) {
			return nil
		}
	}
}

func workerOwner() string {
	return "octo-outbox-" + uuid.New().String()
}
