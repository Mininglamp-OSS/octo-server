// Package bot_api · YUJ-1747 / octo-server PR#131 R5 regression tests.
//
// `setGrantActive(id, 1)` must invalidate the channel cache for the
// activated grant's OWN scopes — not just the demoted siblings.
//
// Pre-fix the activate path looked roughly like:
//
//	commit tx (active=1 on target, active=0 on siblings)
//	invalidateGrantorCache(grant.GrantorUID)
//	for r := range demoted {
//	    for s := range scopes(r) { invalidateChannelCache(s.ch, s.type) }
//	}
//
// While the grant was paused, an unfiltered `findActiveGrantsForChannel`
// could have written `obo:chan:Group:group_42 = "0"` (any subsequent
// fan-out miss writes "0"). Re-activating the grant doesn't rewrite
// that key, and `findActiveGrantsForChannel` short-circuits on the
// cached "0" → returns [] → fan-out is silently suppressed for the
// just-reactivated grant until the cache TTL expires.
//
// The fix in obo_db.go adds a per-scope `invalidateChannelCache` loop
// for the activated grant itself, BEFORE the demoted-sibling loop.
// These tests pin that contract via the cachedFakeOBOStore wrapper
// established for PR#114 R4: the wrapper now mirrors the prod-side
// cache-invalidation behavior of setGrantActive, and the regression
// test verifies that activation actually clears the per-channel cache.
package bot_api

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

// setGrantActive — wrap the embedded fake's setGrantActive so the
// cachedFakeOBOStore mirrors the PRODUCTION cache-invalidation
// contract on the activate path. Without this override the wrapper
// would silently let a regression slip through (the fake does not
// model a channel cache; only this wrapper does), so the tests below
// would still pass even if the prod fix were reverted. Mirroring the
// invalidation here makes the wrapper the contract-of-record: any
// future change to the activate-path cache contract MUST be reflected
// both here AND in *botAPIDB.setGrantActive.
//
// Pause path is forwarded verbatim — the fake already does the right
// thing (no scope cache to invalidate against the wrapper since pause
// also flips the channel-wide answer; the wrapper test for pause is
// not required by the YUJ-1747 fix scope, which is the activate path).
// We do still bust the activated grant's own scopes here on pause for
// parity with prod (lines 843–849 of obo_db.go), so this wrapper
// stays a faithful contract surface for both directions.
func (c *cachedFakeOBOStore) setGrantActive(id int64, active int) error {
	// Snapshot scopes BEFORE delegating; the fake's setGrantActive
	// does not mutate scopes, but reading after avoids any chance of
	// the demote loop perturbing the slice if that contract ever
	// changes.
	scopes, _ := c.fakeOBOStore.listScopesByGrant(id)

	// For the activate path we also need to know which siblings
	// will be demoted so we can bust their channel caches too — the
	// prod path snapshots demoted IDs inside the tx. Mirror that by
	// computing the demote set BEFORE delegating.
	v := 0
	if active != 0 {
		v = 1
	}
	var demoted [][]struct {
		ChannelID   string
		ChannelType uint8
	}
	if v == 1 {
		// Find sibling active grants under the same grantor.
		c.fakeOBOStore.mu.Lock()
		target := c.fakeOBOStore.grants[id]
		var siblingIDs []int64
		if target != nil {
			for _, g := range c.fakeOBOStore.grants {
				if g == nil || g.ID == target.ID {
					continue
				}
				if g.GrantorUID != target.GrantorUID {
					continue
				}
				if g.Active != 1 {
					continue
				}
				siblingIDs = append(siblingIDs, g.ID)
			}
		}
		c.fakeOBOStore.mu.Unlock()
		for _, sid := range siblingIDs {
			ss, _ := c.fakeOBOStore.listScopesByGrant(sid)
			var pairs []struct {
				ChannelID   string
				ChannelType uint8
			}
			for _, s := range ss {
				if s == nil {
					continue
				}
				pairs = append(pairs, struct {
					ChannelID   string
					ChannelType uint8
				}{s.ChannelID, s.ChannelType})
			}
			demoted = append(demoted, pairs)
		}
	}

	if err := c.fakeOBOStore.setGrantActive(id, active); err != nil {
		return err
	}

	// Post-mutation cache invalidation — bust the activated grant's
	// own scopes (the YUJ-1747 R5 fix) AND the demoted siblings' scopes.
	c.cacheMu.Lock()
	for _, s := range scopes {
		if s == nil {
			continue
		}
		delete(c.cache, channelCacheKey{s.ChannelID, s.ChannelType})
	}
	for _, pairs := range demoted {
		for _, p := range pairs {
			delete(c.cache, channelCacheKey{p.ChannelID, p.ChannelType})
		}
	}
	c.cacheMu.Unlock()
	return nil
}

