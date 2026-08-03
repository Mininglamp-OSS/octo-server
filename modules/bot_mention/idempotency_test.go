package bot_mention

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/robot"
)

type memoryClaimBackend struct {
	mu sync.Mutex

	values map[string]string
	ttls   map[string]time.Duration
	queues map[string]map[int64]string

	getErr    error
	setNXErr  error
	commitErr error
	delCASErr error
}

type disappearingSetNXBackend struct{}

func (disappearingSetNXBackend) Get(string) (string, error) {
	return "", errClaimNotFound
}

func (disappearingSetNXBackend) SetNX(string, string, time.Duration) (bool, error) {
	return false, nil
}

func (disappearingSetNXBackend) CompareAndEnqueue(string, string, string, time.Duration, robot.PreparedBotTypedEvent) (bool, error) {
	return false, nil
}

func (disappearingSetNXBackend) CompareAndDelete(string, string) (bool, error) {
	return false, nil
}

func newMemoryClaimBackend() *memoryClaimBackend {
	return &memoryClaimBackend{
		values: make(map[string]string),
		ttls:   make(map[string]time.Duration),
		queues: make(map[string]map[int64]string),
	}
}

func (b *memoryClaimBackend) Get(key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.getErr != nil {
		return "", b.getErr
	}
	v, ok := b.values[key]
	if !ok {
		return "", errClaimNotFound
	}
	return v, nil
}

func (b *memoryClaimBackend) SetNX(key, value string, ttl time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.setNXErr != nil {
		return false, b.setNXErr
	}
	if _, ok := b.values[key]; ok {
		return false, nil
	}
	b.values[key] = value
	b.ttls[key] = ttl
	return true, nil
}

func (b *memoryClaimBackend) CompareAndEnqueue(key, oldValue, newValue string, ttl time.Duration, event robot.PreparedBotTypedEvent) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.commitErr != nil {
		return false, b.commitErr
	}
	if b.values[key] != oldValue {
		return false, nil
	}
	queue := b.queues[event.QueueKey]
	if queue == nil {
		queue = make(map[int64]string)
		b.queues[event.QueueKey] = queue
	}
	queue[event.EventID] = event.Member
	b.ttls[event.QueueKey] = ttl
	b.values[key] = newValue
	b.ttls[key] = ttl
	return true, nil
}

func (b *memoryClaimBackend) CompareAndDelete(key, oldValue string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.delCASErr != nil {
		return false, b.delCASErr
	}
	if b.values[key] != oldValue {
		return false, nil
	}
	delete(b.values, key)
	delete(b.ttls, key)
	return true, nil
}

func (b *memoryClaimBackend) forceDelete(key string) {
	b.mu.Lock()
	delete(b.values, key)
	delete(b.ttls, key)
	b.mu.Unlock()
}

