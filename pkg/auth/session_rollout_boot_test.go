package auth

// Acceptance for the redesign. These are the inverted forms of the tripwires
// recorded in .octospec/tasks/token-session-rollout-simplify/verification.md:
// each one asserts the behaviour that replaced a measured defect, so a
// regression here is a regression to the thing this change removed.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

const rolloutMarkerTestDSN = "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true"

// newMarkerStoreForTest gives each test its own marker row by truncating the
// singleton table, which is safe because it holds exactly one row by
// construction. It creates the table directly rather than booting a server: the
// marker is a single row and pkg/auth has no other reason to depend on the
// module wiring.
func newMarkerStoreForTest(t *testing.T) *RolloutMarkerStore {
	t.Helper()
	db, err := sql.Open("mysql", rolloutMarkerTestDSN)
	if err != nil {
		t.Skipf("test MySQL unavailable: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test MySQL unavailable: %v", err)
	}
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS " + rolloutMarkerTable + " (" +
		"`singleton_id` TINYINT UNSIGNED NOT NULL," +
		"`initialized_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
		"`initialized_floor` VARCHAR(16) NOT NULL DEFAULT ''," +
		"`created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
		"PRIMARY KEY (`singleton_id`)," +
		"CONSTRAINT `chk_session_rollout_marker_singleton` CHECK (`singleton_id` = 1)" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci")
	require.NoError(t, err)
	_, err = db.Exec("DELETE FROM " + rolloutMarkerTable)
	require.NoError(t, err)
	conn := &dbr.Connection{DB: db, Dialect: dialect.MySQL, EventReceiver: &dbr.NullEventReceiver{}}
	t.Cleanup(func() { _ = db.Close() })
	return NewRolloutMarkerStore(conn.NewSession(nil))
}

// A1 (inverts T1): a missing floor no longer refuses startup. Which way it
// resolves depends on the marker, because "never initialised" and "lost to an
// RDB rollback" need opposite reactions and Redis cannot tell them apart.
func TestBootResolvesMissingFloorWithoutFailing(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh: no marker, no floor -> expand", func(t *testing.T) {
		store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
		markers := newMarkerStoreForTest(t)
		boot, err := ResolveRolloutBoot(ctx, store, markers, "", 0)
		require.NoError(t, err)
		require.Equal(t, RolloutBootFresh, boot.Outcome)
		require.Equal(t, SessionModeExpand, boot.Mode)
	})

	t.Run("adopted: no marker, floor present -> adopt unchanged", func(t *testing.T) {
		store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
		markers := newMarkerStoreForTest(t)
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, 0))

		boot, err := ResolveRolloutBoot(ctx, store, markers, "", 0)
		require.NoError(t, err)
		require.Equal(t, RolloutBootAdopted, boot.Outcome)
		require.Equal(t, SessionModeRevoke, boot.Mode, "an in-flight #725 rollout is adopted as-is")
		require.Equal(t, 20, boot.MaxPerUID, "the cap comes from the floor record")
	})

	t.Run("recovered: marker present, floor gone -> enforce, never expand", func(t *testing.T) {
		store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
		markers := newMarkerStoreForTest(t)
		require.NoError(t, markers.StampOnce(ctx, SessionModeRevoke))
		require.NoError(t, client.Del(store.rolloutControlKey()).Err())

		boot, err := ResolveRolloutBoot(ctx, store, markers, "", 20)
		require.NoError(t, err, "a lost floor must not fail the process")
		require.Equal(t, RolloutBootRecovered, boot.Outcome)
		require.Equal(t, SessionModeEnforce, boot.Mode,
			"a lost floor resolves upward; resolving down would re-admit resurrected legacy tokens")
		require.NotEmpty(t, boot.Warning)
	})
}

// A1b: an UNREADABLE floor resolves the same way a missing one does. Falling
// back to expand here would be a fail-open, because expand does not consult
// legacy deny markers — a transient Redis error on a revoke-floor deployment
// would let already-revoked legacy bearers back in.
func TestUnreadableFloorResolvesUpwardNotToExpand(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	markers := newMarkerStoreForTest(t)
	require.NoError(t, markers.StampOnce(ctx, SessionModeRevoke))
	require.NoError(t, client.Set(store.rolloutControlKey(), "{not json", 0).Err())

	boot, err := ResolveRolloutBoot(ctx, store, markers, "", 20)
	require.NoError(t, err, "an unreadable floor must not fail the process")
	require.Equal(t, RolloutBootRecovered, boot.Outcome)
	require.Equal(t, SessionModeEnforce, boot.Mode,
		"corrupt must resolve upward; expand would stop checking deny markers")
	require.NotEmpty(t, boot.Warning)
}

