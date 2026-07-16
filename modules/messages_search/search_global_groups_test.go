package messages_search

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/olivere/elastic"
)

// --- §2.3 trigger gate: three states ---------------------------------------

func TestGroupsTriggerSatisfied(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
		filters GlobalSearchFilters
		want    bool
	}{
		{"keyword", "hello", GlobalSearchFilters{}, true},
		{"sender", "", GlobalSearchFilters{SenderIDs: []string{"u1"}}, true},
		{"member_uids", "", GlobalSearchFilters{MemberUIDs: []string{"u2"}}, true},
		{"member_uid legacy", "", GlobalSearchFilters{MemberUID: "u3"}, true},
		{"channel_ids", "", GlobalSearchFilters{ChannelIDs: []GlobalChannelRef{{ChannelID: "gA", ChannelType: 2}}}, true},
		// The three that must NOT trigger on their own (stricter than
		// validateSearchNotEmpty, which treats sent_at as effective).
		{"sent_at only", "", GlobalSearchFilters{SentAtFrom: "2026-01-01T00:00:00Z"}, false},
		{"content_types only", "", GlobalSearchFilters{ContentTypes: []int{1}}, false},
		{"channel_types only", "", GlobalSearchFilters{ChannelTypes: []uint8{2}}, false},
		{"empty", "", GlobalSearchFilters{}, false},
		// Blank-only entries do not trigger.
		{"blank sender", "", GlobalSearchFilters{SenderIDs: []string{"  "}}, false},
		{"blank channel_id", "", GlobalSearchFilters{ChannelIDs: []GlobalChannelRef{{ChannelID: "  "}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := groupsTriggerSatisfied(tc.keyword, tc.filters); got != tc.want {
				t.Errorf("groupsTriggerSatisfied(%q,%+v)=%v want %v", tc.keyword, tc.filters, got, tc.want)
			}
		})
	}
}

// --- visibles + revoked blocked at the OS query layer ----------------------

// TestGroupsDSL_VisiblesAndRevokedAtOSLayer builds the exact filter the handler
// hands to OS (base _search_global_messages DSL + visibles whitelist) and
// asserts both OS-resident visibility signals are pushed into the query.
func TestGroupsDSL_VisiblesAndRevokedAtOSLayer(t *testing.T) {
	msgReq := SearchGlobalMessagesReq{Keyword: "report", Filters: GlobalSearchFilters{}}
	base, _ := buildGlobalMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, msgReq, []string{"gA", "gB"}, "space1")
	query := elastic.NewBoolQuery().Filter(base)
	applyVisiblesWhitelist(query, "me")
	body := marshalDSL(t, query)

	if !strings.Contains(body, `"visibles"`) {
		t.Errorf("visibles whitelist must be in the OS query:\n%s", body)
	}
	if !strings.Contains(body, `"revoked"`) {
		t.Errorf("revoked must_not must be in the OS query:\n%s", body)
	}
	// The visibles gate must be a should(mustNot exists, term=me) so empty/absent
	// visibles stays visible while a listed caller matches.
	if !strings.Contains(body, `"exists"`) {
		t.Errorf("visibles gate must use exists() for the no-gate branch:\n%s", body)
	}
	if !strings.Contains(body, `"channelId"`) {
		t.Errorf("terms(channelId) allowlist scope must be in the OS query:\n%s", body)
	}
}

// TestApplyVisiblesWhitelist_ShouldShape pins the should/minimum_should_match
// structure independently of the base DSL.
func TestApplyVisiblesWhitelist_ShouldShape(t *testing.T) {
	b := elastic.NewBoolQuery()
	applyVisiblesWhitelist(b, "u42")
	body := marshalDSL(t, b)
	if !strings.Contains(body, `"minimum_should_match":"1"`) {
		t.Errorf("visibles gate must require minimum_should_match=1:\n%s", body)
	}
	if !strings.Contains(body, `"u42"`) {
		t.Errorf("visibles gate must term-match the caller uid:\n%s", body)
	}
}

// --- bucket projection: DM reversal / thread / group ------------------------

func TestAssembleGroupBucket_DMReversal(t *testing.T) {
	loginUID, peerUID := "me", "peer99"
	key := fakeChannelIDFor(loginUID, peerUID)
	pb := parsedBucket{key: key, docCount: 8, latestTS: 1_700_000_000}
	gb := assembleGroupBucket(pb, pb.latestTS, nil, loginUID, nil, nil, map[string]string{peerUID: "张三"})

	if gb.ChannelType != channelTypePerson {
		t.Errorf("DM bucket channel_type=1; got %d", gb.ChannelType)
	}
	if gb.ChannelID != peerUID {
		t.Errorf("DM channel_id must reverse to peer uid %q; got %q", peerUID, gb.ChannelID)
	}
	if gb.ParentGroupNo != "" || gb.ThreadID != "" || gb.ThreadName != "" {
		t.Errorf("DM bucket must not carry parent_group_no/thread fields: %+v", gb)
	}
	if gb.GroupName != "张三" {
		t.Errorf("DM group_name must come from peer profile; got %q", gb.GroupName)
	}
	if !gb.MatchCountApprox || gb.MatchCount != 8 {
		t.Errorf("match_count=8 approx=true expected; got %d/%v", gb.MatchCount, gb.MatchCountApprox)
	}
	// preview must serialise as [] not null.
	if b, _ := json.Marshal(gb); !strings.Contains(string(b), `"preview":[]`) {
		t.Errorf("preview must serialise as []: %s", b)
	}
}

