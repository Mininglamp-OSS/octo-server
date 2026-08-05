package card_template_catalog

// PR-C D5 evidence for the discovery request plumbing.
//
// These cover the parts that decide *what a caller can reach* before any store
// is consulted: the cursor's Space binding, the exact-ref requirement, the
// conditional-request comparison and the page bounds. Each one is a place where
// being permissive would either leak the private catalog or quietly change what
// a paginating caller sees.

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

// A cursor is bound to the Space it was issued for. Replaying one against a
// different Space must restart the caller rather than resume a scan whose
// visibility predicate no longer holds — otherwise a cursor obtained in a Space
// with broad grants becomes a way to page through another Space's view.
func TestDiscoveryCursorIsBoundToItsSpace(t *testing.T) {
	issued := encodeDiscoveryCursor(discoveryCursor{
		Version: discoveryCursorVersion, Space: "space-1",
		Source: discoverySourceDynamic, ID: "pilot.card", Exact: "1.0.0",
	})
	if issued == "" {
		t.Fatal("cursor did not encode")
	}

	decoded, ok := decodeDiscoveryCursor(issued, "space-1")
	if !ok || decoded.ID != "pilot.card" || decoded.Exact != "1.0.0" ||
		decoded.Source != discoverySourceDynamic {
		t.Fatalf("round trip = %+v ok=%v", decoded, ok)
	}

	for _, replay := range []string{"space-2", ""} {
		if _, ok := decodeDiscoveryCursor(issued, replay); ok {
			t.Fatalf("cursor issued for space-1 was accepted in %q", replay)
		}
	}
}

func TestDiscoveryCursorRejectsWhatItCannotAccountFor(t *testing.T) {
	for _, test := range []struct {
		name    string
		encoded string
	}{
		{"not base64", "!!!not-base64!!!"},
		{"not json", "bm90LWpzb24"},
		{"unknown cursor version", encodeDiscoveryCursor(discoveryCursor{
			Version: discoveryCursorVersion + 1, Space: "space-1", Source: discoverySourceStatic})},
		{"unknown source", encodeDiscoveryCursor(discoveryCursor{
			Version: discoveryCursorVersion, Space: "space-1", Source: "elsewhere"})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := decodeDiscoveryCursor(test.encoded, "space-1"); ok {
				t.Fatal("cursor was accepted")
			}
		})
	}

	// An absent cursor is not an error: it is the first page, and it starts on
	// the static source so the two sources are paged in a fixed order.
	first, ok := decodeDiscoveryCursor("", "space-1")
	if !ok || first.Source != discoverySourceStatic || first.ID != "" {
		t.Fatalf("empty cursor = %+v ok=%v", first, ok)
	}
}

// B2 requires an exact version. A bare ID would have to resolve through the
// activation pointer, and a discovery read that followed the pointer would
// describe a different contract from one request to the next.
func TestParseDiscoveryRefRequiresAnExactVersion(t *testing.T) {
	id, version, ok := parseDiscoveryRef("pilot.docs-card@0.4.0-pilot.1")
	if !ok || id != cardtmpl.ID("pilot.docs-card") || version != "0.4.0-pilot.1" {
		t.Fatalf("ref = %s@%s ok=%v", id, version, ok)
	}
	for _, malformed := range []string{"", "pilot.docs-card", "@1.0.0", "pilot.docs-card@", "   "} {
		if _, _, ok := parseDiscoveryRef(malformed); ok {
			t.Fatalf("malformed ref %q was accepted", malformed)
		}
	}
}

func TestMatchesETagHandlesListsWildcardAndWeakValidators(t *testing.T) {
	const etag = `"abc123"`
	for _, test := range []struct {
		header string
		want   bool
	}{
		{"", false},
		{`"abc123"`, true},
		{`W/"abc123"`, true},
		{`"other", "abc123"`, true},
		{"*", true},
		{`"other"`, false},
		{`"abc1234"`, false},
	} {
		if got := matchesETag(test.header, etag); got != test.want {
			t.Fatalf("If-None-Match %q matched = %v, want %v", test.header, got, test.want)
		}
	}
}

func TestDiscoveryPageLimitBounds(t *testing.T) {
	if limit, ok := discoveryPageLimit(""); !ok || limit != defaultDiscoveryPageSize {
		t.Fatalf("default limit = %d ok=%v", limit, ok)
	}
	if limit, ok := discoveryPageLimit("100"); !ok || limit != maxDiscoveryPageSize {
		t.Fatalf("max limit = %d ok=%v", limit, ok)
	}
	// Out-of-range is a rejection, not a clamp: silently returning a different
	// page size than asked for makes a caller's own paging arithmetic wrong.
	for _, raw := range []string{"0", "-1", "101", "abc", "1e2"} {
		if _, ok := discoveryPageLimit(raw); ok {
			t.Fatalf("limit %q was accepted", raw)
		}
	}
}

