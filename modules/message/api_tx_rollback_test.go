package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestTransactionRollbackOnError verifies that transactions are properly
// rolled back when an error occurs, preventing connection leaks.
// This test covers the fix for issue #720.
func TestTransactionRollbackOnError(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// Test that transaction with RollbackUnlessCommitted pattern
	// properly releases connection on early return
	t.Run("transaction rollback releases connection", func(t *testing.T) {
		db := ctx.DB()

		// Simulate the fixed pattern: defer RollbackUnlessCommitted after Begin
		tx, err := db.Begin()
		assert.NoError(t, err)
		defer tx.RollbackUnlessCommitted()

		// Simulate an error condition that causes early return
		// In the real code, this would be a GenSeq failure
		simulatedError := true
		if simulatedError {
			// Early return without explicit rollback
			// The defer will handle the rollback
			return
		}

		// This code path is not reached due to simulated error
		err = tx.Commit()
		assert.NoError(t, err)
	})

	// Verify we can still create new transactions after the rollback
	// If connections were leaking, this would eventually fail
	t.Run("connection available after rollback", func(t *testing.T) {
		db := ctx.DB()

		for i := 0; i < 5; i++ {
			tx, err := db.Begin()
			assert.NoError(t, err)
			defer tx.RollbackUnlessCommitted()

			// Simulate early return with rollback
			if i%2 == 0 {
				continue
			}

			err = tx.Commit()
			assert.NoError(t, err)
		}
	})

	// Test the panic recovery defer pattern doesn't interfere
	t.Run("panic recovery with rollback", func(t *testing.T) {
		db := ctx.DB()

		func() {
			tx, err := db.Begin()
			assert.NoError(t, err)
			defer tx.RollbackUnlessCommitted()
			defer func() {
				if r := recover(); r != nil {
					// Panic recovered, rollback will still happen via defer
				}
			}()

			// Normal exit without panic
		}()

		// Verify connection is still available
		tx, err := db.Begin()
		assert.NoError(t, err)
		err = tx.Commit()
		assert.NoError(t, err)
	})
}
