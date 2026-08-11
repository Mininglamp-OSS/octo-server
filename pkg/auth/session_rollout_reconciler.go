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
	log           func(format string, args ...interface{})
}

type ReconcilerOptions struct {
	Registry      *WriterRegistry
	Markers       *RolloutMarkerStore
	AutoAdvance   bool
	CanaryAhead   bool
	ExpectWriters int
	MaxPerUID     int
	ScanBatchSize int64
	Log           func(format string, args ...interface{})
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
	return &RolloutReconciler{
		store:         store,
		registry:      opts.Registry,
		markers:       opts.Markers,
		autoAdvance:   opts.AutoAdvance,
		canaryAhead:   opts.CanaryAhead,
		expectWriters: opts.ExpectWriters,
		maxPerUID:     opts.MaxPerUID,
		scanBatchSize: batch,
		log:           log,
	}
}

// Run polls the floor forever and reconciles when enabled.
//
// It is started only from the server wiring, never from NewRedisSessionStore:
// a background goroutine that advances floors, started by a constructor, would
// fire in every test that happens to build a store.
func (r *RolloutReconciler) Run(ctx context.Context) {
	pollTicker := time.NewTicker(floorPollInterval)
	defer pollTicker.Stop()
	reconcileTicker := time.NewTicker(reconcileInterval)
	defer reconcileTicker.Stop()

	var nextReconcile time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			r.pollFloor(ctx)
		case <-reconcileTicker.C:
			if !r.store.now().Before(nextReconcile) {
				nextReconcile = r.reconcileOnce(ctx)
			}
		}
	}
}

// pollFloor is what replaces eight of the nine rolling restarts: a replica
// learns the floor moved instead of being redeployed to be told.
func (r *RolloutReconciler) pollFloor(ctx context.Context) {
	control, err := r.store.RolloutControl(ctx)
	if err != nil || control == nil {
		return
	}
	mode := control.ModeFloor
	if r.canaryAhead && mode.rank() < SessionModeEnforce.rank() {
		mode = mode.next()
	}
	if applyErr := r.store.ApplyRolloutState(mode, control.MaxPerUID); applyErr != nil {
		r.log("session rollout: cannot apply floor %s: %v", control.ModeFloor, applyErr)
		return
	}
	metrics.SetSessionRolloutMode(string(r.store.currentMode()))
	if r.registry != nil {
		r.registry.SetAppliedState(string(control.ModeFloor))
	}
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
	})
	if err != nil {
		r.log("session rollout: cannot evaluate advance: %v", err)
		metrics.ObserveSessionReconcileBlocked("evaluate-error")
		return soon
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
		if decision.Scanned {
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
	if err := r.store.recordRolloutAdvance(ctx, decision, actor, r.registry); err != nil {
		return err
	}
	err := r.store.AdvanceRolloutControl(ctx, decision.Target, decision.MaxPerUID)
	switch {
	case err == nil:
		metrics.ObserveSessionFloorAdvance(string(decision.Target), actor)
		if r.markers != nil {
			_ = r.markers.StampOnce(ctx, decision.Target)
		}
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
	return err != nil && (strings.Contains(err.Error(), "advance exactly one phase") ||
		strings.Contains(err.Error(), "first rollout floor must be"))
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
