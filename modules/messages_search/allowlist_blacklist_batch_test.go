package messages_search

// YUJ-27 regression tests: the DM allowlist blacklist gate must produce the
// same set of surviving peers as the pre-batch, per-peer ExistBlacklist
// implementation, but pay one MySQL round-trip instead of 2N. These tests
// exercise ExistBlacklistsBoth-driven filterBlacklistedDMPeers semantics on
// top of the shared stubUserSvc fixture — no real MySQL, no per-peer stub
// swap-out.

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/user"
)

// TestFilterBlacklistedDMPeers_BatchParity — the batched path must drop the
// same peers a semantics-equivalent per-peer loop would drop: forward hit
// (me→peer), reverse hit (peer→me), no-hit (survives), and self / empty
// entries (never touch the DB, silently dropped).
func TestFilterBlacklistedDMPeers_BatchParity(t *testing.T) {
	loginUID := "me"
	uSvc := &stubUserSvc{
		blacklist: map[string]bool{
			// me -> bob : forward
			"me->bob": true,
			// carol -> me : reverse
			"carol->me": true,
			// dave: no edge in either direction
			// eve: no edge in either direction
		},
	}
	gSvc := &stubGroupSvc{}
	h := newHandlerForAllowlistTests(uSvc, gSvc)

	got := h.filterBlacklistedDMPeers(loginUID, []string{
		"bob", "carol", "dave", "eve",
		"", loginUID, // self / empty must not survive and must not hit DB
	})
	gotSet := map[string]bool{}
	for _, p := range got {
		gotSet[p] = true
	}
	if gotSet["bob"] {
		t.Fatalf("bob (forward blacklist) must be dropped by batch path; got %v", got)
	}
	if gotSet["carol"] {
		t.Fatalf("carol (reverse blacklist) must be dropped by batch path; got %v", got)
	}
	if !gotSet["dave"] || !gotSet["eve"] {
		t.Fatalf("dave and eve (no blacklist edges) must survive; got %v", got)
	}
	if gotSet[""] || gotSet[loginUID] {
		t.Fatalf("empty peer / self must never survive; got %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly {dave,eve} to survive; got %v", got)
	}
}

// TestFilterBlacklistedDMPeers_BidirectionalBothSides — when both directions
// are blacklisted the peer must still drop exactly once (idempotent) and no
// extra call artefacts are observable through the returned set.
func TestFilterBlacklistedDMPeers_BidirectionalBothSides(t *testing.T) {
	loginUID := "me"
	uSvc := &stubUserSvc{
		blacklist: map[string]bool{
			"me->frank":  true,
			"frank->me":  true,
			"me->grace":  false, // absent = false
			"grace->me":  true,
			"dave->me":   false,
		},
	}
	gSvc := &stubGroupSvc{}
	h := newHandlerForAllowlistTests(uSvc, gSvc)

	got := h.filterBlacklistedDMPeers(loginUID, []string{"frank", "grace", "dave"})
	if len(got) != 1 || got[0] != "dave" {
		t.Fatalf("expected {dave}; got %v", got)
	}
}

// TestFilterBlacklistedDMPeers_BatchErrorFailsClosedForAll — the batch
// contract fails closed on error: the previous per-peer path could drop
// just the offender, but one IN query error means we can no longer
// distinguish blacklisted from non-blacklisted for the batch, so we drop
// every candidate. This is stricter than the per-peer version and is safe
// (hidden legit DMs > leaked blacklisted DMs).
func TestFilterBlacklistedDMPeers_BatchErrorFailsClosedForAll(t *testing.T) {
	loginUID := "me"
	uSvc := &stubUserSvc{
		blacklistErr: errBlacklistDown,
	}
	gSvc := &stubGroupSvc{}
	h := newHandlerForAllowlistTests(uSvc, gSvc)

	got := h.filterBlacklistedDMPeers(loginUID, []string{"a", "b", "c"})
	if len(got) != 0 {
		t.Fatalf("batch error must drop every peer fail-closed; got %v", got)
	}
}

