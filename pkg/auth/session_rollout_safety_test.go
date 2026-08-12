package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestInstanceBoundScannerRejectsCancellationFailuresAndFailover(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)

	store.redisInstanceIDFn = func() (string, error) { return "", errors.New("INFO unavailable") }
	_, err := newInstanceBoundScanner(store)
	require.ErrorContains(t, err, "INFO unavailable")

	store.redisInstanceIDFn = func() (string, error) { return "redis-a", nil }
	scanner, err := newInstanceBoundScanner(store)
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err = scanner.Scan(canceled, 0, store.tokenPrefix+"*", 10)
	require.ErrorIs(t, err, context.Canceled)
	_, err = scanner.Check(canceled)
	require.ErrorIs(t, err, context.Canceled)

	store.redisInstanceIDFn = func() (string, error) { return "redis-b", nil }
	_, _, restarted, err := scanner.Scan(context.Background(), 0, store.tokenPrefix+"*", 10)
	require.NoError(t, err)
	require.True(t, restarted)
	require.Equal(t, "redis-b", scanner.InstanceID())

	var reads atomic.Int64
	store.redisInstanceIDFn = func() (string, error) {
		if reads.Add(1) <= 2 {
			return "redis-before", nil
		}
		return "redis-after", nil
	}
	scanner, err = newInstanceBoundScanner(store)
	require.NoError(t, err)
	_, _, restarted, err = scanner.Scan(context.Background(), 0, store.tokenPrefix+"*", 10)
	require.NoError(t, err)
	require.True(t, restarted, "a failover after SCAN must discard the returned page")
	require.Equal(t, "redis-after", scanner.InstanceID())

	require.NoError(t, client.Close())
	store.redisInstanceIDFn = func() (string, error) { return "redis-after", nil }
	_, _, _, err = scanner.Scan(context.Background(), 0, store.tokenPrefix+"*", 10)
	require.Error(t, err)
}

func TestRolloutScanLeaseConfirmsOwnershipAndReportsKeepaliveLoss(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()

	var nilLease *rolloutScanLease
	nilLease.Release()
	require.NoError(t, nilLease.Err())
	require.Error(t, nilLease.Confirm())

	lease, err := store.acquireRolloutScanLease(ctx, 5*time.Second)
	require.NoError(t, err)
	_, err = store.acquireRolloutScanLease(ctx, 5*time.Second)
	require.ErrorIs(t, err, ErrRolloutScanLeaseHeld)
	require.NoError(t, lease.Confirm())
	require.NoError(t, client.Set(store.rolloutScanLeaseKey(), "replacement-owner", 10*time.Second).Err())
	require.ErrorIs(t, lease.Confirm(), ErrMigrationLockLost)
	lease.Release()
	owner, err := client.Get(store.rolloutScanLeaseKey()).Result()
	require.NoError(t, err)
	require.Equal(t, "replacement-owner", owner, "release must not delete another owner's lease")

	errCh := make(chan error, 1)
	errCh <- ErrMigrationLockLost
	close(errCh)
	failed := &rolloutScanLease{errors: errCh}
	require.ErrorIs(t, failed.Err(), ErrMigrationLockLost)
	require.NoError(t, failed.Err(), "a drained keepalive channel is not a second failure")

	key := store.UIDTokenPrefix() + "keepalive-test"
	require.NoError(t, client.Set(key, "owner", 90*time.Millisecond).Err())
	keepaliveCtx, cancel := context.WithCancel(ctx)
	keepaliveErrs := store.keepRedisLeaseAlive(keepaliveCtx, key, "owner", 90*time.Millisecond)
	require.Eventually(t, func() bool {
		ttl, ttlErr := client.PTTL(key).Result()
		return ttlErr == nil && ttl > 60*time.Millisecond
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, client.Del(key).Err())
	select {
	case keepaliveErr := <-keepaliveErrs:
		require.ErrorIs(t, keepaliveErr, ErrMigrationLockLost)
	case <-time.After(time.Second):
		t.Fatal("keepalive did not report lease loss")
	}
	cancel()
}

func TestApplyRolloutStateIsMonotonicAndCarriesTheCap(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
	require.Error(t, store.ApplyRolloutState(RolloutState{
		Floor: SessionModeBounded, MaxPerUID: 10, Version: 1,
	}, SessionMode("invalid")))
	require.Error(t, store.ApplyRolloutState(RolloutState{
		Floor: SessionModeBounded, MaxPerUID: sessionMaxPerUIDLimit + 1, Version: 2,
	}, SessionModeBounded))

	require.NoError(t, store.ApplyRolloutState(RolloutState{
		Floor: SessionModeBounded, MaxPerUID: 10, Version: 3,
	}, SessionModeBounded))
	require.Equal(t, SessionModeBounded, store.Mode())
	require.Equal(t, 10, store.currentMaxPerUID())

	require.NoError(t, store.ApplyRolloutState(RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 7, Version: 2,
	}, SessionModeRevoke))
	require.Equal(t, SessionModeBounded, store.Mode(), "a stale floor must not lower reader strictness")
	require.Equal(t, 10, store.currentMaxPerUID(),
		"an older control version must not replace a newer cap")
	store.state.Store(&derivedRolloutState{mode: SessionModeBounded, version: 3})
	require.NoError(t, store.ApplyRolloutState(RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 7, Version: 2,
	}, SessionModeRevoke))
	require.Equal(t, 7, store.currentMaxPerUID(), "a usable cap from a stale floor may heal missing cap state")

	require.NoError(t, store.ApplyRolloutState(RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 5, Version: 4,
	}, SessionModeRevoke))
	require.Equal(t, SessionModeBounded, store.Mode())
	require.Equal(t, 5, store.currentMaxPerUID(), "a newer version may tighten the cap without lowering mode")
	require.NoError(t, store.ApplyRolloutState(RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3,
	}, SessionModeRevoke))
	require.Equal(t, 5, store.currentMaxPerUID(), "an older version cannot undo a cap change")
	require.NoError(t, store.ApplyRolloutState(RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 20, Version: 5,
	}, SessionModeRevoke))
	require.Equal(t, 20, store.currentMaxPerUID(), "a newer version may deliberately raise the cap")

	require.False(t, SessionModeExpand.WritesV3())
	require.True(t, SessionModeV3Write.WritesV3())
	require.False(t, SessionModeV3Write.RevokesSessions())
	require.True(t, SessionModeRevoke.RevokesSessions())
}

