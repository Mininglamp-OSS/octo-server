// Package bot_api · YUJ-1166 — PR#82 R6 P0 friend-gate OBO bypass tests.
//
// The fixture matrix covers the four states the gate can resolve to:
//
//	|                          | bot↔target friends | not friends |
//	|--------------------------|--------------------|-------------|
//	| no OBO grant covers      |          ALLOW     |    DENY     |
//	| active OBO grant covers  |          ALLOW     |    ALLOW    |  ← bypass
//	| stale OBO scope (kicked) |          ALLOW     |    DENY     |  ← TOCTOU
//
// The "ALLOW (bypass)" cell is the new R6 P0 behaviour. The TOCTOU row
// asserts the bypass closes on grantor access loss — same invariant
// checkOBO enforces on the send hot path (PR#82 round-2 P1-A).
//
// We test the helper directly (hasOBOAccessToChannel +
// isFriendOrOBOBypass) so the test surface is decoupled from the HTTP
// handler plumbing. A separate integration test in
// obo_send_test.go would re-verify the full sendMessage / typing /
// readReceipt path; here we lock in the gate logic itself.
package bot_api

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// newBAforFriendGate wires a BotAPI minimally for the friend-gate
// helper tests: in-memory oboStore + injectable friend / channel-access
// overrides. No userService / DB needed.
func newBAforFriendGate(s *fakeOBOStore) *BotAPI {
	return &BotAPI{
		Log:              log.NewTLog("BotAPI-friend-gate-test"),
		oboStoreOverride: s,
		// Default channel-access override: grantor still has access.
		// Individual tests override this for the TOCTOU case.
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return true, nil
		},
	}
}

// TestHasOBOAccessToChannel_NoGrants — common case: nothing covers this
// (bot, channel) pair → no bypass. The negative-cache friendly path
// the production deployment is optimised for.
func TestHasOBOAccessToChannel_NoGrants(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforFriendGate(s)

	ok, err := ba.hasOBOAccessToChannel("bot_clone", "u_bob", common.ChannelTypePerson.Uint8())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("no grant covers (bot_clone, u_bob); bypass must NOT apply")
	}
}

// TestHasOBOAccessToChannel_DM_GrantorHasFriendship_ApplyBypass —
// PR#82 R6 P0 Bug B happy path. admin grants OBO to james with
// scope=peer=u_bob; admin is friends with u_bob; james must therefore
// be allowed to send/typing/readReceipt to u_bob via the bypass even
// though james↔u_bob is NOT a friend pair.
func TestHasOBOAccessToChannel_DM_GrantorHasFriendship_ApplyBypass(t *testing.T) {
	const (
		admin   = "user_admin"
		bot     = "bot_clone_james"
		peer    = "u_bob"
	)
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, err := s.insertGrant(admin, bot, "auto")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}
	if _, err := s.insertScope(gid, peer, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	ba := newBAforFriendGate(s)
	// Verify the access check is queried with grantor=admin and
	// channel_id=peer (i.e. the function passes through the right
	// frame of reference, not some bot-keyed nonsense).
	calls := 0
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		calls++
		if uid != admin || channelID != peer || channelType != ct {
			t.Errorf("access override called with wrong args: uid=%q chan=%q type=%d (want admin=%q peer=%q)", uid, channelID, channelType, admin, peer)
		}
		return true, nil
	}

	ok, err := ba.hasOBOAccessToChannel(bot, peer, ct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("admin grants OBO + admin friends with peer → bypass must apply")
	}
	if calls != 1 {
		t.Fatalf("expected one access re-check (per matching grant), got %d", calls)
	}
}

