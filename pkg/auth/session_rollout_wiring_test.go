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
		// The name must stay within MySQL's 64-character identifier limit. The
		// first version of this concatenated a full UUID onto a 35-character
		// prefix for 67 characters, so every query returned Error 1059
		// ("Identifier name is too long") instead of Error 1146 ("table doesn't
		// exist"). isMissingMarkerTable does not match 1059, so the branch this
		// harness exists to reach was never once executed — and the fail-open
		// living in that branch shipped.
		rolloutMarkerTable = "octo_rollout_marker_absent_" + strings.ReplaceAll(util.GenerUUID(), "-", "")[:16]
		t.Cleanup(func() { rolloutMarkerTable = defaultRolloutMarkerTable })
		require.LessOrEqual(t, len(rolloutMarkerTable), 64, "MySQL identifier limit")
		// And assert the simulation actually simulates. A harness that silently
		// produces the wrong error is worse than no harness: it reports green.
		_, probeErr := db.Query("SELECT 1 FROM " + rolloutMarkerTable + " LIMIT 1") //nolint:rowserrcheck // probe only
		require.Error(t, probeErr)
		require.True(t, isMissingMarkerTable(probeErr),
			"the absent-table simulation must produce the error the production code recognises, got: %v", probeErr)
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

// withFailingFloorRead makes the scripted floor read fail the way a Redis blip,
// a LOADING replica or a slow EVAL against the 2s ReadTimeout does. It is a
// real error from a real Redis, not a double: the point is that
// loadRolloutControl's error path is reachable without corrupting anything, so
// the record on disk stays perfectly valid throughout.
func withFailingFloorRead(t *testing.T) {
	t.Helper()
	savedRolloutReadScript = readRolloutControlScript
	readRolloutControlScript = rd.NewScript(`return redis.error_reply("LOADING simulated transient failure")`)
	t.Cleanup(func() { readRolloutControlScript = savedRolloutReadScript })
}

var savedRolloutReadScript *rd.Script

// W14 (P0-2): an unreadable floor is UNKNOWN state, not a fresh install.
//
// The marker answers "was a floor ever established here?" and nothing else. It
// cannot answer "is there one right now", and when the floor read failed that
// is the question being asked. Collapsing the two means an initialised
// deployment whose floor read blipped is classified fresh and runs at expand —
// and expand stops consulting legacy deny markers, so every bearer revoked at
// revoke/bounded/enforce is admitted again.
//
// The window is wider than the very first pod: the marker ROW is absent until
// some replica's poller stamps it, so `markers.Load` returns (nil, nil) for the
// whole initial rollout, which is exactly when Redis churn is most likely.
func TestWiringUnreadableFloorIsNotAFreshInstall(t *testing.T) {
	t.Run("marker table absent", func(t *testing.T) {
		h := newBootHarness(t, false)
		ctx := context.Background()
		require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
		require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeRevoke, 20))
		withFailingFloorRead(t)

		// No deprecated env: OCTO_AUTH_SESSION_MODE would mask this by raising
		// the mode, and it is the very thing this release tells operators to
		// remove.
		boot := h.boot(t, "", 0)
		require.NotEqual(t, SessionModeExpand, boot.Mode,
			"an unreadable floor must not be read as 'never initialised'; expand re-admits revoked bearers")
		require.NotEqual(t, RolloutBootFresh, boot.Outcome)
	})

	t.Run("table present, marker row not yet stamped", func(t *testing.T) {
		h := newBootHarness(t, true)
		ctx := context.Background()
		require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
		require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeRevoke, 20))
		marker, err := h.markers.Load(ctx)
		require.NoError(t, err)
		require.Nil(t, marker, "this is the un-stamped window every replica boots through")
		withFailingFloorRead(t)

		boot := h.boot(t, "", 0)
		require.NotEqual(t, SessionModeExpand, boot.Mode)
		require.NotEqual(t, RolloutBootFresh, boot.Outcome)
	})
}