// A1c: an unreadable floor with no marker is UNKNOWN, not fresh.
//
// This assertion is inverted from what it originally claimed. It used to read
// "the same unreadable floor on a deployment that never established one stays
// at expand — there is nothing to protect", and that reasoning has a hole big
// enough to drive the whole finding through: the absence of a marker does not
// establish that no floor was ever created. It also covers the boot before the
// marker table's migration has run and the entire window before some replica's
// poller stamps the row. Concluding "fresh" from it put an initialised
// deployment at expand, which stops consulting legacy deny markers.
//
// Note the setup is itself evidence: a *genuinely* fresh deployment has no
// floor key at all. A corrupt one means something wrote it.
func TestUnreadableFloorWithNoMarkerIsUnknownNotFresh(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	markers := newMarkerStoreForTest(t)
	require.NoError(t, client.Set(store.rolloutControlKey(), "{not json", 0).Err())

	boot, err := ResolveRolloutBoot(ctx, store, markers, "", 0)
	require.NoError(t, err)
	require.Equal(t, RolloutBootUnknown, boot.Outcome)
	require.Equal(t, SessionModeEnforce, boot.Mode)
	require.True(t, boot.Provisional,
		"a mode guessed from a failed read must be replaceable by one that was actually read")
	require.NotEmpty(t, boot.Warning)
}

// A1d: and the genuinely fresh case — no floor, no error, no marker — still
// starts at expand. The fix has to separate "unreadable" from "absent", not
// collapse them in the other direction.
func TestGenuinelyAbsentFloorWithNoMarkerStaysAtExpand(t *testing.T) {
	ctx := context.Background()
	store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
	markers := newMarkerStoreForTest(t)

	boot, err := ResolveRolloutBoot(ctx, store, markers, "", 0)
	require.NoError(t, err)
	require.Equal(t, RolloutBootFresh, boot.Outcome)
	require.Equal(t, SessionModeExpand, boot.Mode)
	require.False(t, boot.Provisional)
}

// A2 (§3.5②): the naive `mode = floor` would silently loosen a deployment that
// #725 left mid-canary at bounded on a revoke floor, re-admitting permanent and
// over-max legacy tokens on upgrade. The test environment happened to have MODE
// equal to its floor, so this could not have been caught there.
func TestUpgradeNeverLoosensAMidCanaryDeployment(t *testing.T) {
	ctx := context.Background()
	store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
	markers := newMarkerStoreForTest(t)
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, 0))

	boot, err := ResolveRolloutBoot(ctx, store, markers, SessionModeBounded, 20)
	require.NoError(t, err)
	require.Equal(t, SessionModeBounded, boot.Mode,
		"the deprecated env sits above the floor and must be honoured for one release")
	require.Contains(t, boot.Warning, sessionCanaryAheadEnv)

	// And the reader must actually still reject what bounded rejects.
	require.NoError(t, store.ApplyRolloutState(boot.Mode, boot.MaxPerUID))
	err = store.ValidateLegacySession(ctx, TokenInfo{UID: "u1"}, TokenRecord{TTL: -1})
	require.ErrorIs(t, err, ErrLegacySessionDenied,
		"a persistent legacy token must stay denied across the upgrade")
}

// A3 (inverts T4): an empty keyspace is now the strongest evidence rather than
// a rejected one, so greenfield reaches enforce with no canary login, no empty
// migration and no waiting — via the same code path as brownfield.
//
// It still needs the replica count for the first v3 floor. Emptiness proves
// nothing is stored right now, not that no unregistered writer exists, so that
// gate is unconditional; the deployment supplies a number it already knows and
// runs no commands.
func TestGreenfieldReachesEnforceWithoutCeremony(t *testing.T) {
	ctx := context.Background()
	clock := time.Now().UTC()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	store.now = func() time.Time { return clock }
	registry := NewWriterRegistry(client, store.uidTokenPrefix)
	registryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(registryCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
	t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })

	// Greenfield still pays for the first v3 floor's convergence window — one
	// lease TTL of the writer set holding still, unattended and with no operator
	// command. That is the whole cost, and it is not ceremony: it is the only
	// transition a pre-registry replica could be invisible across.
	convergence := NewWriterConvergence()
	markers := newMarkerStoreForTest(t)
	priming, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{
		Registry: registry, MaxPerUID: 20, ExpectWriters: 1, Convergence: convergence, Markers: markers,
	})
	require.NoError(t, err)
	require.False(t, priming.Allowed)
	clock = clock.Add(writerLeaseTTL + time.Second)

	floor := SessionMode("")
	for i := 0; i < 5; i++ {
		decision, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{
			Registry:      registry,
			MaxPerUID:     20,
			ExpectWriters: 1,
			Convergence:   convergence,
			Markers:       markers,
		})
		require.NoError(t, err)
		if decision.Current == SessionModeEnforce {
			floor = SessionModeEnforce
			break
		}
		require.True(t, decision.Allowed,
			"empty keyspace must not block %s: %s", decision.Target, decision.BlockedSummary())
		require.NoError(t, store.AdvanceRolloutControl(ctx, decision.Target, decision.MaxPerUID))
		registry.SetAppliedState(string(decision.Target))
	}
	require.Equal(t, SessionModeEnforce, floor, "greenfield must reach enforce unattended")
}

