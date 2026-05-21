// Package bot_api · PR#114 R4 (Jerry-Xin + lml2468 cache-poisoning
// blocker, 2026-05-21).
//
// `findActiveGrantsForChannelByGrantors` runs a UID-FILTERED query
// against the `obo_grants` table. Its result answers "do any of the
// mentioned grantors have an active grant", which is a SUBSET answer
// — not the channel-wide answer the `obo:chan:{type}:{id}` cache
// encodes ("does this channel have ANY active grant × enabled
// scope"). Pre-fix, the function wrote its filtered result into the
// channel-wide key and also short-circuited on it. Both directions
// poisoned the fan-out path for legitimate follow-up traffic:
//
//   - Poisoning WRITE: a message in group_42 that mentions @alice
//     (no grant) → filtered query returns 0 rows → cache writes
//     `obo:chan:Group:group_42 = "0"`. The next message in group_42
//     that mentions @admin (HAS a grant) → unfiltered
//     `findActiveGrantsForChannel` reads the cached "0" → returns []
//     → fan-out suppressed.
//
//   - Poisoning READ: a DM peer uid that happens to collide string-
//     wise with a group id can write "0" via the DM path; the
//     filtered group lookup honoring that "0" would suppress the
//     group fan-out for grantors who actually have a grant.
//
// The fix in obo_db.go removes BOTH the write AND the read from
// `findActiveGrantsForChannelByGrantors`. Tests in this file pin
// that contract end-to-end:
//
//  1. Direct test on a cache-aware oboStore wrapper: a UID-filtered
//     miss does NOT poison a subsequent unfiltered call.
//  2. Fan-out integration: @alice (no grant) followed by @bob (has
//     grant) in the same group → bob's fan-out fires. Catches a
//     regression that re-introduces either the cache write or the
//     cache read.
package bot_api

