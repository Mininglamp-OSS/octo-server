package user

import (
	"context"
	"testing"
)

// Multi-device language convergence (Phase 0 §0.9 verification scenario:
// "A 端切语言，B 端下次请求即生效").
//
// Models two devices / API nodes sharing one MySQL row and one Redis hot
// cache — the production topology. The convergence guarantee under test is:
// a PUT /v1/user/language on device A (LanguageService.SetLanguage) must make
// device B observe the new value on its *next* Resolve, without waiting out
// LanguageCacheTTL. The mechanism is the active DEL in SetLanguage; this test
// fails if that invalidation regresses (e.g. someone switches it to a
// write-through that leaves a stale entry, or drops the DEL entirely).
//
// Both devices share the same db + cache instances precisely because in
// production they hit the same backing stores. A single LanguageService
// instance is used for both because LanguageService is stateless beyond its
// injected db/cache; per-node instances would be byte-identical.
func TestLanguageService_MultiDeviceConvergence(t *testing.T) {
	const uid = "u-multidevice"
	db := newFakeLangDB()
	c := newFakeLangCache()
	svc := NewLanguageService(db, c)
	ctx := context.Background()

	// Device B reads first with no preference set → "" and a negative cache
	// entry is written so subsequent reads skip the DB.
	if got, err := svc.Resolve(ctx, uid); err != nil || got != "" {
		t.Fatalf("initial Resolve = (%q, %v), want (\"\", nil)", got, err)
	}
	if c.store[LanguageCacheKeyPrefix+uid] != negativeMarker {
		t.Fatalf("expected negative cache marker after empty resolve, got %q", c.store[LanguageCacheKeyPrefix+uid])
	}

	// Device A switches the language. This must DEL the hot key, not just
	// overwrite the DB — otherwise B keeps serving the stale negative marker
	// until TTL expiry.
	if err := svc.SetLanguage(ctx, uid, "en-US"); err != nil {
		t.Fatalf("device A SetLanguage: %v", err)
	}
	if db.updates[uid] != "en-US" {
		t.Fatalf("DB not updated by device A: %v", db.updates)
	}
	if _, present := c.store[LanguageCacheKeyPrefix+uid]; present {
		t.Fatalf("hot key must be invalidated (DEL) on cross-device switch, still present: %q", c.store[LanguageCacheKeyPrefix+uid])
	}

	// Device B's next read converges immediately: cache miss → DB → "en-US".
	queriesBefore := db.queryCalls
	got, err := svc.Resolve(ctx, uid)
	if err != nil {
		t.Fatalf("device B Resolve after switch: %v", err)
	}
	if got != "en-US" {
		t.Fatalf("device B sees %q after device A switched to en-US; convergence broken", got)
	}
	if db.queryCalls != queriesBefore+1 {
		t.Fatalf("expected exactly one DB re-query on the post-invalidation read, got %d", db.queryCalls-queriesBefore)
	}

	// And the fresh value is re-cached so a third read on either device is a
	// cache hit (no further DB load).
	if c.store[LanguageCacheKeyPrefix+uid] != "en-US" {
		t.Fatalf("post-convergence cache = %q, want en-US", c.store[LanguageCacheKeyPrefix+uid])
	}
	queriesBeforeHit := db.queryCalls
	if got, err := svc.Resolve(ctx, uid); err != nil || got != "en-US" {
		t.Fatalf("cached Resolve = (%q, %v), want (en-US, nil)", got, err)
	}
	if db.queryCalls != queriesBeforeHit {
		t.Fatalf("third read should be a cache hit, but DB was queried again")
	}
}

// TestLanguageService_ClearConvergesToDefault covers the inverse direction:
// device A clears its preference (empty string), device B must converge to
// "" (the OCTO_DEFAULT_LANGUAGE fallback semantic) rather than keep serving
// the previously-cached explicit value.
func TestLanguageService_ClearConvergesToDefault(t *testing.T) {
	const uid = "u-clear"
	db := newFakeLangDB()
	db.lang[uid] = "en-US"
	c := newFakeLangCache()
	svc := NewLanguageService(db, c)
	ctx := context.Background()

	// Warm both devices to the explicit en-US preference.
	if got, _ := svc.Resolve(ctx, uid); got != "en-US" {
		t.Fatalf("warm Resolve = %q, want en-US", got)
	}

	// Device A clears the preference.
	if err := svc.SetLanguage(ctx, uid, ""); err != nil {
		t.Fatalf("device A clear: %v", err)
	}
	if db.updates[uid] != "" {
		t.Fatalf("DB clear not persisted: %q", db.updates[uid])
	}

	// Device B converges to "" (not the stale en-US).
	got, err := svc.Resolve(ctx, uid)
	if err != nil {
		t.Fatalf("device B Resolve after clear: %v", err)
	}
	if got != "" {
		t.Fatalf("device B sees %q after clear; want \"\" (default fallback)", got)
	}
}
