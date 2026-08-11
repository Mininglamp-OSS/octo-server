package auth

// Writer registry: a write LEASE, not a status report.
//
// The distinction decides whether this can gate anything. A registry can only
// enumerate participants, and the dangerous process is the one that does not
// participate — so "absent from the registry" cannot prove "not running".
// Inverting the causality fixes that: a process may only create tokens while it
// holds a live lease, which makes "absent from the registry" prove "not
// writing", and that is the property the gate actually needs.
//
// Losing the lease therefore refuses new token creation. It does NOT fail
// readiness: Redis being unreachable is a fleet-wide event, so draining every
// replica at once would turn an authentication degradation into a total outage,
// and readiness contributes nothing to the gate, which only asks whether
// registrations exist. It also does not panic — a startup panic on missing
// Redis state is the defect this whole change exists to remove.
//
// Deliberately free of auth types so it can be lifted into a shared package
// later: applied state is an opaque string and the caller supplies the
// comparison, the key prefix is passed in, and nothing here imports a
// SessionMode. #627 and #697 both stall on the same unanswered question —
// "has every pre-fix replica gone?" — and both currently push it to a human.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	rd "github.com/go-redis/redis"
)

var publishWriterLeaseScript = rd.NewScript(`
redis.call("SADD", KEYS[1], ARGV[1])
redis.call("PSETEX", KEYS[2], ARGV[2], ARGV[3])
return 1
`)

const (
	// writerLeaseTTL bounds how long a dead process stays visible. Liveness is
	// Redis's own key expiry, so no timestamp is written and cross-pod clock
	// skew cannot poison the gate — unlike the migration Lua, which is handed a
	// client clock.
	writerLeaseTTL = 30 * time.Second
	// writerHeartbeatInterval leaves room for five consecutive failures.
	writerHeartbeatInterval = 5 * time.Second
)

// ErrWriterLeaseLost is returned when this process may not create credentials
// because its write lease has expired. It is a degradation, not a crash: the
// caller surfaces the existing unauthenticated envelope and existing sessions
// keep working.
var ErrWriterLeaseLost = errors.New("auth: writer lease lost; refusing to create a new session")

// ErrSessionCapUnavailable is returned when a v3-writing mode is active with no
// usable per-UID cap. Reader strictness still applies; only new credentials are
// refused, so the failure is closed in both directions.
var ErrSessionCapUnavailable = errors.New("auth: bounded sessions per UID unavailable; refusing to create a new session")

// WriterEntry is one live participant. It carries nothing that could identify a
// user: build, pod name and applied state only.
type WriterEntry struct {
	ID           string `json:"id"`
	Build        string `json:"build"`
	AppliedState string `json:"applied_state"`
	Pod          string `json:"pod,omitempty"`
	StartedAtMS  int64  `json:"started_at_unix_ms"`
}

// WriterRegistry tracks which processes currently hold a write lease.
type WriterRegistry struct {
	client    *rd.Client
	rosterKey string
	entryKey  func(id string) string

	mu   sync.RWMutex
	self WriterEntry
	// lastRefreshAt is when this process last confirmed its lease. A lease has
	// to expire on the HOLDER's clock: tracking only "did the last write
	// succeed" meant a process stalled past the TTL (GC pause, CPU throttling,
	// SIGSTOP) came back believing it still held one, while the registry had
	// already stopped seeing it and the gate may have advanced on that basis.
	lastRefreshAt time.Time
	now           func() time.Time
	// writeMu serializes heartbeat refreshes with applied-state publication. A
	// stale heartbeat captured before a mode change must never overwrite the new
	// registry state after it was published.
	writeMu sync.Mutex
	// publishFn is a test seam for publication failure. Production uses the Lua
	// publisher below.
	publishFn func(WriterEntry) error
}

// writerLeaseMargin keeps the local expiry strictly ahead of Redis's, so a
// writer stops writing before the registry stops listing it, never after.
const writerLeaseMargin = 5 * time.Second

// NewWriterRegistry builds a registry under the given key prefix. The prefix is
// supplied rather than derived so the type stays independent of auth config.
func NewWriterRegistry(client *rd.Client, keyPrefix string) *WriterRegistry {
	if client == nil {
		panic("auth: NewWriterRegistry requires a non-nil redis client")
	}
	return &WriterRegistry{
		client:    client,
		rosterKey: keyPrefix + "writers",
		entryKey:  func(id string) string { return keyPrefix + "writer:" + id },
		now:       time.Now,
	}
}

