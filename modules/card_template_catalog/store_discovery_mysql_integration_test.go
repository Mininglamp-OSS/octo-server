package card_template_catalog

// PR-C D5 real-MySQL evidence for the discovery read model.
//
// The claim these tests exist to defend is an ordering one: visibility, block
// state and the caller's discover grant are settled *inside* the predicate, so
// they apply before LIMIT. That is a property of the SQL, not of the Go around
// it — sqlmock would only replay whatever rows the test itself handed back, so
// it can assert the arguments but never the predicate. The distinction matters
// because the failure mode of post-filtering is silent: pages get shorter, the
// has_more flag starts lying, and the number of missing rows becomes a count
// oracle over the private catalog.
//
// The manager index is exercised in the same file because it is the deliberate
// opposite: no visibility predicate at all, since hiding blocked and disabled
// templates from an operator would remove exactly the rows an incident is
// about. Asserting both against one seeded fixture is what makes the contrast
// checkable rather than merely documented.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const discoveryIntegrationSpaceID = "space-discovery"

// seedDiscoveryVersion writes one claim + artifact pair. blocked stamps the
// artifact the way Block does, which is the only signal ListDiscoverable uses.
func seedDiscoveryVersion(
	t *testing.T, db *sql.DB, id cardtmpl.ID, version, visibility string, blocked bool,
) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO card_template_version_claim
        (template_id, version, source) VALUES (?, ?, 'dynamic')`,
		string(id), version); err != nil {
		t.Fatalf("seed discovery claim %s@%s: %v", id, version, err)
	}
	blockedAt := "NULL"
	if blocked {
		blockedAt = "CURRENT_TIMESTAMP(6)"
	}
	if _, err := db.Exec(`INSERT INTO card_template_artifact
        (template_id, version, owner, visibility, engine_contract, protocol,
         contract_version, canonical_bundle, content_sha256, created_by, blocked_at)
        VALUES (?, ?, 'docs', ?, ?, ?, '1.0.0', ?, ?, 'admin-1', `+blockedAt+`)`,
		string(id), version, visibility, cardtmpl.JSONTemplateEngineV1, cardtmpl.Protocol,
		`{"canonical":true}`, strings.Repeat("b", 64)); err != nil {
		t.Fatalf("seed discovery artifact %s@%s: %v", id, version, err)
	}
}

func seedDiscoveryActivation(t *testing.T, db *sql.DB, id cardtmpl.ID, version string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO card_template_activation
        (template_id, active_version, status, revision, updated_by, reason, change_ticket)
        VALUES (?, ?, 'active', 1, 'admin-1', 'pilot', 'CHG-D1')`,
		string(id), version); err != nil {
		t.Fatalf("seed discovery activation %s: %v", id, err)
	}
}

// seedSpaceDiscoverGrant writes the only canonical shape a `space` principal
// has: global scope, discover-only. The CHECK constraints reject anything else,
// so a test that drifts from the D2 shape fails at INSERT rather than silently
// proving something the production writer could never create.
func seedSpaceDiscoverGrant(t *testing.T, db *sql.DB, id cardtmpl.ID, spaceID, status string) {
	t.Helper()
	discover := 1
	if status != string(GrantStatusActive) {
		discover = 0
	}
	if _, err := db.Exec(`INSERT INTO card_template_grant
        (template_id, principal_type, principal_id, scope_space_id, status,
         can_discover, can_send, can_edit, revision, updated_by, reason, change_ticket)
        VALUES (?, 'space', ?, '', ?, ?, 0, 0, 1, 'admin-1', 'pilot', 'CHG-D2')`,
		string(id), spaceID, status, discover); err != nil {
		t.Fatalf("seed space grant %s: %v", id, err)
	}
}

func discoveryIntegrationContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func discoveryRowIDs(rows []DiscoveryRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, string(row.ID)+"@"+row.Version)
	}
	return ids
}