func TestApplyRolloutStateConcurrentCallersNeverRegressModeOrCapVersion(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
	modes := []SessionMode{
		SessionModeExpand, SessionModeV3Write, SessionModeRevoke, SessionModeBounded, SessionModeEnforce,
	}
	const callers = 256
	start := make(chan struct{})
	observed := make([]SessionMode, 0, callers)
	errs := make([]error, callers)
	var observedMu sync.Mutex
	var wg sync.WaitGroup
	for i := 1; i <= callers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			mode := modes[i%len(modes)]
			errs[i-1] = store.ApplyRolloutState(RolloutState{
				Floor: mode, MaxPerUID: i, Version: int64(i),
			}, mode)
			observedMu.Lock()
			observed = append(observed, store.Mode())
			observedMu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "caller %d", i+1)
	}
	for i := 1; i < len(observed); i++ {
		require.GreaterOrEqual(t, observed[i].rank(), observed[i-1].rank(),
			"completed concurrent applies must never observe a mode regression")
	}
	state := store.currentState()
	require.Equal(t, SessionModeEnforce, state.mode)
	require.EqualValues(t, callers, state.version)
	require.Equal(t, callers, state.maxPerUID,
		"the cap must come from the newest MySQL control version")
}

func TestRolloutStateSafetyMechanismsAreStructural(t *testing.T) {
	t.Run("monotonic apply uses CAS", func(t *testing.T) {
		storeSource, err := os.ReadFile("session_store.go")
		require.NoError(t, err)
		applyStart := strings.Index(string(storeSource), "func (s *RedisSessionStore) ApplyRolloutState(")
		require.NotEqual(t, -1, applyStart)
		applyEnd := strings.Index(string(storeSource)[applyStart:], "\n}\n")
		require.NotEqual(t, -1, applyEnd)
		applyBody := string(storeSource)[applyStart : applyStart+applyEnd]
		require.Contains(t, applyBody, ".CompareAndSwap(",
			"mode monotonicity must survive concurrent callers, not rely on a single-caller convention")
	})

	t.Run("control reads are bounded", func(t *testing.T) {
		runtimeSource, err := os.ReadFile("runtime.go")
		require.NoError(t, err)
		require.Contains(t, string(runtimeSource), "loadRolloutStateWithTimeout(",
			"normal boot must bound a MySQL control read")
		reconcilerSource, err := os.ReadFile("session_rollout_reconciler.go")
		require.NoError(t, err)
		require.GreaterOrEqual(t, strings.Count(string(reconcilerSource), "loadRolloutStateWithTimeout("), 2,
			"both floor polling and advance evaluation must bound MySQL control reads")
	})
}