// A4: the gate refuses on every shape of an unconverged fleet, and an empty
// registry is a failure rather than a pass — the mirror of the token scan,
// where emptiness proves absence and is the strongest evidence.
func TestAdvanceGateRefusesUnconvergedFleets(t *testing.T) {
	ctx := context.Background()

	t.Run("empty registry blocks", func(t *testing.T) {
		store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
		registry := NewWriterRegistry(client, store.uidTokenPrefix)
		decision, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{Registry: registry, MaxPerUID: 20})
		require.NoError(t, err)
		require.False(t, decision.Allowed)
		require.Contains(t, decision.BlockedSummary(), "no live writers")
	})

	t.Run("mixed builds block", func(t *testing.T) {
		store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
		prefix := store.uidTokenPrefix
		a := NewWriterRegistry(client, prefix)
		b := NewWriterRegistry(client, prefix)
		regCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		require.NoError(t, a.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
		require.NoError(t, b.Join(regCtx, "build-b", "pod-b", string(SessionModeExpand), nil))
		t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })

		decision, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{Registry: a, MaxPerUID: 20})
		require.NoError(t, err)
		require.False(t, decision.Allowed)
		require.Contains(t, decision.BlockedSummary(), "distinct builds")
	})

	t.Run("a writer behind the floor blocks", func(t *testing.T) {
		store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
		prefix := store.uidTokenPrefix
		behind := NewWriterRegistry(client, prefix)
		regCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		require.NoError(t, behind.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
		t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })

		decision, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{Registry: behind, MaxPerUID: 20})
		require.NoError(t, err)
		require.False(t, decision.Allowed)
		require.Contains(t, decision.BlockedSummary(), "not applied floor")
	})
}

// A5: the first v3 floor needs the replica count, because a pre-#725 build
// never registers and the registry structurally cannot see it. Only this one
// transition needs it — and it is required whether or not the keyspace is
// empty, since empty only means nothing is stored right now.
func TestFirstV3FloorAlwaysNeedsTheReplicaCount(t *testing.T) {
	ctx := context.Background()
	clock := time.Now().UTC()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	store.now = func() time.Time { return clock }
	prefix := store.uidTokenPrefix
	registry := NewWriterRegistry(client, prefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
	t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })
	require.NoError(t, client.Set(store.tokenKey("legacy"), `v2:{"uid":"u1"}`, time.Hour).Err())

	unset, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{Registry: registry, MaxPerUID: 20})
	require.NoError(t, err)
	require.False(t, unset.Allowed)
	require.Contains(t, unset.BlockedSummary(), sessionExpectWritersEnv)
	require.Contains(t, unset.BlockedSummary(), "first v3 floor")

	short, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{Registry: registry, MaxPerUID: 20, ExpectWriters: 3})
	require.NoError(t, err)
	require.False(t, short.Allowed, "a leftover pre-#725 replica makes the count come up short")
	require.Contains(t, short.BlockedSummary(), "expected 3 writers, registry has 1")

	// The count alone is not enough any more: the set has to hold still for a
	// lease TTL as well, so prime the window and let it mature.
	converged := RolloutAdvanceInput{
		Registry: registry, MaxPerUID: 20, ExpectWriters: 1,
		Convergence: NewWriterConvergence(), Markers: newMarkerStoreForTest(t),
	}
	priming, err := store.EvaluateRolloutAdvance(ctx, converged)
	require.NoError(t, err)
	require.False(t, priming.Allowed, "the count matches, but nothing has been observed over time yet")

	clock = clock.Add(writerLeaseTTL + time.Second)
	matched, err := store.EvaluateRolloutAdvance(ctx, converged)
	require.NoError(t, err)
	require.True(t, matched.Allowed, matched.BlockedSummary())
}

// A6 (inverts T6): a corrupt payload no longer wedges the floor. It was only
// ever a blocker because the evidence validator was stricter than the security
// requirement — a record that fails Decode has never been a usable credential.
func TestCorruptPayloadDoesNotBlockEnforce(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	prefix := store.uidTokenPrefix
	registry := NewWriterRegistry(client, prefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeBounded), nil))
	t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })

	for _, mode := range []SessionMode{SessionModeV3Write, SessionModeRevoke, SessionModeBounded} {
		require.NoError(t, store.AdvanceRolloutControl(ctx, mode, 20))
	}
	// A finite corrupt record inside maxTTL: the exact shape that used to wedge
	// the floor for up to TokenExpire with no tool able to clear it.
	require.NoError(t, client.Set(store.tokenKey("corrupt"), "\x00\x01garbage", 30*time.Minute).Err())

	observation, err := store.ObserveRateLimited(ctx, 100, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), observation.DecodeInvalid, "it is still reported")

	decision, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{Registry: registry, MaxPerUID: 20})
	require.NoError(t, err)
	require.Equal(t, SessionModeEnforce, decision.Target)
	require.True(t, decision.Allowed, "but it no longer blocks: %s", decision.BlockedSummary())
}

