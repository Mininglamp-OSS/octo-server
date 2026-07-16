package messages_search

import "testing"

func cb(docCount, latestTS int64) calibratedBucket {
	return calibratedBucket{pb: parsedBucket{docCount: docCount, latestTS: latestTS}}
}

// TestAllocatePreviewN_FloorAndClamp — every bucket gets at least 1 and never
// more than perGroupMax (§4: 老/低频群 N=1).
func TestAllocatePreviewN_FloorAndClamp(t *testing.T) {
	buckets := []calibratedBucket{cb(1, 1), cb(1, 1), cb(10_000, 1)}
	ns := allocatePreviewN(buckets, 500, 20)
	if len(ns) != 3 {
		t.Fatalf("want 3 allocations; got %d", len(ns))
	}
	for i, n := range ns {
		if n < 1 {
			t.Errorf("bucket %d must get at least 1; got %d", i, n)
		}
		if n > 20 {
			t.Errorf("bucket %d must be clamped to perGroupMax=20; got %d", i, n)
		}
	}
	// The dominant-frequency bucket saturates to the perGroupMax clamp.
	if ns[2] != 20 {
		t.Errorf("high-frequency bucket should clamp to 20; got %d", ns[2])
	}
	// Low-frequency buckets fall to the floor.
	if ns[0] != 1 || ns[1] != 1 {
		t.Errorf("low-frequency buckets should sit at the floor of 1; got %d,%d", ns[0], ns[1])
	}
}

// TestAllocatePreviewN_FrequencyWeighted — a more active bucket gets no fewer
// preview rows than a less active one.
func TestAllocatePreviewN_FrequencyWeighted(t *testing.T) {
	buckets := []calibratedBucket{cb(5, 1), cb(50, 1), cb(500, 1)}
	ns := allocatePreviewN(buckets, 60, 20)
	if !(ns[0] <= ns[1] && ns[1] <= ns[2]) {
		t.Errorf("N must be non-decreasing with frequency; got %v", ns)
	}
}

// TestAllocatePreviewN_BudgetCeiling — the total never exceeds previewBudget.
func TestAllocatePreviewN_BudgetCeiling(t *testing.T) {
	buckets := []calibratedBucket{cb(100, 1), cb(200, 1), cb(300, 1), cb(400, 1)}
	budget := 30
	ns := allocatePreviewN(buckets, budget, 20)
	sum := 0
	for _, n := range ns {
		sum += n
	}
	if sum > budget {
		t.Errorf("total preview rows %d must not exceed budget %d (%v)", sum, budget, ns)
	}
}

// TestAllocatePreviewN_Empty — no buckets → no allocations.
func TestAllocatePreviewN_Empty(t *testing.T) {
	if got := allocatePreviewN(nil, 500, 20); len(got) != 0 {
		t.Errorf("empty buckets → empty allocation; got %v", got)
	}
}
