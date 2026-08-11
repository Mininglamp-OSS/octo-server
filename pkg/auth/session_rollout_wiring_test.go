package auth

// Wiring acceptance for the boot path.
//
// Every blocking finding on this change lived here rather than in the pure
// functions: the order of main's startup, runtime swallowing an apply error,
// the registry publishing a mode nobody applied. The pure-function tests were
// all green while the boot path denied every legacy session on first upgrade,
// so these drive SessionStoreAndClientForContext itself.

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

// bootHarness stands in for the server wiring: a config.Context carrying a real
// DB, plus prefixes isolated per test.
type bootHarness struct {
	ctx     *config.Context
	store   *RedisSessionStore
	markers *RolloutMarkerStore
	db      *sql.DB
}

func newBootHarness(t *testing.T, withMarkerTable bool) *bootHarness {
	t.Helper()
	unsetSessionRuntimeEnv(t)

	db, err := sql.Open("mysql", rolloutMarkerTestDSN)
	if err != nil {
		t.Skipf("test MySQL unavailable: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test MySQL unavailable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Never DROP: this database is shared with every other package's tests and
	// the table is created by a real migration. Dropping it makes an unrelated
	// package fail with Error 1146, order-dependently. "Absent" is simulated by
	// pointing the store at a table name that does not exist.
	if !withMarkerTable {
		rolloutMarkerTable = "octo_session_rollout_marker_absent_" + strings.ReplaceAll(util.GenerUUID(), "-", "")
		t.Cleanup(func() { rolloutMarkerTable = defaultRolloutMarkerTable })
	}
	_, err = db.Exec("DELETE FROM " + defaultRolloutMarkerTable)
	if err != nil && !isMissingMarkerTable(err) {
		require.NoError(t, err)
	}
	if withMarkerTable {
		_, err = db.Exec("CREATE TABLE IF NOT EXISTS " + rolloutMarkerTable + " (" +
			"`singleton_id` TINYINT UNSIGNED NOT NULL," +
			"`initialized_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"`initialized_floor` VARCHAR(16) NOT NULL DEFAULT ''," +
			"`created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"PRIMARY KEY (`singleton_id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
		require.NoError(t, err)
	}

	cfg := config.New()
	cfg.DB.MySQLAddr = rolloutMarkerTestDSN
	cfg.Cache.TokenCachePrefix = "wiring:" + util.GenerUUID() + ":"
	cfg.Cache.UIDTokenCachePrefix = "wiring-uid:" + util.GenerUUID() + ":"
	cfg.Cache.TokenExpire = time.Hour
	ctx := config.NewContext(cfg)

	conn := &dbr.Connection{DB: db, Dialect: dialect.MySQL, EventReceiver: &dbr.NullEventReceiver{}}
	session := conn.NewSession(nil)

	h := &bootHarness{ctx: ctx, markers: NewRolloutMarkerStore(session), db: db}
	// A store built directly, used to seed Redis state before boot runs.
	h.store = NewRedisSessionStore(newTestRedisClient(t), cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Hour)
	t.Cleanup(func() {
		for _, prefix := range []string{cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix} {
			if keys, _ := h.store.client.Keys(prefix + "*").Result(); len(keys) > 0 {
				_ = h.store.client.Del(keys...).Err()
			}
		}
	})
	return h
}

// boot drives the ACTUAL server wiring, not a re-implementation of it.
//
// The earlier version of this helper called ResolveRolloutBoot and
// ApplyRolloutState directly and hand-copied runtime.go's error fallback. That
// is why two critical defects in that fallback — enforce with no cap, fencing
// every login permanently — sat here untested while the file header claimed
// these tests drove the boot path. A harness that reimplements the thing it
// tests cannot catch a bug in the thing it tests.
func (h *bootHarness) boot(t *testing.T, legacyMode SessionMode, legacyCap int) RolloutBoot {
	t.Helper()
	if legacyMode.valid() {
		t.Setenv(sessionModeEnv, string(legacyMode))
	}
	if legacyCap > 0 {
		t.Setenv(sessionMaxPerUIDEnv, strconv.Itoa(legacyCap))
	}
	// A fresh context per boot: the runtime memoises one store per context.
	ctx := config.NewContext(h.ctx.GetConfig())
	store, client := SessionStoreAndClientForContext(ctx)
	t.Cleanup(func() { _ = client.Close() })
	boot, _ := SessionBootForContext(ctx)
	h.store = store
	return boot
}

// W1 — the first boot of this artifact. The marker table is created by a
// modules/user migration that runs long after the session store is built, so on
// this boot it does not exist. Resolving that as "recovered" denied every
// legacy session in the deployment; resolving it as "never stamped" — which is
// literally what a missing table means — adopts the floor and logs nobody out.
func TestWiringFirstBootBeforeMarkerMigration(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, false)

	require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
	require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeRevoke, 0))
	require.NoError(t, h.store.client.Set(h.store.tokenKey("legacyuser"), "u1@Alice", time.Hour).Err())

	boot := h.boot(t, SessionModeRevoke, 20)
	require.Equal(t, SessionModeRevoke, boot.Mode,
		"a floor that read successfully must not be discarded because the marker table is absent")
	require.Equal(t, RolloutBootAdopted, boot.Outcome)

	record, err := h.store.ReadToken(ctx, h.store.tokenKey("legacyuser"))
	require.NoError(t, err)
	info, err := Decode(record.Payload)
	require.NoError(t, err)
	require.NoError(t, h.store.ValidateLegacySession(ctx, info, record),
		"the upgrade must not log anyone out")
}

// W2 — genuine Redis loss after the operator followed the runbook and deleted
// the deprecated cap. Recovery previously could not be applied without a cap,
// so the store stayed at its constructor default expand: strictly looser than
// the floor that was lost, and blind to legacy deny markers.
func TestWiringRecoveryAppliesEnforceWithoutTheDeprecatedCap(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)
	require.NoError(t, h.markers.StampOnce(ctx, SessionModeRevoke))
	require.NoError(t, h.store.client.Set(h.store.tokenKey("revokeduser"), "u1@Alice", time.Hour).Err())

	boot := h.boot(t, "", 0)
	require.Equal(t, RolloutBootRecovered, boot.Outcome)
	require.Equal(t, SessionModeEnforce, boot.Mode,
		"recovery must actually be applied, not degrade to expand")
	require.Positive(t, boot.MaxPerUID, "a cap must be recoverable without the deprecated env")

	record, err := h.store.ReadToken(ctx, h.store.tokenKey("revokeduser"))
	require.NoError(t, err)
	info, err := Decode(record.Payload)
	require.NoError(t, err)
	require.ErrorIs(t, h.store.ValidateLegacySession(ctx, info, record), ErrLegacySessionDenied,
		"a resurrected legacy session must stay denied")
}

// W3 — a corrupt record must be replaced, not left in place by a SET NX that
// reports success. It previously survived "recovery" and crashed the next read.
func TestWiringRecoveryReplacesACorruptRecord(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)
	require.NoError(t, h.markers.StampOnce(ctx, SessionModeRevoke))
	require.NoError(t, h.store.client.Set(h.store.rolloutControlKey(), "{not json", 0).Err())

	boot := h.boot(t, "", 20)
	require.Equal(t, SessionModeEnforce, boot.Mode)

	// Boot itself repairs it. A SET NX left the corrupt payload in place while
	// reporting success, and the very next read then crashed startup.
	control, err := h.store.RolloutControl(ctx)
	require.NoError(t, err, "the record must be readable after boot, not still corrupt")
	require.Equal(t, SessionModeEnforce, control.ModeFloor)

	// And a second attempt is a correct no-op now that the floor is readable.
	recovered, err := h.store.RecoverRolloutControlAtEnforce(ctx, 20)
	require.NoError(t, err)
	require.False(t, recovered, "a readable floor always wins")
}

// W4 — a floor that reappeared between the read and the write always wins, and
// recovery must say it did nothing rather than claim success.
func TestWiringRecoveryNeverLowersAReadableFloor(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)
	require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))

	recovered, err := h.store.RecoverRolloutControlAtEnforce(ctx, 20)
	require.NoError(t, err)
	require.False(t, recovered)

	control, err := h.store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Equal(t, SessionModeV3Write, control.ModeFloor)
}

// W5 — a #725 record carries no cap, so it has to be written back. Without
// this the cap lives only in memory and the next restart has a v3-writing floor
// with no bound.
func TestWiringCapIsPersistedIntoALegacyFloorRecord(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)
	require.NoError(t, h.store.client.Set(h.store.rolloutControlKey(),
		`{"mode_floor":"revoke","writer_version":3,"observation_min_gap_ms":3600000}`, 0).Err())

	control, err := h.store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Zero(t, control.MaxPerUID)

	reconciler := NewRolloutReconciler(h.store, ReconcilerOptions{Markers: h.markers, MaxPerUID: 20})
	reconciler.pollFloor(ctx)

	control, err = h.store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Equal(t, 20, control.MaxPerUID, "the cap must survive a restart")
	require.Equal(t, SessionModeRevoke, h.store.Mode())
}

// W6 — the marker is stamped by the reconciler, so a floor established while
// the table was still missing gets its marker as soon as the migration lands.
// Until it does, a Redis loss reads as "never initialised" and resolves DOWN.
func TestWiringReconcilerStampsAMissingMarker(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)
	require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))

	marker, err := h.markers.Load(ctx)
	require.NoError(t, err)
	require.Nil(t, marker)

	reconciler := NewRolloutReconciler(h.store, ReconcilerOptions{Markers: h.markers, MaxPerUID: 20})
	reconciler.pollFloor(ctx)

	marker, err = h.markers.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, marker, "a floor without a marker resolves downward after a Redis loss")
	require.Equal(t, SessionModeV3Write, marker.InitializedFloor)
}

// W7 — an advance without a marker must not happen. The floor would then exist
// with nothing proving this deployment initialised it.
func TestWiringAdvanceRefusesWithoutAMarker(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, false) // no marker table: stamping cannot succeed
	registry := NewWriterRegistry(h.store.client, h.store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))

	reconciler := NewRolloutReconciler(h.store, ReconcilerOptions{
		Registry: registry, Markers: h.markers, AutoAdvance: true, MaxPerUID: 20,
	})
	reconciler.reconcileOnce(ctx)

	control, err := h.store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Nil(t, control, "the floor must not move when the marker cannot be stamped")
}

// W8 — the registry advertises the mode this replica APPLIED. Publishing the
// resolved value let a replica running at expand claim enforce, which is enough
// to satisfy the convergence gate it is supposed to constrain.
func TestWiringRegistryPublishesTheAppliedMode(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)
	require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
	registry := NewWriterRegistry(h.store.client, h.store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))

	reconciler := NewRolloutReconciler(h.store, ReconcilerOptions{
		Registry: registry, Markers: h.markers, MaxPerUID: 20,
	})
	reconciler.pollFloor(ctx)

	live, err := registry.Live()
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, string(h.store.Mode()), live[0].AppliedState)
	require.Equal(t, string(SessionModeV3Write), live[0].AppliedState)
}

// W9 — reader strictness and write capability are separate. A v3-writing mode
// with no cap must still deny legacy while refusing to mint credentials;
// coupling them is what turned a missing cap into a looser reader.
func TestWiringMissingCapFencesWritesWithoutLooseningReads(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)

	require.NoError(t, h.store.ApplyRolloutState(SessionModeEnforce, 0))
	require.Equal(t, SessionModeEnforce, h.store.Mode(), "the mode applies regardless of the cap")

	require.ErrorIs(t, h.store.IssueNew(ctx, "tok", "u1@Alice", "u1", 1), ErrSessionCapUnavailable)

	require.NoError(t, h.store.client.Set(h.store.tokenKey("legacyuser"), "u1@Alice", time.Hour).Err())
	record, err := h.store.ReadToken(ctx, h.store.tokenKey("legacyuser"))
	require.NoError(t, err)
	info, err := Decode(record.Payload)
	require.NoError(t, err)
	require.ErrorIs(t, h.store.ValidateLegacySession(ctx, info, record), ErrLegacySessionDenied)
}

func newTestRedisClient(t *testing.T) *rd.Client {
	t.Helper()
	client := octoredis.NewInstrumentedClient(config.New())
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// W10 — the fence closes on the holder's own clock. A process stalled past the
// TTL (GC pause, CPU throttling, SIGSTOP) must stop writing even though its
// last refresh succeeded, because the registry has already stopped listing it
// and the gate may have advanced on that basis.
func TestWiringLeaseExpiresOnTheHoldersClock(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)
	registry := NewWriterRegistry(h.store.client, h.store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
	require.True(t, registry.MayWrite())

	registry.mu.Lock()
	registry.lastRefreshAt = registry.lastRefreshAt.Add(-writerLeaseTTL)
	registry.mu.Unlock()
	require.False(t, registry.MayWrite(),
		"a stalled process must fence itself, not wait for a write to fail")
}

// W11 — a failed first refresh must not fence a replica for life. The heartbeat
// has to start regardless so a startup-time Redis blip heals itself.
func TestWiringStartupRefreshFailureStillStartsTheHeartbeat(t *testing.T) {
	h := newBootHarness(t, true)
	unreachable := rd.NewClient(&rd.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = unreachable.Close() })

	registry := NewWriterRegistry(unreachable, h.store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil)
	require.Error(t, err, "the failure is reported")
	require.False(t, registry.MayWrite(), "and the replica is fenced meanwhile")

	// The heartbeat goroutine exists and will retry; the entry id was assigned,
	// which is what a later successful refresh needs.
	registry.mu.RLock()
	id := registry.self.ID
	registry.mu.RUnlock()
	require.NotEmpty(t, id, "a retryable identity must have been established")
}

// W12 — the scope fingerprint distinguishes two genuinely different Redis
// instances. It used to hash only config-derived values, so one config reaching
// two endpoints produced byte-identical fingerprints — the defect that let a
// misplaced config key point a tool at the wrong Redis silently.
func TestWiringScopeFingerprintSeparatesRedisInstances(t *testing.T) {
	h := newBootHarness(t, true)
	second := rd.NewClient(&rd.Options{Addr: secondRedisAddr})
	if err := second.Ping().Err(); err != nil {
		t.Skipf("second Redis at %s unavailable: %v", secondRedisAddr, err)
	}
	t.Cleanup(func() { _ = second.Close() })

	other := NewRedisSessionStore(second, h.store.tokenPrefix, h.store.uidTokenPrefix, h.store.maxTTL)
	require.NotEqual(t,
		h.store.rolloutObservationScopeFingerprint(),
		other.rolloutObservationScopeFingerprint(),
		"identical config reaching two instances must not produce one fingerprint")

	// Degrades rather than breaks when instance identity is unavailable.
	require.NotEmpty(t, h.store.scopeFingerprintWithInstance(""))
}

// secondRedisAddr is a genuinely different Redis instance, used to reproduce
// the 2026-08-11 misconfiguration.
const secondRedisAddr = "127.0.0.1:6380"

// W13 — bounded is gated on a live scan. It was not, and bounded rejects every
// persistent and over-max legacy record at read time, so a converged fleet
// could be walked into a user-visible denial phase with no scan, no migration
// and no cutoff — logging out exactly the population whose fate is supposed to
// be the operator's decision.
func TestWiringBoundedIsGatedOnPersistentLegacy(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)
	for _, mode := range []SessionMode{SessionModeV3Write, SessionModeRevoke} {
		require.NoError(t, h.store.AdvanceRolloutControl(ctx, mode, 20))
	}
	registry := NewWriterRegistry(h.store.client, h.store.uidTokenPrefix)
	regCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeRevoke), nil))

	// A permanent legacy token: exactly what bounded denies on sight.
	require.NoError(t, h.store.client.Set(h.store.tokenKey("permanent"), "u1@Alice", 0).Err())

	blocked, err := h.store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{Registry: registry, MaxPerUID: 20})
	require.NoError(t, err)
	require.Equal(t, SessionModeBounded, blocked.Target)
	require.False(t, blocked.Allowed, "bounded must not be reached before that population is decided")
	require.Contains(t, blocked.BlockedSummary(), "persistent=1")
	require.NotEmpty(t, blocked.Options, "a blocker must come with something the operator can do")

	// Once it is gone, the same gate opens.
	require.NoError(t, h.store.client.Del(h.store.tokenKey("permanent")).Err())
	allowed, err := h.store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{Registry: registry, MaxPerUID: 20})
	require.NoError(t, err)
	require.True(t, allowed.Allowed, allowed.BlockedSummary())
}
