package messages_search

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/olivere/elastic"
)

// presenceHit builds a synthetic OS SearchHit with a typed _source and the
// time_desc sort tuple [timestamp, messageId, subSeq] so projectDocRef and
// buildSearchAfterFromHit both read it the way the production top_hits /
// deep-probe hits are shaped.
func presenceHit(id int64, seq uint64, channelID string, channelType uint32, ts int64) *elastic.SearchHit {
	doc := Doc{
		MessageID:   id,
		MessageSeq:  seq,
		From:        "sender",
		ChannelID:   channelID,
		ChannelType: channelType,
		Timestamp:   ts,
	}
	body, _ := json.Marshal(doc)
	src := json.RawMessage(body)
	return &elastic.SearchHit{
		Source: &src,
		Sort:   []any{float64(ts), float64(id), float64(0)},
	}
}

func presenceHandler(p visibilityProbe, k2 int) *Handler {
	return &Handler{
		Log:        log.NewTLog("messages_search-presence-test"),
		cfg:        SearchConfig{Groups: GroupAggConfig{K2: k2, PerGroupMax: 20, PresenceProbe: 20, PreviewBudget: 500}},
		visibility: p,
	}
}

// deletedIDs turns a list of message ids into the stubProbe globally-deleted
// map (message_extra.is_deleted=1 — one of the MySQL-only gates OS can't see).
func deletedIDs(ids ...int64) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[strconv.FormatInt(id, 10)] = true
	}
	return m
}

// --- classifyPresence: the three-state boundary (§2.2, tests ①⑤) -----------

func TestClassifyPresence(t *testing.T) {
	cases := []struct {
		name       string
		sampleSize int
		docCount   int64
		visible    int
		want       presenceVerdict
	}{
		{"has visible", 40, 128, 3, presenceInclude},
		{"unique hit filtered (①)", 1, 1, 0, presenceExclude},
		{"T==doc_count 0 visible (⑤)", 40, 40, 0, presenceExclude},
		{"sample covers all, none visible", 5, 5, 0, presenceExclude},
		{"recent T hidden, more behind", 40, 200, 0, presenceUnresolved},
		{"one short of full, none visible", 39, 40, 0, presenceUnresolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPresence(tc.sampleSize, tc.docCount, tc.visible); got != tc.want {
				t.Errorf("classifyPresence(%d,%d,%d)=%v want %v", tc.sampleSize, tc.docCount, tc.visible, got, tc.want)
			}
		})
	}
}

// --- calibratePresence: INCLUDE keeps only visible, EXCLUDE drops the bucket -

// TestCalibratePresence_IncludeFiltersPreview is the A-review Blocker guard: a
// bucket whose top-T mixes visible + admin-deleted hits must INCLUDE only the
// visible ones — the deleted message's hit never survives into preview.
func TestCalibratePresence_IncludeFiltersPreview(t *testing.T) {
	// hit 10 visible, hit 11 globally-deleted, hit 12 visible.
	pb := parsedBucket{
		key:      "gA",
		docCount: 3,
		latestTS: 1_700_000_200,
		hits: []*elastic.SearchHit{
			presenceHit(10, 3, "gA", 2, 1_700_000_200),
			presenceHit(11, 2, "gA", 2, 1_700_000_100),
			presenceHit(12, 1, "gA", 2, 1_700_000_000),
		},
	}
	h := presenceHandler(&stubProbe{deleted: deletedIDs(11)}, 500)
	var tm searchPhaseTimings
	out, stats, err := h.calibratePresence(context.Background(), nil, nil, []parsedBucket{pb}, "me", &tm)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("bucket with visible hits must INCLUDE; got %d buckets", len(out))
	}
	if len(out[0].visible) != 2 {
		t.Fatalf("preview must carry only the 2 visible hits; got %d", len(out[0].visible))
	}
	for _, hit := range out[0].visible {
		if id, _ := lastHitMessageIDAndSubSeq(hit); id == 11 {
			t.Errorf("globally-deleted hit 11 leaked into preview")
		}
	}
	if stats.deepProbeBuckets != 0 {
		t.Errorf("no deep probe expected when the sample has a visible hit; got %d", stats.deepProbeBuckets)
	}
}