// boundedDiscoveryLimit is the store's own clamp, which exists so a caller that
// reached it through a different path than the handler still cannot ask for an
// unbounded page.
func TestBoundedDiscoveryLimitClampsAtTheStore(t *testing.T) {
	if got := boundedDiscoveryLimit(0); got != defaultDiscoveryPageSize {
		t.Fatalf("zero limit = %d", got)
	}
	if got := boundedDiscoveryLimit(1_000); got != maxDiscoveryPageSize {
		t.Fatalf("oversized limit = %d", got)
	}
	if got := boundedDiscoveryLimit(7); got != 7 {
		t.Fatalf("in-range limit = %d", got)
	}
}

// fakeDiscoveryStore records what the handler asked for so the tests can assert
// on the shape of the probe, not only on the answer.
type fakeDiscoveryStore struct {
	rows      []DiscoveryRow
	hasMore   bool
	listErr   error
	listCalls []int
	probedIDs [][]cardtmpl.ID
	granted   map[cardtmpl.ID]struct{}
}

func (s *fakeDiscoveryStore) ListDiscoverable(
	_ context.Context, _ string, _ cardtmpl.ID, _ string, limit int,
) ([]DiscoveryRow, bool, error) {
	s.listCalls = append(s.listCalls, limit)
	if s.listErr != nil {
		return nil, false, s.listErr
	}
	if len(s.rows) > limit {
		return s.rows[:limit], true, nil
	}
	return s.rows, s.hasMore, nil
}

func (s *fakeDiscoveryStore) LoadDiscoverable(
	_ context.Context, _ string, _ cardtmpl.ID, _ string,
) (DiscoveryRow, error) {
	return DiscoveryRow{}, ErrDiscoveryNotVisible
}

func (s *fakeDiscoveryStore) StaticDiscoverGrants(
	_ context.Context, _ string, ids []cardtmpl.ID,
) (map[cardtmpl.ID]struct{}, error) {
	s.probedIDs = append(s.probedIDs, append([]cardtmpl.ID(nil), ids...))
	if s.granted == nil {
		return map[cardtmpl.ID]struct{}{}, nil
	}
	return s.granted, nil
}

// Review follow-up (#14): the private-ID probe is keyed by template ID. A card
// registered at several versions must not take several slots in the bounded
// IN-list, or a genuinely granted template can fall past the cut and be
// silently treated as ungranted.
func TestPrivateTemplateIDsDeduplicatesAndDropsPublic(t *testing.T) {
	entries := []cardtmpl.StaticCatalogEntry{
		{ID: "docs.access-request", Version: "0.2.0", Visibility: cardtmpl.CatalogVisibilityPrivate},
		{ID: "docs.access-request", Version: "0.3.0", Visibility: cardtmpl.CatalogVisibilityPrivate},
		{ID: "ai.reasoning-process", Version: "0.1.0", Visibility: cardtmpl.CatalogVisibilityPrivate},
		{ID: "ai.reasoning-process", Version: "0.2.0", Visibility: cardtmpl.CatalogVisibilityPrivate},
		{ID: "ai.reasoning-process", Version: "0.3.0", Visibility: cardtmpl.CatalogVisibilityPrivate},
		{ID: "docs.shared", Version: "1.0.0", Visibility: cardtmpl.CatalogVisibilityPublic},
	}
	got := privateTemplateIDs(entries)
	want := []cardtmpl.ID{"docs.access-request", "ai.reasoning-process"}
	if len(got) != len(want) {
		t.Fatalf("probe ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("probe ids = %v, want %v (registration order preserved)", got, want)
		}
	}
	// A public template needs no grant, so probing for one would be pure noise.
	for _, id := range got {
		if id == "docs.shared" {
			t.Fatal("a public template was probed for a discover grant")
		}
	}
}

// Review follow-up (#13): when the static source exactly fills the page,
// has_more must reflect whether the dynamic source actually holds a row.
func TestDynamicPageFollowsPeeksInsteadOfAssuming(t *testing.T) {
	empty := &fakeDiscoveryStore{}
	cursor, more, err := dynamicPageFollows(context.Background(), empty, "space-1")
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if more || cursor != "" {
		t.Fatalf("empty dynamic source advertised another page: cursor=%q more=%v", cursor, more)
	}
	// The peek answers "is there anything" — it must not fetch a page nobody
	// asked for.
	if len(empty.listCalls) != 1 || empty.listCalls[0] != 1 {
		t.Fatalf("peek budgets = %v, want [1]", empty.listCalls)
	}

	stocked := &fakeDiscoveryStore{rows: []DiscoveryRow{{ID: "pilot.card", Version: "1.0.0"}}}
	cursor, more, err = dynamicPageFollows(context.Background(), stocked, "space-1")
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if !more || cursor == "" {
		t.Fatalf("stocked dynamic source did not advertise another page: cursor=%q more=%v", cursor, more)
	}
	decoded, ok := decodeDiscoveryCursor(cursor, "space-1")
	if !ok || decoded.Source != discoverySourceDynamic || decoded.ID != "" {
		t.Fatalf("boundary cursor = %+v ok=%v, want the top of the dynamic source", decoded, ok)
	}

	failing := &fakeDiscoveryStore{listErr: errors.New("db unavailable")}
	if _, _, err := dynamicPageFollows(context.Background(), failing, "space-1"); err == nil {
		t.Fatal("a failed peek was reported as no-more-pages")
	}
}