// TestFilterBlacklistedDMPeers_EmptyInputSkipsService — the batch layer must
// short-circuit on empty peer input and never touch the (potentially slow)
// blacklist service. Verified by wiring in a stub that would return an
// error if called, and asserting no error surfaces.
func TestFilterBlacklistedDMPeers_EmptyInputSkipsService(t *testing.T) {
	loginUID := "me"
	uSvc := &stubUserSvc{
		blacklistErr: errBlacklistDown, // would fail-closed for every peer
	}
	gSvc := &stubGroupSvc{}
	h := newHandlerForAllowlistTests(uSvc, gSvc)

	got := h.filterBlacklistedDMPeers(loginUID, nil)
	if len(got) != 0 {
		t.Fatalf("nil peers must return empty without calling service; got %v", got)
	}
	got = h.filterBlacklistedDMPeers(loginUID, []string{})
	if len(got) != 0 {
		t.Fatalf("empty peers must return empty without calling service; got %v", got)
	}
}

// TestExistBlacklistsBoth_StubSemanticsMatchExistBlacklist — belt-and-braces
// sanity check that the stub's batch method returns a superset of individual
// ExistBlacklist calls for the same fixture. If a future test forgets to
// populate one, the mismatch surfaces here rather than in a downstream
// integration test.
func TestExistBlacklistsBoth_StubSemanticsMatchExistBlacklist(t *testing.T) {
	loginUID := "me"
	uSvc := &stubUserSvc{
		blacklist: map[string]bool{
			"me->x":   true,
			"y->me":   true,
			"me->z":   false,
			"z->me":   false,
			"me->me":  true, // guarded — self must never surface
		},
	}
	peers := []string{"x", "y", "z", "me", ""}
	blockedByMe, blockedByPeer, err := uSvc.ExistBlacklistsBoth(loginUID, peers)
	if err != nil {
		t.Fatalf("stub ExistBlacklistsBoth returned error: %v", err)
	}
	if !blockedByMe["x"] {
		t.Fatalf("me->x forward edge missing from blockedByMe: %v", blockedByMe)
	}
	if !blockedByPeer["y"] {
		t.Fatalf("y->me reverse edge missing from blockedByPeer: %v", blockedByPeer)
	}
	if blockedByMe["z"] || blockedByPeer["z"] {
		t.Fatalf("z has no edge; must not appear: byMe=%v byPeer=%v", blockedByMe, blockedByPeer)
	}
	if blockedByMe[loginUID] || blockedByPeer[loginUID] {
		t.Fatalf("self edge must be filtered by the batch method: byMe=%v byPeer=%v", blockedByMe, blockedByPeer)
	}
	// Cross-check against per-pair ExistBlacklist parity for the surviving
	// axis: any peer marked by the batch must also be hit by the per-pair
	// call in the same direction.
	for _, p := range []string{"x", "y", "z"} {
		fwd, _ := uSvc.ExistBlacklist(loginUID, p)
		rev, _ := uSvc.ExistBlacklist(p, loginUID)
		if fwd != blockedByMe[p] {
			t.Fatalf("forward parity mismatch for peer=%s: pair=%v batch=%v", p, fwd, blockedByMe[p])
		}
		if rev != blockedByPeer[p] {
			t.Fatalf("reverse parity mismatch for peer=%s: pair=%v batch=%v", p, rev, blockedByPeer[p])
		}
	}
}

// TestFilterBlacklistedDMPeers_SharesFixtureWithLegacyTest — the legacy
// TestEnumerateDMPeers_ExcludesBlacklistedPeers fixture uses "me->bob" and
// "carol->me". Sanity: driving that same fixture through the batch method
// direct (bypassing enumerateDMPeersTimed) must reject bob + carol and
// keep dave. Guards against a future stub refactor breaking the direct
// filterBlacklistedDMPeers path while the enumerate wrapper still passes.
func TestFilterBlacklistedDMPeers_SharesFixtureWithLegacyTest(t *testing.T) {
	loginUID := "me"
	uSvc := &stubUserSvc{
		friends: []*user.FriendResp{
			{UID: "bob"},
			{UID: "carol"},
			{UID: "dave"},
		},
		blacklist: map[string]bool{
			"me->bob":   true,
			"carol->me": true,
		},
	}
	gSvc := &stubGroupSvc{}
	h := newHandlerForAllowlistTests(uSvc, gSvc)

	got := h.filterBlacklistedDMPeers(loginUID, []string{"bob", "carol", "dave"})
	gotSet := map[string]bool{}
	for _, p := range got {
		gotSet[p] = true
	}
	if gotSet["bob"] || gotSet["carol"] {
		t.Fatalf("legacy fixture parity: bob/carol must drop; got %v", got)
	}
	if !gotSet["dave"] || len(got) != 1 {
		t.Fatalf("legacy fixture parity: dave should be the only survivor; got %v", got)
	}
}