type rolloutStateLoaderFunc func(context.Context) (RolloutState, error)

func (f rolloutStateLoaderFunc) Load(ctx context.Context) (RolloutState, error) { return f(ctx) }

func TestRolloutControlReadHelperAddsDeadlineAndPreservesCancellation(t *testing.T) {
	var deadline time.Time
	state, err := loadRolloutStateWithTimeout(context.Background(), rolloutStateLoaderFunc(
		func(ctx context.Context) (RolloutState, error) {
			var ok bool
			deadline, ok = ctx.Deadline()
			require.True(t, ok)
			return RolloutState{Floor: SessionModeRevoke}, nil
		},
	))
	require.NoError(t, err)
	require.Equal(t, SessionModeRevoke, state.Floor)
	require.WithinDuration(t, time.Now().Add(rolloutControlReadTimeout), deadline, time.Second)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = loadRolloutStateWithTimeout(canceled, rolloutStateLoaderFunc(
		func(ctx context.Context) (RolloutState, error) { return RolloutState{}, ctx.Err() },
	))
	require.ErrorIs(t, err, context.Canceled)
}

func TestRolloutControlRetryClassificationAndConstructorGuards(t *testing.T) {
	require.Panics(t, func() { NewRolloutControlStore(nil) })
	require.False(t, isMySQLRetryableTakeover(errors.New("not mysql")))
	for _, code := range []uint16{1062, 1205, 1213} {
		require.True(t, isMySQLRetryableTakeover(&mysql.MySQLError{Number: code}), code)
	}
	require.False(t, isMySQLRetryableTakeover(&mysql.MySQLError{Number: 1045}))
	require.Equal(t, "value", firstNonEmpty(" ", " value "))
	require.Empty(t, firstNonEmpty("", " "))
}

func TestMigrationOptionsRejectUnsafeBoundaries(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	valid := LegacyMigrationOptions{
		CampaignID:   "campaign",
		CutoffAt:     time.Now().UTC().Add(time.Hour),
		FinitePolicy: LegacyFinitePolicyCap,
		BatchSize:    100,
		Apply:        true,
		Lease:        5 * time.Second,
	}
	for _, mutate := range []func(*LegacyMigrationOptions){
		func(options *LegacyMigrationOptions) { options.CampaignID = "" },
		func(options *LegacyMigrationOptions) { options.CampaignID = string(make([]byte, 65)) },
		func(options *LegacyMigrationOptions) { options.CutoffAt = time.Time{} },
		func(options *LegacyMigrationOptions) { options.FinitePolicy = "unsafe" },
		func(options *LegacyMigrationOptions) { options.BatchSize = 10_001 },
		func(options *LegacyMigrationOptions) { options.Interval = -time.Second },
		func(options *LegacyMigrationOptions) { options.Lease = time.Second },
	} {
		options := valid
		mutate(&options)
		_, err := store.validateMigrationOptions(options)
		require.Error(t, err)
	}
}

func TestWriterRegistryStatePublicationAndShutdownAreFenced(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	registry := NewWriterRegistry(client, store.UIDTokenPrefix())
	require.Error(t, registry.SetAppliedState(""))
	require.Error(t, registry.SetAppliedState(string(SessionModeRevoke)), "an unjoined registry cannot publish")
	require.Error(t, registry.refresh())

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, registry.Join(ctx, "build-a", "pod-a", string(SessionModeRevoke), time.Now))
	require.NoError(t, registry.SetAppliedStateIfChanged(string(SessionModeRevoke)))
	require.NoError(t, registry.SetAppliedStateIfChanged(string(SessionModeBounded)))
	require.NoError(t, registry.SetAppliedState(string(SessionModeEnforce)))
	live, err := registry.Live()
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, string(SessionModeEnforce), live[0].AppliedState)
	require.True(t, registry.MayWrite())

	id := live[0].ID
	cancel()
	require.Eventually(t, func() bool { return !registry.MayWrite() }, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		member, memberErr := client.SIsMember(registry.rosterKey, id).Result()
		return memberErr == nil && !member
	}, time.Second, 10*time.Millisecond)

	staleID := "stale-" + util.GenerUUID()
	malformedID := "malformed-" + util.GenerUUID()
	require.NoError(t, client.SAdd(registry.rosterKey, staleID, malformedID).Err())
	require.NoError(t, client.Set(registry.entryKey(malformedID), "not-json", time.Minute).Err())
	live, err = registry.Live()
	require.NoError(t, err)
	require.Empty(t, live)
	require.Eventually(t, func() bool {
		count, countErr := client.SCard(registry.rosterKey).Result()
		return countErr == nil && count == 0
	}, time.Second, 10*time.Millisecond)
}

