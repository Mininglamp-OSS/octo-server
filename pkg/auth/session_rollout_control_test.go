package auth

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	rd "github.com/go-redis/redis"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

func TestResolveRolloutSeedPreservesPersistedCapAndStrictestFloor(t *testing.T) {
	tests := []struct {
		name         string
		redisFloor   SessionMode
		legacyMode   SessionMode
		redisMax     int
		legacyMax    int
		wantFloor    SessionMode
		wantMaxPerID int
	}{
		{
			name:         "fresh deployment starts at expand",
			wantFloor:    SessionModeExpand,
			wantMaxPerID: defaultRecoveryMaxPerUID,
		},
		{
			name:         "adopt existing #725 floor",
			redisFloor:   SessionModeRevoke,
			redisMax:     12,
			legacyMax:    20,
			wantFloor:    SessionModeRevoke,
			wantMaxPerID: 12,
		},
		{
			name:         "persist legacy MODE as compatibility floor",
			redisFloor:   SessionModeRevoke,
			legacyMode:   SessionModeBounded,
			legacyMax:    20,
			wantFloor:    SessionModeBounded,
			wantMaxPerID: 20,
		},
		{
			name:         "redis floor cannot be lowered by stale env",
			redisFloor:   SessionModeEnforce,
			legacyMode:   SessionModeRevoke,
			redisMax:     8,
			legacyMax:    20,
			wantFloor:    SessionModeEnforce,
			wantMaxPerID: 8,
		},
		{
			name:         "persisted redis cap is not tightened by deprecated env",
			redisFloor:   SessionModeRevoke,
			redisMax:     50,
			legacyMax:    5,
			wantFloor:    SessionModeRevoke,
			wantMaxPerID: 50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed, err := ResolveRolloutSeed(tt.redisFloor, tt.legacyMode, tt.redisMax, tt.legacyMax)
			require.NoError(t, err)
			require.Equal(t, tt.wantFloor, seed.Floor)
			require.Equal(t, tt.wantMaxPerID, seed.MaxPerUID)
		})
	}
}

func TestRolloutBootOutcomesAreRegisteredMetricsLabels(t *testing.T) {
	for _, outcome := range []RolloutBootOutcome{RolloutBootFresh, RolloutBootAdopted, RolloutBootNormal} {
		require.True(t, metrics.KnownSessionRolloutBootOutcome(string(outcome)), string(outcome))
	}
}

