package user

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLangCache implements octo-lib cache.Cache for LanguageService tests
// without spinning up Redis. The struct also records Set/Delete calls so
// negative-cache and invalidation assertions can fire without timing tricks.
type fakeLangCache struct {
	store     map[string]string
	expire    map[string]time.Duration
	getErr    error
	setErr    error
	delErr    error
	deletes   []string
	setCalls  int
}

func newFakeLangCache() *fakeLangCache {
	return &fakeLangCache{store: map[string]string{}, expire: map[string]time.Duration{}}
}

func (c *fakeLangCache) Set(key, value string) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.store[key] = value
	c.setCalls++
	return nil
}
func (c *fakeLangCache) SetAndExpire(key, value string, exp time.Duration) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.store[key] = value
	c.expire[key] = exp
	c.setCalls++
	return nil
}
func (c *fakeLangCache) Get(key string) (string, error) {
	if c.getErr != nil {
		return "", c.getErr
	}
	return c.store[key], nil
}
func (c *fakeLangCache) Delete(key string) error {
	c.deletes = append(c.deletes, key)
	if c.delErr != nil {
		return c.delErr
	}
	delete(c.store, key)
	return nil
}

// fakeLangDB stubs the read+write surface so the service can be exercised
// without a real *DB.
type fakeLangDB struct {
	lang    map[string]string
	queryErr error
	updates map[string]string
	updateErr error
	queryCalls int
}

func newFakeLangDB() *fakeLangDB {
	return &fakeLangDB{lang: map[string]string{}, updates: map[string]string{}}
}

func (d *fakeLangDB) QueryLanguageByUID(uid string) (string, error) {
	d.queryCalls++
	if d.queryErr != nil {
		return "", d.queryErr
	}
	return d.lang[uid], nil
}
func (d *fakeLangDB) UpdateLanguageByUID(uid, lang string) error {
	if d.updateErr != nil {
		return d.updateErr
	}
	d.updates[uid] = lang
	return nil
}

// readOnlyLangDB intentionally omits UpdateLanguageByUID so SetLanguage can
// assert the explicit "no write surface" path.
type readOnlyLangDB struct{ lang map[string]string }

func (d *readOnlyLangDB) QueryLanguageByUID(uid string) (string, error) {
	return d.lang[uid], nil
}