func TestAssembleGroupBucket_Thread(t *testing.T) {
	loginUID := "me"
	groupNo, shortID := "gA", "thr1"
	key := thread.BuildChannelID(groupNo, shortID)
	pb := parsedBucket{key: key, docCount: 12, latestTS: 1_700_000_100}
	gb := assembleGroupBucket(pb, pb.latestTS, []MessageHit{}, loginUID,
		map[string]string{groupNo: "项目群"}, map[string]string{shortID: "需求评审"}, nil)

	if gb.ChannelType != channelTypeThread {
		t.Errorf("thread bucket channel_type=5; got %d", gb.ChannelType)
	}
	if gb.ParentGroupNo != groupNo {
		t.Errorf("thread parent_group_no must be %q; got %q", groupNo, gb.ParentGroupNo)
	}
	if gb.ThreadID != key {
		t.Errorf("thread_id must be the composite channel_id %q; got %q", key, gb.ThreadID)
	}
	if gb.ThreadName != "需求评审" {
		t.Errorf("thread_name mismatch; got %q", gb.ThreadName)
	}
	if gb.GroupName != "项目群" {
		t.Errorf("thread group_name must be the parent group name; got %q", gb.GroupName)
	}
	if gb.ChannelID != key {
		t.Errorf("thread channel_id echoes the composite id; got %q", gb.ChannelID)
	}
}

func TestAssembleGroupBucket_Group(t *testing.T) {
	pb := parsedBucket{key: "gZ", docCount: 128, latestTS: 1_700_000_200}
	gb := assembleGroupBucket(pb, pb.latestTS, nil, "me", map[string]string{"gZ": "大群"}, nil, nil)

	if gb.ChannelType != channelTypeGroup {
		t.Errorf("group bucket channel_type=2; got %d", gb.ChannelType)
	}
	if gb.ParentGroupNo != "gZ" {
		t.Errorf("group parent_group_no must equal its own channel_id; got %q", gb.ParentGroupNo)
	}
	if gb.ThreadID != "" || gb.ThreadName != "" {
		t.Errorf("group bucket must not carry thread fields: %+v", gb)
	}
	if gb.GroupName != "大群" {
		t.Errorf("group_name mismatch; got %q", gb.GroupName)
	}
}

// TestSortByVisibleLatest_NotBiasedByHiddenHits is the RC ordering regression:
// bucket order must follow the CALIBRATED visible latest_at, not the OS
// pre-filter max. gHidden has the newest pre-filter max (its newest match is
// hidden) but the OLDEST visible hit — it must sort LAST, not first.
func TestSortByVisibleLatest_NotBiasedByHiddenHits(t *testing.T) {
	calibrated := []calibratedBucket{
		// OS candidate order (pre-filter max desc): gHidden, gA, gB.
		{pb: parsedBucket{key: "gHidden", latestTS: 1_700_009_999}, latestVisibleTS: 1_700_000_000},
		{pb: parsedBucket{key: "gA", latestTS: 1_700_000_500}, latestVisibleTS: 1_700_000_500},
		{pb: parsedBucket{key: "gB", latestTS: 1_700_000_300}, latestVisibleTS: 1_700_000_300},
	}
	sortByVisibleLatest(calibrated)

	gotOrder := []string{calibrated[0].pb.key, calibrated[1].pb.key, calibrated[2].pb.key}
	wantOrder := []string{"gA", "gB", "gHidden"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("visible-latest order = %v; want %v (gHidden must not jump the order via its hidden newest match)", gotOrder, wantOrder)
		}
	}
}

// TestSortByVisibleLatest_StableTiebreak — equal visible latest_at keeps the OS
// candidate order (input order) as a deterministic tiebreak.
func TestSortByVisibleLatest_StableTiebreak(t *testing.T) {
	calibrated := []calibratedBucket{
		{pb: parsedBucket{key: "first"}, latestVisibleTS: 1_700_000_000},
		{pb: parsedBucket{key: "second"}, latestVisibleTS: 1_700_000_000},
		{pb: parsedBucket{key: "third"}, latestVisibleTS: 1_700_000_000},
	}
	sortByVisibleLatest(calibrated)
	for i, want := range []string{"first", "second", "third"} {
		if calibrated[i].pb.key != want {
			t.Errorf("stable tiebreak broken at %d: got %q want %q", i, calibrated[i].pb.key, want)
		}
	}
}

