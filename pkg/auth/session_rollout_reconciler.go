package auth

// The rollout reconciler.
//
// The advance predicate contains no human judgement, so requiring a person to
// type a command was design inertia rather than a safety property. What is left
// for a human is the migration cutoff and finite policy — genuinely a business
// decision about how many people get logged out early — and the reconciler
// stalls exactly there and says so, which is why no separate approval mechanism
// is needed.
//
// Advancing an irreversible switch automatically is safe here because every
// gate is fail-closed, an advance only makes the reader stricter, and the
// predicate proves the set it would newly reject is empty. The worst outcome of
// a wrong predicate is people re-logging in, not someone keeping access they
// should have lost. What it costs is the ability to undo, so the predicate is
// the part of this change that deserves the most review.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	rd "github.com/go-redis/redis"
)

const (
	// floorPollInterval bounds how long a replica can run behind a floor that
	// has moved. It replaces a rolling restart, so it wants to be short.
	floorPollInterval = 5 * time.Second
	// reconcileInterval paces the predicate, which is cheap unless it scans.
	reconcileInterval = 30 * time.Second
	// reconcileScanBackoff applies after a cycle that scanned and stayed
	// blocked. Waiting out a legacy deadline measured in days must not mean
	// rescanning the keyspace every thirty seconds.
	reconcileScanBackoff = time.Hour
	// defaultReconcileScanInterval caps the scan at ~200 records/sec. The scan
	// is a full SCAN plus a read per key, unattended, on the shared pool.
	defaultReconcileScanInterval = 5 * time.Millisecond
)

// RolloutAdvanceRecord is the evidence snapshot written BEFORE each advance.
// The ordering is the requirement, not atomicity: a snapshot with no advance is
// harmless, an advance with no snapshot is not. Writing it first gets that with
// two Redis writes and no transaction.
type RolloutAdvanceRecord struct {
	From        SessionMode `json:"from"`
	To          SessionMode `json:"to"`
	Actor       string      `json:"actor"`
	AtMS        int64       `json:"at_unix_ms"`
	LiveWriters int         `json:"live_writers"`
	Builds      []string    `json:"builds"`
	V1          int64       `json:"v1"`
	V2          int64       `json:"v2"`
	V3          int64       `json:"v3"`
	Total       int64       `json:"total"`
	RedisID     string      `json:"redis_instance_id,omitempty"`
}

// RolloutReconciler polls the floor and, when enabled, advances it.
type RolloutReconciler struct {
	store    *RedisSessionStore
	registry *WriterRegistry
	markers  *RolloutMarkerStore

	autoAdvance   bool
	canaryAhead   bool
	expectWriters int
	// maxPerUID seeds the floor record the first time the floor crosses into a
	// v3-writing phase, when there is no record to carry it forward from.
	maxPerUID     int
	scanBatchSize int64
	scanInterval  time.Duration
	markerStamped atomic.Bool
	// convergence carries the writer-set observation window across cycles. It
	// belongs to the reconciler rather than the predicate because the predicate
	// is evaluated per decision and the window spans several.
	convergence *WriterConvergence
	log         func(format string, args ...interface{})
}

type ReconcilerOptions struct {
	Registry      *WriterRegistry
	Markers       *RolloutMarkerStore
	AutoAdvance   bool
	CanaryAhead   bool
	ExpectWriters int
	MaxPerUID     int
	ScanBatchSize int64
	// ScanInterval throttles the keyspace scan. This runs unattended, on the
	// shared session pool, from every replica, so it must not be zero in
	// production — ObserveRateLimited reserves that for tests.
	ScanInterval time.Duration
	Log          func(format string, args ...interface{})
}