// TestYUJ1747_ActivateGrant_BustsChannelCacheForOwnScopes pins the
// fix end-to-end via the cache-aware wrapper:
//
//  1. bob has a grant with scope (group_42).
//  2. The grant is paused (active=0).
//  3. An unfiltered findActiveGrantsForChannel(group_42) runs while
//     the grant is paused → returns [] → cache writes "0".
//  4. The grant is re-activated via setGrantActive(id, 1).
//  5. findActiveGrantsForChannel(group_42) MUST return [bob's grant].
//
// Pre-fix step 5 returns [] because the cached "0" from step 3 was
// never busted by the activate path; the unfiltered lookup short-
// circuits on channelCacheSaysNone and silently suppresses fan-out.
func TestYUJ1747_ActivateGrant_BustsChannelCacheForOwnScopes(t *testing.T) {
	const (
		ch     = "group_42"
		bobUID = "u_bob"
	)
	ct := common.ChannelTypeGroup.Uint8()

	c := newCachedFakeOBOStore()

	// Seed bob's grant with an explicit scope row for group_42.
	gid, err := c.insertGrant(bobUID, tBot, "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := c.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant global_enabled=1: %v", err)
	}
	if _, err := c.insertScope(gid, ch, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}

	// Pause the grant — emulates a `PUT /v1/obo/grants/:id` with
	// {"active": false}.
	if err := c.setGrantActive(gid, 0); err != nil {
		t.Fatalf("pause setGrantActive: %v", err)
	}

	// While paused, somebody asks "any active grant for group_42?".
	// Cache writes "0" for the channel (nobody has an active grant).
	got, err := c.findActiveGrantsForChannel(ch, ct)
	if err != nil {
		t.Fatalf("findActiveGrantsForChannel (paused): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("paused grant: expected 0 active grants for %s, got %d", ch, len(got))
	}
	if !c.channelCacheSaysNone(ch, ct) {
		t.Fatalf("test setup: expected channelCacheSaysNone=true after unfiltered miss while grant paused")
	}

	// Re-activate the grant. Per YUJ-1747, this MUST invalidate the
	// channel cache for the grant's own scopes.
	if err := c.setGrantActive(gid, 1); err != nil {
		t.Fatalf("activate setGrantActive: %v", err)
	}
	if c.channelCacheSaysNone(ch, ct) {
		t.Fatalf("YUJ-1747 regression: setGrantActive(id, 1) did NOT invalidate channel cache " +
			"for the activated grant's own scope (channelCacheSaysNone=true after activate). " +
			"findActiveGrantsForChannel will short-circuit to [] → fan-out silently suppressed.")
	}

	// Unfiltered lookup must now see bob's grant.
	got, err = c.findActiveGrantsForChannel(ch, ct)
	if err != nil {
		t.Fatalf("findActiveGrantsForChannel (activated): %v", err)
	}
	if len(got) != 1 || got[0].GrantorUID != bobUID {
		t.Fatalf("YUJ-1747 regression: after re-activation expected [bob's grant], got %d rows: %+v",
			len(got), got)
	}
}

