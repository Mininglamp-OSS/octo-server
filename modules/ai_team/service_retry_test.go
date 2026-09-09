package ai_team

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryAITeamMutation(t *testing.T) {
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}
	lockTimeout := &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}

	t.Run("retries transient lock conflicts", func(t *testing.T) {
		attempts := 0
		err := retryAITeamMutation(func() error {
			attempts++
			if attempts < 3 {
				return fmt.Errorf("activate agent: %w", deadlock)
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 3, attempts)
	})

	t.Run("returns non-lock errors immediately", func(t *testing.T) {
		want := errors.New("forbidden")
		attempts := 0
		err := retryAITeamMutation(func() error {
			attempts++
			return want
		})
		assert.ErrorIs(t, err, want)
		assert.Equal(t, 1, attempts)
	})

	t.Run("returns lock wait timeout immediately", func(t *testing.T) {
		attempts := 0
		err := retryAITeamMutation(func() error {
			attempts++
			return lockTimeout
		})
		assert.ErrorIs(t, err, lockTimeout)
		assert.Equal(t, 1, attempts)
	})

	t.Run("preserves the final lock error", func(t *testing.T) {
		attempts := 0
		err := retryAITeamMutation(func() error {
			attempts++
			return deadlock
		})
		assert.ErrorIs(t, err, deadlock)
		assert.Equal(t, aiTeamMutationRetryAttempts, attempts)
	})
}