func (b *memoryClaimBackend) queuedEventIDs(key string) []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	queue := b.queues[key]
	ids := make([]int64, 0, len(queue))
	for eventID := range queue {
		ids = append(ids, eventID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func preparedTestEvent(eventID int64) robot.PreparedBotTypedEvent {
	return robot.PreparedBotTypedEvent{
		EventID:  eventID,
		QueueKey: "robotEvent:bot-a",
		Member:   "event-payload",
	}
}

func deterministicTokens(tokens ...string) func() (string, error) {
	var mu sync.Mutex
	next := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if next >= len(tokens) {
			return "", errors.New("no token available")
		}
		v := tokens[next]
		next++
		return v, nil
	}
}

func TestClaimStoreLifecycleAndConflict(t *testing.T) {
	backend := newMemoryClaimBackend()
	store := newClaimStore(backend, 2*time.Hour, deterministicTokens("lease-a", "lease-b"))
	key := mentionClaimKey("bot-a", "idem-a")

	outcome, err := store.Lookup(key, "sha-a")
	if err != nil || outcome.State != claimMissing {
		t.Fatalf("initial lookup = %+v, %v", outcome, err)
	}

	outcome, lease, err := store.Begin(key, "sha-a")
	if err != nil || outcome.State != claimAcquired || lease == nil {
		t.Fatalf("begin = %+v, lease=%+v, err=%v", outcome, lease, err)
	}
	if backend.ttls[key] != claimPendingTTL {
		t.Fatalf("pending TTL = %v, want %v", backend.ttls[key], claimPendingTTL)
	}

	pending, _, err := store.Begin(key, "sha-a")
	if err != nil || pending.State != claimPending {
		t.Fatalf("duplicate begin = %+v, %v", pending, err)
	}
	conflict, _, err := store.Begin(key, "sha-b")
	if err != nil || conflict.State != claimConflict {
		t.Fatalf("conflicting begin = %+v, %v", conflict, err)
	}

	event := preparedTestEvent(4242)
	ok, err := store.Commit(lease, event)
	if err != nil || !ok {
		t.Fatalf("commit = %v, %v", ok, err)
	}
	if backend.ttls[key] != 2*time.Hour {
		t.Fatalf("done TTL = %v, want 2h", backend.ttls[key])
	}
	if backend.ttls[event.QueueKey] != 2*time.Hour {
		t.Fatalf("queue TTL = %v, want 2h", backend.ttls[event.QueueKey])
	}
	if ids := backend.queuedEventIDs(event.QueueKey); len(ids) != 1 || ids[0] != event.EventID {
		t.Fatalf("queued event IDs = %v, want [%d]", ids, event.EventID)
	}
	replay, err := store.Lookup(key, "sha-a")
	if err != nil || replay.State != claimReplay || replay.EventID != 4242 {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
}

func TestClaimStoreTreatsDisappearingSetNXLoserAsInProgress(t *testing.T) {
	store := newClaimStore(disappearingSetNXBackend{}, time.Hour, deterministicTokens("lease-a"))
	outcome, lease, err := store.Begin("claim-key", "sha-a")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if outcome.State != claimPending || lease != nil {
		t.Fatalf("Begin() = %+v, lease=%+v; want pending with no lease", outcome, lease)
	}
}

func TestClaimStoreStaleLeaseCannotMutateReplacement(t *testing.T) {
	backend := newMemoryClaimBackend()
	store := newClaimStore(backend, time.Hour, deterministicTokens("old", "new"))
	key := mentionClaimKey("bot-a", "idem-a")

	_, oldLease, err := store.Begin(key, "sha-a")
	if err != nil {
		t.Fatal(err)
	}
	backend.forceDelete(key) // model pending expiry before the old worker resumes
	_, newLease, err := store.Begin(key, "sha-a")
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := store.Release(oldLease)
	if err != nil || deleted {
		t.Fatalf("stale release = %v, %v; must not delete replacement", deleted, err)
	}
	ok, err := store.Commit(newLease, preparedTestEvent(200))
	if err != nil || !ok {
		t.Fatalf("new commit = %v, %v", ok, err)
	}
	ok, err = store.Commit(oldLease, preparedTestEvent(100))
	if err != nil || ok {
		t.Fatalf("stale commit = %v, %v; must not enqueue or overwrite replacement", ok, err)
	}
	if ids := backend.queuedEventIDs("robotEvent:bot-a"); len(ids) != 1 || ids[0] != 200 {
		t.Fatalf("queued event IDs = %v, want [200]", ids)
	}
	replay, err := store.Lookup(key, "sha-a")
	if err != nil || replay.EventID != 200 {
		t.Fatalf("replacement replay = %+v, %v", replay, err)
	}
}

func TestClaimStorePropagatesBackendAndEncodingErrors(t *testing.T) {
	backend := newMemoryClaimBackend()
	store := newClaimStore(backend, time.Hour, deterministicTokens("lease-a"))
	key := mentionClaimKey("bot-a", "idem-a")

	backend.getErr = errors.New("redis get failed")
	if _, err := store.Lookup(key, "sha-a"); err == nil {
		t.Fatal("expected lookup error")
	}
	backend.getErr = nil
	backend.setNXErr = errors.New("redis setnx failed")
	if _, _, err := store.Begin(key, "sha-a"); err == nil {
		t.Fatal("expected begin error")
	}

	backend.setNXErr = nil
	backend.values[key] = "not-json"
	if _, err := store.Lookup(key, "sha-a"); err == nil {
		t.Fatal("expected corrupt record error")
	}
}

func TestMentionClaimKeyDoesNotExposeIdentifiers(t *testing.T) {
	key := mentionClaimKey("sensitive-bot", "sensitive-idempotency-key")
	if !strings.HasPrefix(key, mentionClaimPrefix) {
		t.Fatalf("key %q missing prefix %q", key, mentionClaimPrefix)
	}
	if strings.Contains(key, "sensitive") {
		t.Fatalf("claim key leaks raw identifiers: %q", key)
	}
	if key != mentionClaimKey("sensitive-bot", "sensitive-idempotency-key") {
		t.Fatal("claim key must be stable")
	}
}

func TestClaimStoreDefaultsAndErrorBranches(t *testing.T) {
	backend := newMemoryClaimBackend()
	store := newClaimStore(backend, 0, nil)
	if store.doneTTL != defaultClaimDoneTTL || store.newToken == nil {
		t.Fatalf("defaults doneTTL=%v tokenConfigured=%v", store.doneTTL, store.newToken != nil)
	}
	if token, err := newClaimToken(); err != nil || len(token) != 32 {
		t.Fatalf("newClaimToken() = %q, %v", token, err)
	}
	if ok, err := store.Commit(nil, robot.PreparedBotTypedEvent{}); err == nil || ok {
		t.Fatalf("Commit(nil) = %v, %v", ok, err)
	}
	if ok, err := store.Release(nil); err == nil || ok {
		t.Fatalf("Release(nil) = %v, %v", ok, err)
	}

	tokenErrStore := newClaimStore(backend, time.Hour, func() (string, error) {
		return "", errors.New("entropy unavailable")
	})
	if _, _, err := tokenErrStore.Begin("new-key", "sha"); err == nil {
		t.Fatal("expected token generation error")
	}

	backend.values["unknown"] = `{"state":"future","sha":"sha"}`
	if _, err := store.Lookup("unknown", "sha"); err == nil {
		t.Fatal("expected unknown state error")
	}
	backend.values["done-without-event"] = `{"state":"done","sha":"sha"}`
	if _, err := store.Lookup("done-without-event", "sha"); err == nil {
		t.Fatal("expected missing event id error")
	}
}

func TestClaimStorePropagatesCommitAndReleaseErrors(t *testing.T) {
	backend := newMemoryClaimBackend()
	store := newClaimStore(backend, time.Hour, deterministicTokens("lease-a", "lease-b"))
	_, lease, err := store.Begin("confirm-key", "sha")
	if err != nil {
		t.Fatal(err)
	}
	backend.commitErr = errors.New("atomic commit failed")
	if ok, err := store.Commit(lease, preparedTestEvent(1)); err == nil || ok {
		t.Fatalf("Commit error = %v, %v", ok, err)
	}
	backend.commitErr = nil

	_, releaseLease, err := store.Begin("release-key", "sha")
	if err != nil {
		t.Fatal(err)
	}
	backend.delCASErr = errors.New("cas delete failed")
	if ok, err := store.Release(releaseLease); err == nil || ok {
		t.Fatalf("Release CAS error = %v, %v", ok, err)
	}
}
