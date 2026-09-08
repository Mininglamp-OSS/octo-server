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
// fn must own its whole transaction: a retry re-runs it from BEGIN, which is only
// sound because InnoDB has already rolled the failed attempt back. Anything fn
// wrote — including outbox rows — is gone with it, so a retry cannot duplicate a
// side effect that lives in the same transaction.
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
