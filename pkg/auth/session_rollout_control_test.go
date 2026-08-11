package auth

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

func TestResolveRolloutSeedPersistsTheHighestLegacyPosture(t *testing.T) {
	tests := []struct {
		name         string
		redisFloor   SessionMode
		legacyMode   SessionMode
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
			legacyMax:    12,
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
			legacyMax:    20,
			wantFloor:    SessionModeEnforce,
			wantMaxPerID: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed, err := ResolveRolloutSeed(tt.redisFloor, tt.legacyMode, tt.legacyMax)
			require.NoError(t, err)
			require.Equal(t, tt.wantFloor, seed.Floor)
			require.Equal(t, tt.wantMaxPerID, seed.MaxPerUID)
		})
	}
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

	err := store.ApplyAndPublishRolloutState(registry, SessionModeBounded, 20)
	require.ErrorContains(t, err, "registry unavailable")
	require.Equal(t, SessionModeBounded, store.Mode(), "reader strictness must still apply")
	require.ErrorIs(t, store.CanIssue(), ErrWriterLeaseLost,
		"a replica whose applied state was not published must remain fenced")
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

func newMockRolloutControlStore(t *testing.T) (*RolloutControlStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return NewRolloutControlStore(conn.NewSession(nil)), mock, func() { _ = rawDB.Close() }
}
