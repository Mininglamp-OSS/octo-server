package messages_search

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
)

// buildBotAllowlist (#E / YUJ-52) is the as-bot counterpart of buildAllowlist:
// friends ∪ active groups ∪ threads under those groups, with the real-user-only
// Space + blacklist edges stripped. These tests pin the acceptance criteria:
//   - 命中集 = 好友 DM ∪ 群 ∪ 子区;
//   - 越界频道 (非好友 / 非活跃群成员) 不返回;
//   - bot 主体不查黑名单、不并 Space 成员;
//   - ExistMembersActive 失败 fail-closed (返回错误, 不降级为无门 allowlist).

// newBotAllowlistHandler wires the enumeration-capable stubs + a thread seam so
// buildBotAllowlist can be exercised without a real MySQL/OS dependency.
func newBotAllowlistHandler(gSvc group.IService, uSvc user.IService, threadsByGroup map[string][]string) *Handler {
	h := newHandlerForGlobalTests()
	h.groupService = gSvc
	h.userService = uSvc
	h.threadEnumFn = func(groupNos []string) (map[string][]string, error) {
		out := map[string][]string{}
		for _, gn := range groupNos {
			if sids, ok := threadsByGroup[gn]; ok {
				out[gn] = sids
			}
		}
		return out, nil
	}
	return h
}

// TestBuildBotAllowlist_FriendsGroupsThreads — the happy path: a bot that is an
// active member of one group (with a sub-thread) and friends with one peer sees
// exactly that group, that thread, and that DM. Nothing else.
func TestBuildBotAllowlist_FriendsGroupsThreads(t *testing.T) {
	const botUID = "bot9"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{botUID: {{GroupNo: "g1"}}},
		// nil activeGroupsForMember => every candidate group is active.
	}
	uSvc := &stubUserSvc{friends: []*user.FriendResp{{UID: "alice"}}}
	h := newBotAllowlistHandler(gSvc, uSvc, map[string][]string{"g1": {"t1"}})

	allowGroup, allowDM, allowThread, _, err := h.buildBotAllowlist(botUID)
	if err != nil {
		t.Fatalf("buildBotAllowlist: %v", err)
	}
	if len(allowGroup) != 1 || allowGroup[0].OSChannelID != "g1" || allowGroup[0].ChannelType != channelTypeGroup {
		t.Fatalf("expected group g1, got %+v", allowGroup)
	}
	if len(allowThread) != 1 || allowThread[0].OSChannelID != thread.BuildChannelID("g1", "t1") ||
		allowThread[0].ChannelType != channelTypeThread {
		t.Fatalf("expected thread g1/t1, got %+v", allowThread)
	}
	if len(allowDM) != 1 {
		t.Fatalf("expected exactly one DM peer, got %+v", allowDM)
	}
	if allowDM[0].WireID != "alice" || allowDM[0].ChannelType != channelTypePerson {
		t.Fatalf("expected DM peer alice, got %+v", allowDM[0])
	}
	// DM OS channelId must be the fake-channel id built from (botUID, peer), the
	// same shape resolveGlobalScope reverses back to the peer uid on the wire.
	if allowDM[0].OSChannelID != fakeChannelIDFor(botUID, "alice") {
		t.Fatalf("DM OS channelId mismatch: got %q", allowDM[0].OSChannelID)
	}
}