func TestWriterRegistryLiveDoesNotPruneWriterRefreshedAfterSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name  string
		stale func(*testing.T, *rd.Client, string)
	}{
		{
			name: "missing entry",
			stale: func(t *testing.T, client *rd.Client, key string) {
				t.Helper()
				require.NoError(t, client.Del(key).Err())
			},
		},
		{
			name: "malformed entry",
			stale: func(t *testing.T, client *rd.Client, key string) {
				t.Helper()
				require.NoError(t, client.Set(key, "not-json", time.Minute).Err())
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, publisherClient := newLegacyMigrationTestStore(t, SessionModeRevoke)
			writer := NewWriterRegistry(publisherClient, store.UIDTokenPrefix())
			writerID := "writer-" + util.GenerUUID()
			writer.mu.Lock()
			writer.self = WriterEntry{
				ID:           writerID,
				Build:        "build-a",
				AppliedState: string(SessionModeRevoke),
				StartedAtMS:  time.Now().UTC().UnixMilli(),
			}
			writer.mu.Unlock()
			require.NoError(t, writer.refresh())
			tt.stale(t, publisherClient, writer.entryKey(writerID))

			readerClient := octoredis.NewInstrumentedClient(config.New())
			t.Cleanup(func() { _ = readerClient.Close() })
			reader := NewWriterRegistry(readerClient, store.UIDTokenPrefix())
			var refreshed atomic.Bool
			var refreshErr error
			readerClient.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
				return func(cmd rd.Cmder) error {
					err := old(cmd)
					if cmd.Name() == "mget" && refreshed.CompareAndSwap(false, true) {
						refreshErr = writer.refresh()
					}
					return err
				}
			})

			live, err := reader.Live()
			require.NoError(t, err)
			require.NoError(t, refreshErr)
			require.True(t, refreshed.Load(), "the test must refresh after the MGET snapshot")
			require.Empty(t, live, "the completed snapshot may still report the old entry state")
			require.True(t, writer.MayWrite(), "the refreshed writer is allowed to issue credentials")

			member, err := readerClient.SIsMember(reader.rosterKey, writerID).Result()
			require.NoError(t, err)
			require.True(t, member, "cleanup from an older snapshot must not hide a refreshed writer")

			live, err = reader.Live()
			require.NoError(t, err)
			require.Len(t, live, 1)
			require.Equal(t, writerID, live[0].ID)
		})
	}
}

func TestRolloutAuditAndLegacyTakeoverRejectInvalidRedisState(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()

	require.Error(t, store.recordMigrationCompletion("campaign", "redis-a", LegacyMigrationResult{}))
	_, err := store.loadMigrationCompletionEvidence()
	require.ErrorContains(t, err, "missing")

	key := store.latestMigrationCompletionKey()
	require.NoError(t, client.Set(key, `{}`, 0).Err())
	_, err = store.loadMigrationCompletionEvidence()
	require.ErrorContains(t, err, "invalid")
	require.NoError(t, client.Set(key, `not-json`, 0).Err())
	_, err = store.loadMigrationCompletionEvidence()
	require.ErrorContains(t, err, "decode evidence")
	require.NoError(t, client.Set(key, `{}`, time.Minute).Err())
	_, err = store.loadMigrationCompletionEvidence()
	require.ErrorContains(t, err, "must not expire")

	store.redisInstanceIDFn = func() (string, error) { return "", errors.New("INFO denied") }
	fallbackFingerprint := store.rolloutObservationScopeFingerprint()
	store.redisInstanceIDFn = func() (string, error) { return "redis-a", nil }
	require.NotEqual(t, fallbackFingerprint, store.rolloutObservationScopeFingerprint())
	fingerprint, err := store.RedisInstanceFingerprint()
	require.NoError(t, err)
	require.NotEmpty(t, fingerprint)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.RolloutControl(canceled)
	require.ErrorIs(t, err, context.Canceled)

	legacyKey := store.rolloutControlKey()
	require.NoError(t, client.Set(legacyKey, `{"mode_floor":"revoke","writer_version":3,"observation_min_gap_ms":0}`, time.Minute).Err())
	_, err = store.RolloutControl(ctx)
	require.ErrorContains(t, err, "must not expire")
	for _, raw := range []string{
		`not-json`,
		`{"mode_floor":"expand","writer_version":3,"observation_min_gap_ms":0}`,
		`{"mode_floor":"revoke","writer_version":2,"observation_min_gap_ms":0}`,
		`{"mode_floor":"revoke","writer_version":3,"observation_min_gap_ms":-1}`,
	} {
		_, decodeErr := decodeRolloutControl(raw)
		require.Error(t, decodeErr, raw)
	}
}