// walkDiscoverable pages through the whole listing the way B1's cursor does and
// returns the flattened result. Anything the predicate hides must be absent
// from *every* page, not merely from the first.
func walkDiscoverable(
	t *testing.T, ctx context.Context, s *store, spaceID string, limit int,
) []DiscoveryRow {
	t.Helper()
	var (
		all          []DiscoveryRow
		afterID      cardtmpl.ID
		afterVersion string
	)
	for page := 0; page < 20; page++ {
		rows, more, err := s.ListDiscoverable(ctx, spaceID, afterID, afterVersion, limit)
		if err != nil {
			t.Fatalf("ListDiscoverable page %d: %v", page, err)
		}
		if len(rows) > limit {
			t.Fatalf("page %d returned %d rows over the limit %d", page, len(rows), limit)
		}
		if more && len(rows) != limit {
			t.Fatalf("page %d claims more rows follow but returned a short page of %d",
				page, len(rows))
		}
		all = append(all, rows...)
		if !more {
			return all
		}
		last := rows[len(rows)-1]
		afterID, afterVersion = last.ID, last.Version
	}
	t.Fatal("cursor never terminated")
	return nil
}

// seedDiscoveryFixture lays out one row of each kind the predicate must
// distinguish, interleaved in primary-key order so that a hidden row sits
// between two visible ones. That interleaving is the point: if filtering ran
// after LIMIT, the hidden middle row would eat a page slot and the walk below
// would come back short.
func seedDiscoveryFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	seedDiscoveryVersion(t, db, "test.disco-a-public", "1.0.0", cardtmpl.CatalogVisibilityPublic, false)
	seedDiscoveryVersion(t, db, "test.disco-b-private", "1.0.0", cardtmpl.CatalogVisibilityPrivate, false)
	seedDiscoveryVersion(t, db, "test.disco-c-granted", "1.0.0", cardtmpl.CatalogVisibilityPrivate, false)
	seedDiscoveryVersion(t, db, "test.disco-d-blocked", "1.0.0", cardtmpl.CatalogVisibilityPublic, true)
	seedDiscoveryVersion(t, db, "test.disco-e-revoked", "1.0.0", cardtmpl.CatalogVisibilityPrivate, false)
	seedDiscoveryVersion(t, db, "test.disco-f-public", "1.0.0", cardtmpl.CatalogVisibilityPublic, false)
	seedDiscoveryVersion(t, db, "test.disco-f-public", "2.0.0", cardtmpl.CatalogVisibilityPublic, false)
	seedDiscoveryActivation(t, db, "test.disco-f-public", "1.0.0")

	seedSpaceDiscoverGrant(t, db, "test.disco-c-granted", discoveryIntegrationSpaceID,
		string(GrantStatusActive))
	seedSpaceDiscoverGrant(t, db, "test.disco-e-revoked", discoveryIntegrationSpaceID,
		string(GrantStatusRevoked))
	// Another Space holds an active grant on the same private row. It must not
	// widen what this Space can see; a grant that leaked across Spaces would
	// make the whole per-Space model decorative.
	seedSpaceDiscoverGrant(t, db, "test.disco-b-private", "space-someone-else",
		string(GrantStatusActive))
}

// The visible set is exactly: public unblocked rows, plus private rows this
// Space holds an *active* discover grant on. Everything else — private without
// a grant, blocked regardless of visibility, and a tombstoned grant — is absent
// from every page.
func TestListDiscoverableFiltersBeforePagingRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedDiscoveryFixture(t, db)
	catalogStore := newStore(db)
	ctx := discoveryIntegrationContext(t)

	want := []string{
		"test.disco-a-public@1.0.0",
		"test.disco-c-granted@1.0.0",
		"test.disco-f-public@1.0.0",
		"test.disco-f-public@2.0.0",
	}
	// A limit of 1 forces every boundary to be a page boundary, so a hidden row
	// consuming a slot would show up as a short or empty page rather than as a
	// merely reordered result.
	for _, limit := range []int{1, 2, 50} {
		got := discoveryRowIDs(walkDiscoverable(t, ctx, catalogStore, discoveryIntegrationSpaceID, limit))
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("limit %d walked %v, want %v", limit, got, want)
		}
	}

	// The same walk from a Space with no grants at all sees only public rows.
	// The difference between the two sets is exactly the granted row, which is
	// what "the grant is applied in the predicate" means observationally.
	ungranted := discoveryRowIDs(walkDiscoverable(t, ctx, catalogStore, "space-no-grants", 2))
	wantUngranted := []string{
		"test.disco-a-public@1.0.0",
		"test.disco-f-public@1.0.0",
		"test.disco-f-public@2.0.0",
	}
	if strings.Join(ungranted, ",") != strings.Join(wantUngranted, ",") {
		t.Fatalf("ungranted Space walked %v, want %v", ungranted, wantUngranted)
	}
}

