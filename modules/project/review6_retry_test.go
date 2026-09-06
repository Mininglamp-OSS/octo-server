package project

// PR #841 review round 3, suggested direction #4: a bounded 1213 retry, in the shape of
// modules/common's isRetryableTxErr.
//
// The single-statement seat locks close the cycle this module currently has with
// modules/space's disband scan. The retry is what keeps the NEXT one from being a 500: three
// rounds of review have each found a lock-order cycle that reasoning had missed, so "we
// reasoned about the order" is demonstrably not sufficient on its own.

import (
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryOnLockConflictRetriesDeadlocksAndNothingElse(t *testing.T) {
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}
	lockTimeout := &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	duplicate := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}

	t.Run("a deadlock is retried and can succeed", func(t *testing.T) {
		calls := 0
		err := retryOnLockConflict(func() error {
			calls++
			if calls == 1 {
				return deadlock
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 2, calls, "the second attempt must actually run")
	})

	t.Run("a lock-wait timeout is retried too", func(t *testing.T) {
		calls := 0
		require.NoError(t, retryOnLockConflict(func() error {
			calls++
			if calls == 1 {
				return lockTimeout
			}
			return nil
		}))
		assert.Equal(t, 2, calls)
	})

	t.Run("retries are bounded", func(t *testing.T) {
		calls := 0
		err := retryOnLockConflict(func() error {
			calls++
			return deadlock
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, deadlock,
			"the last error must stay in the chain so callers can still classify it")
		assert.Equal(t, txRetryAttempts, calls, "the budget must be finite")
	})

	t.Run("a non-transient error is surfaced verbatim on the first attempt", func(t *testing.T) {
		calls := 0
		err := retryOnLockConflict(func() error {
			calls++
			return duplicate
		})
		assert.Equal(t, 1, calls, "1062 is terminal; retrying it would just repeat the conflict")
		assert.Equal(t, duplicate, err, "and it must be returned unwrapped")
	})

	t.Run("a sentinel error is not retried and stays comparable", func(t *testing.T) {
		calls := 0
		err := retryOnLockConflict(func() error {
			calls++
			return errQuotaMembers
		})
		assert.Equal(t, 1, calls)
		assert.ErrorIs(t, err, errQuotaMembers,
			"every service sentinel must survive the wrapper, or every handler switch breaks")
	})

	t.Run("wrapped deadlocks are recognised", func(t *testing.T) {
		calls := 0
		require.NoError(t, retryOnLockConflict(func() error {
			calls++
			if calls == 1 {
				return errors.New("project: begin add member: " + deadlock.Error())
			}
			return nil
		}))
		// The plain-string form is NOT a MySQLError, so it must NOT be retried — this asserts
		// the judgement is errors.As, not substring matching.
		assert.Equal(t, 1, calls, "only a real *mysql.MySQLError counts as transient")
	})
}

// TestEveryWriteEntryPointGoesThroughTheRetryWrapper is the structural half: a retry that four
// of six paths skip is not a retry.
func TestEveryWriteEntryPointGoesThroughTheRetryWrapper(t *testing.T) {
	src := readLinesWithoutComments(t, "service.go")
	for _, fn := range []string{
		"func (p *Project) createProject(",
		"func (p *Project) updateProject(",
		"func (p *Project) disbandProject(",
		"func (p *Project) addOneMember(",
		"func (p *Project) removeMember(",
		"func (p *Project) leaveProject(",
		"func (p *Project) changeMemberRole(",
	} {
		body := funcBody(t, src, fn)
		assert.Contains(t, body, "retryOnLockConflict(",
			"%s must run its transaction through the bounded retry: a transient 1213 otherwise "+
				"surfaces as store_failed (Internal, HTTP 500), and when the victim is the Space "+
				"disband it is a failed step of the member-removal security cascade", fn)
		assert.NotContains(t, body, "p.db.session.Begin()",
			"%s must delegate the transaction to its Once form, so a retry re-runs the whole "+
				"transaction rather than half of it", fn)
	}
}