// TestCalibratePresence_ExcludeUniqueFiltered — case ①: a group whose only hit
// is filtered is dropped entirely (0 visible, sample covers all).
func TestCalibratePresence_ExcludeUniqueFiltered(t *testing.T) {
	pb := parsedBucket{
		key:      "gLonely",
		docCount: 1,
		hits:     []*elastic.SearchHit{presenceHit(50, 1, "gLonely", 2, 1_700_000_000)},
	}
	h := presenceHandler(&stubProbe{deleted: deletedIDs(50)}, 500)
	var tm searchPhaseTimings
	out, stats, err := h.calibratePresence(context.Background(), nil, nil, []parsedBucket{pb}, "me", &tm)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("group with its only hit filtered must EXCLUDE; got %d buckets", len(out))
	}
	if stats.deepProbeBuckets != 0 {
		t.Errorf("EXCLUDE (sample covers all) must not deep probe; got %d", stats.deepProbeBuckets)
	}
}

// TestCalibratePresence_FailClosed — a DB gate error propagates so the handler
// can surface INTERNAL_ERROR instead of leaking uncalibrated hits.
func TestCalibratePresence_FailClosed(t *testing.T) {
	pb := parsedBucket{key: "gA", docCount: 1, hits: []*elastic.SearchHit{presenceHit(10, 1, "gA", 2, 1_700_000_000)}}
	h := presenceHandler(&stubProbe{deletedErr: context.DeadlineExceeded}, 500)
	var tm searchPhaseTimings
	if _, _, err := h.calibratePresence(context.Background(), nil, nil, []parsedBucket{pb}, "me", &tm); err == nil {
		t.Fatal("expected calibrate to propagate the fail-closed DB error")
	}
}

// TestCalibratePresence_LatestVisibleTS is the RC regression: when a bucket's
// most-recent match is admin-deleted and an older message is visible, the
// calibrated latest_at must be the OLDER visible message's timestamp — never
// the hidden newest one's. This is the value assembleGroupBucket puts on the
// wire and buildGroupsResult sorts by, so a hidden newest match can no longer
// leak its recency or bias the bucket order.
func TestCalibratePresence_LatestVisibleTS(t *testing.T) {
	// hit 10 is the NEWEST match (ts 1_700_000_200) but admin-deleted; hit 11
	// (ts 1_700_000_100) is the newest VISIBLE one.
	pb := parsedBucket{
		key:      "gA",
		docCount: 2,
		latestTS: 1_700_000_200, // OS pre-filter max — must NOT reach the wire
		hits: []*elastic.SearchHit{
			presenceHit(10, 2, "gA", 2, 1_700_000_200),
			presenceHit(11, 1, "gA", 2, 1_700_000_100),
		},
	}
	h := presenceHandler(&stubProbe{deleted: deletedIDs(10)}, 500)
	var tm searchPhaseTimings
	out, _, err := h.calibratePresence(context.Background(), nil, nil, []parsedBucket{pb}, "me", &tm)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("bucket with an older visible hit must INCLUDE; got %d", len(out))
	}
	if out[0].latestVisibleTS != 1_700_000_100 {
		t.Errorf("latest_at must be the visible hit's ts 1_700_000_100 (not the deleted newest 1_700_000_200); got %d", out[0].latestVisibleTS)
	}
	// The deleted newest hit must not have leaked into the visible set either.
	for _, hit := range out[0].visible {
		if id, _ := lastHitMessageIDAndSubSeq(hit); id == 10 {
			t.Errorf("admin-deleted newest hit 10 leaked into the visible set")
		}
	}
}

