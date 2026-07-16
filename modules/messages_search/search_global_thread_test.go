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

	allowGroup, _, allowThread, _, err := h.buildAllowlist(nil, loginUID, "")
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
// QueryNonDeletedShortIDsByGroupNos must NOT sink the whole request; the
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

	allowGroup, allowDM, allowThread, _, err := h.buildAllowlist(nil, loginUID, "")
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

	allowGroup, _, allowThread, _, err := h.buildAllowlist(nil, loginUID, "")
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
//
// Real-cap coverage (RC N1 on PR #553): the previous version of this test
// filled a single "grpB" with `maxTotalThreadChannelIDs - 100` (=1900) rows,
// which exceeded maxThreadsPerGroup (=200) and made the test t.Skip — so
// the global-cap branch was never actually exercised. This version uses N
// groups each filled up to (but not over) the per-group cap, so the sum
// crosses the global cap while every individual group is under the
// per-group cap. That way we hit the aggregate-cap `break` and not the
// per-group `continue`.
func TestBuildAllowlist_ThreadGlobalAggregateCap(t *testing.T) {
	loginUID := "me"

	// Design: fill N groups each with `maxThreadsPerGroup` shortIDs. Choose
	// N so that N * maxThreadsPerGroup > maxTotalThreadChannelIDs while
	// (N-1) * maxThreadsPerGroup < maxTotalThreadChannelIDs. That gives a
	// deterministic break point on the Nth group: (N-1) groups fit, the Nth
	// group would push us past the aggregate cap.
	//
	// With the shipped constants (per-group=200, total=2000): N=11 gives
	// 10 * 200 = 2000 fitting exactly, and adding the 11th (200 more) would
	// exceed 2000 — so grpN is skipped whole and allowThread has exactly
	// 2000 entries drawn from groups 0..9. (`total + len(shortIDs) > cap`
	// is a strict >; total==cap after group 9 makes `2000 + 200 > 2000`
	// true for group 10, triggering the break.)
	perGroup := maxThreadsPerGroup
	if perGroup <= 0 {
		t.Fatalf("maxThreadsPerGroup must be > 0; got %d", perGroup)
	}
	numGroups := (maxTotalThreadChannelIDs / perGroup) + 1 // guarantees N*perGroup > cap
	if (numGroups-1)*perGroup > maxTotalThreadChannelIDs {
		t.Fatalf("config sanity: expected (N-1)*perGroup <= cap, got %d > %d",
			(numGroups-1)*perGroup, maxTotalThreadChannelIDs)
	}
	groupNames := make([]string, 0, numGroups)
	groupsSlice := make([]*group.InfoResp, 0, numGroups)
	for i := 0; i < numGroups; i++ {
		name := "grp" + itoa(i)
		groupNames = append(groupNames, name)
		groupsSlice = append(groupsSlice, &group.InfoResp{GroupNo: name})
	}
	gSvc := &stubGroupSvc{groupsByUID: map[string][]*group.InfoResp{loginUID: groupsSlice}}
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
		m := make(map[string][]string, numGroups)
		for _, gn := range groupNames {
			m[gn] = makeIDs(gn+"_thr", perGroup)
		}
		return m, nil
	}

	_, _, allowThread, _, err := h.buildAllowlist(nil, loginUID, "")
	if err != nil {
		t.Fatalf("buildAllowlist: %v", err)
	}
	// (a) Aggregate cap actually kicked in: total must not exceed the cap.
	if len(allowThread) > maxTotalThreadChannelIDs {
		t.Fatalf("allowThread must respect global cap %d; got %d",
			maxTotalThreadChannelIDs, len(allowThread))
	}
	// (b) It kicked in on the RIGHT group: the last group's threads must
	//     be missing entirely (skipped whole, not partial-filled).
	skippedPrefix := groupNames[numGroups-1] + thread.ChannelIDSeparator
	for _, r := range allowThread {
		if strings.HasPrefix(r.OSChannelID, skippedPrefix) {
			t.Errorf("grp that trips aggregate cap must be skipped WHOLE; leaked %q",
				r.OSChannelID)
		}
	}
	// (c) Preceding groups populated exactly: we expect (numGroups-1) groups
	//     each at perGroup entries when (N-1)*perGroup <= cap.
	wantFitted := (numGroups - 1) * perGroup
	if wantFitted > maxTotalThreadChannelIDs {
		wantFitted = maxTotalThreadChannelIDs
	}
	if len(allowThread) != wantFitted {
		t.Fatalf("expected exactly %d thread entries under global cap, got %d",
			wantFitted, len(allowThread))
	}
	// (d) Each fitted group must show up with all `perGroup` shortIDs.
	perGroupCount := make(map[string]int)
	for _, r := range allowThread {
		for _, gn := range groupNames[:numGroups-1] {
			if strings.HasPrefix(r.OSChannelID, gn+thread.ChannelIDSeparator) {
				perGroupCount[gn]++
				break
			}
		}
	}
	for _, gn := range groupNames[:numGroups-1] {
		if perGroupCount[gn] != perGroup {
			t.Errorf("group %s: expected %d threads, got %d",
				gn, perGroup, perGroupCount[gn])
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

	_, _, allowThread, _, err := h.buildAllowlist(nil, loginUID, "")
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

// TestChannelsForMembers_ThreadsUnderSharedGroupSurface — bug 5 flips the v1
// behaviour: a member filter that keeps a group must ALSO keep every
// allowlisted thread under that group (統一 rule: 群 → 群 + 其子区). The v1
// unconditional thread-drop this test previously locked in is now obsolete;
// it is replaced with the new positive assertion.
func TestChannelsForMembers_ThreadsUnderSharedGroupSurface(t *testing.T) {
	loginUID := "me"
	memberUID := "colleague"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			memberUID: {{GroupNo: "grpShared"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	threadID := thread.BuildChannelID("grpShared", "thrX")
	allowSet := map[string]channelRef{
		"grpShared": {OSChannelID: "grpShared", WireID: "grpShared", ChannelType: channelTypeGroup},
		threadID:    {OSChannelID: threadID, WireID: threadID, ChannelType: channelTypeThread},
		"dmFake":    {OSChannelID: "dmFake", WireID: memberUID, ChannelType: channelTypePerson},
	}
	got, err := h.channelsForMembers(loginUID, []string{memberUID}, "", allowSet)
	if err != nil {
		t.Fatalf("channelsForMembers: %v", err)
	}
	if _, ok := got["grpShared"]; !ok {
		t.Errorf("shared group must be kept; got %+v", got)
	}
	if _, ok := got["dmFake"]; !ok {
		t.Errorf("DM with member must be kept; got %+v", got)
	}
	if _, ok := got[threadID]; !ok {
		t.Errorf("thread under shared group must now surface (bug 5 統一 rule); got %+v", got)
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
	osIDs, _, singleFast, _, ok := h.resolveGlobalScope(c, loginUID,
		[]GlobalChannelRef{{ChannelID: threadID, ChannelType: channelTypeThread}}, nil, "")
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
	osIDs, _, singleFast, _, ok := h.resolveGlobalScope(c, loginUID,
		[]GlobalChannelRef{{ChannelID: foreignThread, ChannelType: channelTypeThread}}, nil, "")
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

// TestEnumerateThreadsForGroups_FatGroupDoesNotStarveOthers — RC 3 P1
// regression on PR #553. The DB-side per-group PARTITION cap (see
// thread.NonDeletedByGroupNosPerGroupHardLimit) is what actually prevents
// starvation, but this caller-level test locks in the surface the caller
// promises: given a byGroup map where one group is over the caller's
// per-group cap AND other groups are normal, enumerateThreadsForGroups
// must (a) drop the fat group with WARN (per-group cap `continue`) AND
// (b) still surface every other group's threads. Pre-RC3 that combination
// silently produced an empty allowlist because the fat group ate the
// global DB LIMIT and normal groups arrived empty.
func TestEnumerateThreadsForGroups_FatGroupDoesNotStarveOthers(t *testing.T) {
	gSvc := &stubGroupSvc{}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	h.threadEnumFn = func(groupNos []string) (map[string][]string, error) {
		// Simulate what the post-fix SQL now guarantees: the fat group is
		// bounded to `maxThreadsPerGroup + 1` rows by the DB PARTITION cap,
		// and normal groups arrive complete. The 1-row overshoot on the fat
		// group is the observability signal for the caller's per-group cap.
		fatOver := make([]string, maxThreadsPerGroup+1)
		for i := range fatOver {
			fatOver[i] = "fatThr_" + itoa(i)
		}
		return map[string][]string{
			"grpFat":    fatOver,
			"grpNorm1":  {"thr_n1_a", "thr_n1_b"},
			"grpNorm2":  {"thr_n2_a"},
		}, nil
	}

	got := h.enumerateThreadsForGroups([]string{"grpFat", "grpNorm1", "grpNorm2"})

	// Fat group must NOT appear in the allowlist (per-group cap `continue`).
	fatPrefix := "grpFat" + thread.ChannelIDSeparator
	for _, r := range got {
		if strings.HasPrefix(r.OSChannelID, fatPrefix) {
			t.Errorf("fat group must be downgraded to group-only; leaked %q into thread allowlist", r.OSChannelID)
		}
	}

	// Normal groups MUST appear in full — this is the P1 assertion. Under
	// the pre-RC3 implementation these would all be absent.
	want := map[string]bool{
		thread.BuildChannelID("grpNorm1", "thr_n1_a"): true,
		thread.BuildChannelID("grpNorm1", "thr_n1_b"): true,
		thread.BuildChannelID("grpNorm2", "thr_n2_a"): true,
	}
	if len(got) != len(want) {
		t.Fatalf("normal groups must be fully surfaced (want %d refs, got %d)", len(want), len(got))
	}
	for _, r := range got {
		if !want[r.OSChannelID] {
			t.Errorf("unexpected channelId in normal-group allowlist: %q", r.OSChannelID)
		}
		if r.ChannelType != channelTypeThread {
			t.Errorf("ChannelType must be thread=5; got %d", r.ChannelType)
		}
	}
}

// TestEnumerateThreadsForGroups_NoTrailingBlankInflation — defensive: the
// running total that gates the aggregate cap must count *appended* rows,
// not raw `len(shortIDs)`. Production QueryNonDeletedShortIDsByGroupNos
// strips blank shortIDs at the SQL parse boundary, but a future
// threadEnumFn source (mock, alternative backend) may not. Verify a group
// containing blank shortIDs contributes only its non-blank rows to the
// caller's aggregate count, so a subsequent group is NOT prematurely
// starved by phantom rows. (RC N3 on PR #553.)
func TestEnumerateThreadsForGroups_NoTrailingBlankInflation(t *testing.T) {
	gSvc := &stubGroupSvc{}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	// Design: N groups each holding K blanks + K real shortIDs, chosen so
	// that:
	//   sum(raw len) across N groups     > maxTotalThreadChannelIDs (2000)
	//   sum(appended non-blank) across N groups <= maxTotalThreadChannelIDs
	//   each group's raw len                    <= maxThreadsPerGroup (200)
	// so per-group cap doesn't fire but the aggregate would fire with the
	// buggy raw-count accounting. Correct accounting must let all N groups
	// through and surface a trailing extra group's rows too.
	const (
		K      = 90 // per-group non-blank rows
		blanks = 90 // per-group blank rows (raw len = 180, under per-group cap 200)
		N      = 14 // N * (K+blanks) = 2520 > 2000 raw aggregate, N * K = 1260 well under
	)
	groupNos := make([]string, 0, N+1)
	enumMap := make(map[string][]string, N+1)
	for gi := 0; gi < N; gi++ {
		gn := "grp_pad_" + itoa(gi)
		groupNos = append(groupNos, gn)
		ids := make([]string, 0, K+blanks)
		for si := 0; si < K; si++ {
			ids = append(ids, gn+"_thr_"+itoa(si))
		}
		for si := 0; si < blanks; si++ {
			ids = append(ids, "") // blank shortIDs — must not inflate aggregate
		}
		enumMap[gn] = ids
	}
	// Extra tail group with 10 real threads. Under the bug this group is
	// dropped whole (aggregate cap trips on inflated running total). Under
	// the fix, it survives.
	tail := "grp_tail"
	groupNos = append(groupNos, tail)
	enumMap[tail] = []string{"tail_a", "tail_b", "tail_c"}

	h.threadEnumFn = func([]string) (map[string][]string, error) {
		return enumMap, nil
	}

	got := h.enumerateThreadsForGroups(groupNos)

	// Expect exactly N*K + 3 non-blank refs — tail group survives.
	wantTotal := N*K + 3
	if len(got) != wantTotal {
		t.Fatalf("expected %d non-blank refs (N*K + tail); got %d", wantTotal, len(got))
	}
	// Confirm tail group actually surfaced (this is the load-bearing check
	// for the `total += appended` fix; the buggy `total += len(shortIDs)`
	// would starve it).
	tailPrefix := tail + thread.ChannelIDSeparator
	tailHits := 0
	for _, r := range got {
		if r.OSChannelID == "" {
			t.Errorf("blank channelId must never surface")
		}
		if strings.HasPrefix(r.OSChannelID, tailPrefix) {
			tailHits++
		}
	}
	if tailHits != 3 {
		t.Errorf("tail group must contribute 3 refs (fix for raw-len inflation); got %d", tailHits)
	}
}

// TestBuildAllowlist_ArchivedThreadsIncluded — RC blocker on PR #553:
// archived (=2) threads must be surfaced by global search, matching the
// single-channel search / message-read contract of "reject deleted, allow
// archived". The stub returns whatever the underlying DB call would return;
// after the fix, that call is QueryNonDeletedShortIDsByGroupNos which
// surfaces status IN (active, archived). This test locks the invariant that
// whatever the enumerator returns — including shortIDs the DB layer classes
// as archived — lands in the allowlist verbatim (no downstream status
// re-filter is silently applied).
func TestBuildAllowlist_ArchivedThreadsIncluded(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	// The stub simulates the fixed enumerator behaviour: an active thread
	// AND an archived thread from the same group both surface.
	active := "thr_active"
	archived := "thr_archived"
	h.threadEnumFn = func(groupNos []string) (map[string][]string, error) {
		return map[string][]string{"grpA": {active, archived}}, nil
	}

	_, _, allowThread, _, err := h.buildAllowlist(nil, loginUID, "")
	if err != nil {
		t.Fatalf("buildAllowlist: %v", err)
	}
	want := map[string]bool{
		thread.BuildChannelID("grpA", active):   true,
		thread.BuildChannelID("grpA", archived): true,
	}
	if len(allowThread) != len(want) {
		t.Fatalf("expected %d threads (active + archived), got %d (%+v)",
			len(want), len(allowThread), allowThread)
	}
	for _, r := range allowThread {
		if !want[r.OSChannelID] {
			t.Errorf("unexpected channelId %q in allowlist", r.OSChannelID)
		}
		delete(want, r.OSChannelID)
	}
	if len(want) > 0 {
		t.Errorf("missing expected channelIds: %v", want)
	}
}

// TestBuildAllowlist_DeletedThreadsExcluded — RC blocker on PR #553:
// deleted threads must NOT leak into the allowlist. The DB-level
// enforcement lives in QueryNonDeletedShortIDsByGroupNos (`status !=
// ThreadStatusDeleted`); this stub-level test locks the invariant that
// the enumerator's exclusions are respected verbatim by buildAllowlist
// (no rediscovery / re-widening downstream).
func TestBuildAllowlist_DeletedThreadsExcluded(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	// The stub simulates a group that has one visible (active) thread and
	// zero returned rows for the deleted thread — that's what the fixed
	// enumerator does: `status != deleted` filter excludes deleted rows.
	visible := "thr_visible"
	h.threadEnumFn = func(groupNos []string) (map[string][]string, error) {
		// Deleted shortID is deliberately NOT in the returned map — same
		// as what QueryNonDeletedShortIDsByGroupNos does at the SQL level.
		return map[string][]string{"grpA": {visible}}, nil
	}

	_, _, allowThread, _, err := h.buildAllowlist(nil, loginUID, "")
	if err != nil {
		t.Fatalf("buildAllowlist: %v", err)
	}
	if len(allowThread) != 1 {
		t.Fatalf("expected exactly 1 thread (deleted excluded), got %d (%+v)",
			len(allowThread), allowThread)
	}
	wantID := thread.BuildChannelID("grpA", visible)
	if allowThread[0].OSChannelID != wantID {
		t.Errorf("expected %q, got %q", wantID, allowThread[0].OSChannelID)
	}
	// And a deleted shortID must not appear via any path.
	deletedID := thread.BuildChannelID("grpA", "thr_deleted")
	for _, r := range allowThread {
		if r.OSChannelID == deletedID {
			t.Errorf("deleted thread channelId %q must not appear in allowlist", deletedID)
		}
	}
}

// TestResolveGlobalScope_ArchivedThreadNarrowingHits — RC blocker on
// PR #553: an explicit narrowing to an archived thread (visible via
// single-channel search, msg-read) must now also resolve on the global
// endpoint. Before the fix, this collapsed to empty scope because
// enumerateThreadsForGroups excluded archived shortIDs, so the requested
// channelID was not in allowSet.
func TestResolveGlobalScope_ArchivedThreadNarrowingHits(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	archivedShortID := "thr_archived"
	archivedChan := thread.BuildChannelID("grpA", archivedShortID)
	h.threadEnumFn = func([]string) (map[string][]string, error) {
		// Post-fix enumerator would return archived alongside active. Here
		// we simulate only the archived one to prove the narrowing hits it.
		return map[string][]string{"grpA": {archivedShortID}}, nil
	}

	c, _ := newValidatorCtx(t)
	osIDs, _, singleFast, _, ok := h.resolveGlobalScope(c, loginUID,
		[]GlobalChannelRef{{ChannelID: archivedChan, ChannelType: channelTypeThread}}, nil, "")
	if !ok {
		t.Fatalf("resolveGlobalScope must succeed; a response was already written")
	}
	if len(osIDs) != 1 || osIDs[0] != archivedChan {
		t.Fatalf("scope must be exactly {%q}; got %v", archivedChan, osIDs)
	}
	if singleFast == nil {
		t.Fatalf("single-channel fast path must fire for a lone archived-thread scope")
	}
	if singleFast.ChannelType != channelTypeThread || singleFast.OSChannelID != archivedChan {
		t.Errorf("singleFast mismatch: %+v", singleFast)
	}
}

// ---------------------------------------------------------------------------
// bug 4 · a selected group covers its sub-threads; a selected thread stays
// scoped to itself. resolveGlobalScope folds every allowlisted thread whose
// parent group was named in channel_ids into the resolved scope, then keeps
// the allowlist intersection so a caller can never over-reach.
// ---------------------------------------------------------------------------

// TestResolveGlobalScope_GroupChannelIncludesThreads — selecting a group
// (channelType=2) resolves scope = { group_no } ∪ { that group's threads }.
// Threads of OTHER groups must not leak in.
func TestResolveGlobalScope_GroupChannelIncludesThreads(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}, {GroupNo: "grpB"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	h.threadEnumFn = func([]string) (map[string][]string, error) {
		return map[string][]string{
			"grpA": {"t1", "t2"},
			"grpB": {"t3"},
		}, nil
	}

	c, _ := newValidatorCtx(t)
	osIDs, _, singleFast, _, ok := h.resolveGlobalScope(c, loginUID,
		[]GlobalChannelRef{{ChannelID: "grpA", ChannelType: channelTypeGroup}}, nil, "")
	if !ok {
		t.Fatalf("resolveGlobalScope must succeed")
	}
	want := map[string]bool{
		"grpA": true,
		thread.BuildChannelID("grpA", "t1"): true,
		thread.BuildChannelID("grpA", "t2"): true,
	}
	if len(osIDs) != len(want) {
		t.Fatalf("group scope must expand to group + its threads (want %d entries); got %v", len(want), osIDs)
	}
	for _, id := range osIDs {
		if !want[id] {
			t.Errorf("unexpected channelId %q in group-expanded scope (grpB thread must NOT leak)", id)
		}
	}
	if singleFast != nil {
		t.Errorf("a group with threads yields a multi-id scope; single-channel fast path must NOT fire; got %+v", singleFast)
	}
}

// TestResolveGlobalScope_ThreadChannelScopesToThreadOnly — selecting a single
// thread (channelType=5) resolves scope = { that thread } only: neither the
// parent group nor sibling threads are pulled in.
func TestResolveGlobalScope_ThreadChannelScopesToThreadOnly(t *testing.T) {
	loginUID := "me"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	// grpA has three allowlisted threads; the request narrows to just one.
	h.threadEnumFn = func([]string) (map[string][]string, error) {
		return map[string][]string{"grpA": {"t1", "t2", "t3"}}, nil
	}

	target := thread.BuildChannelID("grpA", "t2")
	c, _ := newValidatorCtx(t)
	osIDs, _, singleFast, _, ok := h.resolveGlobalScope(c, loginUID,
		[]GlobalChannelRef{{ChannelID: target, ChannelType: channelTypeThread}}, nil, "")
	if !ok {
		t.Fatalf("resolveGlobalScope must succeed")
	}
	if len(osIDs) != 1 || osIDs[0] != target {
		t.Fatalf("thread selection must scope to the thread alone (no parent group, no siblings); got %v", osIDs)
	}
	if singleFast == nil || singleFast.OSChannelID != target || singleFast.ChannelType != channelTypeThread {
		t.Errorf("lone-thread scope should take the single-channel fast path; got %+v", singleFast)
	}
}

// TestResolveGlobalScope_GroupExpansionIntersectsAllowlist — the group→thread
// expansion never lets a caller over-reach: a group the caller is NOT a member
// of resolves to an empty scope (no group body, no threads), because both the
// group and any folded threads are dropped by the allowlist intersection.
func TestResolveGlobalScope_GroupExpansionIntersectsAllowlist(t *testing.T) {
	loginUID := "me"
	// Caller is only a member of grpA; grpZ is requested but not joined.
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			loginUID: {{GroupNo: "grpA"}},
		},
	}
	uSvc := &stubUserSvc{}
	h := newAllowlistHandler(t, gSvc, uSvc)
	h.threadEnumFn = func([]string) (map[string][]string, error) {
		return map[string][]string{"grpA": {"t1"}}, nil
	}

	c, _ := newValidatorCtx(t)
	osIDs, _, singleFast, _, ok := h.resolveGlobalScope(c, loginUID,
		[]GlobalChannelRef{{ChannelID: "grpZ", ChannelType: channelTypeGroup}}, nil, "")
	if !ok {
		t.Fatalf("resolveGlobalScope must succeed even when scope collapses to empty")
	}
	if len(osIDs) != 0 {
		t.Fatalf("a group outside the caller's allowlist must expand to nothing (no thread over-reach); got %v", osIDs)
	}
	if singleFast != nil {
		t.Errorf("empty scope must not set singleFast; got %+v", singleFast)
	}
}