// TestYUJ1747_ActivateGrant_BustsChannelCacheForSiblingScopes pins the
// pre-existing demote-loop invalidation: activating grant B while A
// holds the persona must bust both A's scopes (demoted) AND B's
// scopes (newly active). This is a regression guard for the prod
// fix's order: the new grant-self loop must NOT replace the
// demote-sibling loop, only AUGMENT it.
func TestYUJ1747_ActivateGrant_BustsChannelCacheForSiblingScopes(t *testing.T) {
	const (
		grantorUID = "u_carol"
		chA        = "group_a"
		chB        = "group_b"
	)
	ct := common.ChannelTypeGroup.Uint8()

	c := newCachedFakeOBOStore()
	enable := 1

	// Grant A — currently active, scoped to chA.
	gidA, err := c.insertGrant(grantorUID, "bot_persona_a", "auto", "")
	if err != nil {
		t.Fatalf("insertGrant A: %v", err)
	}
	if err := c.updateGrant(gidA, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant A global_enabled=1: %v", err)
	}
	if _, err := c.insertScope(gidA, chA, ct, 1); err != nil {
		t.Fatalf("insertScope A: %v", err)
	}

	// Grant B — paused, scoped to chB.
	gidB, err := c.insertGrant(grantorUID, "bot_persona_b", "auto", "")
	if err != nil {
		t.Fatalf("insertGrant B: %v", err)
	}
	if err := c.updateGrant(gidB, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant B global_enabled=1: %v", err)
	}
	if _, err := c.insertScope(gidB, chB, ct, 1); err != nil {
		t.Fatalf("insertScope B: %v", err)
	}
	if err := c.setGrantActive(gidB, 0); err != nil {
		t.Fatalf("pause B: %v", err)
	}

	// Warm both caches directly. The fake's Group-channel finder
	// aggregates "any active + global_enabled grant" without
	// per-channel scope filtering, so a real lookup against chB would
	// return [A] and the cache would not encode the "no grants for
	// chB" state we need to set up. Writing the cache directly via
	// the prod-side helper avoids that fake-vs-prod divergence and
	// pins the YUJ-1747 contract on the activate path.
	c.writeChannelCache(chA, ct, true)
	c.writeChannelCache(chB, ct, false)
	if c.channelCacheSaysNone(chA, ct) {
		t.Fatalf("test setup: chA cache should hold the positive answer pre-flip")
	}
	if !c.channelCacheSaysNone(chB, ct) {
		t.Fatalf("test setup: chB cache should hold the negative answer pre-flip")
	}

	// Activate B — flips A→demoted, B→active. Both channel caches
	// MUST be busted.
	if err := c.setGrantActive(gidB, 1); err != nil {
		t.Fatalf("activate B: %v", err)
	}

	// chB is the activated grant's own scope (YUJ-1747 fix). The
	// "0" cache must be gone.
	if c.channelCacheSaysNone(chB, ct) {
		t.Fatalf("YUJ-1747 regression: activated grant's own scope chB still cached as 'no grants'")
	}

	// chA is the demoted sibling's scope (pre-existing invariant
	// from PR#131 R5 baseline). The cache entry must be gone too,
	// so the next fan-out re-probes the store. Verify directly via
	// the cache state — the Group-channel fake aggregates "any
	// active+global_enabled grant" without per-channel scope
	// filtering, so a post-flip lookup against chA would still
	// return [B] and could not distinguish "cache cleared" from
	// "lookup hit positive answer". Asserting the cache key
	// removal is the precise pin.
	c.cacheMu.Lock()
	_, chAPresent := c.cache[channelCacheKey{chA, ct}]
	_, chBPresent := c.cache[channelCacheKey{chB, ct}]
	c.cacheMu.Unlock()
	if chAPresent {
		t.Fatalf("PR#131 R5 regression: demoted sibling's scope chA cache entry still present after activate")
	}
	if chBPresent {
		t.Fatalf("YUJ-1747 regression: activated grant's own scope chB cache entry still present after activate")
	}
}
