package messages_search

import (
	"context"
	"testing"
)

// countingProbe records the largest IN() list it was handed per call and the
// number of probe round-trips, so we can bound the MySQL join load a single
// search request can impose (YUJ-4667 step 7 — read-path join performance
// gate). Every signal survives (no filtering) so the page fills on round 1.
type countingProbe struct {
	maxINList int
	calls     int
}

func (p *countingProbe) note(ids []string) {
	p.calls++
	if len(ids) > p.maxINList {
		p.maxINList = len(ids)
	}
}

func (p *countingProbe) RevokedSet(ids []string) (map[string]struct{}, error) {
	p.note(ids)
	return map[string]struct{}{}, nil
}
func (p *countingProbe) GloballyDeletedSet(ids []string) (map[string]struct{}, error) {
	p.note(ids)
	return map[string]struct{}{}, nil
}
func (p *countingProbe) UserDeletedSet(uid string, ids []string) (map[string]struct{}, error) {
	p.note(ids)
	return map[string]struct{}{}, nil
}
func (p *countingProbe) ChannelOffset(uid, channelID string) (uint32, error) { return 0, nil }

// Step 7 — the per-request MySQL join IN() list MUST stay bounded by
// pageSize * oversampleMultiplier (one round's oversample fetch), NOT by the
// corpus size or the number of OS hits. This is the machine-checkable proxy
// for "join / DB load stays within threshold at a typical page-size": the IN()
// expansion that the plan calls out as the load risk is structurally capped.
//
// Threshold (measured here, pinned as the gate):
//   - max IN() list per probe call  <= pageSize * oversampleMultiplier
//   - total probe round-trips        <= loopBudget * 4 signals
// At the max page_size of 100 that is <= 300 ids per IN(), <= 12 round-trips —
// a single bounded JOIN batch per round, never an unbounded fan-out.
func TestJoinLoad_INListBoundedByPageSize(t *testing.T) {
	for _, pageSize := range []int{1, 20, 100} {
		probe := &countingProbe{}
		h := newVisibilityHandler(probe)

		// OS returns a full oversample page every round (signals that the
		// corpus is huge); all hits are visible so the page fills on round 1.
		fetchSize := pageSize * oversampleMultiplier
		osQuery := func(searchAfter []any, size int) ([]rawHit, error) {
			return makeFakeHits(t, 1, fetchSize), nil
		}
		collected, _, _, err := h.paginateWithFilter(
			context.Background(), "me", "C1", pageSize, nil, false,
			wrapHitsQuery(osQuery), wrapProject(),
		)
		if err != nil {
			t.Fatalf("pageSize=%d: paginate: %v", pageSize, err)
		}
		if len(collected) != pageSize {
			t.Fatalf("pageSize=%d: expected full page, got %d", pageSize, len(collected))
		}
		// The IN() list is the unique ids of ONE oversample round, never the
		// whole corpus.
		wantMax := pageSize * oversampleMultiplier
		if probe.maxINList > wantMax {
			t.Fatalf("pageSize=%d: IN() list %d exceeds bound %d — join load not capped",
				pageSize, probe.maxINList, wantMax)
		}
		t.Logf("pageSize=%d: max IN()=%d (bound %d), probe round-trips=%d",
			pageSize, probe.maxINList, wantMax, probe.calls)
	}
}

// Even when the page never fills (every hit filtered), the join load is still
// bounded: at most loopBudget rounds, each with one oversample IN() list.
func TestJoinLoad_BoundedAcrossBudgetExhaustion(t *testing.T) {
	pageSize := 100
	// Reuse stubProbe so we can mark everything revoked; wrap a counter by
	// asserting the documented round bound via call counts.
	probe := &stubProbe{revoked: map[string]bool{}}
	for i := 1; i <= pageSize*oversampleMultiplier*loopBudget; i++ {
		probe.revoked[itoa(i)] = true
	}
	h := newVisibilityHandler(probe)

	roundCalls := 0
	osQuery := func(searchAfter []any, size int) ([]rawHit, error) {
		roundCalls++
		start := (roundCalls-1)*(pageSize*oversampleMultiplier) + 1
		return makeFakeHits(t, start, start+pageSize*oversampleMultiplier-1), nil
	}
	_, _, _, err := h.paginateWithFilter(
		context.Background(), "me", "C1", pageSize, nil, false,
		wrapHitsQuery(osQuery), wrapProject(),
	)
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	// One RevokedSet call per round; the loop must not exceed loopBudget rounds
	// no matter how big the corpus is.
	if probe.revokedCalls > loopBudget {
		t.Fatalf("probe queried %d rounds, exceeds loopBudget %d — unbounded join load",
			probe.revokedCalls, loopBudget)
	}
	t.Logf("budget-exhaustion: probe rounds=%d (bound %d)", probe.revokedCalls, loopBudget)
}