// --- response shell + has_more (parse from a synthetic OS aggregation) ------

func aggResult(t *testing.T, raw string) *elastic.SearchResult {
	t.Helper()
	var aggs elastic.Aggregations
	if err := json.Unmarshal([]byte(raw), &aggs); err != nil {
		t.Fatalf("bad test aggregation json: %v", err)
	}
	return &elastic.SearchResult{Aggregations: aggs}
}

func TestParseGroupsAggregation_HasMoreAndTotals(t *testing.T) {
	// sum_other_doc_count > 0 → more groups than the bucket cap → has_more.
	raw := `{
	  "group_count": {"value": 37},
	  "by_channel": {
	    "doc_count_error_upper_bound": 0,
	    "sum_other_doc_count": 5,
	    "buckets": [
	      {"key":"gA","doc_count":12,"latest":{"value":1700000000.0},"preview":{"hits":{"total":1,"hits":[]}}},
	      {"key":"gB","doc_count":3,"latest":{"value":1699999000.0},"preview":{"hits":{"total":1,"hits":[]}}}
	    ]
	  }
	}`
	buckets, total, hasMore, ok := parseGroupsAggregation(aggResult(t, raw), 20)
	if !ok {
		t.Fatal("expected ok=true for a well-formed aggregation")
	}
	if total != 37 {
		t.Errorf("total_groups=37 expected; got %d", total)
	}
	if !hasMore {
		t.Errorf("has_more must be true when sum_other_doc_count>0")
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets; got %d", len(buckets))
	}
	if buckets[0].key != "gA" || buckets[0].docCount != 12 || buckets[0].latestTS != 1700000000 {
		t.Errorf("bucket[0] parse mismatch: %+v", buckets[0])
	}
}

func TestParseGroupsAggregation_NoMore(t *testing.T) {
	raw := `{
	  "group_count": {"value": 2},
	  "by_channel": {"sum_other_doc_count": 0, "buckets": [
	    {"key":"gA","doc_count":1,"latest":{"value":1700000000.0},"preview":{"hits":{"total":1,"hits":[]}}}
	  ]}
	}`
	_, total, hasMore, ok := parseGroupsAggregation(aggResult(t, raw), 20)
	if !ok || total != 2 || hasMore {
		t.Errorf("expected ok/total=2/has_more=false; got ok=%v total=%d hasMore=%v", ok, total, hasMore)
	}
}

// TestGroupsEnvelope_Shape pins the {data, pagination} wire shell so the
// frontend's literal next_cursor==="" check and the approx flags hold.
func TestGroupsEnvelope_Shape(t *testing.T) {
	result := GroupsResult{
		Sequence:          1042,
		QueryID:           "q1",
		TotalGroups:       37,
		TotalGroupsApprox: true,
		Groups:            []GroupBucket{},
	}
	b, err := json.Marshal(groupsEnvelope(result, true))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"data"`, `"pagination"`, `"has_more":true`, `"next_cursor":""`,
		`"sequence":1042`, `"query_id":"q1"`, `"total_groups":37`,
		`"total_groups_approx":true`, `"groups":[]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("envelope missing %s:\n%s", want, s)
		}
	}
}

func TestEmptyGroupsResult_Shape(t *testing.T) {
	r := emptyGroupsResult(7)
	if r.Sequence != 7 || !r.TotalGroupsApprox || r.TotalGroups != 0 || r.Groups == nil {
		t.Errorf("empty result shape wrong: %+v", r)
	}
	if b, _ := json.Marshal(r); !strings.Contains(string(b), `"groups":[]`) {
		t.Errorf("empty groups must serialise as []: %s", b)
	}
}

// TestClassifyGroupBucket covers the three bucket-key shapes directly.
func TestClassifyGroupBucket(t *testing.T) {
	loginUID := "me"
	dmKey := fakeChannelIDFor(loginUID, "peer")
	if ct, wire, _, _ := classifyGroupBucket(dmKey, loginUID); ct != channelTypePerson || wire != "peer" {
		t.Errorf("DM classify wrong: ct=%d wire=%s", ct, wire)
	}
	thrKey := thread.BuildChannelID("gA", "s1")
	if ct, _, pg, sid := classifyGroupBucket(thrKey, loginUID); ct != channelTypeThread || pg != "gA" || sid != "s1" {
		t.Errorf("thread classify wrong: ct=%d pg=%s sid=%s", ct, pg, sid)
	}
	if ct, wire, pg, _ := classifyGroupBucket("gPlain", loginUID); ct != channelTypeGroup || wire != "gPlain" || pg != "gPlain" {
		t.Errorf("group classify wrong: ct=%d wire=%s pg=%s", ct, wire, pg)
	}
}