// TestCalibratePresence_DeepProbeLatestVisibleTS is the UNRESOLVED-path variant
// of the RC regression: every top-T hit is hidden, so the bucket only INCLUDEs
// via the deep probe — and its latest_at must be the deep-probe-found visible
// hit's timestamp, still never the OS pre-filter max.
func TestCalibratePresence_DeepProbeLatestVisibleTS(t *testing.T) {
	// docCount 3 with a top-T of 1 all-hidden hit → sample doesn't cover the
	// bucket → UNRESOLVED. deepProbeBucket reuses base as an OS query, so route
	// the probe through a Handler whose visibility hides only the newest.
	// Here we exercise deepProbeVisible directly for a deterministic stream.
	stream := []*elastic.SearchHit{
		presenceHit(20, 3, "gA", 2, 1_700_000_050), // hidden (older than top-T boundary)
		presenceHit(21, 2, "gA", 2, 1_700_000_040), // visible → newest visible
		presenceHit(22, 1, "gA", 2, 1_700_000_030), // visible
	}
	f := &fakeOS{stream: stream}
	h := presenceHandler(&stubProbe{deleted: deletedIDs(20)}, 500)
	var tm searchPhaseTimings
	found, vis, _, err := h.deepProbeVisible(context.Background(), "me", []any{float64(1_700_000_060), float64(19), float64(0)}, 500, f.fn(), &tm)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !found || len(vis) == 0 {
		t.Fatal("expected the probe to find a visible hit")
	}
	if got := latestVisibleTS(vis); got != 1_700_000_040 {
		t.Errorf("deep-probe latest_at must be the newest visible hit ts 1_700_000_040; got %d", got)
	}
}

// --- deepProbeVisible: first-visible / exhausted / K2 budget (②③⑥⑦) --------

// fakeOS models an OpenSearch search_after stream: it returns up to `size` hits
// per call from a flat, time-desc-ordered slice, advancing an internal cursor.
// A page shorter than the requested size means the stream is exhausted — the
// same signal the real deep probe reads. gotSizes records the requested sizes
// so the budget-driven page trimming can be asserted.
type fakeOS struct {
	stream   []*elastic.SearchHit
	idx      int
	gotSizes []int
}

func (f *fakeOS) fn() osQueryFn {
	return func(_ []any, size int) ([]*elastic.SearchHit, error) {
		f.gotSizes = append(f.gotSizes, size)
		if f.idx >= len(f.stream) {
			return nil, nil
		}
		end := f.idx + size
		if end > len(f.stream) {
			end = len(f.stream)
		}
		page := f.stream[f.idx:end]
		f.idx = end
		return page, nil
	}
}

func hidden(base int64, n int) []*elastic.SearchHit {
	out := make([]*elastic.SearchHit, n)
	for i := 0; i < n; i++ {
		id := base + int64(i)
		out[i] = presenceHit(id, uint64(10000-int(id)), "gA", 2, 900-int64(i))
	}
	return out
}

// TestDeepProbeVisible_FirstVisibleInclude — case ②: the recent T are all
// hidden but an older doc is visible → INCLUDE at that hit.
func TestDeepProbeVisible_FirstVisibleInclude(t *testing.T) {
	// 20,21,23 deleted; 22 visible (time-desc order 20,21,22,23).
	stream := []*elastic.SearchHit{
		presenceHit(20, 20, "gA", 2, 900), presenceHit(21, 19, "gA", 2, 890),
		presenceHit(22, 18, "gA", 2, 880), presenceHit(23, 17, "gA", 2, 870),
	}
	f := &fakeOS{stream: stream}
	h := presenceHandler(&stubProbe{deleted: deletedIDs(20, 21, 23)}, 500)
	var tm searchPhaseTimings
	found, vis, scanned, err := h.deepProbeVisible(context.Background(), "me", []any{float64(900), float64(20), float64(0)}, 500, f.fn(), &tm)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !found {
		t.Fatal("expected INCLUDE once an older visible hit is found (②)")
	}
	if len(vis) != 1 {
		t.Fatalf("expected 1 visible hit on the found page; got %d", len(vis))
	}
	if id, _ := lastHitMessageIDAndSubSeq(vis[0]); id != 22 {
		t.Errorf("expected visible hit 22; got %d", id)
	}
	if scanned != 4 {
		t.Errorf("scanned should count the page (4); got %d", scanned)
	}
}

