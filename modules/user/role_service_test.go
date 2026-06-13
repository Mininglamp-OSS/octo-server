package user

import (
	"context"
	"errors"
	"testing"
)

// fakeRoleDB stubs the read surface so RoleService can be exercised without a
// real *DB.
type fakeRoleDB struct {
	role       map[string]string
	queryErr   error
	queryCalls int
}

func newFakeRoleDB() *fakeRoleDB {
	return &fakeRoleDB{role: map[string]string{}}
}

func (d *fakeRoleDB) QueryRoleByUID(uid string) (string, error) {
	d.queryCalls++
	if d.queryErr != nil {
		return "", d.queryErr
	}
	return d.role[uid], nil
}

func TestRoleServiceResolveCacheHit(t *testing.T) {
	c := newFakeLangCache()
	c.store[RoleCacheKeyPrefix+"u1"] = "admin"
	db := newFakeRoleDB()
	svc := NewRoleService(db, c)

	got, err := svc.ResolveRole(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ResolveRole: %v", err)
	}
	if got != "admin" {
		t.Fatalf("role = %q, want admin", got)
	}
	if db.queryCalls != 0 {
		t.Fatalf("cache hit must not touch DB, queryCalls=%d", db.queryCalls)
	}
}

func TestRoleServiceResolveNegativeCacheSkipsDB(t *testing.T) {
	c := newFakeLangCache()
	c.store[RoleCacheKeyPrefix+"u1"] = roleNegativeMarker
	db := newFakeRoleDB()
	svc := NewRoleService(db, c)

	got, err := svc.ResolveRole(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ResolveRole: %v", err)
	}
	if got != "" {
		t.Fatalf("role = %q, want \"\" (negative cache)", got)
	}
	if db.queryCalls != 0 {
		t.Fatalf("negative cache hit must not touch DB, queryCalls=%d", db.queryCalls)
	}
}

func TestRoleServiceResolveCacheMissPopulatesCache(t *testing.T) {
	c := newFakeLangCache()
	db := newFakeRoleDB()
	db.role["u1"] = "superAdmin"
	svc := NewRoleService(db, c)

	got, err := svc.ResolveRole(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ResolveRole: %v", err)
	}
	if got != "superAdmin" {
		t.Fatalf("role = %q, want superAdmin", got)
	}
	if c.store[RoleCacheKeyPrefix+"u1"] != "superAdmin" {
		t.Fatalf("cache not populated with DB value, got %q", c.store[RoleCacheKeyPrefix+"u1"])
	}
}

func TestRoleServiceResolveCacheMissEmptyPopulatesNegativeMarker(t *testing.T) {
	c := newFakeLangCache()
	db := newFakeRoleDB() // u1 has no role → normal user
	svc := NewRoleService(db, c)

	got, err := svc.ResolveRole(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ResolveRole: %v", err)
	}
	if got != "" {
		t.Fatalf("role = %q, want \"\"", got)
	}
	if c.store[RoleCacheKeyPrefix+"u1"] != roleNegativeMarker {
		t.Fatalf("empty DB role must cache negative marker, got %q", c.store[RoleCacheKeyPrefix+"u1"])
	}
}

// TestRoleServiceResolveCacheErrorFallsThroughToDB pins the security-sensitive
// degradation branch: a Redis Get error must NOT be treated as "no role" — it
// falls through to the authoritative DB read so a momentary cache outage can't
// silently strip (or grant) privileges.
func TestRoleServiceResolveCacheErrorFallsThroughToDB(t *testing.T) {
	c := newFakeLangCache()
	c.getErr = errors.New("redis down")
	db := newFakeRoleDB()
	db.role["u1"] = "superAdmin"
	svc := NewRoleService(db, c)

	got, err := svc.ResolveRole(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ResolveRole: %v", err)
	}
	if got != "superAdmin" {
		t.Fatalf("role = %q, want superAdmin (cache error must fall through to DB truth)", got)
	}
	if db.queryCalls != 1 {
		t.Fatalf("expected exactly 1 DB query on cache error, got %d", db.queryCalls)
	}
}

func TestRoleServiceResolvePropagatesDBError(t *testing.T) {
	c := newFakeLangCache()
	db := newFakeRoleDB()
	db.queryErr = errors.New("db down")
	svc := NewRoleService(db, c)

	_, err := svc.ResolveRole(context.Background(), "u1")
	if err == nil {
		t.Fatalf("expected DB error to propagate (parser keeps token snapshot on error)")
	}
}

func TestRoleServiceResolveEmptyUID(t *testing.T) {
	svc := NewRoleService(newFakeRoleDB(), newFakeLangCache())
	got, err := svc.ResolveRole(context.Background(), "")
	if err != nil || got != "" {
		t.Fatalf("empty uid => (\"\", nil), got (%q, %v)", got, err)
	}
}

func TestRoleServiceResolveContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := NewRoleService(newFakeRoleDB(), newFakeLangCache())
	if _, err := svc.ResolveRole(ctx, "u1"); err == nil {
		t.Fatalf("cancelled context must surface an error")
	}
}

func TestRoleServiceInvalidateDeletesHotKey(t *testing.T) {
	c := newFakeLangCache()
	c.store[RoleCacheKeyPrefix+"u1"] = "admin"
	svc := NewRoleService(newFakeRoleDB(), c)

	svc.Invalidate("u1")
	if _, ok := c.store[RoleCacheKeyPrefix+"u1"]; ok {
		t.Fatalf("Invalidate must delete the hot key")
	}
}

func TestNewRoleServicePanicsOnNilDeps(t *testing.T) {
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil_db", func() { _ = NewRoleService(nil, newFakeLangCache()) }},
		{"nil_cache", func() { _ = NewRoleService(newFakeRoleDB(), nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic on %s", tc.name)
				}
			}()
			tc.fn()
		})
	}
}