// has_more is a promise about the *next* page, and the +1 peek is how it is
// kept. A row that exists only beyond the limit must flip the flag without
// appearing in the page.
func TestListDiscoverableReportsMoreWithoutLeakingTheNextRowRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedDiscoveryFixture(t, db)
	catalogStore := newStore(db)
	ctx := discoveryIntegrationContext(t)

	rows, more, err := catalogStore.ListDiscoverable(ctx, discoveryIntegrationSpaceID, "", "", 1)
	if err != nil {
		t.Fatalf("ListDiscoverable: %v", err)
	}
	if len(rows) != 1 || !more {
		t.Fatalf("rows = %v more = %v", discoveryRowIDs(rows), more)
	}
	if rows[0].ID != "test.disco-a-public" {
		t.Fatalf("first row = %+v", rows[0])
	}

	// The last visible row must not claim a successor. Reading the peek row as
	// part of the page would report more=true here and hand the caller a cursor
	// that returns nothing.
	last, moreAfterLast, err := catalogStore.ListDiscoverable(
		ctx, discoveryIntegrationSpaceID, "test.disco-f-public", "1.0.0", 10)
	if err != nil {
		t.Fatalf("ListDiscoverable tail: %v", err)
	}
	if moreAfterLast || len(last) != 1 || last[0].Version != "2.0.0" {
		t.Fatalf("tail = %v more = %v", discoveryRowIDs(last), moreAfterLast)
	}
}

// active_for_new_send follows the activation pointer exactly. A template with
// two versions and a pointer at the older one must report the newer as
// inactive, or a producer would send against a version the runtime will not
// resolve.
func TestListDiscoverableReportsTheActivationPointerRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedDiscoveryFixture(t, db)
	catalogStore := newStore(db)
	ctx := discoveryIntegrationContext(t)

	rows := walkDiscoverable(t, ctx, catalogStore, discoveryIntegrationSpaceID, 50)
	active := map[string]bool{}
	for _, row := range rows {
		active[string(row.ID)+"@"+row.Version] = row.ActiveForNewSend
	}
	if !active["test.disco-f-public@1.0.0"] {
		t.Fatal("the pointed-at version is not reported active")
	}
	if active["test.disco-f-public@2.0.0"] {
		t.Fatal("a newer unpointed version is reported active")
	}
	if active["test.disco-a-public@1.0.0"] {
		t.Fatal("a template with no activation row is reported active")
	}
}

// B2 has one negative answer. Unknown, invisible and blocked must be
// indistinguishable at this layer, because the handler above cannot invent a
// distinction the store did not make.
func TestLoadDiscoverableGivesOneIndistinguishableMissRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedDiscoveryFixture(t, db)
	catalogStore := newStore(db)
	ctx := discoveryIntegrationContext(t)

	row, err := catalogStore.LoadDiscoverable(ctx, discoveryIntegrationSpaceID,
		"test.disco-c-granted", "1.0.0")
	if err != nil {
		t.Fatalf("granted private LoadDiscoverable: %v", err)
	}
	if row.Visibility != cardtmpl.CatalogVisibilityPrivate || row.Owner != "docs" {
		t.Fatalf("row = %+v", row)
	}

	for _, miss := range []struct {
		name    string
		id      cardtmpl.ID
		version string
	}{
		{"unknown template", "test.disco-nonexistent", "1.0.0"},
		{"known template, unknown version", "test.disco-a-public", "9.9.9"},
		{"private without a grant", "test.disco-b-private", "1.0.0"},
		{"private with a revoked grant", "test.disco-e-revoked", "1.0.0"},
		{"blocked but public", "test.disco-d-blocked", "1.0.0"},
	} {
		_, err := catalogStore.LoadDiscoverable(ctx, discoveryIntegrationSpaceID, miss.id, miss.version)
		if !errors.Is(err, ErrDiscoveryNotVisible) {
			t.Fatalf("%s: err = %v, want ErrDiscoveryNotVisible", miss.name, err)
		}
	}
}