func TestLanguageServiceResolveCacheHit(t *testing.T) {
	t.Parallel()
	c := newFakeLangCache()
	_ = c.Set(LanguageCacheKeyPrefix+"u1", "zh-CN")
	db := newFakeLangDB()
	db.lang["u1"] = "en-US" // intentionally different to prove cache wins
	svc := NewLanguageService(db, c)

	got, err := svc.Resolve(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "zh-CN" {
		t.Fatalf("want zh-CN from cache, got %q", got)
	}
	if db.queryCalls != 0 {
		t.Fatalf("cache hit must not hit DB, but DB was queried %d times", db.queryCalls)
	}
}

func TestLanguageServiceResolveNegativeCache(t *testing.T) {
	t.Parallel()
	c := newFakeLangCache()
	_ = c.Set(LanguageCacheKeyPrefix+"u1", negativeMarker)
	db := newFakeLangDB()
	db.lang["u1"] = "zh-CN" // also intentional: negative cache must win
	svc := NewLanguageService(db, c)

	got, err := svc.Resolve(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "" {
		t.Fatalf("negative marker must return empty, got %q", got)
	}
	if db.queryCalls != 0 {
		t.Fatalf("negative marker hit must not hit DB, got %d calls", db.queryCalls)
	}
}

func TestLanguageServiceResolveCacheMissPopulatesCache(t *testing.T) {
	t.Parallel()
	c := newFakeLangCache()
	db := newFakeLangDB()
	db.lang["u1"] = "zh-CN"
	svc := NewLanguageService(db, c)

	got, err := svc.Resolve(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "zh-CN" {
		t.Fatalf("want zh-CN, got %q", got)
	}
	if cached, _ := c.Get(LanguageCacheKeyPrefix + "u1"); cached != "zh-CN" {
		t.Fatalf("cache should be populated with zh-CN, got %q", cached)
	}
	if c.expire[LanguageCacheKeyPrefix+"u1"] != LanguageCacheTTL {
		t.Fatalf("expected TTL %s, got %s", LanguageCacheTTL, c.expire[LanguageCacheKeyPrefix+"u1"])
	}
}

func TestLanguageServiceResolveCacheMissEmptyPopulatesNegativeMarker(t *testing.T) {
	t.Parallel()
	c := newFakeLangCache()
	db := newFakeLangDB() // db has no preference for u1
	svc := NewLanguageService(db, c)

	got, err := svc.Resolve(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "" {
		t.Fatalf("want empty for no preference, got %q", got)
	}
	if cached, _ := c.Get(LanguageCacheKeyPrefix + "u1"); cached != negativeMarker {
		t.Fatalf("cache should be populated with negative marker, got %q", cached)
	}
}

func TestLanguageServiceResolveUnsupportedCacheValueDrops(t *testing.T) {
	t.Parallel()
	c := newFakeLangCache()
	_ = c.Set(LanguageCacheKeyPrefix+"u1", "klingon")
	db := newFakeLangDB()
	db.lang["u1"] = "en-US"
	svc := NewLanguageService(db, c)

	got, err := svc.Resolve(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "en-US" {
		t.Fatalf("want en-US from DB after dropping stale cache, got %q", got)
	}
	if len(c.deletes) == 0 || c.deletes[0] != LanguageCacheKeyPrefix+"u1" {
		t.Fatalf("expected stale cache to be deleted, deletes=%v", c.deletes)
	}
}

func TestLanguageServiceResolvePropagatesDBError(t *testing.T) {
	t.Parallel()
	c := newFakeLangCache()
	db := newFakeLangDB()
	db.queryErr = errors.New("connection refused")
	svc := NewLanguageService(db, c)

	got, err := svc.Resolve(context.Background(), "u1")
	if err == nil {
		t.Fatalf("expected DB error to propagate, got nil")
	}
	if !errors.Is(err, db.queryErr) {
		t.Fatalf("err = %v, want wrap of %v", err, db.queryErr)
	}
	if got != "" {
		t.Fatalf("on error result must be empty, got %q", got)
	}
}

func TestLanguageServiceResolveEmptyUID(t *testing.T) {
	t.Parallel()
	svc := NewLanguageService(newFakeLangDB(), newFakeLangCache())
	got, err := svc.Resolve(context.Background(), "")
	if err != nil || got != "" {
		t.Fatalf("empty uid must short-circuit, got (%q, %v)", got, err)
	}
}

func TestLanguageServiceResolveContextCancelled(t *testing.T) {
	t.Parallel()
	svc := NewLanguageService(newFakeLangDB(), newFakeLangCache())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.Resolve(ctx, "u1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ctx.Canceled, got %v", err)
	}
}

func TestLanguageServiceSetLanguageHappyPath(t *testing.T) {
	t.Parallel()
	c := newFakeLangCache()
	_ = c.Set(LanguageCacheKeyPrefix+"u1", "zh-CN") // existing hot entry
	db := newFakeLangDB()
	svc := NewLanguageService(db, c)

	if err := svc.SetLanguage(context.Background(), "u1", "en-US"); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	if db.updates["u1"] != "en-US" {
		t.Fatalf("db not updated, got %v", db.updates)
	}
	if len(c.deletes) == 0 || c.deletes[0] != LanguageCacheKeyPrefix+"u1" {
		t.Fatalf("cache invalidation missed, deletes=%v", c.deletes)
	}
}

func TestLanguageServiceSetLanguageRejectsUnsupported(t *testing.T) {
	t.Parallel()
	svc := NewLanguageService(newFakeLangDB(), newFakeLangCache())
	err := svc.SetLanguage(context.Background(), "u1", "klingon")
	if err == nil {
		t.Fatal("expected error for unsupported language")
	}
	// Sentinel must wrap; handler relies on errors.Is to split user-facing
	// copy between unsupported (400 with "不支持的语言") and infra error
	// (generic 400 with "设置语言偏好失败！").
	if !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("err = %v, want wrap of ErrUnsupportedLanguage", err)
	}
}

func TestLanguageServiceSetLanguageAcceptsEmptyToClear(t *testing.T) {
	t.Parallel()
	db := newFakeLangDB()
	db.lang["u1"] = "zh-CN"
	svc := NewLanguageService(db, newFakeLangCache())
	if err := svc.SetLanguage(context.Background(), "u1", ""); err != nil {
		t.Fatalf("SetLanguage with empty should clear: %v", err)
	}
	if db.updates["u1"] != "" {
		t.Fatalf("expected DB to be cleared, got %q", db.updates["u1"])
	}
}

func TestLanguageServiceSetLanguageWithoutWriteSurface(t *testing.T) {
	t.Parallel()
	svc := NewLanguageService(&readOnlyLangDB{lang: map[string]string{}}, newFakeLangCache())
	err := svc.SetLanguage(context.Background(), "u1", "en-US")
	if err == nil {
		t.Fatal("expected error when underlying DB does not implement languageWriter")
	}
}

func TestLanguageServiceSetLanguageEmptyUID(t *testing.T) {
	t.Parallel()
	svc := NewLanguageService(newFakeLangDB(), newFakeLangCache())
	if err := svc.SetLanguage(context.Background(), "", "zh-CN"); err == nil {
		t.Fatal("empty uid must error")
	}
}

func TestNewLanguageServicePanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil_db", func() { _ = NewLanguageService(nil, newFakeLangCache()) }},
		{"nil_cache", func() { _ = NewLanguageService(newFakeLangDB(), nil) }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			tc.fn()
		})
	}
}