import (
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// channelCacheKey mirrors the production `obo:chan:{type}:{id}`
// composite key shape so the wrapper below cannot accidentally
// collide DM and Group entries that share the same string.
type channelCacheKey struct {
	channelID   string
	channelType uint8
}

// cachedFakeOBOStore wraps *fakeOBOStore and adds a minimal in-memory
// channel cache that mirrors the PRODUCTION cache contract for the
// `obo:chan:{type}:{id}` scalar:
//
//   - The unfiltered `findActiveGrantsForChannel` WRITES the cache
//     after each call (true if any grant, false if zero) and READS
//     the cache before hitting the underlying fake store, returning
//     early on a cached "no grants" answer.
//
//   - The filtered `findActiveGrantsForChannelByGrantors` does NOT
//     touch the cache in either direction. This is the PR#114 R4
//     fix: a UID-filtered subset cannot prove the channel-wide
//     negative answer the cache encodes, so writing it would
//     suppress legitimate fan-out for OTHER grantors and reading
//     a cross-namespace DM write would suppress legitimate group
//     traffic.
//
// All other oboStore methods are delegated to the embedded fake
// verbatim, so this wrapper can be dropped in anywhere a
// *fakeOBOStore was previously used.
//
// We use a SEPARATE wrapper rather than extending *fakeOBOStore
// itself so existing tests that depend on the cache-less fake
// shape are not perturbed; the cache contract is opt-in for the
// tests that need it.
type cachedFakeOBOStore struct {
	*fakeOBOStore
	cacheMu sync.Mutex
	// cache holds the trinary state per (channelID, channelType):
	//   - present and value=true  → "1" (any grant)
	//   - present and value=false → "0" (no grants)
	//   - absent                  → cache miss / unknown
	cache map[channelCacheKey]bool
}

func newCachedFakeOBOStore() *cachedFakeOBOStore {
	return &cachedFakeOBOStore{
		fakeOBOStore: newFakeOBOStore(),
		cache:        map[channelCacheKey]bool{},
	}
}

// channelCacheSaysNone returns true iff the cache holds a definitive
// "no grants" answer for the (channelID, channelType) pair. Mirrors
// the production helper of the same name on *botAPIDB.
func (c *cachedFakeOBOStore) channelCacheSaysNone(channelID string, channelType uint8) bool {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	v, ok := c.cache[channelCacheKey{channelID, channelType}]
	return ok && !v
}

// writeChannelCache populates the cache. Mirrors the production
// helper of the same name on *botAPIDB.
func (c *cachedFakeOBOStore) writeChannelCache(channelID string, channelType uint8, any bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	c.cache[channelCacheKey{channelID, channelType}] = any
}

// findActiveGrantsForChannel — wraps the fake to add the channel
// cache read/write loop. Behavior is otherwise verbatim.
func (c *cachedFakeOBOStore) findActiveGrantsForChannel(channelID string, channelType uint8) ([]*oboGrantModel, error) {
	if c.channelCacheSaysNone(channelID, channelType) {
		// Increment the call counter so tests that pin "early return on
		// cached miss" can still observe the call shape.
		c.fakeOBOStore.mu.Lock()
		c.fakeOBOStore.findGrantsChannelCalls++
		c.fakeOBOStore.mu.Unlock()
		return []*oboGrantModel{}, nil
	}
	grants, err := c.fakeOBOStore.findActiveGrantsForChannel(channelID, channelType)
	if err != nil {
		return nil, err
	}
	c.writeChannelCache(channelID, channelType, len(grants) > 0)
	return grants, nil
}

// findActiveGrantsForChannelByGrantors — wraps the fake but MUST
// NOT touch the channel cache in either direction (PR#114 R4 fix).
// This wrapper intentionally calls neither channelCacheSaysNone nor
// writeChannelCache so the fan-out tests catch a regression that
// re-introduces either operation.
func (c *cachedFakeOBOStore) findActiveGrantsForChannelByGrantors(channelID string, channelType uint8, grantorUIDs []string) ([]*oboGrantModel, error) {
	return c.fakeOBOStore.findActiveGrantsForChannelByGrantors(channelID, channelType, grantorUIDs)
}

// ==================== Tests ====================

// TestPR114R4_FilteredQueryDoesNotPoisonChannelCache pins the DIRECT
// store-layer contract: a UID-filtered query whose result is empty
// must NOT write "no grants" into the channel-wide cache key. A
// subsequent unfiltered call against the same channel must still see
// the actual rows.
//
// Scenario:
//   - group_42 has one grant: bob → bot (global_enabled=1, no scope row)
//   - alice has no grant
//   - Call findActiveGrantsForChannelByGrantors(group_42, ["alice"])
//     → returns [] (alice has no grant). Pre-fix this would write
//     `obo:chan:Group:group_42 = "0"` (cache poisoned).
//   - Call findActiveGrantsForChannel(group_42)
//     → must return [bob's grant]. Pre-fix this would return [] from
//     the cached "0" short-circuit.
func TestPR114R4_FilteredQueryDoesNotPoisonChannelCache(t *testing.T) {
	const (
		ch       = "group_42"
		bobUID   = "u_bob"
		aliceUID = "u_alice"
	)
	ct := common.ChannelTypeGroup.Uint8()

	c := newCachedFakeOBOStore()
	// Seed bob's grant: active=1, global_enabled=1, no scope row.
	gid, err := c.insertGrant(bobUID, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := c.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}

	// 1. Filtered miss for alice — alice has no grant, returns [].
	got, err := c.findActiveGrantsForChannelByGrantors(ch, ct, []string{aliceUID})
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("alice has no grant: expected 0 rows, got %d", len(got))
	}

	// 2. The filtered miss MUST NOT have written "no grants" into the
	//    channel-wide cache. channelCacheSaysNone is the production
	//    short-circuit; if it returns true the unfiltered call below
	//    early-returns empty and the fan-out is silently suppressed.
	if c.channelCacheSaysNone(ch, ct) {
		t.Fatalf("PR#114 R4 regression: filtered query for alice poisoned the channel-wide cache " +
			"(channelCacheSaysNone=true after a UID-filtered miss). The cache key answers a " +
			"CHANNEL-WIDE question; a UID-filtered subset cannot prove it.")
	}

	// 3. Unfiltered lookup must return bob's grant.
	got, err = c.findActiveGrantsForChannel(ch, ct)
	if err != nil {
		t.Fatalf("unfiltered query: %v", err)
	}
	if len(got) != 1 || got[0].GrantorUID != bobUID {
		t.Fatalf("unfiltered lookup expected [bob], got %d rows: %+v", len(got), got)
	}
}