// A7: losing the lease refuses new credentials and nothing else. It must not
// panic, and it must not fail readiness — Redis is unreachable fleet-wide, so
// draining every replica would turn a degradation into an outage.
func TestWriterLeaseFencesWritesOnly(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	prefix := store.uidTokenPrefix
	registry := NewWriterRegistry(client, prefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
	t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })
	store.UseWriterLease(registry)

	require.True(t, registry.MayWrite())
	require.NoError(t, store.IssueNew(ctx, "tok-live", `v2:{"uid":"u1"}`, "u1", 1))

	// Simulate a lost lease the way Redis would: the entry key is gone and the
	// next refresh fails.
	live, err := registry.Live()
	require.NoError(t, err)
	require.Len(t, live, 1)
	// Simulate the process stalling past the lease TTL: the entry expires in
	// Redis while this process keeps running. The fence must close on our own
	// clock, not on the outcome of the last write.
	registry.mu.Lock()
	registry.lastRefreshAt = registry.lastRefreshAt.Add(-writerLeaseTTL)
	registry.mu.Unlock()

	require.ErrorIs(t, store.IssueNew(ctx, "tok-fenced", `v2:{"uid":"u1"}`, "u1", 1), ErrWriterLeaseLost)

	// Reads keep working: existing sessions must not be logged out by a fence.
	record, err := store.ReadToken(ctx, store.tokenKey("tok-live"))
	require.NoError(t, err)
	require.NotEmpty(t, record.Payload)
}

// A8: pause takes effect within one cycle, and an unreadable flag is treated as
// paused — the conservative side of "should I advance an irreversible switch".
func TestRolloutPauseIsRuntimeAndFailsSafe(t *testing.T) {
	ctx := context.Background()
	store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)

	paused, err := store.RolloutPaused(ctx)
	require.NoError(t, err)
	require.False(t, paused)

	require.NoError(t, store.SetRolloutPaused(ctx, true))
	paused, err = store.RolloutPaused(ctx)
	require.NoError(t, err)
	require.True(t, paused, "no restart involved")

	require.NoError(t, store.SetRolloutPaused(ctx, false))
	paused, err = store.RolloutPaused(ctx)
	require.NoError(t, err)
	require.False(t, paused)
}

// A9: the reconciler must not advance while paused or while auto-advance is
// off, and the two combine conservatively.
func TestReconcilerHonoursBothStopSwitches(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	prefix := store.uidTokenPrefix
	registry := NewWriterRegistry(client, prefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
	t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })

	off := NewRolloutReconciler(store, ReconcilerOptions{Registry: registry, AutoAdvance: false})
	off.reconcileOnce(ctx)
	control, err := store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Nil(t, control, "auto-advance off must not move the floor")

	require.NoError(t, store.SetRolloutPaused(ctx, true))
	on := NewRolloutReconciler(store, ReconcilerOptions{Registry: registry, AutoAdvance: true, MaxPerUID: 20})
	on.reconcileOnce(ctx)
	control, err = store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Nil(t, control, "pause must win over auto-advance")
}

// A10: every advance is preceded by its evidence snapshot. Ordering, not
// atomicity, is the requirement — a snapshot with no advance is harmless, an
// advance with no snapshot is not.
func TestEveryAdvanceLeavesAnEvidenceSnapshot(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	prefix := store.uidTokenPrefix
	registry := NewWriterRegistry(client, prefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
	t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })

	markers := newMarkerStoreForTest(t)
	clock := time.Now().UTC()
	store.now = func() time.Time { return clock }
	reconciler := NewRolloutReconciler(store, ReconcilerOptions{
		Registry: registry, Markers: markers, AutoAdvance: true, MaxPerUID: 20, ExpectWriters: 1,
	})
	// Two cycles: the first only starts the convergence window, the second
	// crosses it. The reconciler owns the window precisely because it spans more
	// than one evaluation.
	reconciler.reconcileOnce(ctx)
	clock = clock.Add(writerLeaseTTL + time.Second)
	reconciler.reconcileOnce(ctx)

	control, err := store.RolloutControl(ctx)
	require.NoError(t, err)
	require.NotNil(t, control)
	record, err := store.LastRolloutAdvance(ctx)
	require.NoError(t, err)
	require.NotNil(t, record, "an advance without a snapshot must be impossible")
	require.Equal(t, control.ModeFloor, record.To)
	require.Equal(t, "reconciler", record.Actor)
	require.Equal(t, 1, record.LiveWriters)
	require.Equal(t, []string{"build-a"}, record.Builds)
	require.NotNil(t, record.RedisID, "the snapshot names the instance it looked at")
}

// A11: the reconciler goes quiet at enforce rather than rescanning forever.
func TestReconcilerStopsScanningAtEnforce(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	prefix := store.uidTokenPrefix
	registry := NewWriterRegistry(client, prefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeEnforce), nil))
	t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })
	for _, mode := range []SessionMode{SessionModeV3Write, SessionModeRevoke, SessionModeBounded, SessionModeEnforce} {
		require.NoError(t, store.AdvanceRolloutControl(ctx, mode, 20))
	}

	reconciler := NewRolloutReconciler(store, ReconcilerOptions{Registry: registry, AutoAdvance: true, MaxPerUID: 20})
	next := reconciler.reconcileOnce(ctx)
	require.True(t, next.After(store.now().Add(time.Hour)), "terminal floor must stop the scan loop")
}