func NewRolloutReconciler(store *RedisSessionStore, opts ReconcilerOptions) *RolloutReconciler {
	if store == nil {
		panic("auth: NewRolloutReconciler requires a session store")
	}
	log := opts.Log
	if log == nil {
		log = func(string, ...interface{}) {}
	}
	batch := opts.ScanBatchSize
	if batch <= 0 {
		batch = 200
	}
	interval := opts.ScanInterval
	if interval <= 0 {
		interval = defaultReconcileScanInterval
	}
	return &RolloutReconciler{
		store:         store,
		registry:      opts.Registry,
		markers:       opts.Markers,
		autoAdvance:   opts.AutoAdvance,
		canaryAhead:   opts.CanaryAhead,
		expectWriters: opts.ExpectWriters,
		maxPerUID:     opts.MaxPerUID,
		scanBatchSize: batch,
		scanInterval:  interval,
		convergence:   NewWriterConvergence(),
		log:           log,
	}
}

// Run polls the floor forever and reconciles when enabled.
//
// It is started only from the server wiring, never from NewRedisSessionStore:
// a background goroutine that advances floors, started by a constructor, would
// fire in every test that happens to build a store.
// Run polls the floor and, when enabled, reconciles — in two goroutines.
//
// One loop was not enough: a rate-limited full-keyspace scan inside
// reconcileOnce can run for an hour on a large keyspace, and while it did, the
// select never reached the poll tick. A floor advance published by another
// replica would not be applied here for that whole hour, defeating the
// five-second propagation the poller exists for, and the registry would keep
// advertising a stale applied state — which is exactly what the convergence
// gate reads.
//
// It is started only from the server wiring, never from NewRedisSessionStore:
// a background goroutine that advances floors, started by a constructor, would
// fire in every test that happens to build a store.
func (r *RolloutReconciler) Run(ctx context.Context) {
	go r.runPoller(ctx)
	r.runReconciler(ctx)
}

func (r *RolloutReconciler) runPoller(ctx context.Context) {
	ticker := time.NewTicker(floorPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.pollFloor(ctx)
		}
	}
}

func (r *RolloutReconciler) runReconciler(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	var next time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !r.store.now().Before(next) {
				next = r.reconcileOnce(ctx)
			}
		}
	}
}

// pollFloor is what replaces eight of the nine rolling restarts: a replica
// learns the floor moved instead of being redeployed to be told.
func (r *RolloutReconciler) pollFloor(ctx context.Context) {
	control, err := r.store.RolloutControl(ctx)
	if err != nil {
		// Transient and undecidable: keep whatever this replica is running,
		// including a provisional guess.
		return
	}
	if control == nil {
		// "Read succeeded, nothing there" is an OBSERVATION, not an absence of
		// work — and returning here left a provisional enforce pinned for the
		// life of the process, denying every existing v1/v2 session on that pod
		// because of a Redis error that lasted two seconds inside the boot
		// window.
		//
		// It is also three-way, exactly as it is at boot, so it needs the marker
		// for the same reason: clearing the guess to expand unconditionally would
		// be a fail-open on the RDB-rollback case.
		r.resolveAbsentFloor(ctx)
		return
	}

	// A #725 record predates the cap field, so without writing it back the cap
	// only ever lives in this process's memory and is gone on the next restart —
	// leaving a v3-writing floor with no bound to apply. Retried here rather
	// than done once at boot so a MySQL or Redis hiccup does not lose it.
	if control.MaxPerUID <= 0 && r.maxPerUID > 0 {
		if capErr := r.store.EnsureRolloutMaxPerUID(ctx, r.maxPerUID); capErr != nil {
			r.log("session rollout: cannot persist max_per_uid: %v", capErr)
		}
	}
	r.ensureMarker(ctx, control.ModeFloor)

	mode := control.ModeFloor
	if r.canaryAhead && mode.rank() < SessionModeEnforce.rank() {
		mode = mode.next()
	}
	cap := control.MaxPerUID
	if cap <= 0 {
		cap = r.maxPerUID
	}
	if applyErr := r.store.ApplyRolloutState(mode, cap); applyErr != nil {
		r.log("session rollout: cannot apply floor %s: %v", control.ModeFloor, applyErr)
		return
	}
	applied := r.store.currentMode()
	metrics.SetSessionRolloutMode(string(applied))
	if r.registry != nil {
		// Publish what was APPLIED, never what was resolved. Advertising a mode
		// this replica is not actually running lets the convergence gate be
		// satisfied by a replica that has not converged. Only on an actual
		// change — the heartbeat already refreshes the entry on its own tick,
		// so rewriting an identical one every poll doubles registry traffic.
		r.registry.SetAppliedStateIfChanged(string(applied))
	}
}

