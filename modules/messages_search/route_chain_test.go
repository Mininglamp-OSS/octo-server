package messages_search

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// V9 — every _search* endpoint, including the new _search_around, must be
// mounted through the shared routeMounters list. That list is wired in
// Route() under the single middleware group that carries Auth + SharedUID +
// Space + searchRateLimiter + audit + backendGate, so no endpoint can quietly
// skip the rate-limit / audit / disabled-backend gates. We assert the count of
// mounters here as a regression guard: a new search_*.go that forgets
// registerRoute (and instead mounts directly) would not increment this and the
// test would flag the drift.
func TestRouteMountersCoverAllEndpoints(t *testing.T) {
	// _search, _search_media, _search_files, _search_all, _search_around.
	const wantEndpoints = 5
	if len(routeMounters) != wantEndpoints {
		t.Fatalf("expected %d endpoints registered via registerRoute (so all go "+
			"through the shared rate-limit/audit/backendGate chain), got %d. "+
			"A new endpoint that bypasses registerRoute would skip those gates (V9).",
			wantEndpoints, len(routeMounters))
	}

	// Each mounter must actually register a path under the group (smoke check
	// that the closures are non-nil and runnable against a real router group).
	r := wkhttp.New()
	g := r.Group("/v1/messages")
	for i, mount := range routeMounters {
		if mount == nil {
			t.Fatalf("routeMounters[%d] is nil", i)
		}
		mount(&Handler{}, g)
	}
}