func TestRolloutControlInitializeRetriesConcurrentInsertAndAdoptsStricterSeed(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT floor, max_per_uid, version, paused, updated_at FROM octo_session_rollout_state")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO octo_session_rollout_state")).
		WithArgs("bounded", 12).
		WillReturnError(&mysql.MySQLError{Number: 1062, Message: "duplicate singleton"})
	mock.ExpectRollback()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT floor, max_per_uid, version, paused, updated_at FROM octo_session_rollout_state")).
		WillReturnRows(sqlmock.NewRows([]string{"floor", "max_per_uid", "version", "paused", "updated_at"}).
			AddRow("revoke", 20, 1, false, time.Unix(100, 0)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE octo_session_rollout_state")).
		WithArgs("bounded", 12, int64(1), "revoke").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO octo_session_rollout_advance")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	state, err := store.Initialize(context.Background(), RolloutSeed{
		Floor: SessionModeBounded, MaxPerUID: 12, Actor: "startup", Source: "legacy-redis",
	})
	require.NoError(t, err)
	require.Equal(t, SessionModeBounded, state.Floor)
	require.EqualValues(t, 2, state.Version)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRolloutControlInitializeIsMonotonicAcrossLegacyTakeover(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT floor, max_per_uid, version, paused, updated_at FROM octo_session_rollout_state")).
		WillReturnRows(sqlmock.NewRows([]string{"floor", "max_per_uid", "version", "paused", "updated_at"}).
			AddRow("enforce", 20, 9, false, time.Unix(100, 0)))
	mock.ExpectCommit()

	state, err := store.Initialize(context.Background(), RolloutSeed{
		Floor:     SessionModeRevoke,
		MaxPerUID: 12,
		Actor:     "startup",
		Source:    "legacy-redis",
	})
	require.NoError(t, err)
	require.Equal(t, SessionModeEnforce, state.Floor)
	require.EqualValues(t, 9, state.Version)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRolloutControlInitializeCreatesStateAndAuditInOneTransaction(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT floor, max_per_uid, version, paused, updated_at FROM octo_session_rollout_state")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO octo_session_rollout_state")).
		WithArgs("revoke", 20).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO octo_session_rollout_advance")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	state, err := store.Initialize(context.Background(), RolloutSeed{
		Floor: SessionModeRevoke, MaxPerUID: 20, Actor: "startup", Source: "legacy-redis",
	})
	require.NoError(t, err)
	require.Equal(t, SessionModeRevoke, state.Floor)
	require.EqualValues(t, 1, state.Version)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRolloutControlLoadValidatesPersistedState(t *testing.T) {
	t.Run("loads valid singleton", func(t *testing.T) {
		store, mock, cleanup := newMockRolloutControlStore(t)
		defer cleanup()
		updated := time.Unix(123, 0)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT floor, max_per_uid, version, paused, updated_at FROM octo_session_rollout_state")).
			WillReturnRows(sqlmock.NewRows([]string{"floor", "max_per_uid", "version", "paused", "updated_at"}).
				AddRow("bounded", 12, 7, true, updated))

		state, err := store.Load(context.Background())
		require.NoError(t, err)
		require.Equal(t, RolloutState{
			Floor: SessionModeBounded, MaxPerUID: 12, Version: 7, Paused: true, UpdatedAt: updated,
		}, state)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("missing singleton is explicit", func(t *testing.T) {
		store, mock, cleanup := newMockRolloutControlStore(t)
		defer cleanup()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT floor, max_per_uid, version, paused, updated_at FROM octo_session_rollout_state")).
			WillReturnRows(sqlmock.NewRows([]string{"floor", "max_per_uid", "version", "paused", "updated_at"}))

		_, err := store.Load(context.Background())
		require.ErrorIs(t, err, ErrRolloutStateUninitialized)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	for _, tc := range []struct {
		name    string
		floor   string
		max     int
		version int64
	}{
		{name: "invalid floor", floor: "banana", max: 20, version: 1},
		{name: "invalid cap", floor: "revoke", max: 0, version: 1},
		{name: "invalid version", floor: "revoke", max: 20, version: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, mock, cleanup := newMockRolloutControlStore(t)
			defer cleanup()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT floor, max_per_uid, version, paused, updated_at FROM octo_session_rollout_state")).
				WillReturnRows(sqlmock.NewRows([]string{"floor", "max_per_uid", "version", "paused", "updated_at"}).
					AddRow(tc.floor, tc.max, tc.version, false, time.Unix(123, 0)))

			_, err := store.Load(context.Background())
			require.Error(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestRolloutControlAdvanceCommitsEvidenceAndCASAtomically(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()

	current := RolloutState{Floor: SessionModeRevoke, MaxPerUID: 20, Version: 7}
	record := RolloutAdvanceRecord{
		From:              SessionModeRevoke,
		To:                SessionModeBounded,
		Actor:             "reconciler",
		RedisID:           "redis-run-id",
		WriterFingerprint: "writers-sha256",
		Observation:       &SessionObservation{Complete: true, Total: 4, V3: 4},
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO octo_session_rollout_advance")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE octo_session_rollout_state")).
		WithArgs("bounded", 20, int64(7), "revoke").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	next, err := store.Advance(context.Background(), current, record)
	require.NoError(t, err)
	require.Equal(t, SessionModeBounded, next.Floor)
	require.EqualValues(t, 8, next.Version)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRolloutControlAdvanceRollsBackEvidenceWhenCASLoses(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO octo_session_rollout_advance")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE octo_session_rollout_state")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	_, err := store.Advance(context.Background(), RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 20, Version: 7,
	}, RolloutAdvanceRecord{
		From: SessionModeRevoke, To: SessionModeBounded, Actor: "racing-reconciler",
	})
	require.ErrorIs(t, err, ErrRolloutControlChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRolloutControlAdvanceRejectsInvalidTransitionBeforeTransaction(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()

	_, err := store.Advance(context.Background(), RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 20, Version: 7,
	}, RolloutAdvanceRecord{
		From: SessionModeRevoke, To: SessionModeEnforce,
	})
	require.ErrorIs(t, err, ErrRolloutFloorNotNext)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRolloutControlSetMaxPerUIDCommitsAuditAndCASAtomically(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()

	current := RolloutState{Floor: SessionModeRevoke, MaxPerUID: 20, Version: 7}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO octo_session_rollout_advance")).
		WithArgs("revoke", "revoke", "operator", `{"from_max_per_uid":20,"to_max_per_uid":5}`, "", "", "set-cap").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE octo_session_rollout_state SET max_per_uid = ?, version = version + 1")).
		WithArgs(5, int64(7), "revoke", 20).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	next, err := store.SetMaxPerUID(context.Background(), current, 5, "operator")
	require.NoError(t, err)
	require.Equal(t, SessionModeRevoke, next.Floor)
	require.Equal(t, 5, next.MaxPerUID)
	require.EqualValues(t, 8, next.Version)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRolloutControlSetMaxPerUIDRollsBackAuditWhenCASLoses(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO octo_session_rollout_advance")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE octo_session_rollout_state SET max_per_uid = ?, version = version + 1")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	_, err := store.SetMaxPerUID(context.Background(), RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 20, Version: 7,
	}, 5, "operator")
	require.ErrorIs(t, err, ErrRolloutControlChanged)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRolloutControlSetMaxPerUIDRejectsUnsafeValuesBeforeTransaction(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()
	current := RolloutState{Floor: SessionModeRevoke, MaxPerUID: 20, Version: 7}

	for _, value := range []int{0, -1, sessionMaxPerUIDLimit + 1} {
		_, err := store.SetMaxPerUID(context.Background(), current, value, "operator")
		require.Error(t, err, value)
	}
	unchanged, err := store.SetMaxPerUID(context.Background(), current, current.MaxPerUID, "operator")
	require.NoError(t, err)
	require.Equal(t, current, unchanged)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRolloutControlAdvanceHonorsItsWriteDeadline(t *testing.T) {
	store, mock, cleanup := newMockRolloutControlStore(t)
	defer cleanup()
	store.writeTimeout = 20 * time.Millisecond
	mock.ExpectBegin().WillDelayFor(500 * time.Millisecond)

	started := time.Now()
	_, err := store.Advance(context.Background(), RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 20, Version: 7,
	}, RolloutAdvanceRecord{
		From: SessionModeRevoke, To: SessionModeBounded, Actor: "reconciler",
	})
	require.Error(t, err)
	require.Less(t, time.Since(started), 250*time.Millisecond,
		"a blocked control write must not wait for the driver's full delay")
}

func TestRolloutControlPauseAndLastAdvance(t *testing.T) {
	t.Run("pause increments control version", func(t *testing.T) {
		store, mock, cleanup := newMockRolloutControlStore(t)
		defer cleanup()
		mock.ExpectExec(regexp.QuoteMeta("UPDATE octo_session_rollout_state SET paused = ?, version = version + 1")).
			WithArgs(true).
			WillReturnResult(sqlmock.NewResult(0, 1))

		require.NoError(t, store.SetPaused(context.Background(), true))
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("missing singleton cannot be paused", func(t *testing.T) {
		store, mock, cleanup := newMockRolloutControlStore(t)
		defer cleanup()
		mock.ExpectExec(regexp.QuoteMeta("UPDATE octo_session_rollout_state SET paused = ?, version = version + 1")).
			WithArgs(false).
			WillReturnResult(sqlmock.NewResult(0, 0))

		require.ErrorIs(t, store.SetPaused(context.Background(), false), ErrRolloutStateUninitialized)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("last advance decodes evidence and kind", func(t *testing.T) {
		store, mock, cleanup := newMockRolloutControlStore(t)
		defer cleanup()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT from_floor, to_floor, actor, evidence_json, redis_run_id, writer_fingerprint, transition_kind")).
			WillReturnRows(sqlmock.NewRows([]string{
				"from_floor", "to_floor", "actor", "evidence_json", "redis_run_id", "writer_fingerprint", "transition_kind", "created_ms",
			}).AddRow("revoke", "bounded", "reconciler", `{"complete":true,"total":2,"v3":2}`, "redis-a", "writers-a", "advance", int64(123000)))

		record, err := store.LastAdvance(context.Background())
		require.NoError(t, err)
		require.Equal(t, "advance", record.Kind)
		require.EqualValues(t, 123000, record.AtMS)
		require.NotNil(t, record.Observation)
		require.True(t, record.Observation.Complete)
		require.EqualValues(t, 2, record.Observation.Total)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("last transition decodes cap audit", func(t *testing.T) {
		store, mock, cleanup := newMockRolloutControlStore(t)
		defer cleanup()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT from_floor, to_floor, actor, evidence_json, redis_run_id, writer_fingerprint, transition_kind")).
			WillReturnRows(sqlmock.NewRows([]string{
				"from_floor", "to_floor", "actor", "evidence_json", "redis_run_id", "writer_fingerprint", "transition_kind", "created_ms",
			}).AddRow("revoke", "revoke", "operator", `{"from_max_per_uid":20,"to_max_per_uid":5}`, "", "", "set-cap", int64(123000)))

		record, err := store.LastAdvance(context.Background())
		require.NoError(t, err)
		require.Equal(t, "set-cap", record.Kind)
		require.Equal(t, &RolloutCapChange{FromMaxPerUID: 20, ToMaxPerUID: 5}, record.CapChange)
		require.Nil(t, record.Observation)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("no advance is nil", func(t *testing.T) {
		store, mock, cleanup := newMockRolloutControlStore(t)
		defer cleanup()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT from_floor, to_floor, actor, evidence_json, redis_run_id, writer_fingerprint, transition_kind")).
			WillReturnRows(sqlmock.NewRows([]string{
				"from_floor", "to_floor", "actor", "evidence_json", "redis_run_id", "writer_fingerprint", "transition_kind", "created_ms",
			}))

		record, err := store.LastAdvance(context.Background())
		require.NoError(t, err)
		require.Nil(t, record)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestApplyAndPublishFailureKeepsIssuanceFenced(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	registry := &WriterRegistry{
		now: time.Now,
		self: WriterEntry{
			ID:           "writer-1",
			Build:        "build-1",
			AppliedState: string(SessionModeRevoke),
		},
		lastRefreshAt: time.Now(),
		publishFn: func(WriterEntry) error {
			return errors.New("registry unavailable")
		},
	}
	store.UseWriterLease(registry)
	require.NoError(t, store.CanIssue())

	err := store.ApplyAndPublishRolloutState(registry, RolloutState{
		Floor: SessionModeBounded, MaxPerUID: 20, Version: 2,
	}, SessionModeBounded)
	require.ErrorContains(t, err, "registry unavailable")
	require.Equal(t, SessionModeBounded, store.Mode(), "reader strictness must still apply")
	require.ErrorIs(t, store.CanIssue(), ErrWriterLeaseLost,
		"a replica whose applied state was not published must remain fenced")
}

func TestWriterRegistryPublishesAppliedStateAndLeaseAtomically(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	registry := NewWriterRegistry(client, store.UIDTokenPrefix())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, registry.Join(ctx, "build-1", "pod-1", string(SessionModeRevoke), time.Now))

	require.NoError(t, registry.PublishAppliedState(string(SessionModeBounded)))
	live, err := registry.Live()
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, string(SessionModeBounded), live[0].AppliedState)
	ttl, err := client.PTTL(registry.entryKey(live[0].ID)).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
}

func TestObserveRestartsFromZeroWhenRedisInstanceChanges(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	require.NoError(t, client.Set(store.tokenKey("a"), "u1@n", time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("b"), "u2@n", time.Hour).Err())

	var calls int
	store.redisInstanceIDFn = func() (string, error) {
		calls++
		if calls <= 2 {
			return "redis-a", nil
		}
		return "redis-b", nil
	}

	got, err := store.Observe(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, got.Complete)
	require.Equal(t, "redis-b", got.RedisInstanceID)
	require.EqualValues(t, 2, got.Total,
		"counts from the abandoned redis-a cursor must not leak into redis-b evidence")
}

func TestObserveRechecksRedisInstanceBeforeCompletingFinalBatch(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	require.NoError(t, client.Set(store.tokenKey("only"), "u1@n", time.Hour).Err())

	var tokenRead atomic.Bool
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			err := old(cmd)
			if cmd.Name() == "evalsha" {
				tokenRead.Store(true)
			}
			return err
		}
	})
	store.redisInstanceIDFn = func() (string, error) {
		if tokenRead.Load() {
			return "redis-b", nil
		}
		return "redis-a", nil
	}

	got, err := store.Observe(context.Background(), 10)
	require.NoError(t, err)
	require.True(t, got.Complete)
	require.Equal(t, "redis-b", got.RedisInstanceID)
	require.EqualValues(t, 1, got.Total,
		"the final batch from redis-a must be discarded when failover happens while its keys are processed")
}

func TestObserveRefusesCompleteEvidenceWhenScanLeaseIsLostInFinalBatch(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	require.NoError(t, client.Set(store.tokenKey("only"), "u1@n", time.Hour).Err())

	var tokenRead atomic.Bool
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			err := old(cmd)
			if cmd.Name() == "evalsha" {
				tokenRead.Store(true)
			}
			return err
		}
	})
	var deleted atomic.Bool
	store.redisInstanceIDFn = func() (string, error) {
		if tokenRead.Load() && deleted.CompareAndSwap(false, true) {
			require.NoError(t, client.Del(store.rolloutScanLeaseKey()).Err())
		}
		return "redis-a", nil
	}

	got, err := store.Observe(context.Background(), 10)
	require.ErrorIs(t, err, ErrMigrationLockLost)
	require.False(t, got.Complete)
}

func TestFullKeyspaceScansShareOneOwnerLease(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
	lease, err := store.acquireRolloutScanLease(context.Background(), rolloutScanLeaseTTL)
	require.NoError(t, err)
	defer lease.Release()

	_, err = store.Observe(context.Background(), 10)
	require.ErrorIs(t, err, ErrRolloutScanLeaseHeld)
}

func TestControlReadFailureKeepsCurrentModeAndPreventsAdvance(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	control := &failingRolloutController{loadErr: errors.New("mysql unavailable")}
	reconciler := NewRolloutReconciler(store, ReconcilerOptions{
		Control: control, AutoAdvance: true,
	})

	reconciler.pollFloor(context.Background())
	reconciler.reconcileOnce(context.Background())

	require.Equal(t, SessionModeRevoke, store.Mode())
	require.Zero(t, control.advanceCalls)
}

func TestPausedRolloutBlocksBeforeRegistryOrKeyspaceWork(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	decision, err := store.EvaluateRolloutAdvance(context.Background(), RolloutAdvanceInput{
		State: RolloutState{
			Floor: SessionModeRevoke, MaxPerUID: 20, Version: 4, Paused: true,
		},
	})
	require.NoError(t, err)
	require.False(t, decision.Allowed)
	require.False(t, decision.Scanned)
	require.Equal(t, []string{"rollout is paused"}, decision.BlockedBy)
}

func TestRolloutAdvanceGatesAndWriterFingerprint(t *testing.T) {
	t.Run("first v3 floor requires stable expected writers", func(t *testing.T) {
		store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
		registry := NewWriterRegistry(client, store.UIDTokenPrefix())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		now := time.Unix(100, 0)
		store.now = func() time.Time { return now }
		require.NoError(t, registry.Join(ctx, "build-a", "pod-a", string(SessionModeExpand), func() time.Time { return now }))

		input := RolloutAdvanceInput{
			State:    RolloutState{Floor: SessionModeExpand, MaxPerUID: 20, Version: 1},
			Registry: registry, ExpectWriters: 1, Convergence: NewWriterConvergence(), ScanBatchSize: 10,
		}
		first, err := store.EvaluateRolloutAdvance(ctx, input)
		require.NoError(t, err)
		require.False(t, first.Allowed)
		require.Contains(t, first.BlockedSummary(), convergenceBlockedPrefix)

		now = now.Add(writerLeaseTTL + time.Second)
		second, err := store.EvaluateRolloutAdvance(ctx, input)
		require.NoError(t, err)
		require.True(t, second.Allowed, second.BlockedSummary())
		require.Equal(t, SessionModeV3Write, second.Target)
	})

	t.Run("bounded mirrors persistent and over-max reader rejection", func(t *testing.T) {
		store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
		registry := NewWriterRegistry(client, store.UIDTokenPrefix())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.NoError(t, registry.Join(ctx, "build-a", "pod-a", string(SessionModeRevoke), time.Now))
		require.NoError(t, client.Set(store.tokenKey("persistent"), "u1@n", 0).Err())

		input := RolloutAdvanceInput{
			State:    RolloutState{Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3},
			Registry: registry, ScanBatchSize: 10,
		}
		blocked, err := store.EvaluateRolloutAdvance(ctx, input)
		require.NoError(t, err)
		require.False(t, blocked.Allowed)
		require.Contains(t, blocked.BlockedSummary(), "persistent=1")

		require.NoError(t, client.Del(store.tokenKey("persistent")).Err())
		allowed, err := store.EvaluateRolloutAdvance(ctx, input)
		require.NoError(t, err)
		require.True(t, allowed.Allowed, allowed.BlockedSummary())
	})

	t.Run("enforce rejects remaining v2", func(t *testing.T) {
		store, client := newLegacyMigrationTestStore(t, SessionModeBounded)
		registry := NewWriterRegistry(client, store.UIDTokenPrefix())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.NoError(t, registry.Join(ctx, "build-a", "pod-a", string(SessionModeBounded), time.Now))
		require.NoError(t, client.Set(store.tokenKey("legacy"), v2Fixture(t, "u1"), time.Hour).Err())

		input := RolloutAdvanceInput{
			State:    RolloutState{Floor: SessionModeBounded, MaxPerUID: 20, Version: 4},
			Registry: registry, ScanBatchSize: 10,
		}
		blocked, err := store.EvaluateRolloutAdvance(ctx, input)
		require.NoError(t, err)
		require.False(t, blocked.Allowed)
		require.Contains(t, blocked.BlockedSummary(), "v1=0 v2=1")
	})

	t.Run("writer mutation during scan invalidates evidence", func(t *testing.T) {
		store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
		registry := NewWriterRegistry(client, store.UIDTokenPrefix())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.NoError(t, registry.Join(ctx, "build-a", "pod-a", string(SessionModeRevoke), time.Now))

		var once sync.Once
		store.redisInstanceIDFn = func() (string, error) {
			once.Do(func() {
				require.NoError(t, registry.PublishAppliedState(string(SessionModeBounded)))
			})
			return "redis-a", nil
		}
		decision, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{
			State:    RolloutState{Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3},
			Registry: registry, ScanBatchSize: 10,
		})
		require.NoError(t, err)
		require.False(t, decision.Allowed)
		require.Contains(t, decision.BlockedBy, "writer set changed during evaluation")
	})
}

func TestRedisFloorRollbackCannotLowerMySQLAuthoritativeMode(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	registry := &WriterRegistry{
		now:           time.Now,
		self:          WriterEntry{ID: "writer-1", Build: "build-1", AppliedState: string(SessionModeRevoke)},
		lastRefreshAt: time.Now(),
		publishFn:     func(WriterEntry) error { return nil },
	}
	store.UseWriterLease(registry)
	control := &staticRolloutController{state: RolloutState{
		Floor: SessionModeBounded, MaxPerUID: 20, Version: 4,
	}}
	reconciler := NewRolloutReconciler(store, ReconcilerOptions{Control: control, Registry: registry})

	require.NoError(t, client.Set(store.rolloutControlKey(),
		`{"mode_floor":"enforce","writer_version":3,"observation_min_gap_ms":0}`, 0).Err())
	reconciler.pollFloor(context.Background())
	require.Equal(t, SessionModeBounded, store.Mode())
	require.NoError(t, client.Del(store.rolloutControlKey()).Err(), "simulate Redis restore before the old floor")
	reconciler.pollFloor(context.Background())
	require.Equal(t, SessionModeBounded, store.Mode(), "Redis contents are not a runtime authority")
}

type staticRolloutController struct{ state RolloutState }

func (c *staticRolloutController) Load(context.Context) (RolloutState, error) { return c.state, nil }
func (c *staticRolloutController) Advance(
	context.Context,
	RolloutState,
	RolloutAdvanceRecord,
) (RolloutState, error) {
	return c.state, nil
}

type failingRolloutController struct {
	loadErr      error
	advanceCalls int
}

func (c *failingRolloutController) Load(context.Context) (RolloutState, error) {
	return RolloutState{}, c.loadErr
}

func (c *failingRolloutController) Advance(
	context.Context,
	RolloutState,
	RolloutAdvanceRecord,
) (RolloutState, error) {
	c.advanceCalls++
	return RolloutState{}, nil
}

func newMockRolloutControlStore(t *testing.T) (*RolloutControlStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return NewRolloutControlStore(conn.NewSession(nil)), mock, func() { _ = rawDB.Close() }
}