// resolveAbsentFloor handles rows B2/B3/B4 of the boot-state table: the floor
// read succeeded and returned nothing.
//
// Only the marker can say which of the three absences this is, and getting it
// wrong costs in both directions — treating a loss as greenfield re-admits the
// legacy bearers the rollout was retiring, and treating greenfield as a loss
// pins the replica at enforce.
//
// Deliberately NOT gated on the pause flag. Pause stops the floor moving
// FORWARD; this restores one that already existed, and boot performs the same
// recovery unconditionally on every replica. Gating it here would only mean a
// paused deployment recovers via pod restarts instead — the same outcome,
// arrived at less predictably.
func (r *RolloutReconciler) resolveAbsentFloor(ctx context.Context) {
	if r.markers == nil {
		return
	}
	loadCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	marker, err := r.markers.Load(loadCtx)
	switch {
	case err != nil && !isMissingMarkerTable(err):
		// B4: cannot decide. Keep the current mode, guess included.
		return
	case err == nil && marker != nil:
		// B2: this deployment established a floor and Redis no longer has it.
		// Recovery writes enforce with the marker as its evidence.
		recovered, recoverErr := r.store.RecoverRolloutControlAtEnforce(ctx, r.recoveryMaxPerUID())
		if recoverErr != nil {
			r.log("session rollout: cannot recover lost floor: %v", recoverErr)
			return
		}
		if !recovered {
			// A valid floor reappeared between the read and the CAS — another
			// replica's recovery, or an operator restoring a backup. Whatever it
			// says is authoritative, and forcing enforce over it would raise this
			// replica above a floor that is readable right now. The next poll
			// reads it through the normal path.
			return
		}
		if applyErr := r.store.ApplyRolloutState(SessionModeEnforce, r.recoveryMaxPerUID()); applyErr != nil {
			r.log("session rollout: cannot apply recovered floor: %v", applyErr)
			return
		}
		r.log("session rollout: floor was missing but this deployment initialised one at %s; "+
			"restored at enforce", marker.InitializedAt.UTC().Format(time.RFC3339))
	default:
		// B3: genuinely never established. Applying expand is a no-op for an
		// observed mode — the no-lowering rule still holds — and clears a
		// provisional guess, which is the asymmetry `provisional` exists for.
		if applyErr := r.store.ApplyRolloutState(SessionModeExpand, r.maxPerUID); applyErr != nil {
			r.log("session rollout: cannot clear provisional mode: %v", applyErr)
		}
	}
}

// recoveryMaxPerUID keeps recovery appliable when no cap was configured: a
// v3-writing mode with no bound fences every login.
func (r *RolloutReconciler) recoveryMaxPerUID() int {
	if r.maxPerUID > 0 {
		return r.maxPerUID
	}
	return defaultRecoveryMaxPerUID
}

// ensureMarker stamps the initialisation marker whenever a floor exists without
// one. It lives here, not at boot, for two reasons: the marker table is created
// by a migration that runs after the session store is built, so the first boot
// of this artifact cannot stamp it; and until it is stamped, a Redis loss reads
// as "never initialised" and resolves DOWN to expand.
func (r *RolloutReconciler) ensureMarker(ctx context.Context, floor SessionMode) {
	if r.markers == nil || r.markerStamped.Load() {
		return
	}
	// Bounded: the rollout context has no deadline, and a stalled MySQL
	// connection here would otherwise hold the poll loop open indefinitely.
	loadCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	marker, err := r.markers.Load(loadCtx)
	if err != nil {
		return
	}
	if marker != nil {
		// Latch it: once stamped it stays stamped, so there is no reason to
		// query for the rest of the process's life.
		r.markerStamped.Store(true)
		return
	}
	if stampErr := r.markers.StampOnce(loadCtx, floor); stampErr != nil {
		r.log("session rollout: cannot stamp initialisation marker: %v", stampErr)
		return
	}
	r.markerStamped.Store(true)
}

