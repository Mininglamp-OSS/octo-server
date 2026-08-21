package adminrbac

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakePermissionCache struct {
	values    map[string]string
	deleted   []string
	deleteErr error
}

func newFakePermissionCache() *fakePermissionCache {
	return &fakePermissionCache{values: make(map[string]string)}
}

func (f *fakePermissionCache) Set(key, value string) error {
	f.values[key] = value
	return nil
}

func (f *fakePermissionCache) Delete(key string) error {
	f.deleted = append(f.deleted, key)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.values, key)
	return nil
}

func (f *fakePermissionCache) SetAndExpire(key, value string, _ time.Duration) error {
	return f.Set(key, value)
}

func (f *fakePermissionCache) Get(key string) (string, error) {
	value, ok := f.values[key]
	if !ok {
		return "", errors.New("cache miss")
	}
	return value, nil
}

func TestPermissionCacheUsesRoleVersionsAndContractNamespace(t *testing.T) {
	backend := newFakePermissionCache()
	permissionCache := NewPermissionCache(backend)
	snapshots := []RoleSnapshot{{RoleKey: "reader", AuthorizationVersion: 3, Status: activeStatus, Permissions: []string{"user.read"}}}
	result, err := Evaluate("u1", snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if err := permissionCache.Set(result); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cached, ok, err := permissionCache.Get("u1", snapshots)
	if err != nil || !ok {
		t.Fatalf("Get = (%+v, %v, %v), want hit", cached, ok, err)
	}
	if len(backend.values) != 1 {
		t.Fatalf("cache entries = %d, want 1", len(backend.values))
	}
	var envelope cacheEnvelope
	for _, raw := range backend.values {
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatalf("unmarshal cache envelope: %v", err)
		}
	}
	if envelope.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("cache expires_at = %d, want future timestamp", envelope.ExpiresAt)
	}
	changed, ok, err := permissionCache.Get("u1", []RoleSnapshot{{RoleKey: "reader", AuthorizationVersion: 4, Status: activeStatus, Permissions: []string{"user.read"}}})
	if err != nil || ok || !reflectEffectiveEmpty(changed) {
		t.Fatalf("changed-version Get = (%+v, %v, %v), want miss", changed, ok, err)
	}
}

func TestPermissionCacheInvalidationSurfacesDeleteFailure(t *testing.T) {
	backend := newFakePermissionCache()
	backend.deleteErr = errors.New("redis unavailable")
	permissionCache := NewPermissionCache(backend)
	if err := permissionCache.InvalidateUser("u1"); err == nil {
		t.Fatal("InvalidateUser succeeded, want cache error")
	}
	if err := permissionCache.InvalidateRole("reader"); err == nil {
		t.Fatal("InvalidateRole succeeded, want cache error")
	}
}

func reflectEffectiveEmpty(result EffectivePermissions) bool {
	return result.UID == "" && len(result.Permissions) == 0 && len(result.RoleVersions) == 0
}
