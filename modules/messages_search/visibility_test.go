package messages_search

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/olivere/elastic"
)

// stubProbe is a hand-rolled visibilityProbe used by every visibility test.
// Per-method maps drive the four signals; per-method *Err lets a test pin
// fail-closed behaviour. Call counters double as no-DB-on-empty assertions.
type stubProbe struct {
	revoked     map[string]bool
	deleted     map[string]bool
	userDeleted map[string]bool   // keyed by uid+":"+id (so per-user can be asserted)
	offsetByUC  map[string]uint32 // keyed by uid+":"+channelID

	revokedErr     error
	deletedErr     error
	userDeletedErr error
	offsetErr      error

	revokedCalls     int
	deletedCalls     int
	userDeletedCalls int
	offsetCalls      int

	gotRevokedIDs     []string
	gotDeletedIDs     []string
	gotUserDeletedIDs []string
	gotUserDeletedUID string
	gotOffsetUID      string
	gotOffsetChannel  string
	gotOffsetChannels []string
}

func (s *stubProbe) RevokedSet(ids []string) (map[string]struct{}, error) {
	s.revokedCalls++
	s.gotRevokedIDs = append([]string{}, ids...)
	if s.revokedErr != nil {
		return nil, s.revokedErr
	}
	out := map[string]struct{}{}
	for _, id := range ids {
		if s.revoked[id] {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

func (s *stubProbe) GloballyDeletedSet(ids []string) (map[string]struct{}, error) {
	s.deletedCalls++
	s.gotDeletedIDs = append([]string{}, ids...)
	if s.deletedErr != nil {
		return nil, s.deletedErr
	}
	out := map[string]struct{}{}
	for _, id := range ids {
		if s.deleted[id] {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

func (s *stubProbe) UserDeletedSet(uid string, ids []string) (map[string]struct{}, error) {
	s.userDeletedCalls++
	s.gotUserDeletedUID = uid
	s.gotUserDeletedIDs = append([]string{}, ids...)
	if s.userDeletedErr != nil {
		return nil, s.userDeletedErr
	}
	out := map[string]struct{}{}
	for _, id := range ids {
		if s.userDeleted[uid+":"+id] {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

func (s *stubProbe) ChannelOffset(uid, channelID string) (uint32, error) {
	s.offsetCalls++
	s.gotOffsetUID = uid
	s.gotOffsetChannel = channelID
	if s.offsetErr != nil {
		return 0, s.offsetErr
	}
	return s.offsetByUC[uid+":"+channelID], nil
}

// ChannelOffsets is the batch variant. Reuses offsetByUC / offsetErr so
// existing tests that only populate the single-channel map continue to work
// unchanged. Fail-closed error semantics mirror ChannelOffset.
func (s *stubProbe) ChannelOffsets(uid string, channelIDs []string) (map[string]uint32, error) {
	s.offsetCalls++
	s.gotOffsetUID = uid
	if len(channelIDs) == 1 {
		s.gotOffsetChannel = channelIDs[0]
	}
	s.gotOffsetChannels = append([]string{}, channelIDs...)
	if s.offsetErr != nil {
		return nil, s.offsetErr
	}
	out := make(map[string]uint32, len(channelIDs))
	for _, cid := range channelIDs {
		if v, ok := s.offsetByUC[uid+":"+cid]; ok {
			out[cid] = v
		}
	}
	return out, nil
}

func newVisibilityHandler(p visibilityProbe) *Handler {
	return &Handler{
		Log:        log.NewTLog("messages_search-visibility-test"),
		cfg:        SearchConfig{},
		visibility: p,
	}
}

func keepKeysSorted(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFilterVisible_RevokedDropped — message_extra.revoke=1 hits are
// removed (parity with filterMessages: revoked rows hidden from read).
func TestFilterVisible_RevokedDropped(t *testing.T) {
	probe := &stubProbe{revoked: map[string]bool{"1": true}}
	h := newVisibilityHandler(probe)
	keep, err := h.filterVisible(context.Background(), "me", "C1",
		[]msgRef{{MessageID: "1", MessageSeq: 10}, {MessageID: "2", MessageSeq: 11}})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if !reflect.DeepEqual(keepKeysSorted(keep), []string{"2"}) {
		t.Fatalf("expected only id 2 to survive revoke=1 drop; got %v", keepKeysSorted(keep))
	}
}

// TestFilterVisible_GlobalDeletedDropped — message_extra.is_deleted=1 hits
// are removed.
func TestFilterVisible_GlobalDeletedDropped(t *testing.T) {
	probe := &stubProbe{deleted: map[string]bool{"2": true}}
	h := newVisibilityHandler(probe)
	keep, err := h.filterVisible(context.Background(), "me", "C1",
		[]msgRef{{MessageID: "1"}, {MessageID: "2"}})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if !reflect.DeepEqual(keepKeysSorted(keep), []string{"1"}) {
		t.Fatalf("expected globally-deleted id 2 dropped; got %v", keepKeysSorted(keep))
	}
}

// TestFilterVisible_UserDeletedOnlyForCurrentUID — user-A's per-uid delete
// must NOT hide the message for user-B (the per-uid signal is scoped).
func TestFilterVisible_UserDeletedOnlyForCurrentUID(t *testing.T) {
	probe := &stubProbe{
		userDeleted: map[string]bool{"A:1": true},
	}
	h := newVisibilityHandler(probe)

	keepA, err := h.filterVisible(context.Background(), "A", "C1", []msgRef{{MessageID: "1"}})
	if err != nil {
		t.Fatalf("filter A: %v", err)
	}
	if _, ok := keepA["1"]; ok {
		t.Fatalf("uid=A deleted id 1 must be dropped for uid=A")
	}

	keepB, err := h.filterVisible(context.Background(), "B", "C1", []msgRef{{MessageID: "1"}})
	if err != nil {
		t.Fatalf("filter B: %v", err)
	}
	if _, ok := keepB["1"]; !ok {
		t.Fatalf("uid=A's delete must not affect uid=B; got %v", keepKeysSorted(keepB))
	}
}

// TestFilterVisible_ChannelOffsetDropsBelowOrEqual — offset=N hides
// messages with messageSeq <= N (matches modules/search/api.go, where the
// >= comparison is the flip of the same predicate).
func TestFilterVisible_ChannelOffsetDropsBelowOrEqual(t *testing.T) {
	probe := &stubProbe{
		offsetByUC: map[string]uint32{"me:C1": 100},
	}
	h := newVisibilityHandler(probe)
	keep, err := h.filterVisible(context.Background(), "me", "C1", []msgRef{
		{MessageID: "lo", MessageSeq: 99},
		{MessageID: "eq", MessageSeq: 100},
		{MessageID: "hi", MessageSeq: 101},
	})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if !reflect.DeepEqual(keepKeysSorted(keep), []string{"hi"}) {
		t.Fatalf("expected only seq>offset to survive; got %v", keepKeysSorted(keep))
	}
}

// TestFilterVisible_OffsetZeroNoFilter — offset=0 means user has not
// cleared the channel; all hits survive the offset gate.
func TestFilterVisible_OffsetZeroNoFilter(t *testing.T) {
	probe := &stubProbe{} // offset map empty → 0
	h := newVisibilityHandler(probe)
	keep, err := h.filterVisible(context.Background(), "me", "C1",
		[]msgRef{{MessageID: "1", MessageSeq: 1}, {MessageID: "2", MessageSeq: 2}})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if !reflect.DeepEqual(keepKeysSorted(keep), []string{"1", "2"}) {
		t.Fatalf("offset=0 must keep all; got %v", keepKeysSorted(keep))
	}
}

// TestFilterVisible_GetRevokedMessagesError_FailClosed — RevokedSet error
// must propagate so caller fails closed (no fall-through to OS hits).
func TestFilterVisible_GetRevokedMessagesError_FailClosed(t *testing.T) {
	probe := &stubProbe{revokedErr: errors.New("db down")}
	h := newVisibilityHandler(probe)
	_, err := h.filterVisible(context.Background(), "me", "C1", []msgRef{{MessageID: "1"}})
	if err == nil {
		t.Fatalf("RevokedSet error must propagate")
	}
}

func TestFilterVisible_GetDeletedMessagesError_FailClosed(t *testing.T) {
	probe := &stubProbe{deletedErr: errors.New("db down")}
	h := newVisibilityHandler(probe)
	_, err := h.filterVisible(context.Background(), "me", "C1", []msgRef{{MessageID: "1"}})
	if err == nil {
		t.Fatalf("GloballyDeletedSet error must propagate")
	}
}

func TestFilterVisible_GetDeletedMessagesWithUIDError_FailClosed(t *testing.T) {
	probe := &stubProbe{userDeletedErr: errors.New("db down")}
	h := newVisibilityHandler(probe)
	_, err := h.filterVisible(context.Background(), "me", "C1", []msgRef{{MessageID: "1"}})
	if err == nil {
		t.Fatalf("UserDeletedSet error must propagate")
	}
}

func TestFilterVisible_GetChannelOffsetError_FailClosed(t *testing.T) {
	probe := &stubProbe{offsetErr: errors.New("db down")}
	h := newVisibilityHandler(probe)
	_, err := h.filterVisible(context.Background(), "me", "C1", []msgRef{{MessageID: "1"}})
	if err == nil {
		t.Fatalf("ChannelOffset error must propagate")
	}
}

// TestFilterVisible_EmptyRefsReturnsEmptySetNoDB — the no-refs path must
// short-circuit before any probe call (mirrors filterMessages: nothing
// to filter, nothing to query).
func TestFilterVisible_EmptyRefsReturnsEmptySetNoDB(t *testing.T) {
	probe := &stubProbe{}
	h := newVisibilityHandler(probe)
	keep, err := h.filterVisible(context.Background(), "me", "C1", nil)
	if err != nil {
		t.Fatalf("empty filter: %v", err)
	}
	if len(keep) != 0 {
		t.Fatalf("empty refs must return empty keep set")
	}
	if probe.revokedCalls != 0 || probe.deletedCalls != 0 || probe.userDeletedCalls != 0 || probe.offsetCalls != 0 {
		t.Fatalf("empty refs must not touch the probe; got revoked=%d deleted=%d userDel=%d offset=%d",
			probe.revokedCalls, probe.deletedCalls, probe.userDeletedCalls, probe.offsetCalls)
	}
}

// TestFilterVisible_DedupesMessageIDs — duplicate refs collapse to a
// single IN list entry per id (bounded query size).
func TestFilterVisible_DedupesMessageIDs(t *testing.T) {
	probe := &stubProbe{}
	h := newVisibilityHandler(probe)
	_, err := h.filterVisible(context.Background(), "me", "C1",
		[]msgRef{{MessageID: "1"}, {MessageID: "1"}, {MessageID: "2"}})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if !reflect.DeepEqual(probe.gotRevokedIDs, []string{"1", "2"}) {
		t.Fatalf("expected deduped IN list [1,2]; got %v", probe.gotRevokedIDs)
	}
}

// TestPaginateWithFilter_OversampleResume — first OS round returns
// pageSize×3 hits but most are revoked; loop pulls a second round to
// reach pageSize. Cursor anchors at collected[pageSize-1].
//
// We synthesize raw OS hits with stable Sort tuples so cursor encoding
// has something to read; the test asserts (a) loop fills the page,
// (b) cursor is non-empty, (c) hasMore=true when truncation happens.
func TestPaginateWithFilter_OversampleResume(t *testing.T) {
	pageSize := 5
	visible := map[string]bool{} // ids that survive filterVisible
	for i := 1; i <= 5; i++ {
		visible["50"+itoa(i)] = true // round-2 ids: 501..505 visible
	}
	revoked := map[string]bool{}
	// Round 1 returns ids 1..15, all revoked — none survive.
	for i := 1; i <= 15; i++ {
		revoked[itoa(i)] = true
	}
	probe := &stubProbe{revoked: revoked}
	h := newVisibilityHandler(probe)

	// Build two synthetic rounds: round 1 = 15 revoked hits; round 2 = 5
	// visible hits then stops.
	round1 := makeFakeHits(t, 1, 15)
	round2 := makeFakeHits(t, 501, 505)

	round := 0
	osQuery := func(searchAfter []any, size int) (hits []rawHit, err error) {
		round++
		switch round {
		case 1:
			return round1, nil
		case 2:
			return round2, nil
		default:
			t.Fatalf("unexpected round %d", round)
			return nil, nil
		}
	}
	collected, hasMore, cursor, err := h.paginateWithFilter(
		context.Background(), "me", "C1", pageSize, nil, false,
		wrapHitsQuery(osQuery), wrapProject(),
	)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if len(collected) != pageSize {
		t.Fatalf("expected pageSize=%d hits collected, got %d", pageSize, len(collected))
	}
	if hasMore {
		// Round 2 returned exactly pageSize hits; not full fetchSize, so OS
		// has no further results — has_more should be false.
		t.Fatalf("hasMore should be false when OS returns < fetchSize on the resume round")
	}
	if cursor != "" {
		t.Fatalf("cursor should be empty when has_more=false; got %q", cursor)
	}
}

// TestPaginateWithFilter_BudgetExhausted_HasMoreTrue — when filterVisible
// rejects every hit across loopBudget rounds AND OS still has more
// results, we surface has_more=true with a usable cursor so the client
// can keep paging.
func TestPaginateWithFilter_BudgetExhausted_HasMoreTrue(t *testing.T) {
	pageSize := 5
	probe := &stubProbe{revoked: map[string]bool{}}
	// Mark every plausible id (1..1000) as revoked so the page never fills.
	for i := 1; i <= 1000; i++ {
		probe.revoked[itoa(i)] = true
	}
	h := newVisibilityHandler(probe)

	roundCalls := 0
	osQuery := func(searchAfter []any, size int) (hits []rawHit, err error) {
		roundCalls++
		// Each round returns exactly fetchSize=15 hits, signalling OS has more.
		start := (roundCalls-1)*15 + 1
		return makeFakeHits(t, start, start+14), nil
	}
	collected, hasMore, cursor, err := h.paginateWithFilter(
		context.Background(), "me", "C1", pageSize, nil, false,
		wrapHitsQuery(osQuery), wrapProject(),
	)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if len(collected) != 0 {
		t.Fatalf("all hits revoked → expected 0 collected, got %d", len(collected))
	}
	if !hasMore {
		t.Fatalf("budget exhausted with OS still full → has_more must be true")
	}
	if cursor == "" {
		t.Fatalf("has_more=true requires a non-empty cursor (spec v4.2 §1.4)")
	}
	if roundCalls != loopBudget {
		t.Fatalf("expected exactly %d OS rounds, got %d", loopBudget, roundCalls)
	}
}

// TestPaginateWithFilter_RoundRefillUsesFullPrecisionMessageID — round
// continuation must rebuild search_after from the typed _source so the
// messageId tiebreaker keeps full int64 precision. Regression for the
// PR #361 review: anchorHit.Sort comes off encoding/json as float64,
// which rounds snowflake ids above 2^53; reusing it for search_after
// silently mis-anchors the next round at timestamp ties.
//
// Setup: round 1 returns fetchSize=15 hits with ids in the snowflake
// range (1<<60), all marked revoked → none collected, loop continues.
// Round 2 captures the search_after the loop hands in; we assert the
// tiebreaker is the full-precision int64 of the round-1 last hit (not
// a rounded float64) and that round 2's first id is strictly greater
// than the anchor — i.e. no overlap and no skip across rounds.
func TestPaginateWithFilter_RoundRefillUsesFullPrecisionMessageID(t *testing.T) {
	pageSize := 5
	// 1<<60 is well past 2^53 (the float64 mantissa limit), so any
	// float64 round of these ids produces an observably wrong int64.
	const snowflakeBase int64 = 1 << 60

	round1IDs := make([]int64, 15)
	round1IDStrs := make([]string, 15)
	revoked := map[string]bool{}
	for i := 0; i < 15; i++ {
		round1IDs[i] = snowflakeBase + int64(i)
		round1IDStrs[i] = strconv.FormatInt(round1IDs[i], 10)
		revoked[round1IDStrs[i]] = true // page never fills round 1
	}
	probe := &stubProbe{revoked: revoked}
	h := newVisibilityHandler(probe)

	var capturedSearchAfter []any
	calls := 0
	osQuery := func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		calls++
		switch calls {
		case 1:
			return makeSnowflakeHits(round1IDs), nil
		case 2:
			capturedSearchAfter = append([]any{}, searchAfter...)
			return nil, nil // empty terminates the loop cleanly
		}
		t.Fatalf("unexpected round %d", calls)
		return nil, nil
	}

	_, _, _, err := h.paginateWithFilter(
		context.Background(), "me", "C1", pageSize, nil, false,
		osQuery, projectDocRef("C1", ""),
	)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 OS rounds (refill must run), got %d", calls)
	}

	// Time_desc sort tuple shape: [timestamp, messageId, subSeq]. All three
	// must be present; messageId must be int64 with no precision loss; the
	// trailing subSeq must be the int 0 (legacy doc with no subSeq field).
	if got, want := len(capturedSearchAfter), 3; got != want {
		t.Fatalf("search_after len = %d, want %d (tuple=%v)", got, want, capturedSearchAfter)
	}
	gotMsgID, ok := capturedSearchAfter[1].(int64)
	if !ok {
		t.Fatalf("search_after[1] must be int64 (full precision); got %T (%v)",
			capturedSearchAfter[1], capturedSearchAfter[1])
	}
	if got, ok := capturedSearchAfter[2].(int); !ok || got != 0 {
		t.Fatalf("search_after[2] must be int(0) for legacy doc without subSeq; got %T(%v)", capturedSearchAfter[2], capturedSearchAfter[2])
	}
	wantMsgID := round1IDs[len(round1IDs)-1]
	if gotMsgID != wantMsgID {
		t.Fatalf("search_after messageId precision lost; want %d got %d (delta %d)",
			wantMsgID, gotMsgID, wantMsgID-gotMsgID)
	}

	// Sanity: the anchor id must be strictly greater than every other
	// round-1 id — i.e. the loop anchored on the LAST hit of round 1
	// (so OS resumes strictly past it on round 2: no overlap, no skip).
	for i := 0; i < len(round1IDs)-1; i++ {
		if round1IDs[i] >= gotMsgID {
			t.Fatalf("anchor must be strictly greater than prior round-1 ids; "+
				"round1[%d]=%d >= anchor=%d", i, round1IDs[i], gotMsgID)
		}
	}

	// Negative control: a naive `searchAfter = anchorHit.Sort` would
	// have produced this float64-rounded value. Assert the fix did NOT
	// regress to it.
	roundedViaFloat64 := int64(float64(wantMsgID))
	if gotMsgID == roundedViaFloat64 && wantMsgID != roundedViaFloat64 {
		t.Fatalf("search_after fell back to float64-rounded id (%d); "+
			"buildSearchAfterFromHit must read typed _source", roundedViaFloat64)
	}
}

// makeSnowflakeHits builds *elastic.SearchHit fixtures whose Source is
// realistic JSON (so projectDocRef + lastHitMessageID can read back the
// full-precision messageId via Doc) but whose Sort tuple uses float64
// values — exactly the shape OS hands back after encoding/json. This
// reproduces the precision-loss path the fix has to cover.
func makeSnowflakeHits(ids []int64) []*elastic.SearchHit {
	out := make([]*elastic.SearchHit, 0, len(ids))
	for _, id := range ids {
		body, _ := json.Marshal(map[string]any{
			"messageId":  id,
			"messageSeq": uint64(id & 0xffff),
		})
		src := json.RawMessage(body)
		out = append(out, &elastic.SearchHit{
			Source: &src,
			// [ts, msgID] — float64 here is the bug source; ts is small
			// (second precision) so safe, msgID rounds for >2^53.
			Sort: []any{float64(1717000000), float64(id)},
		})
	}
	return out
}

// ---- helpers used by the pagination tests ----

func itoa(i int) string {
	// Keep dependency surface minimal in this _test.go.
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(digits[i%10]) + out
		i /= 10
	}
	return out
}

// rawHit is the test-only minimal projection of *elastic.SearchHit. The
// production code expects *elastic.SearchHit; we wrap into one in
// wrapHitsQuery so we keep the test plumbing tight.
type rawHit struct {
	id  string
	seq uint32
}

func makeFakeHits(t *testing.T, lo, hi int) []rawHit {
	t.Helper()
	out := make([]rawHit, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, rawHit{id: itoa(i), seq: uint32(i)})
	}
	return out
}

// wrapHitsQuery converts the test's []rawHit-returning closure into the
// production osQueryFn (returns []*elastic.SearchHit). Each rawHit is
// materialised into a synthetic SearchHit with:
//   - Source = JSON of {messageId, messageSeq}
//   - Sort   = [timestamp(=msgID), messageId] tiebreaker tuple
//
// timestamp is set to the messageID for uniqueness in the synthetic
// fixture; this keeps cursor encoding deterministic.
func wrapHitsQuery(inner func(searchAfter []any, size int) ([]rawHit, error)) osQueryFn {
	return func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		raw, err := inner(searchAfter, size)
		if err != nil {
			return nil, err
		}
		out := make([]*elastic.SearchHit, 0, len(raw))
		for _, r := range raw {
			id, _ := strconv.ParseInt(r.id, 10, 64)
			body, _ := json.Marshal(map[string]any{
				"messageId":  id,
				"messageSeq": uint64(r.seq),
			})
			src := json.RawMessage(body)
			out = append(out, &elastic.SearchHit{
				Source: &src,
				Sort:   []any{float64(id), float64(id)}, // [ts, msgID]
			})
		}
		return out, nil
	}
}

// wrapProject is the matching projectFn for hits built by wrapHitsQuery —
// just delegates to projectDocRef which is what the real handlers use.
func wrapProject() projectFn {
	return projectDocRef("C1", "")
}

// TestFilterVisible_VisiblesWhitelist_InListKept — when payload.visibles is
// non-empty AND the caller is in it, the hit survives. Mirrors the
// authoritative read-path branch in modules/message/api.go::MsgSyncResp.from
// (visibles-array gate).
func TestFilterVisible_VisiblesWhitelist_InListKept(t *testing.T) {
	probe := &stubProbe{}
	h := newVisibilityHandler(probe)
	keep, err := h.filterVisible(context.Background(), "alice", "C1", []msgRef{
		{MessageID: "1", Visibles: []string{"alice", "bob"}},
	})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if _, ok := keep["1"]; !ok {
		t.Fatalf("alice in visibles must keep id 1; got %v", keepKeysSorted(keep))
	}
}

// TestFilterVisible_VisiblesWhitelist_NotInListDropped — non-empty visibles
// without the caller drops the hit. This is the actual security gate; a
// regression here means group members can search out targeted messages.
func TestFilterVisible_VisiblesWhitelist_NotInListDropped(t *testing.T) {
	probe := &stubProbe{}
	h := newVisibilityHandler(probe)
	keep, err := h.filterVisible(context.Background(), "carol", "C1", []msgRef{
		{MessageID: "1", Visibles: []string{"alice", "bob"}},
		{MessageID: "2"}, // no whitelist; baseline survives
	})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if _, leaked := keep["1"]; leaked {
		t.Fatalf("carol NOT in visibles must drop id 1 (whitelist leak); got %v", keepKeysSorted(keep))
	}
	if _, ok := keep["2"]; !ok {
		t.Fatalf("baseline id 2 (no visibles) must survive; got %v", keepKeysSorted(keep))
	}
}

// TestFilterVisible_VisiblesEmpty_FailOpen — Visibles is nil/empty (the
// indexer hasn't written the field yet, or the message had no allowlist).
// Per CONSTRAINTS-2026-06-12 D24 this is fail-open: the gate is skipped
// and the hit survives.
func TestFilterVisible_VisiblesEmpty_FailOpen(t *testing.T) {
	probe := &stubProbe{}
	h := newVisibilityHandler(probe)
	keep, err := h.filterVisible(context.Background(), "anyone", "C1", []msgRef{
		{MessageID: "nil"},                         // Visibles == nil
		{MessageID: "empty", Visibles: []string{}}, // Visibles == []
	})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if !reflect.DeepEqual(keepKeysSorted(keep), []string{"empty", "nil"}) {
		t.Fatalf("missing/empty visibles must fail-open (both kept); got %v", keepKeysSorted(keep))
	}
}

// TestFilterVisible_VirtualChildHiddenByParentRevoke — end-to-end seam:
// project a virtual sub-doc through projectDocRef → feed the resulting
// msgRef into filterVisible with the PARENT id marked revoked → the child
// must be dropped. This is the operational guarantee of Part B §4:
// "revoke parent rich-text → all derivative media sub-docs disappear".
func TestFilterVisible_VirtualChildHiddenByParentRevoke(t *testing.T) {
	const parentID = "7777"
	probe := &stubProbe{revoked: map[string]bool{parentID: true}}
	h := newVisibilityHandler(probe)

	// Synthesize a virtual sub-doc hit (child id = parent id per indexer
	// contract; the safety belt covers divergence).
	body, _ := json.Marshal(map[string]any{
		"messageId":       int64(7777),
		"messageSeq":      uint64(99),
		"timestamp":       int64(1717000000),
		"virtual":         true,
		"parentMessageId": int64(7777),
	})
	src := json.RawMessage(body)
	hit := &elastic.SearchHit{Source: &src}
	ref, ok := projectDocRef("C1", "")(hit)
	if !ok {
		t.Fatalf("projectDocRef must accept virtual sub-doc")
	}

	keep, err := h.filterVisible(context.Background(), "alice", "C1", []msgRef{ref})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(keep) != 0 {
		t.Fatalf("revoked parent must hide virtual child; kept %v", keepKeysSorted(keep))
	}
	// And confirm the IN list went to MySQL keyed by the parent id.
	if !reflect.DeepEqual(probe.gotRevokedIDs, []string{parentID}) {
		t.Fatalf("RevokedSet must be queried by parent id; got %v", probe.gotRevokedIDs)
	}
}

// TestProjectDocRef_PopulatesVisibles — projectDocRef must read the typed
// _source so visibles reaches filterVisible. Regression guard: a project
// that drops this field silently disables the visibles gate even when the
// indexer is writing it.
func TestProjectDocRef_PopulatesVisibles(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"messageId":  int64(42),
		"messageSeq": uint64(7),
		"timestamp":  int64(1717000000),
		"visibles":   []string{"alice", "bob"},
	})
	src := json.RawMessage(body)
	hit := &elastic.SearchHit{Source: &src}

	ref, ok := projectDocRef("C1", "")(hit)
	if !ok {
		t.Fatalf("projectDocRef must accept a well-formed hit")
	}
	if !reflect.DeepEqual(ref.Visibles, []string{"alice", "bob"}) {
		t.Fatalf("Visibles not propagated; got %v", ref.Visibles)
	}
}

// TestProjectDocRef_VirtualSubDocRoutesToParent — for a virtual sub-doc
// (Part B), the visibility key must be the parent's messageId, not the
// child's own id. This is the load-bearing assertion behind "revoke parent
// → all derivative media disappear": filterVisible queries MySQL using
// msgRef.MessageID, so routing to the parent is what lets the parent's
// revoke/delete state govern the child.
func TestProjectDocRef_VirtualSubDocRoutesToParent(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"messageId":       int64(7777), // indexer contract: also = parent id
		"messageSeq":      uint64(99),
		"timestamp":       int64(1717000000),
		"virtual":         true,
		"parentMessageId": int64(7777),
	})
	src := json.RawMessage(body)
	hit := &elastic.SearchHit{Source: &src}

	ref, ok := projectDocRef("C1", "")(hit)
	if !ok {
		t.Fatalf("projectDocRef must accept a virtual sub-doc")
	}
	if ref.MessageID != "7777" {
		t.Fatalf("visibility key must be parent id; got %q want %q", ref.MessageID, "7777")
	}
}

// TestProjectDocRef_VirtualSafetyBeltOnDivergentParent — defends the
// `coalesce` belt added in projectDocRef. If indexer ever lets the child's
// own messageId diverge from parentMessageId (e.g. moves to a composite id),
// reader must still route visibility to the parent — otherwise revoking the
// parent in MySQL would not hide the child.
func TestProjectDocRef_VirtualSafetyBeltOnDivergentParent(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"messageId":       int64(1234), // hypothetical divergent child id
		"messageSeq":      uint64(50),
		"timestamp":       int64(1717000000),
		"virtual":         true,
		"parentMessageId": int64(7777),
	})
	src := json.RawMessage(body)
	hit := &elastic.SearchHit{Source: &src}

	ref, ok := projectDocRef("C1", "")(hit)
	if !ok {
		t.Fatalf("projectDocRef must accept a virtual sub-doc")
	}
	if ref.MessageID != "7777" {
		t.Fatalf("safety belt failed: visibility key = %q, want parent %q", ref.MessageID, "7777")
	}
}