// TestBuildBotAllowlist_InactiveGroupDropped — a group the bot is NOT an active
// member of (kicked / group-blacklisted → status!=Normal, so ExistMembersActive
// excludes it) must not appear, and neither must its threads. 越界群不返回.
func TestBuildBotAllowlist_InactiveGroupDropped(t *testing.T) {
	const botUID = "bot9"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{botUID: {{GroupNo: "g1"}, {GroupNo: "g2"}}},
		// Only g1 is active for the bot; g2 was left/removed.
		activeGroupsForMember: map[string]map[string]bool{botUID: {"g1": true}},
	}
	uSvc := &stubUserSvc{}
	h := newBotAllowlistHandler(gSvc, uSvc, map[string][]string{"g1": {"t1"}, "g2": {"t2"}})

	allowGroup, _, allowThread, _, err := h.buildBotAllowlist(botUID)
	if err != nil {
		t.Fatalf("buildBotAllowlist: %v", err)
	}
	if len(allowGroup) != 1 || allowGroup[0].OSChannelID != "g1" {
		t.Fatalf("inactive group g2 must be dropped; got %+v", allowGroup)
	}
	// g2's thread must not leak through the thread enumeration either.
	for _, tr := range allowThread {
		if tr.OSChannelID == thread.BuildChannelID("g2", "t2") {
			t.Fatalf("thread under inactive group g2 leaked: %+v", allowThread)
		}
	}
	if len(allowThread) != 1 || allowThread[0].OSChannelID != thread.BuildChannelID("g1", "t1") {
		t.Fatalf("expected only g1's thread, got %+v", allowThread)
	}
}

// TestBuildBotAllowlist_NoBlacklistNoSpaceMembers — the bot subject skips the
// bidirectional blacklist gate AND the Space-member union entirely:
//   - a friend the bot has "blacklisted" (fixture) still appears (blacklistNone);
//   - the Space-member seam (spaceMembersFn) is never consulted;
//   - only friends surface as DM peers — no non-friend Space colleague.
func TestBuildBotAllowlist_NoBlacklistNoSpaceMembers(t *testing.T) {
	const botUID = "bot9"
	gSvc := &stubGroupSvc{}
	uSvc := &stubUserSvc{
		friends: []*user.FriendResp{{UID: "alice"}},
		// A blacklist edge in BOTH directions between bot and alice. The real-user
		// path would drop alice; the bot path must NOT consult this at all.
		blacklist: map[string]bool{
			botUID + "->alice": true,
			"alice->" + botUID: true,
		},
	}
	h := newBotAllowlistHandler(gSvc, uSvc, nil)
	// Guard: these real-user-only seams must never fire on the bot path.
	h.spaceMembersFn = func(string, string) ([]string, error) {
		t.Fatalf("bot allowlist must NOT union Space members")
		return nil, nil
	}
	h.dmBotFilterFn = func(string, []string) ([]string, error) {
		t.Fatalf("bot allowlist must NOT run the bot-in-Space filter")
		return nil, nil
	}

	_, allowDM, _, _, err := h.buildBotAllowlist(botUID)
	if err != nil {
		t.Fatalf("buildBotAllowlist: %v", err)
	}
	if len(allowDM) != 1 || allowDM[0].WireID != "alice" {
		t.Fatalf("blacklisted friend must still surface for a bot (blacklistNone); got %+v", allowDM)
	}
}

// TestBuildBotAllowlist_MemberActiveError_FailClosed — an ExistMembersActive
// failure must propagate as an error (fail-closed), NOT degrade to an un-gated
// group allowlist. Mirrors the real-user buildAllowlist policy.
func TestBuildBotAllowlist_MemberActiveError_FailClosed(t *testing.T) {
	const botUID = "bot9"
	gSvc := &stubGroupSvc{
		groupsByUID:              map[string][]*group.InfoResp{botUID: {{GroupNo: "g1"}}},
		activeGroupsForMemberErr: errors.New("db down"),
	}
	uSvc := &stubUserSvc{}
	h := newBotAllowlistHandler(gSvc, uSvc, nil)

	allowGroup, _, _, _, err := h.buildBotAllowlist(botUID)
	if err == nil {
		t.Fatalf("ExistMembersActive failure must fail closed with an error")
	}
	if allowGroup != nil {
		t.Fatalf("fail-closed must not return a degraded allowlist; got %+v", allowGroup)
	}
}

// TestBuildBotAllowlist_GroupsErrorFailClosed — GetGroupsWithMemberUID failure
// propagates (no silent empty allowlist that would look like "no rooms").
func TestBuildBotAllowlist_GroupsErrorFailClosed(t *testing.T) {
	gSvc := &stubGroupSvc{groupsByUIDErr: errors.New("db down")}
	h := newBotAllowlistHandler(gSvc, &stubUserSvc{}, nil)
	if _, _, _, _, err := h.buildBotAllowlist("bot9"); err == nil {
		t.Fatalf("GetGroupsWithMemberUID failure must propagate")
	}
}