// reconcileOnce evaluates the predicate and returns the earliest time the next
// cycle should run.
func (r *RolloutReconciler) reconcileOnce(ctx context.Context) time.Time {
	now := r.store.now()
	soon := now.Add(reconcileInterval)

	if r.registry != nil {
		if live, err := r.registry.Live(); err == nil {
			metrics.SetSessionLiveWriters(len(live))
		}
	}
	paused, err := r.store.RolloutPaused(ctx)
	if err != nil {
		return soon
	}
	if paused || !r.autoAdvance {
		return soon
	}

	decision, err := r.store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{
		Registry:      r.registry,
		ExpectWriters: r.expectWriters,
		MaxPerUID:     r.maxPerUID,
		ScanBatchSize: r.scanBatchSize,
		ScanInterval:  r.scanInterval,
		Convergence:   r.convergence,
		Markers:       r.markers,
	})
	if err != nil {
		// Back off on errors too. A scan that aborts partway makes this return
		// early and, without a backoff, retries in thirty seconds — amplifying
		// load exactly while Redis is unhealthy.
		r.log("session rollout: cannot evaluate advance: %v", err)
		metrics.ObserveSessionReconcileBlocked("evaluate-error")
		return now.Add(reconcileScanBackoff)
	}
	if decision.Scanned {
		metrics.ObserveSessionReconcileScan()
	}
	if decision.Current == SessionModeEnforce {
		// Terminal: stop scanning entirely. The machine has nothing left to do.
		return now.Add(24 * time.Hour)
	}
	if !decision.Allowed {
		for _, reason := range decision.BlockedBy {
			metrics.ObserveSessionReconcileBlocked(blockedReasonLabel(reason))
		}
		r.log("session rollout: holding at %s, blocked by %s", decision.Current, decision.BlockedSummary())
		// The scan backoff is for blockers that need the keyspace to change —
		// waiting out a legacy deadline measured in days must not mean rescanning
		// every thirty seconds. The convergence window is not one of those: it is
		// a statement about elapsed time that clears in one lease TTL, and the
		// first evaluation is ALWAYS blocked on it because Observe has nothing to
		// compare against yet. Backing off an hour for it made the first v3 floor
		// take an hour of unattended wall clock on every deployment, greenfield
		// included.
		if decision.Scanned && !blockedOnlyOnConvergence(decision) {
			return now.Add(reconcileScanBackoff)
		}
		return soon
	}

	if err := r.advance(ctx, decision, "reconciler"); err != nil {
		r.log("session rollout: advance to %s did not apply: %v", decision.Target, err)
		return soon
	}
	r.log("session rollout: floor advanced %s -> %s", decision.Current, decision.Target)
	// Something changed; look again promptly rather than waiting a full cycle.
	return now
}

// advance writes the evidence snapshot and then performs the CAS.
//
// A loser in a race surfaces either ErrRolloutControlChanged or the one-phase
// pre-check rejection, and BOTH are benign no-ops — with several replicas
// reconciling, treating them as errors would make the fleet alert on itself.
func (r *RolloutReconciler) advance(ctx context.Context, decision RolloutAdvanceDecision, actor string) error {
	err := r.store.advanceFloor(ctx, decision, actor, r.registry, r.markers)
	switch {
	case err == nil:
		metrics.ObserveSessionFloorAdvance(string(decision.Target), actor)
		return nil
	case errors.Is(err, ErrRolloutControlChanged):
		return nil
	case isBenignAdvanceRace(err):
		return nil
	default:
		return err
	}
}

// isBenignAdvanceRace covers the second shape a losing racer takes. The CAS
// returns ErrRolloutControlChanged when the racer read before the winner wrote;
// when it read after, the optimistic pre-check rejects it here instead. Both
// mean "someone else already did it".
func isBenignAdvanceRace(err error) bool {
	return errors.Is(err, ErrRolloutFloorNotNext) || errors.Is(err, ErrRolloutFirstFloor)
}

