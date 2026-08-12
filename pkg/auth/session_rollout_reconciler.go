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
	"errors"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
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

// RolloutAdvanceRecord is the audit payload inserted in the same MySQL
// transaction as the versioned floor CAS.
type RolloutAdvanceRecord struct {
	From              SessionMode         `json:"from"`
	To                SessionMode         `json:"to"`
	Actor             string              `json:"actor"`
	AtMS              int64               `json:"at_unix_ms"`
	Kind              string              `json:"transition_kind"`
	RedisID           string              `json:"redis_instance_id,omitempty"`
	WriterFingerprint string              `json:"writer_fingerprint,omitempty"`
	Observation       *SessionObservation `json:"observation,omitempty"`
	CapChange         *RolloutCapChange   `json:"cap_change,omitempty"`
}

// RolloutCapChange is stored in the append-only audit row for a cap-only
// control transition. The floor remains unchanged for this transition.
type RolloutCapChange struct {
	FromMaxPerUID int `json:"from_max_per_uid"`
	ToMaxPerUID   int `json:"to_max_per_uid"`
}

// RolloutReconciler polls the floor and, when enabled, advances it.
type RolloutReconciler struct {
	store    *RedisSessionStore
	registry *WriterRegistry
	control  RolloutStateController

	autoAdvance   bool
	canaryAhead   bool
	expectWriters int
	scanBatchSize int64
	scanInterval  time.Duration
	// convergence carries the writer-set observation window across cycles. It
	// belongs to the reconciler rather than the predicate because the predicate
	// is evaluated per decision and the window spans several.
	convergence *WriterConvergence
	log         func(format string, args ...interface{})
}

type RolloutStateController interface {
	Load(context.Context) (RolloutState, error)
	Advance(context.Context, RolloutState, RolloutAdvanceRecord) (RolloutState, error)
}

type ReconcilerOptions struct {
	Registry      *WriterRegistry
	Control       RolloutStateController
	AutoAdvance   bool
	CanaryAhead   bool
	ExpectWriters int
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
		control:       opts.Control,
		autoAdvance:   opts.AutoAdvance,
		canaryAhead:   opts.CanaryAhead,
		expectWriters: opts.ExpectWriters,
		scanBatchSize: batch,
		scanInterval:  interval,
		convergence:   NewWriterConvergence(),
		log:           log,
	}
}

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
	if r.control == nil {
		return
	}
	state, err := loadRolloutStateWithTimeout(ctx, r.control)
	if err != nil {
		// Keep the last applied state and leave issuance fenced if publication was
		// already in progress. A DB read failure is never permission to derive a
		// mode from Redis or environment.
		r.log("session rollout: cannot read MySQL control state: %v", err)
		return
	}
	if applyErr := r.applyAndPublish(state); applyErr != nil {
		r.log("session rollout: cannot apply floor %s: %v", state.Floor, applyErr)
	}
}

// applyAndPublish is the ONLY way this reconciler changes the mode it runs at.
//
// It exists because the three steps are not separable. Applying without
// publishing leaves the registry advertising a mode this replica is not running,
// and the convergence gate only rejects writers advertising a mode BELOW the
// floor — so a replica advertising one ABOVE what it runs is invisible to it and
// can satisfy convergence for the first v3 floor while still writing v2. That is
// precisely the hole the gate exists to close, and it opened because a second
// caller (resolveAbsentFloor) called ApplyRolloutState on its own.
//
// The canary offset belongs here too: it is a property of this replica's
// posture, not of where the mode came from, so a mode installed by
// guess-clearing has to carry it exactly like one read from a floor record.

func (r *RolloutReconciler) applyAndPublish(state RolloutState) error {
	mode := state.Floor
	if r.canaryAhead && mode.rank() < SessionModeEnforce.rank() {
		mode = mode.next()
	}
	return r.store.ApplyAndPublishRolloutState(r.registry, state, mode)
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
	if r.control == nil {
		return soon
	}
	state, err := loadRolloutStateWithTimeout(ctx, r.control)
	if err != nil {
		r.log("session rollout: cannot read MySQL control state for advance: %v", err)
		metrics.ObserveSessionReconcileBlocked("control-read-error")
		return soon
	}
	if state.Paused || !r.autoAdvance {
		return soon
	}

	decision, err := r.store.EvaluateRolloutAdvance(ctx, RolloutAdvanceInput{
		State:         state,
		Registry:      r.registry,
		ExpectWriters: r.expectWriters,
		ScanBatchSize: r.scanBatchSize,
		ScanInterval:  r.scanInterval,
		Convergence:   r.convergence,
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

// advance commits the evidence snapshot and CAS in one MySQL transaction.
//
// A losing replica sees the versioned MySQL CAS miss. That is a benign no-op:
// another replica committed the same transition first.
func (r *RolloutReconciler) advance(ctx context.Context, decision RolloutAdvanceDecision, actor string) error {
	if r.control == nil {
		return errors.New("auth: rollout control store unavailable")
	}
	record := rolloutAdvanceRecord(decision, actor)
	_, err := r.control.Advance(ctx, RolloutState{
		Floor: decision.Current, MaxPerUID: decision.MaxPerUID, Version: decision.StateVersion,
	}, record)
	switch {
	case err == nil:
		metrics.ObserveSessionFloorAdvance(string(decision.Target), actor)
		return nil
	case errors.Is(err, ErrRolloutControlChanged):
		return nil
	default:
		return err
	}
}

func rolloutAdvanceRecord(decision RolloutAdvanceDecision, actor string) RolloutAdvanceRecord {
	return RolloutAdvanceRecord{
		From:              decision.Current,
		To:                decision.Target,
		Actor:             actor,
		AtMS:              time.Now().UTC().UnixMilli(),
		RedisID:           decision.RedisInstanceID,
		WriterFingerprint: decision.WriterFingerprint,
		Observation:       decision.Observation,
	}
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
	case strings.Contains(reason, "did not complete"):
		return "scan-incomplete"
	case strings.Contains(reason, "already enforce"):
		return "terminal"
	default:
		return "other"
	}
}