// W15: a deployment that genuinely never had a floor still starts at expand.
// The fix must separate "unreadable" from "absent", not collapse them the other
// way — resolving a true greenfield install upward to enforce would deny
// nothing (there is nothing there) but would write an enforce floor nobody
// asked for.
func TestWiringGenuinelyFreshInstallStillStartsAtExpand(t *testing.T) {
	h := newBootHarness(t, false)
	boot := h.boot(t, "", 0)
	require.Equal(t, RolloutBootFresh, boot.Outcome)
	require.Equal(t, SessionModeExpand, boot.Mode)
}

// W16: a provisional enforce is replaced by the first floor that actually
// reads, even though that is downward.
//
// ApplyRolloutState refuses to lower the mode, which is right for an OBSERVED
// floor — that rule is what stops an advance being walked back. Applied to a
// GUESS it is a different thing entirely: a single slow EVAL during a rolling
// upgrade would pin every replica at enforce for the rest of its life, logging
// out the entire user base over an error that lasted two seconds. The
// distinction is provenance, not rank.
func TestWiringProvisionalEnforceHealsDownwardOnceTheFloorReads(t *testing.T) {
	ctx := context.Background()
	h := newBootHarness(t, true)
	require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
	require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeRevoke, 20))

	withFailingFloorRead(t)
	boot := h.boot(t, "", 0)
	require.Equal(t, RolloutBootUnknown, boot.Outcome)
	require.Equal(t, SessionModeEnforce, h.store.Mode(), "boots strict while it cannot see")

	// Redis comes back. The poller reads the real floor.
	restoreRolloutRead(t)
	reconciler := NewRolloutReconciler(h.store, ReconcilerOptions{Markers: h.markers, MaxPerUID: 20})
	reconciler.pollFloor(ctx)

	require.Equal(t, SessionModeRevoke, h.store.Mode(),
		"the observed floor must replace the guess, downward")

	// And once healed it is no longer provisional: a genuine floor cannot be
	// walked back by anything.
	require.NoError(t, h.store.ApplyRolloutState(SessionModeExpand, 20))
	require.Equal(t, SessionModeRevoke, h.store.Mode(),
		"an observed floor must never be lowered")
}

// W17: an unknown boot writes NO floor record. Recovery exists to restore a
// floor that provably existed; here nothing proves one did, and writing enforce
// on a guess would jump a greenfield deployment's entire ladder irreversibly.
func TestWiringUnknownBootWritesNoFloor(t *testing.T) {
	h := newBootHarness(t, false)
	require.NoError(t, h.store.client.Set(h.store.rolloutControlKey(), "{not json", 0).Err())

	boot := h.boot(t, "", 0)
	require.Equal(t, RolloutBootUnknown, boot.Outcome)

	raw, err := h.store.client.Get(h.store.rolloutControlKey()).Result()
	require.NoError(t, err)
	require.Equal(t, "{not json", raw,
		"an unproven floor must be left exactly as found, not overwritten with enforce")
}

// restoreRolloutRead puts the real script back mid-test, standing in for Redis
// recovering while the process keeps running.
func restoreRolloutRead(t *testing.T) {
	t.Helper()
	require.NotNil(t, savedRolloutReadScript, "withFailingFloorRead must run first")
	readRolloutControlScript = savedRolloutReadScript
}