// Static templates are private by default, so a granted static card is only
// visible through this probe. The probe must honor the same three predicates as
// the dynamic path — right Space, active status, discover permission.
func TestStaticDiscoverGrantsHonorsSpaceAndStatusRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedDiscoveryFixture(t, db)
	catalogStore := newStore(db)
	ctx := discoveryIntegrationContext(t)

	probe := []cardtmpl.ID{
		"test.disco-c-granted", "test.disco-e-revoked",
		"test.disco-b-private", "test.disco-nonexistent",
	}
	granted, err := catalogStore.StaticDiscoverGrants(ctx, discoveryIntegrationSpaceID, probe)
	if err != nil {
		t.Fatalf("StaticDiscoverGrants: %v", err)
	}
	if len(granted) != 1 {
		t.Fatalf("granted = %v, want only the active grant", granted)
	}
	if _, ok := granted["test.disco-c-granted"]; !ok {
		t.Fatalf("granted = %v", granted)
	}

	// An empty Space cannot hold a grant; probing with one must not degrade
	// into a query that matches the '' sentinel rows.
	empty, err := catalogStore.StaticDiscoverGrants(ctx, "  ", probe)
	if err != nil || len(empty) != 0 {
		t.Fatalf("blank Space: granted = %v err = %v", empty, err)
	}
	none, err := catalogStore.StaticDiscoverGrants(ctx, discoveryIntegrationSpaceID, nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("empty probe: granted = %v err = %v", none, err)
	}
}

// The IN-list is bounded so a caller cannot turn one request into an unbounded
// query. Truncation drops the tail rather than failing, which is safe only
// because dropping a grant can never widen what is shown.
func TestStaticDiscoverGrantsBoundsTheProbeRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedDiscoveryFixture(t, db)
	catalogStore := newStore(db)
	ctx := discoveryIntegrationContext(t)

	oversized := make([]cardtmpl.ID, 0, maxStaticGrantProbe+8)
	for i := 0; i < maxStaticGrantProbe+7; i++ {
		oversized = append(oversized, cardtmpl.ID("test.disco-filler-"+string(rune('a'+i%26))+
			strings.Repeat("x", i%3)))
	}
	// The genuinely granted ID sits past the bound, so it must be dropped.
	oversized = append(oversized, "test.disco-c-granted")
	granted, err := catalogStore.StaticDiscoverGrants(ctx, discoveryIntegrationSpaceID, oversized)
	if err != nil {
		t.Fatalf("oversized probe: %v", err)
	}
	if len(granted) != 0 {
		t.Fatalf("a probe past the bound was answered: %v", granted)
	}
}

// The operator index is the deliberate inverse of B1: every claimed template
// appears, including the blocked and the ungranted, with the counts an incident
// responder needs.
func TestListTemplatesShowsTheWholeCatalogToOperatorsRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedDiscoveryFixture(t, db)
	catalogStore := newStore(db)
	ctx := discoveryIntegrationContext(t)

	page, err := catalogStore.ListTemplates(ctx, "", 0)
	if err != nil {
		t.Fatalf("ListTemplates: %v", err)
	}
	summaries := map[cardtmpl.ID]ManagerTemplateSummary{}
	for _, summary := range page.Templates {
		summaries[summary.ID] = summary
	}
	if len(summaries) != 6 {
		t.Fatalf("manager index reported %d templates: %+v", len(summaries), page.Templates)
	}
	if _, ok := summaries["test.disco-d-blocked"]; !ok {
		t.Fatal("the blocked template is missing from the operator index")
	}
	if _, ok := summaries["test.disco-b-private"]; !ok {
		t.Fatal("an ungranted private template is missing from the operator index")
	}
	if got := summaries["test.disco-d-blocked"].BlockedVersions; got != 1 {
		t.Fatalf("blocked_versions = %d", got)
	}
	multi := summaries["test.disco-f-public"]
	if multi.VersionCount != 2 || multi.LatestVersion != "2.0.0" {
		t.Fatalf("multi-version summary = %+v", multi)
	}
	if multi.ActiveVersion != "1.0.0" || multi.ActivationStatus != "active" {
		t.Fatalf("activation pointer lost in the summary: %+v", multi)
	}
	if summaries["test.disco-a-public"].ActivationStatus != "" {
		t.Fatalf("a template with no activation row claimed a status: %+v",
			summaries["test.disco-a-public"])
	}
}