// blockedReasonLabel keeps the metric label bounded: the human-readable reason
// carries counts, which would otherwise be unbounded cardinality.
func blockedReasonLabel(reason string) string {
	switch {
	case strings.Contains(reason, "no live writers"):
		return "no-writers"
	case strings.Contains(reason, "distinct builds"):
		return "mixed-builds"
	case strings.Contains(reason, "have not applied"):
		return "not-converged"
	case strings.Contains(reason, "expected"):
		return "writer-count-mismatch"
	case strings.Contains(reason, "stable for"), strings.Contains(reason, "convergence has not been observed"):
		return "not-converged-long-enough"
	case strings.Contains(reason, sessionExpectWritersEnv):
		return "expect-writers-unset"
	case strings.Contains(reason, "v1="):
		return "legacy-remaining"
	case strings.Contains(reason, "max_per_uid"):
		return "cap-unset"
	case strings.Contains(reason, "did not complete"):
		return "scan-incomplete"
	case strings.Contains(reason, "already enforce"):
		return "terminal"
	default:
		return "other"
	}
}

func (s *RedisSessionStore) recordRolloutAdvance(
	ctx context.Context,
	decision RolloutAdvanceDecision,
	actor string,
	registry *WriterRegistry,
) error {
	record := RolloutAdvanceRecord{
		From:  decision.Current,
		To:    decision.Target,
		Actor: actor,
		AtMS:  s.now().UTC().UnixMilli(),
	}
	if registry != nil {
		if live, err := registry.Live(); err == nil {
			record.LiveWriters = len(live)
			seen := map[string]struct{}{}
			for _, entry := range live {
				if _, ok := seen[entry.Build]; ok {
					continue
				}
				seen[entry.Build] = struct{}{}
				record.Builds = append(record.Builds, entry.Build)
			}
		}
	}
	if observation := decision.Observation; observation != nil {
		record.V1, record.V2, record.V3, record.Total =
			observation.V1, observation.V2, observation.V3, observation.Total
	}
	if id, err := s.currentRedisInstanceID(); err == nil {
		record.RedisID = id
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("auth: encode rollout advance record: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.client.Set(s.rolloutAdvanceRecordKey(), string(encoded), 0).Err(); err != nil {
		return fmt.Errorf("auth: record rollout advance: %w", err)
	}
	return nil
}

// LastRolloutAdvance returns the most recent advance snapshot, for `status`.
func (s *RedisSessionStore) LastRolloutAdvance(ctx context.Context) (*RolloutAdvanceRecord, error) {
	raw, err := s.client.Get(s.rolloutAdvanceRecordKey()).Result()
	if err == rd.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read rollout advance record: %w", err)
	}
	var record RolloutAdvanceRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil, fmt.Errorf("auth: decode rollout advance record: %w", err)
	}
	return &record, nil
}

// RolloutPaused reports the runtime pause flag. It is read at the top of every
// cycle so `pause` takes effect within one interval: stopping a misbehaving
// reconciler must not require a rollout, which would be both too slow during an
// incident and at odds with the point of this change.
func (s *RedisSessionStore) RolloutPaused(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	count, err := s.client.Exists(s.rolloutPauseKey()).Result()
	if err != nil {
		// Fail safe: an unreadable pause flag is treated as paused, because the
		// conservative side of "should I advance an irreversible switch" is no.
		return true, nil
	}
	return count != 0, nil
}

// SetRolloutPaused sets or clears the pause flag.
func (s *RedisSessionStore) SetRolloutPaused(ctx context.Context, paused bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if paused {
		if err := s.client.Set(s.rolloutPauseKey(), "1", 0).Err(); err != nil {
			return fmt.Errorf("auth: pause rollout: %w", err)
		}
		return nil
	}
	if err := s.client.Del(s.rolloutPauseKey()).Err(); err != nil {
		return fmt.Errorf("auth: resume rollout: %w", err)
	}
	return nil
}

func (s *RedisSessionStore) rolloutPauseKey() string {
	return s.uidTokenPrefix + "auth:rollout-paused"
}

func (s *RedisSessionStore) rolloutAdvanceRecordKey() string {
	return s.uidTokenPrefix + "auth:rollout-last-advance"
}