// TestDeepProbeVisible_PagesAdvance — case ⑦ (self-consistency): the probe
// pages old-ward via search_after, filtering each page exactly once against
// recall-time state and never re-scanning an earlier page, until it hits a
// visible doc on a later page.
func TestDeepProbeVisible_PagesAdvance(t *testing.T) {
	stream := append(hidden(100, deepProbePageSize), presenceHit(9999, 1, "gA", 2, 100)) // 50 hidden + 1 visible
	del := make([]int64, deepProbePageSize)
	for i := range del {
		del[i] = 100 + int64(i)
	}
	probe := &stubProbe{deleted: deletedIDs(del...)}
	f := &fakeOS{stream: stream}
	h := presenceHandler(probe, 500)
	var tm searchPhaseTimings
	found, vis, scanned, err := h.deepProbeVisible(context.Background(), "me", nil, 500, f.fn(), &tm)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !found || len(vis) != 1 {
		t.Fatalf("expected INCLUDE on the second page; found=%v vis=%d", found, len(vis))
	}
	if id, _ := lastHitMessageIDAndSubSeq(vis[0]); id != 9999 {
		t.Errorf("expected the visible hit 9999 from page 2; got %d", id)
	}
	if scanned != deepProbePageSize+1 {
		t.Errorf("scanned must span both pages (%d); got %d", deepProbePageSize+1, scanned)
	}
	// filterVisible runs once per page (2 pages) — no earlier page re-scanned.
	if probe.deletedCalls != 2 {
		t.Errorf("expected one filterVisible pass per page (2); got %d", probe.deletedCalls)
	}
}

// TestDeepProbeVisible_ChannelExhausted — case ③: M_group ≤ K2 fully scanned,
// all hidden → EXCLUDE. A short first page signals exhaustion.
func TestDeepProbeVisible_ChannelExhausted(t *testing.T) {
	stream := []*elastic.SearchHit{presenceHit(30, 5, "gA", 2, 800), presenceHit(31, 4, "gA", 2, 790)}
	f := &fakeOS{stream: stream}
	h := presenceHandler(&stubProbe{deleted: deletedIDs(30, 31)}, 500)
	var tm searchPhaseTimings
	found, _, scanned, err := h.deepProbeVisible(context.Background(), "me", nil, 500, f.fn(), &tm)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if found {
		t.Fatal("fully-scanned all-hidden channel must EXCLUDE (③)")
	}
	if scanned != 2 {
		t.Errorf("scanned=2 expected; got %d", scanned)
	}
}

// TestDeepProbeVisible_K2BudgetExhausted — case ⑥: the probe never scans past
// the K2-anchored budget; a visible hit sitting beyond it is a metered residual
// and the bucket is EXCLUDEd. Budget 4 → first (and only) page trimmed to 4.
func TestDeepProbeVisible_K2BudgetExhausted(t *testing.T) {
	// A long all-hidden stream with a visible hit far past the budget.
	stream := append(hidden(100, 20), presenceHit(9999, 1, "gA", 2, 100))
	del := make([]int64, 20)
	for i := range del {
		del[i] = 100 + int64(i)
	}
	f := &fakeOS{stream: stream}
	h := presenceHandler(&stubProbe{deleted: deletedIDs(del...)}, 500)
	var tm searchPhaseTimings
	found, _, scanned, err := h.deepProbeVisible(context.Background(), "me", nil, 4, f.fn(), &tm)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if found {
		t.Fatal("budget-bounded probe must EXCLUDE when the visible hit is past K2 (⑥)")
	}
	if scanned > 4 {
		t.Errorf("scanned must not exceed the budget of 4; got %d", scanned)
	}
	if len(f.gotSizes) == 0 || f.gotSizes[0] != 4 {
		t.Errorf("first page size must be trimmed to the residual budget 4; got %v", f.gotSizes)
	}
}