// Keyset paging, not offset: a publish landing between two pages must not make
// the operator skip a row.
func TestListTemplatesPagesByKeysetRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedDiscoveryFixture(t, db)
	catalogStore := newStore(db)
	ctx := discoveryIntegrationContext(t)

	first, err := catalogStore.ListTemplates(ctx, "", 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Templates) != 2 || first.NextCursor != "test.disco-b-private" {
		t.Fatalf("first page = %+v cursor = %q", first.Templates, first.NextCursor)
	}

	// A template that sorts before the cursor is inserted between the reads.
	// Under offset paging this would shift every later row and the operator
	// would silently lose one; under keyset it simply is not in the tail.
	seedDiscoveryVersion(t, db, "test.disco-aa-late", "1.0.0", cardtmpl.CatalogVisibilityPublic, false)

	var walked []cardtmpl.ID
	for _, summary := range first.Templates {
		walked = append(walked, summary.ID)
	}
	cursor := first.NextCursor
	for page := 0; page < 20 && cursor != ""; page++ {
		next, err := catalogStore.ListTemplates(ctx, cursor, 2)
		if err != nil {
			t.Fatalf("page after %q: %v", cursor, err)
		}
		for _, summary := range next.Templates {
			walked = append(walked, summary.ID)
		}
		cursor = next.NextCursor
	}
	want := []cardtmpl.ID{
		"test.disco-a-public", "test.disco-b-private", "test.disco-c-granted",
		"test.disco-d-blocked", "test.disco-e-revoked", "test.disco-f-public",
	}
	if len(walked) != len(want) {
		t.Fatalf("walked %v, want %v", walked, want)
	}
	for i := range want {
		if walked[i] != want[i] {
			t.Fatalf("walked %v, want %v", walked, want)
		}
	}
}

// Review P2-1: B1's predicate had no owner clause while B2's authorizer
// enforces the approved-owner allowlist, so the two could disagree about the
// same row. Latent while publish gates on the same list — but that list is the
// per-owner kill switch. Narrow it during an incident and B1 would keep listing
// every affected template with full metadata, and keep spending LIMIT slots on
// rows B2 answers not-found for.
//
// The row is seeded directly rather than published, because publish rejects an
// unapproved owner: the state under test is one that already existed when the
// allowlist was narrowed, which is exactly the incident.
func TestDiscoveryHidesRowsWhoseOwnerLeftTheAllowlistRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedDiscoveryFixture(t, db)
	catalogStore := newStore(db)
	ctx := discoveryIntegrationContext(t)

	const retiredOwner = "retired-owner"
	if isApprovedRuntimeOwner(retiredOwner) {
		t.Fatalf("fixture precondition broken: %q is still approved", retiredOwner)
	}
	seedDiscoveryVersion(t, db, "test.disco-g-retired", "1.0.0", cardtmpl.CatalogVisibilityPublic, false)
	if _, err := db.Exec(`UPDATE card_template_artifact SET owner = ?
        WHERE template_id = ? AND version = '1.0.0'`, retiredOwner, "test.disco-g-retired"); err != nil {
		t.Fatalf("retire owner: %v", err)
	}

	// B1: absent from every page, and it does not consume a slot on the way.
	walked := discoveryRowIDs(walkDiscoverable(t, ctx, catalogStore, discoveryIntegrationSpaceID, 2))
	for _, id := range walked {
		if strings.HasPrefix(id, "test.disco-g-retired@") {
			t.Fatalf("a retired-owner row was listed: %v", walked)
		}
	}

	// B2: the same row is not visible either, so list and detail agree.
	if _, err := catalogStore.LoadDiscoverable(ctx, discoveryIntegrationSpaceID,
		"test.disco-g-retired", "1.0.0"); !errors.Is(err, ErrDiscoveryNotVisible) {
		t.Fatalf("retired-owner detail err = %v, want ErrDiscoveryNotVisible", err)
	}

	// An approved owner is untouched — the predicate narrows, it does not
	// swallow the catalog.
	if _, err := catalogStore.LoadDiscoverable(ctx, discoveryIntegrationSpaceID,
		"test.disco-a-public", "1.0.0"); err != nil {
		t.Fatalf("an approved-owner row was hidden: %v", err)
	}
}