// A12: the marker table is written exactly once. There is no UPDATE path in the
// codebase, which is what makes the row write-once, so this guards the source
// as well as the behaviour.
func TestMarkerIsWriteOnce(t *testing.T) {
	ctx := context.Background()
	markers := newMarkerStoreForTest(t)
	require.NoError(t, markers.StampOnce(ctx, SessionModeRevoke))
	first, err := markers.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, SessionModeRevoke, first.InitializedFloor)

	require.NoError(t, markers.StampOnce(ctx, SessionModeEnforce))
	second, err := markers.Load(ctx)
	require.NoError(t, err)
	require.Equal(t, SessionModeRevoke, second.InitializedFloor, "a second stamp is a no-op")
	require.Equal(t, first.InitializedAt, second.InitializedAt)

	source, err := os.ReadFile("session_rollout_marker.go")
	require.NoError(t, err)
	require.NotContains(t, strings.ToUpper(string(source)), "UPDATE "+strings.ToUpper(rolloutMarkerTable),
		"the marker must have no UPDATE path anywhere")
}

// A13: a registry entry carries nothing that could identify a user.
func TestWriterEntryCarriesNoUserData(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	prefix := store.uidTokenPrefix
	registry := NewWriterRegistry(client, prefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
	t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })

	live, err := registry.Live()
	require.NoError(t, err)
	require.Len(t, live, 1)
	rendered := fmt.Sprintf("%+v", live[0])
	for _, forbidden := range []string{"uid", "token", "generation"} {
		require.NotContains(t, strings.ToLower(rendered), forbidden)
	}
}

// A14: an in-place restart is visible. Keying on the pod UID would let the new
// process overwrite the old entry and hide it; a per-incarnation identity makes
// a crash loop show up as several live registrations instead.
func TestWriterIdentityIsPerProcessNotPerPod(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	prefix := store.uidTokenPrefix
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	first := NewWriterRegistry(client, prefix)
	second := NewWriterRegistry(client, prefix)
	require.NoError(t, first.Join(regCtx, "build-a", "same-pod", string(SessionModeExpand), nil))
	require.NoError(t, second.Join(regCtx, "build-a", "same-pod", string(SessionModeExpand), nil))
	t.Cleanup(func() { _ = client.Del(prefix + "writers").Err() })

	live, err := second.Live()
	require.NoError(t, err)
	require.Len(t, live, 2, "two incarnations of one pod must be two registrations")
	require.NotEqual(t, live[0].ID, live[1].ID)
}

var _ = util.GenerUUID

// A15: observe and migrate must count the same keyspace the same way. They
// disagreed by one on the 2026-08-11 test environment (53 vs 54) because the
// migration Lua defaulted anything without a v2/v3 prefix to v1, while observe
// ran Decode. The enforce gate reads observe's numbers and the convergence is
// done by migrate, so two rulers for one measurement is a live hazard once a
// reconciler acts on it unattended.
func TestObserveAndMigrateAgreeOnPayloadVersions(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)

	require.NoError(t, client.Set(store.tokenKey("real-v1"), `u1@Alice`, time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("real-v1-role"), `u2@Bob@admin`, time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("real-v2"), `v2:{"uid":"u3"}`, time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("corrupt"), "\x00\x01not-a-token", time.Hour).Err())
	// A prefix is not proof of decodability. Each of these carries a real
	// version prefix and is still rejected by Decode, which is the case that
	// classifying on the three prefix bytes alone got wrong.
	require.NoError(t, client.Set(store.tokenKey("v2-not-json"), `v2:long`, time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("v2-no-uid"), `v2:{"name":"nobody"}`, time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("v2-uid-empty"), `v2:{"uid":""}`, time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("v2-uid-number"), `v2:{"uid":7}`, time.Hour).Err())
	require.NoError(t, client.Set(store.tokenKey("v3-array"), `v3:[1,2]`, time.Hour).Err())

	observation, err := store.ObserveRateLimited(ctx, 100, 0)
	require.NoError(t, err)
	result, err := store.MigrateLegacySessions(ctx, LegacyMigrationOptions{
		CampaignID:   "parity",
		CutoffAt:     time.Now().UTC().Add(time.Hour),
		FinitePolicy: LegacyFinitePolicyNatural,
		BatchSize:    100,
	})
	require.NoError(t, err)

	require.Equal(t, observation.V1, result.V1, "v1 counts must agree")
	require.Equal(t, observation.V2, result.V2, "v2 counts must agree")
	require.Equal(t, observation.V3, result.V3, "v3 counts must agree")
	require.Equal(t, observation.DecodeInvalid, result.InvalidPayload,
		"decode_invalid and invalid_payload must be the same number")
	require.Equal(t, int64(2), result.V1)
	require.Equal(t, int64(1), result.V2)
	require.Equal(t, int64(0), result.V3)
	require.Equal(t, int64(6), result.InvalidPayload,
		"one unprefixed corruption plus five prefixed-but-undecodable records")
}