// Join registers this process and starts refreshing its lease until ctx ends.
// The identity is a fresh random value per process incarnation, not the pod UID:
// private deployments are not always Kubernetes, and an in-place container
// restart keeps its pod UID, so keying on it would let a new process silently
// overwrite the old entry and hide the restart. The pod name rides along as a
// label so a crash-looping pod shows up as several live registrations rather
// than as confusion.
func (r *WriterRegistry) Join(ctx context.Context, build, pod, appliedState string, now func() time.Time) error {
	id, err := newSessionGeneration()
	if err != nil {
		return fmt.Errorf("auth: create writer registry identity: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	r.mu.Lock()
	r.now = now
	r.self = WriterEntry{
		ID:           id,
		Build:        build,
		AppliedState: appliedState,
		Pod:          pod,
		StartedAtMS:  now().UTC().UnixMilli(),
	}
	r.mu.Unlock()

	// Start the heartbeat regardless of the first attempt. Returning here left a
	// replica that hit a momentary Redis blip during startup fenced for the rest
	// of its life, with readiness green and login traffic still arriving — the
	// retry loop was already written, just gated behind success.
	refreshErr := r.refresh()
	go r.heartbeat(ctx)
	return refreshErr
}

// SetAppliedState records that this process has applied a new rollout state.
// The gate reads it to prove the fleet has converged before advancing.
// SetAppliedStateIfChanged skips the Redis round trip when nothing moved. The
// applied mode changes a handful of times across an entire rollout, while the
// poller runs every five seconds.
func (r *WriterRegistry) SetAppliedStateIfChanged(state string) error {
	r.mu.RLock()
	unchanged := r.self.AppliedState == state
	r.mu.RUnlock()
	if unchanged {
		return nil
	}
	return r.PublishAppliedState(state)
}

func (r *WriterRegistry) SetAppliedState(state string) error {
	return r.PublishAppliedState(state)
}

// PublishAppliedState atomically renews this process's write lease and changes
// its advertised state. The in-memory entry is committed only after Redis
// accepts the candidate, so a failed publication cannot be refreshed later by
// the heartbeat as though it succeeded.
func (r *WriterRegistry) PublishAppliedState(state string) error {
	if strings.TrimSpace(state) == "" {
		return errors.New("auth: writer applied state is required")
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.RLock()
	candidate := r.self
	r.mu.RUnlock()
	if candidate.ID == "" {
		return errors.New("auth: writer registry has not joined")
	}
	candidate.AppliedState = state
	if err := r.publish(candidate); err != nil {
		return err
	}
	r.mu.Lock()
	r.self = candidate
	r.lastRefreshAt = r.clock()
	r.mu.Unlock()
	return nil
}

// MayWrite reports whether this process still holds its lease. A writer without
// one must refuse to create new tokens, or "absent from the registry" stops
// meaning "not writing" and the gate becomes fail-open.
func (r *WriterRegistry) MayWrite() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lastRefreshAt.IsZero() {
		return false
	}
	return r.clock().Sub(r.lastRefreshAt) < writerLeaseTTL-writerLeaseMargin
}

func (r *WriterRegistry) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

func (r *WriterRegistry) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(writerHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			entry := r.self
			r.lastRefreshAt = time.Time{}
			r.mu.Unlock()
			// Best effort: drop out of the roster promptly on a clean shutdown
			// so a rolling update does not have to wait out the lease.
			_ = r.client.Del(r.entryKey(entry.ID)).Err()
			_ = r.client.SRem(r.rosterKey, entry.ID).Err()
			return
		case <-ticker.C:
			_ = r.refresh()
		}
	}
}

func (r *WriterRegistry) refresh() error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.RLock()
	entry := r.self
	r.mu.RUnlock()
	if entry.ID == "" {
		return errors.New("auth: writer registry has not joined")
	}
	if err := r.publish(entry); err != nil {
		return err
	}
	r.mu.Lock()
	r.lastRefreshAt = r.clock()
	r.mu.Unlock()
	return nil
}

