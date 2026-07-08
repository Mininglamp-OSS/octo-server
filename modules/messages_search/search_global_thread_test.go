package messages_search

import (
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
)

// YUJ-10 · v1 thread coverage on the global endpoints.
//
// buildAllowlist / enumerateThreadsForGroups now surface active threads as
// composite `{groupNo}____{shortID}` channelIds alongside groups + DMs, with
// soft-fail on DB errors and two hard caps (per-group + global aggregate).
// These tests exercise every branch of that path without touching a real
// MySQL / OpenSearch instance.

// newAllowlistHandler wires the minimal Handler surface needed by
// buildAllowlist + enumerateThreadsForGroups. threadEnumFn is left nil per
// test so each case decides what the "DB" returns.
func newAllowlistHandler(t *testing.T, gSvc group.IService, uSvc user.IService) *Handler {
	t.Helper()
	h := &Handler{
		Log:          log.NewTLog("messages_search-thread-test"),
		cfg:          SearchConfig{},
		userService:  uSvc,
		groupService: gSvc,
		cache:        newSenderCache(4, 0),
	}
	// External-group / space_member / bot lookups are unused by these tests
	// but the buildAllowlist helper still walks them. Space-related stubs
	// short-circuit to empty so buildAllowlist stays local.
	h.spaceMembersFn = func(string, string) ([]string, error) { return nil, nil }
	h.dmBotFilterFn = func(_ string, peers []string) ([]string, error) { return peers, nil }
	h.externalGroupFn = func(string) (map[string]string, error) { return map[string]string{}, nil }
	return h
}

// TestBuildAllowlist_IncludesThreadChannelIDs — the load-bearing YUJ-10
// assertion: threads under joined groups appear in the flat allowlist with
// channelType=5 and the composite `{groupNo}____{shortID}` channelId.
func TestBuildAllowlist_IncludesThreadChannelIDs(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {
				{GroupNo: "grpA", SpaceID: ""},
				{GroupNo: "grpB", SpaceID: ""},
			},
		},
	}
	uSvc := &stubUserSvc{friends: []*user.FriendResp{}}
	h := newAllowlistHandler(t, gSvc, uSvc)
	h.threadEnumFn = func(groupNos []string) (map[string][]string, error) {
		// Sanity: caller passes exactly the joined groups.
		got := map[string]bool{}
		for _, gn := range groupNos {
			got[gn] = true
		}
		if !got["grpA"] || !got["grpB"] {
			t.Fatalf("threadEnumFn must receive joined groups; got %v", groupNos)
		}
		return map[string][]string{
			"grpA": {"thr1", "thr2"},
			"grpB": {"thr3"},
		}, nil
	}

	allowGroup, _, allowThread, err := h.buildAllowlist(nil, loginUID, "")
	if err != nil {
		t.Fatalf("buildAllowlist: %v", err)
	}
	if len(allowGroup) != 2 {
		t.Fatalf("group allowlist should have 2 entries, got %d", len(allowGroup))
	}
	want := map[string]bool{
		thread.BuildChannelID("grpA", "thr1"): true,
		thread.BuildChannelID("grpA", "thr2"): true,
		thread.BuildChannelID("grpB", "thr3"): true,
	}
	if len(allowThread) != len(want) {
		t.Fatalf("thread allowlist should have %d entries, got %d (%+v)",
			len(want), len(allowThread), allowThread)
	}
	for _, r := range allowThread {
		if !want[r.OSChannelID] {
			t.Errorf("unexpected thread channelId %q", r.OSChannelID)
		}
		if r.ChannelType != channelTypeThread {
			t.Errorf("thread channelRef must have channelType=5, got %d", r.ChannelType)
		}
		if r.WireID != r.OSChannelID {
			// Thread channelId is echoed to the wire verbatim (no reversal).
			t.Errorf("thread WireID must equal OSChannelID; got %q vs %q", r.WireID, r.OSChannelID)
		}
	}
}

// TestBuildAllowlist_ThreadEnumerateSoftFail — a DB error inside
// QueryActiveShortIDsByGroupNos must NOT sink the whole request; the
// group + DM parts still populate.
func TestBuildAllowlist_ThreadEnumerateSoftFail(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}},
		},
	}
	uSvc := &stubUserSvc{friends: []*user.FriendResp{{UID: "friend1"}}}
	h := newAllowlistHandler(t, gSvc, uSvc)
	h.threadEnumFn = func([]string) (map[string][]string, error) {
		return nil, errors.New("mysql: connection refused")
	}

	allowGroup, allowDM, allowThread, err := h.buildAllowlist(nil, loginUID, "")
	if err != nil {
		t.Fatalf("thread DB error must NOT propagate; got %v", err)
	}
	if len(allowGroup) != 1 {
		t.Errorf("group allowlist must survive thread failure; got %+v", allowGroup)
	}
	if len(allowDM) != 1 {
		t.Errorf("DM allowlist must survive thread failure; got %+v", allowDM)
	}
	if len(allowThread) != 0 {
		t.Errorf("thread allowlist must be empty on DB error; got %+v", allowThread)
	}
}