// TestBuildBotAllowlist_FriendsErrorFailClosed — GetFriends failure propagates.
func TestBuildBotAllowlist_FriendsErrorFailClosed(t *testing.T) {
	uSvc := &stubUserSvc{friendsErr: errors.New("db down")}
	h := newBotAllowlistHandler(&stubGroupSvc{}, uSvc, nil)
	if _, _, _, _, err := h.buildBotAllowlist("bot9"); err == nil {
		t.Fatalf("GetFriends failure must propagate")
	}
}

// TestBuildBotAllowlist_EmptySubject — a blank botUID owns no channel; return an
// empty allowlist without touching any service (defensive fail-closed).
func TestBuildBotAllowlist_EmptySubject(t *testing.T) {
	gSvc := &stubGroupSvc{groupsByUIDErr: errors.New("must not be called")}
	uSvc := &stubUserSvc{friendsErr: errors.New("must not be called")}
	h := newBotAllowlistHandler(gSvc, uSvc, nil)
	allowGroup, allowDM, allowThread, _, err := h.buildBotAllowlist("")
	if err != nil {
		t.Fatalf("empty subject must not error: %v", err)
	}
	if len(allowGroup)+len(allowDM)+len(allowThread) != 0 {
		t.Fatalf("empty subject must yield an empty allowlist")
	}
}

// TestBuildBotAllowlist_SelfAndBlankPeersDropped — the bot never DMs itself, and
// blank / duplicate friend rows are collapsed.
func TestBuildBotAllowlist_SelfAndBlankPeersDropped(t *testing.T) {
	const botUID = "bot9"
	uSvc := &stubUserSvc{friends: []*user.FriendResp{
		{UID: botUID}, // self — drop
		{UID: ""},     // blank — drop
		{UID: "alice"},
		{UID: "alice"}, // dup — collapse
	}}
	h := newBotAllowlistHandler(&stubGroupSvc{}, uSvc, nil)
	_, allowDM, _, _, err := h.buildBotAllowlist(botUID)
	if err != nil {
		t.Fatalf("buildBotAllowlist: %v", err)
	}
	if len(allowDM) != 1 || allowDM[0].WireID != "alice" {
		t.Fatalf("self/blank/dup peers must be dropped; got %+v", allowDM)
	}
}

// TestBuildBotAllowlist_ThreadCapDowngrade — a group whose thread count exceeds
// maxThreadsPerGroup downgrades to "group only" (its own message hits survive,
// its thread hits are dropped) — identical to the real-user path since both go
// through enumerateThreadsForGroups. Confirms 超上限降级行为与真人一致.
func TestBuildBotAllowlist_ThreadCapDowngrade(t *testing.T) {
	const botUID = "bot9"
	overCap := make([]string, maxThreadsPerGroup+1)
	for i := range overCap {
		overCap[i] = "t" + itoaTest(i)
	}
	gSvc := &stubGroupSvc{groupsByUID: map[string][]*group.InfoResp{botUID: {{GroupNo: "g1"}}}}
	h := newBotAllowlistHandler(gSvc, &stubUserSvc{}, map[string][]string{"g1": overCap})

	allowGroup, _, allowThread, _, err := h.buildBotAllowlist(botUID)
	if err != nil {
		t.Fatalf("buildBotAllowlist: %v", err)
	}
	if len(allowGroup) != 1 || allowGroup[0].OSChannelID != "g1" {
		t.Fatalf("group must still surface after thread cap downgrade; got %+v", allowGroup)
	}
	if len(allowThread) != 0 {
		t.Fatalf("over-cap group must downgrade to group-only (no threads); got %d threads", len(allowThread))
	}
}

// itoaTest is a tiny local int→string helper so the cap test doesn't pull in
// strconv just for fixture ids.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