// TestPR114R4_FilteredQueryIgnoresCrossNamespaceChannelCache pins the
// READ-side contract: even if the channel-wide cache holds "0" (e.g.
// written by an earlier UNFILTERED DM lookup whose peer uid happens
// to string-collide with this group id), the FILTERED group lookup
// must STILL probe the underlying store. Reading the cached "0"
// would incorrectly suppress group fan-out for grantors who actually
// have a grant.
//
// Pre-fix `findActiveGrantsForChannelByGrantors` short-circuited on
// `channelCacheSaysNone`; the fix removes that read.
func TestPR114R4_FilteredQueryIgnoresCrossNamespaceChannelCache(t *testing.T) {
	const (
		ch     = "group_42"
		bobUID = "u_bob"
	)
	ct := common.ChannelTypeGroup.Uint8()

	c := newCachedFakeOBOStore()
	gid, err := c.insertGrant(bobUID, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := c.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}

	// Pre-seed the channel cache with "no grants" — simulates a stale
	// or cross-namespace write the filtered path must NOT honor.
	c.writeChannelCache(ch, ct, false)
	if !c.channelCacheSaysNone(ch, ct) {
		t.Fatalf("test setup: expected channelCacheSaysNone=true after writeChannelCache(false)")
	}

	got, err := c.findActiveGrantsForChannelByGrantors(ch, ct, []string{bobUID})
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	if len(got) != 1 || got[0].GrantorUID != bobUID {
		t.Fatalf("PR#114 R4 regression: filtered query honored a stale channelCacheSaysNone=true "+
			"and returned %d rows; expected [bob's grant] (the filtered path must probe its "+
			"own UID set, not read the channel-wide cache)", len(got))
	}
}

// TestFanout_PR114R4_AliceMissDoesNotPoisonBobFanout — the
// end-to-end fan-out test the issue calls out. Two grantors in the
// same group: alice (no grant) and bob (has grant). Send @alice
// first, then @bob — bob's fan-out MUST fire on the second message,
// not be suppressed by a cache "0" left behind from alice's miss.
func TestFanout_PR114R4_AliceMissDoesNotPoisonBobFanout(t *testing.T) {
	const (
		ch       = "group_42"
		bobUID   = "u_bob"
		aliceUID = "u_alice"
	)
	ct := common.ChannelTypeGroup.Uint8()

	c := newCachedFakeOBOStore()
	// Bob has an active grant on the group (global_enabled=1, no
	// scope row — Group channel type does not require a scope per
	// YUJ-1538). Alice has nothing.
	gid, err := c.insertGrant(bobUID, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant bob: %v", err)
	}
	enable := 1
	if err := c.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant bob: %v", err)
	}

	fc := &fanoutCapture{}
	ba := &BotAPI{
		Log:               log.NewTLog("BotAPI-PR114R4-test"),
		oboStoreOverride:  c,
		oboFanoutDispatch: fc.hook,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			// Both alice and bob can read the channel — strip the
			// PR#82 P1-A re-check from this scenario's variables.
			return true, nil
		},
	}

	// Message 1 — sender mentions @alice, who has no grant. The
	// filtered query returns 0 rows. No fan-out.
	msg1 := &config.MessageResp{
		FromUID:     "u_carol",
		ChannelID:   ch,
		ChannelType: ct,
		Payload: []byte(`{"type":1,"content":"@alice ping",` +
			`"mention":{"uids":["` + aliceUID + `"]}}`),
	}
	if got := ba.fanoutForMessage(msg1); got != 0 {
		t.Fatalf("alice has no grant: expected 0 fan-out, got %d", got)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("alice has no grant: expected 0 dispatched copies, got %d", len(fc.copies))
	}

	// Message 2 — same group, sender mentions @bob (HAS a grant).
	// Pre-fix this returned 0 because alice's miss had poisoned the
	// channel-wide cache. Post-fix bob's filtered query goes
	// straight to the store and finds his grant.
	msg2 := &config.MessageResp{
		FromUID:     "u_carol",
		ChannelID:   ch,
		ChannelType: ct,
		Payload: []byte(`{"type":1,"content":"@bob help",` +
			`"mention":{"uids":["` + bobUID + `"]}}`),
	}
	if got := ba.fanoutForMessage(msg2); got != 1 {
		t.Fatalf("PR#114 R4 cache poisoning regression: @bob in same group must fan out "+
			"(alice's prior miss must not poison the channel-wide cache), got %d", got)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("PR#114 R4: expected exactly 1 dispatched copy for bob, got %d", len(fc.copies))
	}
	if cp := fc.copies[0]; cp.ChannelID != tBot {
		t.Fatalf("dispatched copy channel_id: want bot mailbox %q, got %q", tBot, cp.ChannelID)
	}
}