// TestBuildAllowlist_ThreadPerGroupCap — a group whose thread count exceeds
// maxThreadsPerGroup downgrades to group-only for this request (its group
// entry still populates, no thread entries).
func TestBuildAllowlist_ThreadPerGroupCap(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpFat"}, {GroupNo: "grpThin"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	fat := make([]string, maxThreadsPerGroup+1)
	for i := range fat {
		fat[i] = "thr" + itoa(i)
	}
	h.threadEnumFn = func([]string) (map[string][]string, error) {
		return map[string][]string{
			"grpFat":  fat,
			"grpThin": {"thrOK"},
		}, nil
	}

	allowGroup, _, allowThread, err := h.buildAllowlist(nil, loginUID, "")
	if err != nil {
		t.Fatalf("buildAllowlist: %v", err)
	}
	// Group side unchanged: both groups still in the flat allowlist.
	gotGroups := map[string]bool{}
	for _, r := range allowGroup {
		gotGroups[r.OSChannelID] = true
	}
	if !gotGroups["grpFat"] || !gotGroups["grpThin"] {
		t.Errorf("group allowlist must contain both groups; got %+v", allowGroup)
	}
	// Thread side: only grpThin's one thread. grpFat is downgraded.
	if len(allowThread) != 1 || allowThread[0].OSChannelID != thread.BuildChannelID("grpThin", "thrOK") {
		t.Errorf("only grpThin's single thread should survive the per-group cap; got %+v", allowThread)
	}
}

// TestBuildAllowlist_ThreadGlobalAggregateCap — once the running total of
// thread channelIDs would cross maxTotalThreadChannelIDs on the NEXT group,
// remaining groups are skipped (their group entries still populate).
func TestBuildAllowlist_ThreadGlobalAggregateCap(t *testing.T) {
	loginUID := "me"
	// Craft two groups whose combined thread count would exceed the global
	// cap but each individually is under the per-group cap. We deliberately
	// stay under maxThreadsPerGroup so the cap under test is the global one.
	// Choose sizes: A = 150, B = maxTotalThreadChannelIDs - 100 (so A+B > cap
	// while A < maxThreadsPerGroup < B is possible only if the global cap is
	// > per-group cap, which is our config).
	sizeA := 150
	sizeB := maxTotalThreadChannelIDs - 100 // guarantees A + B > global cap
	if sizeB > maxThreadsPerGroup {
		// If someone lowers the global cap under the per-group cap, this test
		// stops being meaningful for the global cap — skip loudly.
		t.Skipf("test config assumes maxTotalThreadChannelIDs (%d) > maxThreadsPerGroup (%d)",
			maxTotalThreadChannelIDs, maxThreadsPerGroup)
	}
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}, {GroupNo: "grpB"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	makeIDs := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := 0; i < n; i++ {
			out[i] = prefix + itoa(i)
		}
		return out
	}
	h.threadEnumFn = func([]string) (map[string][]string, error) {
		return map[string][]string{
			"grpA": makeIDs("a", sizeA),
			"grpB": makeIDs("b", sizeB),
		}, nil
	}

	_, _, allowThread, err := h.buildAllowlist(nil, loginUID, "")
	if err != nil {
		t.Fatalf("buildAllowlist: %v", err)
	}
	// Deterministic iteration is groupNos order. grpA fits under the running
	// total; grpB pushes past and is skipped whole (we do NOT partial-fill
	// a group — simpler, avoids "some threads visible, some not" UX).
	if len(allowThread) != sizeA {
		t.Fatalf("expected exactly grpA's %d threads under global cap, got %d",
			sizeA, len(allowThread))
	}
	prefix := "grpA" + thread.ChannelIDSeparator
	for _, r := range allowThread {
		if !strings.HasPrefix(r.OSChannelID, prefix) {
			t.Errorf("unexpected non-grpA thread in allowlist: %q", r.OSChannelID)
		}
	}
}