// The parity above is exact for everything a writer can produce. It is not
// unlimited, and the boundary is worth pinning rather than discovering: migrate
// mirrors decodeV2 exactly but stops short of decodeV3's lifetime, generation
// and revision checks, so a v3 body that parses with a uid and fails those is
// counted v3 by migrate and decode_invalid by observe.
//
// This costs nothing today. EncodeV3 enforces all five checks before a value is
// ever stored, so such a record cannot be written — it can only arrive by
// corruption — and no rollout gate reads the v3 count. If that ever changes, the
// fix is to mirror the remaining checks in the Lua, and this test is what will
// say so.
func TestObserveAndMigrateDivergeOnlyOnUnwritableV3Bodies(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)

	_, err := EncodeV3(TokenInfo{UID: "u1", Name: "u1"})
	require.Error(t, err, "the writer refuses the very shape this test injects")
	require.NoError(t, client.Set(store.tokenKey("v3-no-lifetime"), `v3:{"uid":"u1"}`, time.Hour).Err())

	observation, err := store.ObserveRateLimited(ctx, 100, 0)
	require.NoError(t, err)
	result, err := store.MigrateLegacySessions(ctx, LegacyMigrationOptions{
		CampaignID:   "v3-divergence",
		CutoffAt:     time.Now().UTC().Add(time.Hour),
		FinitePolicy: LegacyFinitePolicyNatural,
		BatchSize:    100,
	})
	require.NoError(t, err)

	require.Equal(t, int64(1), observation.DecodeInvalid, "observe applies decodeV3 in full")
	require.Equal(t, int64(1), result.V3, "migrate stops at prefix + json object + uid")
	require.Equal(t, int64(0), result.InvalidPayload)
	require.Equal(t, int64(0), observation.V1+observation.V2, "no gate input is affected")
	require.Equal(t, int64(0), result.V1+result.V2)
}

// A16: migrate leaves an undecodable record alone rather than mutating
// something it cannot parse. It is not a usable credential and no longer holds
// the floor, so there is nothing to gain from touching it.
func TestMigrateSkipsUndecodablePayloads(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, 0))
	require.NoError(t, client.Set(store.tokenKey("corrupt"), "\x00\x01garbage", 0).Err())

	result, err := store.MigrateLegacySessions(ctx, LegacyMigrationOptions{
		CampaignID:   "skip-invalid",
		CutoffAt:     time.Now().UTC().Add(30 * time.Minute),
		FinitePolicy: LegacyFinitePolicyCap,
		BatchSize:    100,
		Apply:        true,
		Lease:        30 * time.Second,
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), result.InvalidPayload)
	require.Equal(t, int64(0), result.Shortened)
	require.Equal(t, int64(0), result.Deleted)
	require.Equal(t, -time.Millisecond, mustPTTL(t, client, store.tokenKey("corrupt")),
		"an undecodable record is left exactly as it was")
}

// A17: every path that moves the floor stamps the initialisation marker first.
//
// The reconciler did this and the operator fault channel did not, which is the
// worst possible split: --force is used precisely when the reconciler is broken,
// so the one command an operator reaches for in an incident was the one that
// left the deployment in the single state that resolves DOWNWARD after a Redis
// loss — a floor with no marker reads as "never initialised" and boots at
// expand, which stops checking legacy deny markers.
func TestEveryFloorAdvancePathStampsTheMarker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		actor   string
		advance func(t *testing.T, store *RedisSessionStore, registry *WriterRegistry, markers *RolloutMarkerStore, decision RolloutAdvanceDecision) error
	}{
		{
			name:  "reconciler",
			actor: "reconciler",
			advance: func(t *testing.T, store *RedisSessionStore, registry *WriterRegistry, markers *RolloutMarkerStore, decision RolloutAdvanceDecision) error {
				r := NewRolloutReconciler(store, ReconcilerOptions{Registry: registry, Markers: markers, MaxPerUID: 20})
				return r.advance(context.Background(), decision, "reconciler")
			},
		},
		{
			name:  "advance --force",
			actor: "operator",
			advance: func(t *testing.T, store *RedisSessionStore, registry *WriterRegistry, markers *RolloutMarkerStore, decision RolloutAdvanceDecision) error {
				return store.ForceAdvanceRollout(context.Background(), decision, registry, markers)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
			registry := NewWriterRegistry(client, store.uidTokenPrefix)
			t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })
			markers := newMarkerStoreForTest(t)

			before, err := markers.Load(ctx)
			require.NoError(t, err)
			require.Nil(t, before, "the fixture starts with no marker")

			decision := RolloutAdvanceDecision{
				Current: SessionModeExpand, Target: SessionModeV3Write, Allowed: true, MaxPerUID: 20,
			}
			require.NoError(t, tc.advance(t, store, registry, markers, decision))

			control, err := store.RolloutControl(ctx)
			require.NoError(t, err)
			require.NotNil(t, control)
			require.Equal(t, SessionModeV3Write, control.ModeFloor)

			marker, err := markers.Load(ctx)
			require.NoError(t, err)
			require.NotNil(t, marker, "a floor must never exist without a marker")
			require.Equal(t, SessionModeV3Write, marker.InitializedFloor)

			record, err := store.LastRolloutAdvance(ctx)
			require.NoError(t, err)
			require.NotNil(t, record)
			require.Equal(t, tc.actor, record.Actor)
		})
	}
}