func TestRolloutRuntimeRejectsInvalidContextAndPoolConfiguration(t *testing.T) {
	require.Panics(t, func() { SessionStoreForContext(nil) })
	_, _, err := InitializeSessionRollout(nil)
	require.Error(t, err)
	empty := config.NewContext(config.New())
	_, _, err = InitializeSessionRollout(empty)
	require.ErrorContains(t, err, "constructed before")
	boot, warnings := SessionBootForContext(empty)
	require.Empty(t, boot)
	require.Nil(t, warnings)

	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "not integer", value: "abc"},
		{name: "zero", value: "0"},
		{name: "too large", value: fmt.Sprint(sessionRedisPoolSizeMax + 1)},
	} {
		t.Run("pool size "+tc.name, func(t *testing.T) {
			t.Setenv(sessionRedisPoolSizeEnv, tc.value)
			_, optionErr := sessionRedisOptionsFromEnv()
			require.Error(t, optionErr)
		})
	}
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "invalid", value: "later"},
		{name: "zero", value: "0s"},
		{name: "too large", value: (sessionRedisPoolTimeoutMax + time.Second).String()},
	} {
		t.Run("pool timeout "+tc.name, func(t *testing.T) {
			t.Setenv(sessionRedisPoolTimeoutEnv, tc.value)
			_, optionErr := sessionRedisOptionsFromEnv()
			require.Error(t, optionErr)
		})
	}
	t.Setenv(sessionRedisPoolSizeEnv, "8")
	t.Setenv(sessionRedisPoolTimeoutEnv, "2s")
	options, err := sessionRedisOptionsFromEnv()
	require.NoError(t, err)
	require.Equal(t, 8, options.poolSize)
	require.Equal(t, 2*time.Second, options.poolTimeout)
}

