package user

import (
	"context"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/stretchr/testify/require"
)

func TestSessionRolloutConcurrentFirstTakeoverConvergesWithoutStartupFailure(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	control := auth.NewRolloutControlStore(ctx.DB())
	for round := 0; round < 10; round++ {
		require.NoError(t, deleteRolloutControlRows(ctx))

		const starters = 8
		start := make(chan struct{})
		errs := make(chan error, starters)
		var wg sync.WaitGroup
		for i := 0; i < starters; i++ {
			seed := auth.RolloutSeed{
				Floor:     auth.SessionModeRevoke,
				MaxPerUID: 20,
				Actor:     "integration-startup",
				Source:    "legacy-redis",
			}
			if i%2 == 0 {
				seed.Floor = auth.SessionModeBounded
				seed.MaxPerUID = 12
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := control.Initialize(context.Background(), seed)
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "every replica must survive concurrent first takeover")
		}

		state, err := control.Load(context.Background())
		require.NoError(t, err)
		require.Equal(t, auth.SessionModeBounded, state.Floor)
		require.Equal(t, 12, state.MaxPerUID)
	}
}

func deleteRolloutControlRows(ctx *config.Context) error {
	if _, err := ctx.DB().Exec("DELETE FROM octo_session_rollout_advance"); err != nil {
		return err
	}
	_, err := ctx.DB().Exec("DELETE FROM octo_session_rollout_state")
	return err
}