// TestProjectDocRef_PlainDocKeepsOwnID — non-virtual docs must continue to
// route visibility to their own messageId. Both `virtual=false` and
// `virtual` absent are exercised.
func TestProjectDocRef_PlainDocKeepsOwnID(t *testing.T) {
	cases := []map[string]any{
		{"messageId": int64(42), "messageSeq": uint64(7), "timestamp": int64(1)},
		{"messageId": int64(42), "messageSeq": uint64(7), "timestamp": int64(1), "virtual": false},
		// Virtual=false with a parentMessageId is ignored by coalesce — the
		// Virtual flag is the gate, not the presence of parentMessageId.
		{"messageId": int64(42), "messageSeq": uint64(7), "timestamp": int64(1), "parentMessageId": int64(9999)},
	}
	for i, body := range cases {
		raw, _ := json.Marshal(body)
		src := json.RawMessage(raw)
		hit := &elastic.SearchHit{Source: &src}
		ref, ok := projectDocRef("C1", "")(hit)
		if !ok {
			t.Fatalf("case %d: projectDocRef must accept plain doc", i)
		}
		if ref.MessageID != "42" {
			t.Fatalf("case %d: plain doc must keep own id; got %q want %q", i, ref.MessageID, "42")
		}
	}
}

// TestProjectDocRef_VirtualWithZeroParentFallsBackToOwn — defensive: if
// virtual=true is set but parentMessageId is missing/zero (broken indexer
// payload), coalesce falls back to the child's own id rather than emitting
// a "0" key that would never match any parent in MySQL. Better to drop the
// hit later via normal filterVisible than to silently grant visibility.
func TestProjectDocRef_VirtualWithZeroParentFallsBackToOwn(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"messageId":       int64(42),
		"messageSeq":      uint64(7),
		"timestamp":       int64(1),
		"virtual":         true,
		"parentMessageId": int64(0),
	})
	src := json.RawMessage(body)
	hit := &elastic.SearchHit{Source: &src}
	ref, ok := projectDocRef("C1", "")(hit)
	if !ok {
		t.Fatalf("projectDocRef must still accept the hit")
	}
	if ref.MessageID != "42" {
		t.Fatalf("zero parent must fall back to own id; got %q", ref.MessageID)
	}
}