func (r *WriterRegistry) publish(entry WriterEntry) error {
	if r.publishFn != nil {
		return r.publishFn(entry)
	}
	if r.client == nil {
		return errors.New("auth: writer registry Redis client is unavailable")
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("auth: encode writer registry entry: %w", err)
	}
	_, err = publishWriterLeaseScript.Run(
		r.client,
		[]string{r.rosterKey, r.entryKey(entry.ID)},
		entry.ID,
		writerLeaseTTL.Milliseconds(),
		string(encoded),
	).Result()
	if err != nil {
		return fmt.Errorf("auth: publish writer lease: %w", err)
	}
	return nil
}

// Live enumerates writers whose lease has not expired, and prunes roster
// members whose entry is gone.
//
// SMEMBERS + MGET rather than SCAN: the runbook's Redis preflight already flags
// proxies with incomplete cursor semantics, and there is no reason to take that
// dependency for a set this small. Liveness comes from each entry key's own TTL,
// so Redis's clock is the only clock involved.
func (r *WriterRegistry) Live() ([]WriterEntry, error) {
	ids, err := r.client.SMembers(r.rosterKey).Result()
	if err != nil {
		return nil, fmt.Errorf("auth: read writer roster: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, r.entryKey(id))
	}
	values, err := r.client.MGet(keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("auth: read writer entries: %w", err)
	}
	live := make([]WriterEntry, 0, len(values))
	stale := make([]interface{}, 0)
	for i, value := range values {
		raw, ok := value.(string)
		if !ok || strings.TrimSpace(raw) == "" {
			stale = append(stale, ids[i])
			continue
		}
		var entry WriterEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			stale = append(stale, ids[i])
			continue
		}
		live = append(live, entry)
	}
	if len(stale) > 0 {
		_ = r.client.SRem(r.rosterKey, stale...).Err()
	}
	return live, nil
}

// WriterConvergence answers "how long has the live writer set looked exactly
// like this?".
//
// It exists because a count is not a convergence proof. `live == ExpectWriters`
// holds at the single instant during a maxSurge:1 rollout when the new replicas
// are all up and the last old one — which predates the registry and therefore
// registers nowhere — has not yet terminated. That replica can still mint v2
// credentials on the next login, which is the one thing the first v3 floor must
// rule out.
//
// Stability of the SET, held across at least one lease TTL, is a much stronger
// statement than the count, and it is sound for a specific reason: writer
// identities are per-incarnation. An entry present at both ends of a window
// longer than the lease TTL cannot have expired and been recreated in between —
// a restart mints a new id — and any pod joining or leaving changes the set. So
// an unchanged set across that window proves no membership change occurred,
// i.e. the rollout has stopped moving.
//
// It is a mitigation and not a proof of the absent replica itself, which is
// unobservable by construction; that residual risk is why EXPECT_WRITERS is a
// deployment-supplied number rather than something derived.
type WriterConvergence struct {
	mu          sync.Mutex
	fingerprint string
	since       time.Time
}

func NewWriterConvergence() *WriterConvergence { return &WriterConvergence{} }

// Observe records the roster and returns how long the CURRENT set has held.
// A set that differs from the previous observation restarts the window and
// returns zero.
func (c *WriterConvergence) Observe(entries []WriterEntry, now time.Time) time.Duration {
	fingerprint := writerSetFingerprint(entries)
	c.mu.Lock()
	defer c.mu.Unlock()
	if fingerprint != c.fingerprint {
		c.fingerprint = fingerprint
		c.since = now
		return 0
	}
	if now.Before(c.since) {
		// A clock that went backwards must not manufacture a long window.
		c.since = now
		return 0
	}
	return now.Sub(c.since)
}

// writerSetFingerprint covers identity, build and applied state: a replica that
// merely changes the mode it advertises has changed the fleet's convergence
// picture just as much as one that joins.
func writerSetFingerprint(entries []WriterEntry) string {
	// Prefixed with the count so the empty set is not "" — which equals the
	// struct's zero-value fingerprint, so the very first Observe of an empty
	// roster skipped the reset and subtracted the zero time, reporting a window
	// of ~292 years. Not reachable through the gate (an empty roster is refused
	// independently), but a fail-open-shaped default inside a fail-closed gate is
	// one refactor away from mattering.
	parts := make([]string, 0, len(entries)+1)
	parts = append(parts, strconv.Itoa(len(entries)))
	for _, entry := range entries {
		parts = append(parts, entry.ID+"|"+entry.Build+"|"+entry.AppliedState)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return fmt.Sprintf("%x", sum[:])
}
