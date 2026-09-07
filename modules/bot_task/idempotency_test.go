package bot_task

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/robot"
)

type memoryClaimBackend struct {
	mu     sync.Mutex
	values map[string]string
	events []robot.PreparedBotTypedEvent
}

func (b *memoryClaimBackend) Get(key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.values[key]
	if !ok {
		return "", errClaimNotFound
	}
	return value, nil
}
func (b *memoryClaimBackend) SetNX(key, value string, _ time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.values[key]; ok {
		return false, nil
	}
	b.values[key] = value
	return true, nil
}
func (b *memoryClaimBackend) CompareAndEnqueue(key, oldValue, newValue string, _ time.Duration, event robot.PreparedBotTypedEvent) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.values[key] != oldValue {
		return false, nil
	}
	b.values[key] = newValue
	b.events = append(b.events, event)
	return true, nil
}
func (b *memoryClaimBackend) CompareAndDelete(key, oldValue string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.values[key] != oldValue {
		return false, nil
	}
	delete(b.values, key)
	return true, nil
}

func TestClaimStoreCommitReplayAndConflict(t *testing.T) {
	backend := &memoryClaimBackend{values: map[string]string{}}
	store := &claimStore{backend: backend, doneTTL: time.Hour}
	outcome, lease, err := store.Begin("key", "sha-1")
	if err != nil || outcome.State != claimAcquired || lease == nil {
		t.Fatalf("begin = %+v, %v, %v", outcome, lease, err)
	}
	event := robot.PreparedBotTypedEvent{EventID: 42, QueueKey: "queue", Member: `{"event_id":42}`}
	committed, err := store.Commit(lease, event)
	if err != nil || !committed || len(backend.events) != 1 {
		t.Fatalf("commit = %v, %v, events=%d", committed, err, len(backend.events))
	}
	replay, err := store.Lookup("key", "sha-1")
	if err != nil || replay.State != claimReplay || replay.EventID != 42 {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	conflict, err := store.Lookup("key", "sha-2")
	if err != nil || conflict.State != claimConflict {
		t.Fatalf("conflict = %+v, %v", conflict, err)
	}
}

func TestClaimStoreReleaseRequiresLeaseValue(t *testing.T) {
	backend := &memoryClaimBackend{values: map[string]string{}}
	store := &claimStore{backend: backend, doneTTL: time.Hour}
	_, lease, err := store.Begin("key", "sha")
	if err != nil {
		t.Fatal(err)
	}
	backend.values["key"] = "new-owner"
	released, err := store.Release(lease)
	if err != nil || released {
		t.Fatalf("stale release = %v, %v", released, err)
	}
	if _, err := store.Lookup("missing", "sha"); err != nil && !errors.Is(err, errClaimNotFound) {
		t.Fatalf("missing lookup: %v", err)
	}
}