func TestRolloutReconcilerFailClosedBranchesAndBackoff(t *testing.T) {
	require.Panics(t, func() { NewRolloutReconciler(nil, ReconcilerOptions{}) })
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	now := time.Unix(100, 0)
	store.now = func() time.Time { return now }

	defaults := NewRolloutReconciler(store, ReconcilerOptions{})
	require.EqualValues(t, 200, defaults.scanBatchSize)
	require.Equal(t, defaultReconcileScanInterval, defaults.scanInterval)
	require.Equal(t, now.Add(reconcileInterval), defaults.reconcileOnce(context.Background()))
	defaults.pollFloor(context.Background())

	var logs atomic.Int64
	loadFailure := &recordingRolloutController{loadErr: errors.New("mysql unavailable")}
	failing := NewRolloutReconciler(store, ReconcilerOptions{
		Control: loadFailure,
		Log:     func(string, ...interface{}) { logs.Add(1) },
	})
	failing.pollFloor(context.Background())
	require.Equal(t, now.Add(reconcileInterval), failing.reconcileOnce(context.Background()))
	require.EqualValues(t, 2, logs.Load())

	applyFailure := &recordingRolloutController{state: RolloutState{Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3}}
	withoutRegistry := NewRolloutReconciler(store, ReconcilerOptions{
		Control: applyFailure,
		Log:     func(string, ...interface{}) { logs.Add(1) },
	})
	withoutRegistry.pollFloor(context.Background())
	require.Equal(t, SessionModeRevoke, store.Mode())
	require.ErrorIs(t, store.CanIssue(), ErrWriterLeaseLost)

	registry := NewWriterRegistry(client, store.UIDTokenPrefix())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, registry.Join(ctx, "build-a", "pod-a", string(SessionModeRevoke), time.Now))

	paused := NewRolloutReconciler(store, ReconcilerOptions{
		Registry: registry, Control: &recordingRolloutController{state: RolloutState{
			Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3, Paused: true,
		}}, AutoAdvance: true,
	})
	require.Equal(t, now.Add(reconcileInterval), paused.reconcileOnce(ctx))

	store.redisInstanceIDFn = func() (string, error) { return "", errors.New("INFO unavailable") }
	evaluateFailure := NewRolloutReconciler(store, ReconcilerOptions{
		Registry: registry, Control: &recordingRolloutController{state: RolloutState{
			Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3,
		}}, AutoAdvance: true,
	})
	require.Equal(t, now.Add(reconcileScanBackoff), evaluateFailure.reconcileOnce(ctx))

	store.redisInstanceIDFn = func() (string, error) { return "redis-a", nil }
	require.NoError(t, client.Set(store.tokenKey("persistent"), v2Fixture(t, "legacy"), 0).Err())
	scanBlocked := NewRolloutReconciler(store, ReconcilerOptions{
		Registry: registry, Control: &recordingRolloutController{state: RolloutState{
			Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3,
		}}, AutoAdvance: true, ScanBatchSize: 100,
	})
	require.Equal(t, now.Add(reconcileScanBackoff), scanBlocked.reconcileOnce(ctx))

	require.NoError(t, client.Del(store.tokenKey("persistent")).Err())
	advanceFailure := &recordingRolloutController{
		state:      RolloutState{Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3},
		advanceErr: errors.New("commit failed"),
	}
	failedAdvance := NewRolloutReconciler(store, ReconcilerOptions{
		Registry: registry, Control: advanceFailure, AutoAdvance: true, ScanBatchSize: 100,
	})
	require.Equal(t, now.Add(reconcileInterval), failedAdvance.reconcileOnce(ctx))
	require.Equal(t, 1, advanceFailure.advanceCalls)

	canaryRegistry := &WriterRegistry{
		now:           time.Now,
		self:          WriterEntry{ID: "canary", AppliedState: string(SessionModeExpand)},
		lastRefreshAt: time.Now(),
		publishFn:     func(WriterEntry) error { return nil },
	}
	canaryStore, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
	canary := NewRolloutReconciler(canaryStore, ReconcilerOptions{Registry: canaryRegistry, CanaryAhead: true})
	require.NoError(t, canary.applyAndPublish(RolloutState{
		Floor: SessionModeExpand, MaxPerUID: 20, Version: 1,
	}))
	require.Equal(t, SessionModeV3Write, canaryStore.Mode())
	require.Error(t, canary.advance(ctx, RolloutAdvanceDecision{}, "reconciler"))
}

func TestRevokeCurrentRemovesV3CredentialAndIndexes(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeV3Write, WithSessionMaxPerUID(2))
	uid := "u-" + util.GenerUUID()
	token := "t-" + util.GenerUUID()
	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, store.IssueNewSession(context.Background(), token, TokenInfo{
		UID: uid, Name: "alice", DeviceFlag: 1, DeviceID: "device-a",
	}, fence))

	require.Error(t, store.RevokeCurrent(context.Background(), "", uid, 1))
	require.NoError(t, store.RevokeCurrent(context.Background(), token, uid, 1))
	require.Equal(t, int64(0), mustExists(t, client, store.tokenKey(token), store.uidTokenKey(uid, 1)))
	members, err := client.ZCard(store.sessionIndexKey(uid, fence.Generation)).Result()
	require.NoError(t, err)
	require.Zero(t, members)

	require.NoError(t, store.RevokeCurrent(context.Background(), "missing", uid, 1))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, store.RevokeCurrent(canceled, "another", uid, 1), context.Canceled)

	other := "other-" + util.GenerUUID()
	require.NoError(t, client.Set(store.tokenKey(other), v2Fixture(t, "another-uid"), time.Minute).Err())
	require.ErrorContains(t, store.RevokeCurrent(context.Background(), other, uid, 1), "uid mismatch")
}
