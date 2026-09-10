package db

import (
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

// LockRetryAttempts bounds the retry budget for transient lock conflicts. Three:
// enough for the pathological interleaving to have passed, few enough that a
// genuine hot spot surfaces as an error rather than as latency.
const LockRetryAttempts = 3

// IsRetryableLockErr reports whether err is a TRANSIENT InnoDB lock conflict —
// 1213 (deadlock) or 1205 (lock wait timeout) — and nothing else.
//
// Everything else, including 1062 and every service sentinel, is NOT retryable:
// callers rely on errors.Is against their own sentinels, and re-running a
// transaction that failed for a permanent reason only moves the failure later.
func IsRetryableLockErr(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1213 || myErr.Number == 1205
	}
	return false
}

// RetryOnLockConflict re-runs fn while it fails with a transient lock conflict.
//
// This is the canonical copy. modules/project had the first one (its unexported
// retryOnLockConflict now delegates here) and modules/common still carries a
// third, unexported, for its own callers; converging that one is a separate
// change because it would touch paths this transaction work does not.
//
// Why a shared helper rather than per-module reasoning about lock order: three
// consecutive review rounds on the project module each found a lock-order cycle
// that careful reasoning had missed — first a table-order cycle against
// modules/space, then a row-order cycle within space_member, then a join-order
// inversion the OPTIMIZER chose (a single UPDATE ... JOIN whose driving table
// flips with cardinality, so the lock order is not even a property of the SQL
// text). "We reasoned about the order" is demonstrably not sufficient on its own,
// and the cost of being wrong is not a retry — it is a 500 on a security-relevant
// write, or a rolled-back member removal.
//
// fn must own its whole transaction — meaning it BEGINs, and it rolls back on every
// exit it does not commit on. That requirement, not the engine, is what makes a
// retry from BEGIN sound.
//
// Stating it that way because the reason given here before was wrong for half the
// retryable set. It said the retry is safe "because InnoDB has already rolled the
// failed attempt back", which is true for 1213 — a deadlock victim is rolled back
// whole — and FALSE for 1205: with innodb_rollback_on_timeout at its default OFF, a
// lock wait timeout rolls back only the failing STATEMENT and leaves the transaction
// open. Every fn today is a *Once function holding `defer tx.RollbackUnlessCommitted()`,
// so the transaction does get discarded and the helper is sound — but by the caller's
// discipline, not by the engine's. The wrong reason would have licensed an fn that
// leaves its rollback to InnoDB, and that fn would re-run from BEGIN with the previous
// attempt's statements still holding locks.
//
// Anything fn wrote — including outbox rows — is gone with the rollback, so a retry
// cannot duplicate a side effect that lives in the same transaction.
//
// # Known, deliberately not changed here: the 1205 latency budget
//
// Retries are immediate and identical for both codes. On 1205 the caller has already
// waited innodb_lock_wait_timeout — measured at 50s on the 8.0.46 engine used for this
// branch — so three attempts is up to 150s of one request holding a pooled connection.
// A backoff would make that worse rather than better, and the right shape is probably
// to retry 1213 with jitter and surface 1205 immediately. That is a behaviour change
// across eight call sites in two modules, so it belongs in its own change with its own
// load test, not in a branch about membership epochs.
func RetryOnLockConflict(fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < LockRetryAttempts; attempt++ {
		err := fn()
		if err == nil || !IsRetryableLockErr(err) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("db: transaction retries exhausted: %w", lastErr)
}