// W18 (P0-1): recovery replaces a floor it could not READ only when the bytes
// on the key are genuinely not a floor.
//
// The contract in the doc comment — "a valid one that reappeared always wins" —
// was enforced only on the success path. On the error path the fallback GET
// captured whatever bytes were there and CASed over them without asking whether
// they decoded, so a perfectly good floor was rewritten to enforce.
//
// That matters more than a lost value. It reaches enforce from revoke in ONE
// write, skipping bounded, so it bypasses the one-phase monotonic rule that
// AdvanceRolloutControl enforces — the invariant tests stay green because this
// path does not go through AdvanceRolloutControl at all. It writes no evidence
// snapshot. And it runs from boot, on any replica, unattended, irreversibly.
//
// loadRolloutControl returns an error for four unrelated conditions; only two
// of them mean the record is bad.
func TestRecoveryKeepsAValidFloorTheReadCouldNotSee(t *testing.T) {
	ctx := context.Background()

	t.Run("valid record carrying a TTL", func(t *testing.T) {
		// The guarded invariant "the floor must not expire" is expressed as a
		// read error — and that error was the trigger for the unpredicated jump.
		store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, 20))
		require.NoError(t, client.Expire(store.rolloutControlKey(), time.Hour).Err())

		recovered, err := store.RecoverRolloutControlAtEnforce(ctx, 20)
		require.NoError(t, err)
		require.False(t, recovered, "a valid floor must never be replaced")

		control, err := store.RolloutControl(ctx)
		require.NoError(t, err)
		require.NotNil(t, control)
		require.Equal(t, SessionModeRevoke, control.ModeFloor, "revoke must not become enforce")
		require.Equal(t, -time.Millisecond, mustPTTL(t, client, store.rolloutControlKey()),
			"and the TTL that made it unreadable must be cleared, or the floor still vanishes later")
	})

	t.Run("scripted read fails while the record is intact", func(t *testing.T) {
		// The shape a Redis blip between the two calls produces, and the shape a
		// slow EVAL against the 2s ReadTimeout produces.
		store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
		require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, 20))
		withFailingFloorRead(t)

		recovered, err := store.RecoverRolloutControlAtEnforce(ctx, 20)
		require.NoError(t, err)
		require.False(t, recovered)

		restoreRolloutRead(t)
		control, err := store.RolloutControl(ctx)
		require.NoError(t, err)
		require.Equal(t, SessionModeRevoke, control.ModeFloor)
	})
}

// W19: the cases recovery IS for still work. Narrowing the replacement rule must
// not turn a genuine Redis loss into a no-op — that direction is the fail-open
// the marker mechanism exists to prevent.
func TestRecoveryStillReplacesAnAbsentOrUnparseableFloor(t *testing.T) {
	ctx := context.Background()

	t.Run("absent", func(t *testing.T) {
		store, _ := newLegacyMigrationTestStore(t, SessionModeExpand)
		recovered, err := store.RecoverRolloutControlAtEnforce(ctx, 20)
		require.NoError(t, err)
		require.True(t, recovered)
		control, err := store.RolloutControl(ctx)
		require.NoError(t, err)
		require.Equal(t, SessionModeEnforce, control.ModeFloor)
	})

	for name, payload := range map[string]string{
		"not json":       "{not json",
		"unknown floor":  `{"mode_floor":"banana","writer_version":3}`,
		"expand floor":   `{"mode_floor":"expand","writer_version":3}`,
		"writer version": `{"mode_floor":"revoke","writer_version":2}`,
	} {
		t.Run("unparseable: "+name, func(t *testing.T) {
			store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
			require.NoError(t, client.Set(store.rolloutControlKey(), payload, 0).Err())

			recovered, err := store.RecoverRolloutControlAtEnforce(ctx, 20)
			require.NoError(t, err)
			require.True(t, recovered, "a record that is not a floor must be replaced")
			control, err := store.RolloutControl(ctx)
			require.NoError(t, err)
			require.Equal(t, SessionModeEnforce, control.ModeFloor)
		})
	}
}

// newUnreadableMarkerStore returns a store whose Load fails with something that
// is NOT "table does not exist" — the one marker error that must not be read as
// "nothing was ever stamped".
func newUnreadableMarkerStore(t *testing.T) *RolloutMarkerStore {
	t.Helper()
	db, err := sql.Open("mysql", rolloutMarkerTestDSN)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	conn := &dbr.Connection{DB: db, Dialect: dialect.MySQL, EventReceiver: &dbr.NullEventReceiver{}}
	return NewRolloutMarkerStore(conn.NewSession(nil))
}