// TestBuildAllowlist_EmptyGroupsNoThreadQuery — no joined groups means no
// thread enumeration at all. The DB stub must not be called (avoiding a
// pointless `IN ()` query).
func TestBuildAllowlist_EmptyGroupsNoThreadQuery(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{groupsByUID: map[string][]*group.InfoResp{loginUID: nil}}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	called := false
	h.threadEnumFn = func(groupNos []string) (map[string][]string, error) {
		called = true
		return nil, nil
	}

	_, _, allowThread, err := h.buildAllowlist(nil, loginUID, "")
	if err != nil {
		t.Fatalf("buildAllowlist: %v", err)
	}
	if called {
		t.Errorf("threadEnumFn must NOT be called when the caller has no joined groups")
	}
	if len(allowThread) != 0 {
		t.Errorf("thread allowlist must be empty; got %+v", allowThread)
	}
}

// TestChannelsForMember_DropsThreadsInV1 — the member_uid filter path is v1
// simplified: thread entries in allowSet are dropped from the returned
// scope entirely, regardless of the parent group's co-inhabitance.
func TestChannelsForMember_DropsThreadsInV1(t *testing.T) {
	loginUID := "me"
	memberUID := "colleague"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			memberUID: {{GroupNo: "grpShared"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	// Simulate an already-built allowSet with a group + a thread under it +
	// a DM. channelsForMember should keep the group + DM (co-inhabitance /
	// direct DM) but strip the thread.
	threadID := thread.BuildChannelID("grpShared", "thrX")
	allowSet := map[string]channelRef{
		"grpShared": {OSChannelID: "grpShared", WireID: "grpShared", ChannelType: channelTypeGroup},
		threadID:    {OSChannelID: threadID, WireID: threadID, ChannelType: channelTypeThread},
		"dmFake":    {OSChannelID: "dmFake", WireID: memberUID, ChannelType: channelTypePerson},
	}
	got, err := h.channelsForMember(loginUID, memberUID, "", allowSet)
	if err != nil {
		t.Fatalf("channelsForMember: %v", err)
	}
	if _, ok := got["grpShared"]; !ok {
		t.Errorf("shared group must be kept; got %+v", got)
	}
	if _, ok := got["dmFake"]; !ok {
		t.Errorf("DM with member must be kept; got %+v", got)
	}
	if _, ok := got[threadID]; ok {
		t.Errorf("thread must be dropped in v1 channelsForMember; got %+v", got)
	}
}

// TestResolveGlobalScope_ThreadNarrowingHits — a request explicitly narrowing
// to a thread channel_id under a joined group resolves scope = {threadID}
// (no longer fail-closed).
func TestResolveGlobalScope_ThreadNarrowingHits(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	threadID := thread.BuildChannelID("grpA", "thr1")
	h.threadEnumFn = func([]string) (map[string][]string, error) {
		return map[string][]string{"grpA": {"thr1"}}, nil
	}

	// Build a validator context whose spaceID is empty so RequireSpaceID
	// stays off (the default) and resolveGlobalScope proceeds without a
	// Space gate.
	c, _ := newValidatorCtx(t)
	osIDs, _, singleFast, ok := h.resolveGlobalScope(c, loginUID,
		[]GlobalChannelRef{{ChannelID: threadID, ChannelType: channelTypeThread}}, "")
	if !ok {
		t.Fatalf("resolveGlobalScope must succeed; a response was already written")
	}
	if len(osIDs) != 1 || osIDs[0] != threadID {
		t.Fatalf("scope must be exactly {%q}; got %v", threadID, osIDs)
	}
	if singleFast == nil {
		t.Fatalf("single-channel fast path must fire for a lone thread scope")
	}
	if singleFast.ChannelType != channelTypeThread || singleFast.OSChannelID != threadID {
		t.Errorf("singleFast mismatch: %+v", singleFast)
	}
}

// TestResolveGlobalScope_ThreadOutsideMembership — a request narrowing to a
// thread under a group the caller is NOT in silently drops to an empty
// scope (§6.3: unreachable channel_id is not a rejection).
func TestResolveGlobalScope_ThreadOutsideMembership(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	h.threadEnumFn = func([]string) (map[string][]string, error) {
		// grpA has no threads; grpB is not in allowlist.
		return map[string][]string{}, nil
	}

	c, _ := newValidatorCtx(t)
	foreignThread := thread.BuildChannelID("grpB", "thrX")
	osIDs, _, singleFast, ok := h.resolveGlobalScope(c, loginUID,
		[]GlobalChannelRef{{ChannelID: foreignThread, ChannelType: channelTypeThread}}, "")
	if !ok {
		t.Fatalf("resolveGlobalScope must succeed even when scope collapses to empty")
	}
	if len(osIDs) != 0 {
		t.Errorf("scope must be empty for a thread outside membership; got %v", osIDs)
	}
	if singleFast != nil {
		t.Errorf("singleFast must be nil when scope is empty; got %+v", singleFast)
	}
}

// itoa is provided by visibility_test.go in this package — no local copy.