// A18: with no marker store at all, neither path writes a floor. Skipping the
// stamp because the store is absent is the fail-open the ordering exists to
// prevent, so absence has to be refused rather than tolerated.
func TestFloorAdvanceIsRefusedWithoutAMarkerStore(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	registry := NewWriterRegistry(client, store.uidTokenPrefix)
	t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })
	decision := RolloutAdvanceDecision{
		Current: SessionModeExpand, Target: SessionModeV3Write, Allowed: true, MaxPerUID: 20,
	}

	require.ErrorContains(t, store.ForceAdvanceRollout(ctx, decision, registry, nil), "initialisation marker store")

	r := NewRolloutReconciler(store, ReconcilerOptions{Registry: registry, MaxPerUID: 20})
	require.ErrorContains(t, r.advance(ctx, decision, "reconciler"), "initialisation marker store")

	control, err := store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Nil(t, control, "a refused advance must leave no floor behind")
}

// A19: a decision the predicate rejected cannot be written by either path. This
// is what makes --force a bypass of the RECONCILER and not of the gate.
func TestFloorAdvanceRefusesADisallowedDecision(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	registry := NewWriterRegistry(client, store.uidTokenPrefix)
	t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })
	markers := newMarkerStoreForTest(t)

	blocked := RolloutAdvanceDecision{
		Current:   SessionModeExpand,
		Target:    SessionModeV3Write,
		Allowed:   false,
		BlockedBy: []string{"no live writers registered"},
		MaxPerUID: 20,
	}
	require.ErrorContains(t, store.ForceAdvanceRollout(ctx, blocked, registry, markers), "predicate did not allow")

	control, err := store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Nil(t, control)
	marker, err := markers.Load(ctx)
	require.NoError(t, err)
	require.Nil(t, marker, "a refused advance stamps nothing either")
}

// A20 (P1): a single undecodable PERMANENT record must not wedge the ladder.
//
// This is C6, which the redesign was supposed to dissolve and instead relocated.
// The enforce gate stopped treating decode_invalid as a blocker — but nothing
// changed one step earlier, where bounded rejects on Persistent != 0, and
// ObserveRateLimited increments Persistent from the TTL BEFORE it tries to
// decode. So an undecodable permanent record lands in Persistent, bounded
// refuses, and bounded is the only path to enforce.
//
// What makes it unrecoverable rather than merely slow is that the redesign also
// removed the cleanup path: migration now deliberately skips undecodable records
// and `advance --force` re-evaluates the same predicate and refuses. Nothing
// clears a key with no TTL. The floor stops at revoke forever.
//
// The existing TestCorruptPayloadDoesNotBlockEnforce misses this twice: it
// starts the ladder already at bounded, and its corrupt record has a finite
// within-maxTTL deadline. The test environment's one decode_invalid record was
// permanent — the blocking shape.
func TestUndecodablePermanentRecordDoesNotWedgeTheBoundedGate(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	registry := NewWriterRegistry(client, store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeRevoke), nil))
	t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })

	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, 20))
	// Permanent and undecodable: never a usable credential, and nothing expires
	// it.
	require.NoError(t, client.Set(store.tokenKey("corrupt"), "\x00\x01garbage", 0).Err())

	observation, err := store.ObserveRateLimited(ctx, 100, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), observation.DecodeInvalid)
	require.Equal(t, int64(0), observation.Persistent,
		"an undecodable key is not a legacy credential, so it must not be counted as a persistent one")

	decision, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{
		Registry: registry, MaxPerUID: 20, ExpectWriters: 1,
	})
	require.NoError(t, err)
	require.Equal(t, SessionModeBounded, decision.Target)
	require.True(t, decision.Allowed,
		"a record no reader can decode must not hold the ladder: %s", decision.BlockedSummary())
}

// A21: and the gate still blocks on the records bounded actually rejects. The
// fix must narrow the counter to decodable records, not disable the gate — a
// real persistent legacy session is exactly what bounded logs out, and that
// remains the operator's decision to take deliberately.
func TestBoundedGateStillBlocksOnRealPersistentLegacy(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	registry := NewWriterRegistry(client, store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeRevoke), nil))
	t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })

	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, 20))
	require.NoError(t, client.Set(store.tokenKey("real"), "u1@Alice", 0).Err())

	observation, err := store.ObserveRateLimited(ctx, 100, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), observation.Persistent)
	require.Equal(t, int64(1), observation.V1)

	decision, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{
		Registry: registry, MaxPerUID: 20, ExpectWriters: 1,
	})
	require.NoError(t, err)
	require.False(t, decision.Allowed)
	require.Contains(t, decision.BlockedSummary(), "persistent=1")
}

// A22: an undecodable OVER-MAX record is the same case. Counting it in OverMax
// blocks the same gate for the same wrong reason.
func TestUndecodableOverMaxRecordDoesNotWedgeTheBoundedGate(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	require.NoError(t, client.Set(store.tokenKey("corrupt"), "\x00\x01garbage", 3*store.maxTTL).Err())

	observation, err := store.ObserveRateLimited(ctx, 100, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), observation.DecodeInvalid)
	require.Equal(t, int64(0), observation.OverMax)
	require.Equal(t, int64(0), observation.Finite,
		"the shape counters describe legacy credentials, not keys")
}

