package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

func TestLegacyMigrationRejectsCompletionAfterFinalMutationLockLoss(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	attacker := octoredis.NewInstrumentedClient(config.New())
	t.Cleanup(func() { _ = attacker.Close() })

	require.NoError(t, client.Set(store.tokenKey("final-lock-loss"), v2Fixture(t, "legacy"), 0).Err())

	checkpointKey := store.migrationCheckpointKey("final-lock-loss")
	var replaced atomic.Bool
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			if err := old(cmd); err != nil {
				return err
			}
			args := cmd.Args()
			if cmd.Name() != "set" || len(args) < 3 || fmt.Sprint(args[1]) != checkpointKey {
				return nil
			}
			var checkpoint legacyMigrationCheckpoint
			if err := json.Unmarshal([]byte(fmt.Sprint(args[2])), &checkpoint); err != nil {
				return fmt.Errorf("decode intercepted migration checkpoint: %w", err)
			}
			if checkpoint.Cursor != 0 || checkpoint.Result.Complete || !replaced.CompareAndSwap(false, true) {
				return nil
			}
			return attacker.Set(store.migrationLockKey(), "replacement-owner", 10*time.Second).Err()
		}
	})

	result, err := store.MigrateLegacySessions(context.Background(), LegacyMigrationOptions{
		CampaignID:   "final-lock-loss",
		CutoffAt:     time.Now().UTC().Add(time.Hour),
		FinitePolicy: LegacyFinitePolicyCap,
		BatchSize:    100,
		Apply:        true,
		Lease:        5 * time.Second,
	})

	require.True(t, replaced.Load(), "the test must replace the mutation lock in the final completion window")
	require.ErrorIs(t, err, ErrMigrationLockLost)
	require.False(t, result.Complete)
	require.True(t, result.LockLost)
	require.Equal(t, int64(0), mustExists(t, client, store.latestMigrationCompletionKey()))
}