// Gap 1 (rows A5/A9): an unreadable marker leaves boot undecidable, and the
// caller's fallback must resolve strict WITHOUT writing a floor.
//
// ResolveRolloutBoot surfaces the error rather than guessing — with neither the
// floor nor the marker readable there is nothing to decide from. runtime.go then
// applies StrictRolloutBoot, and the property that matters is that it reports
// `unknown` rather than `rollback-recovered`: Recovered makes runtime WRITE an
// enforce floor, which on a greenfield deployment would jump the entire ladder
// irreversibly on the strength of a MySQL timeout.
func TestBootWithUnreadableMarkerIsUndecidableAndWritesNoFloor(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		failRead bool
	}{
		{name: "A5: floor absent, marker unreadable"},
		{name: "A9: floor unreadable, marker unreadable", failRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
			markers := newUnreadableMarkerStore(t)
			if tc.failRead {
				require.NoError(t, client.Set(store.rolloutControlKey(), "{not json", 0).Err())
			}

			_, err := ResolveRolloutBoot(ctx, store, markers, "", 0)
			require.Error(t, err, "neither source can answer, so boot must not invent one")

			boot := StrictRolloutBoot(err, "", 0)
			require.Equal(t, RolloutBootUnknown, boot.Outcome,
				"Recovered would make runtime write an enforce floor on a MySQL timeout")
			require.Equal(t, SessionModeEnforce, boot.Mode)
			require.True(t, boot.Provisional)
			require.Positive(t, boot.MaxPerUID,
				"enforce with no cap fences every login permanently")
		})
	}
}

// Gap 2 (rows B2/B3/B4): the poller's absent-floor row.
//
// "The floor read succeeded and returned nothing" is an OBSERVATION, not an
// absence of work — and it is three-way. Treating it as nothing to do is what
// left a provisional enforce pinned for the life of the process; treating it as
// greenfield unconditionally would be a fail-open on the RDB-rollback case the
// marker exists to catch.
func TestPollerResolvesAnAbsentFloorAgainstTheMarker(t *testing.T) {
	ctx := context.Background()

	t.Run("B3: no marker -> the guess is cleared to expand", func(t *testing.T) {
		h := newBootHarness(t, true)
		withFailingFloorRead(t)
		boot := h.boot(t, "", 0)
		require.Equal(t, RolloutBootUnknown, boot.Outcome)
		require.Equal(t, SessionModeEnforce, h.store.Mode())

		restoreRolloutRead(t)
		r := NewRolloutReconciler(h.store, ReconcilerOptions{Markers: h.markers, MaxPerUID: 20})
		r.pollFloor(ctx)

		require.Equal(t, SessionModeExpand, h.store.Mode(),
			"a greenfield deployment must not stay pinned at enforce by a transient boot error")
	})

	t.Run("B2: marker present -> recover upward, never expand", func(t *testing.T) {
		// The loss happens to a RUNNING process, after a clean boot. Seeding it
		// through a failed boot read instead would prove nothing: boot's own
		// recovery writes the floor, so the poller would find one and never
		// reach this row at all.
		h := newBootHarness(t, true)
		boot := h.boot(t, "", 0)
		require.Equal(t, RolloutBootFresh, boot.Outcome)
		require.Equal(t, SessionModeExpand, h.store.Mode())

		// This deployment did establish a floor, and Redis has since lost it.
		require.NoError(t, h.markers.StampOnce(ctx, SessionModeBounded))
		require.NoError(t, h.store.client.Del(h.store.rolloutControlKey()).Err())

		r := NewRolloutReconciler(h.store, ReconcilerOptions{Markers: h.markers, MaxPerUID: 20})
		r.pollFloor(ctx)

		require.Equal(t, SessionModeEnforce, h.store.Mode(),
			"a lost floor must resolve upward, never be re-read as greenfield")
		control, err := h.store.RolloutControl(ctx)
		require.NoError(t, err)
		require.NotNil(t, control, "the lost floor must be restored, not left absent")
		require.Equal(t, SessionModeEnforce, control.ModeFloor)
	})

	t.Run("B1: an observed floor is never lowered by the guess-clearing path", func(t *testing.T) {
		h := newBootHarness(t, true)
		require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeV3Write, 20))
		require.NoError(t, h.store.AdvanceRolloutControl(ctx, SessionModeRevoke, 20))
		boot := h.boot(t, "", 0)
		require.False(t, boot.Provisional)
		require.Equal(t, SessionModeRevoke, h.store.Mode())

		// Floor deleted under a running, non-provisional process.
		require.NoError(t, h.store.client.Del(h.store.rolloutControlKey()).Err())
		r := NewRolloutReconciler(h.store, ReconcilerOptions{Markers: h.markers, MaxPerUID: 20})
		r.pollFloor(ctx)

		require.Equal(t, SessionModeRevoke, h.store.Mode(),
			"an observed mode must never be lowered, marker or not")
	})
}