// A23 (P1): the first v3 floor requires the writer set to have held STILL for a
// full lease TTL, not merely to have the right count at one instant.
//
// Count equality alone cannot distinguish "N new replicas" from "N new replicas
// plus one invisible pre-#725 one". Under maxSurge:1 the moment live == N is
// reachable while an old, unregistered replica is still up and still able to
// issue v2 on the next login — and that replica is exactly what EXPECT_WRITERS
// was introduced to exclude.
//
// Requiring the SET (not the count) to be identical across a window of at least
// writerLeaseTTL closes it, because writer identities are per-incarnation: an
// entry present at both ends of a TTL-long window cannot have expired and been
// recreated in between, and a joining pod changes the set. So an unchanged set
// across that window proves the rollout has stopped moving.
//
// brief §2 specified this and it never landed; both lease-window findings name
// it as their mitigation.
func TestFirstV3FloorRequiresAStableWriterSetForALeaseTTL(t *testing.T) {
	ctx := context.Background()
	clock := time.Now().UTC()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	store.now = func() time.Time { return clock }
	registry := NewWriterRegistry(client, store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
	t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })

	convergence := NewWriterConvergence()
	input := RolloutAdvanceInput{
		Registry: registry, MaxPerUID: 20, ExpectWriters: 1,
		Convergence: convergence, Markers: newMarkerStoreForTest(t),
	}

	first, err := store.EvaluateRolloutAdvance(ctx, input)
	require.NoError(t, err)
	require.Equal(t, SessionModeV3Write, first.Target)
	require.False(t, first.Allowed, "one instant of the right count is not convergence")
	require.Contains(t, first.BlockedSummary(), "stable")

	// Not yet: one second short of the lease TTL.
	clock = clock.Add(writerLeaseTTL - time.Second)
	short, err := store.EvaluateRolloutAdvance(ctx, input)
	require.NoError(t, err)
	require.False(t, short.Allowed)

	clock = clock.Add(2 * time.Second)
	settled, err := store.EvaluateRolloutAdvance(ctx, input)
	require.NoError(t, err)
	require.True(t, settled.Allowed, "blocked by: %s", settled.BlockedSummary())
}

// A24: any change to the set restarts the window. A pod joining mid-rollout is
// precisely the signal that the fleet has not settled.
func TestWriterSetChangeRestartsTheConvergenceWindow(t *testing.T) {
	ctx := context.Background()
	clock := time.Now().UTC()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	store.now = func() time.Time { return clock }
	registry := NewWriterRegistry(client, store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
	t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })

	convergence := NewWriterConvergence()
	input := RolloutAdvanceInput{
		Registry: registry, MaxPerUID: 20, ExpectWriters: 2,
		Convergence: convergence, Markers: newMarkerStoreForTest(t),
	}
	_, err := store.EvaluateRolloutAdvance(ctx, input)
	require.NoError(t, err)
	clock = clock.Add(writerLeaseTTL + time.Second)

	// A second replica appears — the count now matches, but the set just moved.
	second := NewWriterRegistry(client, store.uidTokenPrefix)
	require.NoError(t, second.Join(regCtx, "build-a", "pod-b", string(SessionModeExpand), nil))

	joined, err := store.EvaluateRolloutAdvance(ctx, input)
	require.NoError(t, err)
	require.False(t, joined.Allowed,
		"the count matched only because a pod just joined; the window must restart")

	clock = clock.Add(writerLeaseTTL + time.Second)
	settled, err := store.EvaluateRolloutAdvance(ctx, input)
	require.NoError(t, err)
	require.True(t, settled.Allowed, "blocked by: %s", settled.BlockedSummary())
}

// A25: only the first v3 floor pays for this. Every later transition is fully
// machine-gated by the keyspace itself, so charging each one an extra lease TTL
// would be ceremony of exactly the kind this change exists to delete.
func TestLaterFloorsDoNotRequireTheConvergenceWindow(t *testing.T) {
	ctx := context.Background()
	store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
	registry := NewWriterRegistry(client, store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeV3Write), nil))
	t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))

	decision, err := store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{
		Registry: registry, MaxPerUID: 20, Convergence: NewWriterConvergence(),
	})
	require.NoError(t, err)
	require.Equal(t, SessionModeRevoke, decision.Target)
	require.True(t, decision.Allowed, "blocked by: %s", decision.BlockedSummary())
}

// A26: every boot outcome this package defines must have a metric label.
//
// The two lists are separate on purpose — pkg/metrics does not import pkg/auth —
// so nothing but this test stops them drifting. They already did: `unknown` was
// added here and not there, and because the setter walks the label list, that
// did not hide one value, it zeroed all of them and made the gauge's "exactly
// one outcome is 1" contract false for the four boots an operator most needs to
// see. An alert keyed on that gauge would never have fired.
func TestEveryBootOutcomeHasAMetricLabel(t *testing.T) {
	for _, outcome := range []RolloutBootOutcome{
		RolloutBootFresh,
		RolloutBootAdopted,
		RolloutBootRecovered,
		RolloutBootNormal,
		RolloutBootUnknown,
	} {
		require.True(t, metrics.KnownSessionRolloutBootOutcome(string(outcome)),
			"outcome %q has no metric label; the setter would zero every series", outcome)
	}
}