// TestHasOBOAccessToChannel_DM_GrantorLostAccess_NoBypass — TOCTOU
// regression. The scope row is still on file, but the grantor is no
// longer friends with the peer. Bypass must NOT apply — otherwise the
// bot keeps reaching a user the grantor can no longer themselves
// reach, defeating the friend-removal at the IM layer.
func TestHasOBOAccessToChannel_DM_GrantorLostAccess_NoBypass(t *testing.T) {
	const (
		admin = "user_admin"
		bot   = "bot_clone_james"
		peer  = "u_bob"
	)
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(admin, bot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	if _, err := s.insertScope(gid, peer, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	ba := newBAforFriendGate(s)
	// admin has lost friend with peer → access check denies.
	ba.oboChannelAccessOverride = func(uid, channelID string, channelType uint8) (bool, error) {
		return false, nil
	}

	ok, err := ba.hasOBOAccessToChannel(bot, peer, ct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("grantor lost access → bypass must NOT apply (TOCTOU close-out)")
	}
}

// TestHasOBOAccessToChannel_GrantForDifferentBot_NoBypass — a grant
// covering (peer) exists but for a DIFFERENT grantee bot. The current
// bot must not piggy-back on someone else's grant.
func TestHasOBOAccessToChannel_GrantForDifferentBot_NoBypass(t *testing.T) {
	const (
		admin    = "user_admin"
		otherBot = "bot_clone_other"
		thisBot  = "bot_clone_james"
		peer     = "u_bob"
	)
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(admin, otherBot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	if _, err := s.insertScope(gid, peer, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	ba := newBAforFriendGate(s)

	ok, err := ba.hasOBOAccessToChannel(thisBot, peer, ct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("grant is for other bot, not this bot → bypass must NOT apply")
	}
}

// TestHasOBOAccessToChannel_GrantInactive_NoBypass — a soft-deleted
// (revoked) grant must not authorize the bypass even if its scope row
// still happens to exist.
func TestHasOBOAccessToChannel_GrantInactive_NoBypass(t *testing.T) {
	const (
		admin = "user_admin"
		bot   = "bot_clone_james"
		peer  = "u_bob"
	)
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(admin, bot, "auto")
	// NOTE: NOT enabling globally — the grant stays at global_enabled=0.
	// (findActiveGrantsForChannel filters on active=1 AND global_enabled=1,
	//  so this is the "grant exists but not switched on" case.)
	if _, err := s.insertScope(gid, peer, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	ba := newBAforFriendGate(s)

	ok, err := ba.hasOBOAccessToChannel(bot, peer, ct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("grant not globally enabled → bypass must NOT apply")
	}
}

// TestHasOBOAccessToChannel_Group_BypassApplies — the OBO bypass is
// not DM-only. A bot scoped to a group via OBO must be able to
// send/typing in that group via the bypass. The membership check the
// non-OBO BotKindUser group branch enforces is orthogonal — the OBO
// bypass is specifically for the friend gate, which fires on the DM
// branch.  This test exercises the helper with a group channel to lock
// in that the scope/access lookup is type-agnostic at the helper layer.
func TestHasOBOAccessToChannel_Group_BypassApplies(t *testing.T) {
	const (
		admin = "user_admin"
		bot   = "bot_clone_james"
		group = "group_42"
	)
	ct := common.ChannelTypeGroup.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(admin, bot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	if _, err := s.insertScope(gid, group, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	ba := newBAforFriendGate(s)

	ok, err := ba.hasOBOAccessToChannel(bot, group, ct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("group-scoped OBO grant → bypass should apply at helper layer")
	}
}

// TestHasOBOAccessToChannel_StoreErrorFailsClosed — DB / cache failure
// on findActiveGrantsForChannel must surface as (false, err) — the
// caller (isFriendOrOBOBypass) then treats it as no bypass, preserving
// the legacy "not a friend" error rather than widening access.
func TestHasOBOAccessToChannel_StoreErrorFailsClosed(t *testing.T) {
	s := newFakeOBOStore()
	s.failFindGrantsChannel = errors.New("connection refused")
	ba := newBAforFriendGate(s)

	ok, err := ba.hasOBOAccessToChannel("bot_clone", "u_bob", common.ChannelTypePerson.Uint8())
	if err == nil {
		t.Fatalf("expected error to surface from store")
	}
	if ok {
		t.Fatalf("store error must NOT widen access — got ok=true")
	}
}

// TestIsFriendOrOBOBypass_FriendsShortCircuits — when the bot↔user
// friend lookup says "yes", the OBO bypass must NOT be consulted at
// all. The bypass is the slow path (one extra Redis + DB hop in
// production); for the common "bot is a friend" case we should not
// pay that cost.
func TestIsFriendOrOBOBypass_FriendsShortCircuits(t *testing.T) {
	s := newFakeOBOStore()
	// Seed a grant + scope that WOULD allow bypass — but we won't get
	// there because the friend lookup short-circuits.
	gid, _ := s.insertGrant("user_admin", "bot_clone", "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	_, _ = s.insertScope(gid, "u_bob", common.ChannelTypePerson.Uint8(), 1)

	// If the bypass is consulted, the access override would fire and
	// we'd notice. Set it to a sentinel that the test trips on.
	bypassCalled := false
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-friend-shortcircuit-test"),
		oboStoreOverride: s,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			bypassCalled = true
			return true, nil
		},
		friendCheckOverride: func(uid, toUID string) (bool, error) {
			return true, nil // legit friend → must short-circuit
		},
	}

	ok, err := ba.isFriendOrOBOBypass("bot_clone", "u_bob", common.ChannelTypePerson.Uint8(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("friend lookup said yes; gate must allow")
	}
	if bypassCalled {
		t.Fatalf("friend path is the fast path; OBO bypass must NOT be consulted when IsFriend=true")
	}
}

// TestIsFriendOrOBOBypass_NotFriendsButOBOApplies — PR#82 R6 P0 Bug B
// — the headline regression. bot↔user are NOT friends; OBO grant +
// scope are in place; bypass kicks in → gate allows the operation.
// This is the case james surfaced in im-test 2026-05-19.
func TestIsFriendOrOBOBypass_NotFriendsButOBOApplies(t *testing.T) {
	const (
		admin = "user_admin"
		bot   = "bot_clone_james"
		peer  = "u_bob"
	)
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(admin, bot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	_, _ = s.insertScope(gid, peer, ct, 1)

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-friend-bypass-test"),
		oboStoreOverride: s,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			// admin has friend with peer (the grantor IS the original
			// counterparty in the DM admin↔bob conversation).
			return uid == admin && channelID == peer, nil
		},
		friendCheckOverride: func(uid, toUID string) (bool, error) {
			// bot↔peer are NOT friends (the broken state in im-test
			// before SQL-adding rows, and the correct state we no
			// longer require).
			return false, nil
		},
	}

	ok, err := ba.isFriendOrOBOBypass(bot, peer, ct, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("PR#82 R6 P0 BUG B: managed-persona OBO friend-gate bypass did not apply (bot=%q peer=%q grantor=%q) — adapter would fail with 'bot is not a friend of this user'", bot, peer, admin)
	}
}

// TestIsFriendOrOBOBypass_NotFriendsNoOBO_Denies — the classic
// "bot not in friend list, no OBO either" denial case. Locks in that
// the bypass is additive only and doesn't change behaviour for bots
// without an OBO grant.
func TestIsFriendOrOBOBypass_NotFriendsNoOBO_Denies(t *testing.T) {
	s := newFakeOBOStore() // empty
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-friend-deny-test"),
		oboStoreOverride: s,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return true, nil // would allow if reached, but lookup returns 0 grants
		},
		friendCheckOverride: func(uid, toUID string) (bool, error) {
			return false, nil
		},
	}

	ok, err := ba.isFriendOrOBOBypass("bot_clone", "u_bob", common.ChannelTypePerson.Uint8(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("no friend AND no OBO grant → must deny (additive bypass shouldn't change legacy deny)")
	}
}

// TestIsFriendOrOBOBypass_FriendErrorSurfaces — IsFriend errors must
// propagate (caller maps to "查询好友关系失败"). Bypass is only
// consulted when IsFriend cleanly returns false.
func TestIsFriendOrOBOBypass_FriendErrorSurfaces(t *testing.T) {
	boom := errors.New("connection refused")
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-friend-err-test"),
		oboStoreOverride: newFakeOBOStore(),
		friendCheckOverride: func(uid, toUID string) (bool, error) {
			return false, boom
		},
	}

	ok, err := ba.isFriendOrOBOBypass("bot_clone", "u_bob", common.ChannelTypePerson.Uint8(), true)
	if !errors.Is(err, boom) {
		t.Fatalf("IsFriend error must propagate; got err=%v ok=%v", err, ok)
	}
	if ok {
		t.Fatalf("IsFriend error must NOT silently allow")
	}
}

// TestBotAPI_FriendGate_BypassedForOBOContext — issue-named regression
// per PR-A R6 P0 spec. Alias of the headline bypass test above with
// the name the issue body asks for, so a future search for the issue
// number finds the test directly.
func TestBotAPI_FriendGate_BypassedForOBOContext(t *testing.T) {
	TestIsFriendOrOBOBypass_NotFriendsButOBOApplies(t)
}

// TestIsFriendOrOBOBypass_NoOBOContext_GrantsIgnored — PR#82 R7
// regression (Jerry-Xin head a07b372). Bot is NOT friend of peer, an
// active OBO grant covers (bot, peer) — but the caller passes
// hasOBOContext=false (e.g. sendMessage without `on_behalf_of`, or
// typing / readReceipt / messages-sync which have no on_behalf_of
// field at all). The bypass MUST NOT fire — otherwise the bot could
// reach peer directly bot→peer, defeating the user opt-in friend
// gate and exposing the managed-persona bot as a contact.
func TestIsFriendOrOBOBypass_NoOBOContext_GrantsIgnored(t *testing.T) {
	const (
		admin = "user_admin"
		bot   = "bot_clone_james"
		peer  = "u_bob"
	)
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(admin, bot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	_, _ = s.insertScope(gid, peer, ct, 1)

	bypassConsulted := false
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-r7-no-ctx-test"),
		oboStoreOverride: s,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			// If this fires the bypass was reached — which is exactly
			// the bug we are guarding against.
			bypassConsulted = true
			return true, nil
		},
		friendCheckOverride: func(uid, toUID string) (bool, error) {
			return false, nil // bot is NOT friends with peer
		},
	}

	ok, err := ba.isFriendOrOBOBypass(bot, peer, ct, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("PR#82 R7: hasOBOContext=false MUST NOT permit bypass even when a grant exists — bot would reach peer without opt-in")
	}
	if bypassConsulted {
		t.Fatalf("PR#82 R7: hasOBOAccessToChannel must NOT be consulted when hasOBOContext=false (perf + invariant)")
	}
}

// TestIsFriendOrOBOBypass_OBOContextTrue_BypassApplies — companion of
// the negative test above. Same fixture (bot not friend, grant
// exists) but hasOBOContext=true → bypass fires. Locks in that the
// only delta between allow and deny is the caller's OBO-context flag.
func TestIsFriendOrOBOBypass_OBOContextTrue_BypassApplies(t *testing.T) {
	const (
		admin = "user_admin"
		bot   = "bot_clone_james"
		peer  = "u_bob"
	)
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(admin, bot, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	_, _ = s.insertScope(gid, peer, ct, 1)

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-r7-with-ctx-test"),
		oboStoreOverride: s,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return uid == admin && channelID == peer, nil
		},
		friendCheckOverride: func(uid, toUID string) (bool, error) {
			return false, nil
		},
	}

	ok, err := ba.isFriendOrOBOBypass(bot, peer, ct, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("hasOBOContext=true + valid grant → bypass must fire (parity with R6 P0 fix)")
	}
}
