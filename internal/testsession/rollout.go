package testsession

import (
	"context"
	"fmt"
	"os"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
)

const (
	legacyModeEnv = "OCTO_AUTH_SESSION_MODE"
	legacyCapEnv  = "OCTO_AUTH_SESSION_MAX_PER_UID"
)

// StartPackageRollout creates the first test server context and runs the same
// rollout publication required before production starts serving requests.
// register.GetModules caches module instances from this first context, so the
// lease must remain live for the entire package test process.
func StartPackageRollout(build string) (context.CancelFunc, error) {
	restoreEnv := unsetLegacyRolloutInputs()
	defer restoreEnv()

	_, ctx := testutil.NewTestServer()
	return StartRollout(ctx, build)
}

// StartRollout initializes MySQL authority and publishes a live Redis writer
// lease for tests that construct their own isolated server context.
func StartRollout(ctx *config.Context, build string) (context.CancelFunc, error) {
	store, client := auth.SessionStoreAndClientForContext(ctx)
	boot, _, err := auth.InitializeSessionRollout(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize test session rollout: %w", err)
	}

	rolloutCtx, stop := context.WithCancel(context.Background())
	registry := auth.NewWriterRegistry(client, ctx.GetConfig().Cache.UIDTokenCachePrefix)
	store.UseWriterLease(registry)
	if err := registry.Join(rolloutCtx, build, build, string(store.Mode()), nil); err != nil {
		stop()
		return nil, fmt.Errorf("join test writer registry: %w", err)
	}
	if err := store.ApplyAndPublishRolloutState(registry, auth.RolloutState{
		Floor: boot.Floor, MaxPerUID: boot.MaxPerUID, Version: boot.Version,
	}, store.Mode()); err != nil {
		stop()
		return nil, fmt.Errorf("publish test rollout state: %w", err)
	}
	return stop, nil
}

func unsetLegacyRolloutInputs() func() {
	mode, hadMode := os.LookupEnv(legacyModeEnv)
	cap, hadCap := os.LookupEnv(legacyCapEnv)
	_ = os.Unsetenv(legacyModeEnv)
	_ = os.Unsetenv(legacyCapEnv)
	return func() {
		restoreEnv(legacyModeEnv, mode, hadMode)
		restoreEnv(legacyCapEnv, cap, hadCap)
	}
}

func restoreEnv(key, value string, existed bool) {
	if existed {
		_ = os.Setenv(key, value)
		return
	}
	_ = os.Unsetenv(key)
}