// Gap 3 (rows C4/C5): the predicate must not treat a LOST floor as a fresh one.
//
// This is row B2 seen from the other path. An in-flight loss leaves the
// predicate looking at control == nil, and without the marker it targets
// v3-write as a first floor — re-creating the ladder over a stamped marker.
// Any replica restarting in that window boots at v3-write, which accepts
// exactly the persistent and over-max legacy bearers bounded had started to
// reject.
func TestPredicateRefusesToRecreateALostFloor(t *testing.T) {
	ctx := context.Background()

	t.Run("C4: marker present -> refuse, recovery restores it", func(t *testing.T) {
		h := newBootHarness(t, true)
		require.NoError(t, h.markers.StampOnce(ctx, SessionModeBounded))
		registry := NewWriterRegistry(h.store.client, h.store.uidTokenPrefix)
		regCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeBounded), nil))
		t.Cleanup(func() { _ = h.store.client.Del(h.store.uidTokenPrefix + "writers").Err() })

		decision, err := h.store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{
			Registry: registry, MaxPerUID: 20, ExpectWriters: 1,
			Convergence: NewWriterConvergence(), Markers: h.markers,
		})
		require.NoError(t, err)
		require.False(t, decision.Allowed,
			"a floor that was lost must be recovered, not re-created one phase at a time")
		require.Contains(t, decision.BlockedSummary(), "initialised")
	})

	t.Run("C5: no marker store -> refuse, the two cases are indistinguishable", func(t *testing.T) {
		store, client := newLegacyMigrationTestStore(t, SessionModeExpand)
		registry := NewWriterRegistry(client, store.uidTokenPrefix)
		regCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
		t.Cleanup(func() { _ = client.Del(store.uidTokenPrefix + "writers").Err() })

		// Satisfy the convergence window first, so the only thing left standing
		// in the way is the question this row is about. Asserting merely
		// "not allowed" would pass on the convergence blocker alone and pin
		// nothing.
		clock := time.Now().UTC()
		store.now = func() time.Time { return clock }
		input := RolloutAdvanceInput{
			Registry: registry, MaxPerUID: 20, ExpectWriters: 1, Convergence: NewWriterConvergence(),
		}
		_, err := store.EvaluateRolloutAdvance(ctx, input)
		require.NoError(t, err)
		clock = clock.Add(writerLeaseTTL + time.Second)

		decision, err := store.EvaluateRolloutAdvance(ctx, input)
		require.NoError(t, err)
		require.False(t, decision.Allowed)
		require.Contains(t, decision.BlockedSummary(), "ever initialised",
			"the refusal must be about the unanswerable question, not about the window")
	})

	t.Run("C3: genuinely fresh still establishes the first floor", func(t *testing.T) {
		h := newBootHarness(t, true)
		clock := time.Now().UTC()
		h.store.now = func() time.Time { return clock }
		registry := NewWriterRegistry(h.store.client, h.store.uidTokenPrefix)
		regCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		require.NoError(t, registry.Join(regCtx, "build-a", "pod-a", string(SessionModeExpand), nil))
		t.Cleanup(func() { _ = h.store.client.Del(h.store.uidTokenPrefix + "writers").Err() })

		input := RolloutAdvanceInput{
			Registry: registry, MaxPerUID: 20, ExpectWriters: 1,
			Convergence: NewWriterConvergence(), Markers: h.markers,
		}
		_, err := h.store.EvaluateRolloutAdvance(ctx, input)
		require.NoError(t, err)
		clock = clock.Add(writerLeaseTTL + time.Second)

		decision, err := h.store.EvaluateRolloutAdvance(ctx, input)
		require.NoError(t, err)
		require.True(t, decision.Allowed, "blocked by: %s", decision.BlockedSummary())
		require.Equal(t, SessionModeV3Write, decision.Target)
	})
}
