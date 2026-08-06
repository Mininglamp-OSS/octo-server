package card_template_catalog

// PR-C D5 evidence at the HTTP boundary.
//
// The sibling file covers the pure decisions (cursor binding, ETag comparison,
// page bounds). This one drives the handlers themselves, because several
// acceptance promises are only observable in the response: that a 304 carries
// no body, that a private contract is not shared-cacheable, that unknown and
// invisible are wire-identical, and that a hidden row cannot change the shape
// of anyone's pagination.
//
// The handlers are exercised directly rather than through the router. Route
// wiring and middleware are covered by the authenticated-route guard in
// api_test.go; what needs proving here is what the handler writes, and going
// through the full stack would mean standing up auth and Space membership to
// observe a cache header.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/gin-gonic/gin"
)

// newDiscoveryRequest builds a context for one discovery call. spaceID is set
// the way the localized Space middleware sets it, so the handler sees exactly
// what it would see in production after membership was verified.
func newDiscoveryRequest(t *testing.T, target, spaceID string, header http.Header) (
	*wkhttp.Context, *httptest.ResponseRecorder,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, target, nil)
	if header != nil {
		ginCtx.Request.Header = header
	}
	if spaceID != "" {
		ginCtx.Set("space_id", spaceID)
	}
	return &wkhttp.Context{Context: ginCtx}, recorder
}

// discoveryAPI wires only the read surface. The discovery field is the test
// seam for exactly this: a handler test has no business standing up the publish
// store to observe a cache header.
func discoveryAPI(store discoveryStore) *API {
	return &API{discovery: store}
}

func decodeListResponse(t *testing.T, recorder *httptest.ResponseRecorder) discoveryListResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var page discoveryListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list response: %v (body %s)", err, recorder.Body.String())
	}
	return page
}

func dynamicRow(id cardtmpl.ID, version string, active bool) DiscoveryRow {
	return DiscoveryRow{
		ID: id, Version: version, Owner: "docs", Protocol: cardtmpl.Protocol,
		ContractVersion: "1.0.0", Visibility: cardtmpl.CatalogVisibilityPrivate,
		ContentSHA256: strings.Repeat("a", 64), ActiveForNewSend: active,
	}
}

func TestListTemplatesReturnsVisibleRowsAndAdvancesTheCursor(t *testing.T) {
	store := &fakeDiscoveryStore{rows: []DiscoveryRow{
		dynamicRow("pilot.one", "1.0.0", true),
		dynamicRow("pilot.two", "2.0.0", false),
	}}
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates", "space-1", nil)
	discoveryAPI(store).listTemplates(c)

	page := decodeListResponse(t, recorder)
	if len(page.Templates) != 2 {
		t.Fatalf("templates = %+v", page.Templates)
	}
	first := page.Templates[0]
	if first.ID != "pilot.one" || first.Source != discoverySourceDynamic ||
		first.Visibility != cardtmpl.CatalogVisibilityPrivate || !first.ActiveForNewSend {
		t.Fatalf("first row = %+v", first)
	}
	// active_for_new_send says which version the pointer resolves to. It must
	// not be read as permission — nothing in this response grants anything.
	if page.Templates[1].ActiveForNewSend {
		t.Fatalf("second row claimed to be active: %+v", page.Templates[1])
	}
	if page.HasMore || page.NextCursor != "" {
		t.Fatalf("exhausted page still advertised more: %+v", page)
	}
	// A Space-scoped read must not be retained by a shared cache.
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("Cache-Control = %q, want private, no-cache", got)
	}
}

// A page that fills the limit hands back a cursor bound to this Space, and the
// caller can resume with it.
func TestListTemplatesPaginationIsBoundedAndResumable(t *testing.T) {
	store := &fakeDiscoveryStore{rows: []DiscoveryRow{
		dynamicRow("pilot.one", "1.0.0", true),
		dynamicRow("pilot.two", "2.0.0", false),
	}}
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates?limit=1", "space-1", nil)
	discoveryAPI(store).listTemplates(c)

	page := decodeListResponse(t, recorder)
	if len(page.Templates) != 1 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("bounded page = %+v", page)
	}
	if store.listCalls[0] != 1 {
		t.Fatalf("store was asked for %d rows, want the requested limit", store.listCalls[0])
	}
	cursor, ok := decodeDiscoveryCursor(page.NextCursor, "space-1")
	if !ok || cursor.ID != "pilot.one" || cursor.Exact != "1.0.0" {
		t.Fatalf("next cursor = %+v ok=%v, want the last returned row", cursor, ok)
	}
	// The same cursor is refused in another Space, so it cannot be used to
	// resume a scan whose visibility predicate no longer holds.
	replay, replayRecorder := newDiscoveryRequest(t,
		"/v1/message/card/templates?cursor="+page.NextCursor, "space-other", nil)
	discoveryAPI(store).listTemplates(replay)
	if replayRecorder.Code == http.StatusOK {
		t.Fatalf("a cursor from space-1 was accepted in space-other: %s", replayRecorder.Body.String())
	}
}

func TestListTemplatesRejectsAnOutOfRangeLimit(t *testing.T) {
	store := &fakeDiscoveryStore{}
	for _, target := range []string{
		"/v1/message/card/templates?limit=0",
		"/v1/message/card/templates?limit=101",
		"/v1/message/card/templates?limit=abc",
	} {
		c, recorder := newDiscoveryRequest(t, target, "space-1", nil)
		discoveryAPI(store).listTemplates(c)
		if recorder.Code == http.StatusOK {
			t.Fatalf("%s was accepted", target)
		}
	}
}

// A caller with no Space context still gets a well-formed page — the public
// catalog — and a response that is merely not-cacheable rather than private.
func TestListTemplatesWithoutSpaceContextStillAnswers(t *testing.T) {
	store := &fakeDiscoveryStore{}
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates", "", nil)
	discoveryAPI(store).listTemplates(c)

	page := decodeListResponse(t, recorder)
	// Never null: a caller should not have to distinguish "no templates" from
	// "field missing".
	if page.Templates == nil || len(page.Templates) != 0 {
		t.Fatalf("templates = %+v, want an empty array", page.Templates)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestListTemplatesFailsClosedWithoutAStore(t *testing.T) {
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates", "space-1", nil)
	(&API{}).listTemplates(c)
	if recorder.Code == http.StatusOK {
		t.Fatalf("a listing without a store answered OK: %s", recorder.Body.String())
	}
}

// installEmptyRuntimeCatalog points the process default at a catalog that knows
// no templates, so every dynamic B2 resolve answers ErrTemplateUnknown. B2 is
// decided entirely by the runtime catalog now — it deliberately no longer asks
// the discovery predicate a second time — so a handler test that says anything
// about B2's answers has to install one.
func installEmptyRuntimeCatalog(t *testing.T) {
	t.Helper()
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	catalog, err := cardtmpl.NewStaticCatalog(registry)
	if err != nil {
		t.Fatalf("build empty catalog: %v", err)
	}
	previous := cardtmpl.DefaultCatalog()
	cardtmpl.SetDefaultCatalog(catalog)
	t.Cleanup(func() { cardtmpl.SetDefaultCatalog(previous) })
}

// Unknown, invisible and blocked must be wire-identical. If they were not, an
// authenticated caller could map the private catalog by probing IDs. A
// malformed ref has to land on the same answer too, since a distinguishable
// parse error would confirm that the rest of the path exists.
func TestGetTemplateAnswersOneNotFoundForEveryInvisibleReason(t *testing.T) {
	installEmptyRuntimeCatalog(t)
	store := &fakeDiscoveryStore{}
	var bodies []string
	for _, ref := range []string{
		"pilot.does-not-exist@1.0.0", // unknown
		"pilot.private@1.0.0",        // exists, not visible to this Space
		"pilot.blocked@1.0.0",        // exists, visible, blocked
	} {
		c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates/"+ref, "space-1", nil)
		c.Params = gin.Params{{Key: "ref", Value: ref}}
		discoveryAPI(store).getTemplate(c)
		if recorder.Code == http.StatusOK {
			t.Fatalf("%s answered OK", ref)
		}
		bodies = append(bodies, recorder.Body.String())
	}
	for _, body := range bodies[1:] {
		if body != bodies[0] {
			t.Fatalf("invisible reasons are distinguishable:\n%s\n%s", bodies[0], body)
		}
	}
	// A malformed ref must land on the same answer rather than a parse error
	// that confirms the rest of the path exists.
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates/no-version", "space-1", nil)
	c.Params = gin.Params{{Key: "ref", Value: "no-version"}}
	discoveryAPI(store).getTemplate(c)
	if recorder.Body.String() != bodies[0] {
		t.Fatalf("a malformed ref is distinguishable:\n%s\n%s", bodies[0], recorder.Body.String())
	}
}

// writeExport is the only place the projection reaches a caller, so the
// conditional-request and cache contract is asserted on it directly.
func TestWriteExportServesETagAndA304WithoutABody(t *testing.T) {
	export := &cardtmpl.SafeExport{
		ID: "pilot.card", Version: "1.0.0", Visibility: cardtmpl.CatalogVisibilityPrivate,
		Samples: map[string]json.RawMessage{},
		Hash:    "exporthash",
	}
	api := discoveryAPI(&fakeDiscoveryStore{})
	const contentHash = "contenthash"

	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates/pilot.card@1.0.0", "space-1", nil)
	api.writeExport(c, "space-1", cardtmpl.CatalogVisibilityPrivate, contentHash, export)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	etag := recorder.Header().Get("ETag")
	if etag != `"`+contentHash+`"` {
		t.Fatalf("ETag = %q, want the immutable content hash", etag)
	}
	if recorder.Body.Len() == 0 {
		t.Fatal("first response carried no projection")
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("private Cache-Control = %q", got)
	}

	// Same ETag back → 304, and nothing in the body.
	header := http.Header{}
	header.Set("If-None-Match", etag)
	conditional, conditionalRecorder := newDiscoveryRequest(t,
		"/v1/message/card/templates/pilot.card@1.0.0", "space-1", header)
	api.writeExport(conditional, "space-1", cardtmpl.CatalogVisibilityPrivate, contentHash, export)
	if conditionalRecorder.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", conditionalRecorder.Code)
	}
	if conditionalRecorder.Body.Len() != 0 {
		t.Fatalf("304 carried a body: %s", conditionalRecorder.Body.String())
	}

	// A stale ETag gets the projection again.
	stale := http.Header{}
	stale.Set("If-None-Match", `"someotherhash"`)
	fresh, freshRecorder := newDiscoveryRequest(t,
		"/v1/message/card/templates/pilot.card@1.0.0", "space-1", stale)
	api.writeExport(fresh, "space-1", cardtmpl.CatalogVisibilityPrivate, contentHash, export)
	if freshRecorder.Code != http.StatusOK || freshRecorder.Body.Len() == 0 {
		t.Fatalf("stale validator = %d body=%d", freshRecorder.Code, freshRecorder.Body.Len())
	}
}

// A static template has no immutable content hash, so its deterministic export
// digest is the validator instead.
func TestWriteExportFallsBackToTheExportHashForStaticTemplates(t *testing.T) {
	export := &cardtmpl.SafeExport{
		ID: "test.static", Version: "1.0.0", Visibility: cardtmpl.CatalogVisibilityPublic,
		Samples: map[string]json.RawMessage{}, Hash: "staticexporthash",
	}
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates/test.static@1.0.0", "", nil)
	discoveryAPI(&fakeDiscoveryStore{}).writeExport(c, "", cardtmpl.CatalogVisibilityPublic, "", export)
	if got := recorder.Header().Get("ETag"); got != `"staticexporthash"` {
		t.Fatalf("static ETag = %q", got)
	}
	// A public contract read outside any Space is cacheable-with-revalidation
	// rather than private.
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("public Cache-Control = %q", got)
	}
}

// The response must never carry control-plane state, whatever the projection
// happens to hold.
func TestDiscoveryResponsesCarryNoControlPlaneFields(t *testing.T) {
	store := &fakeDiscoveryStore{rows: []DiscoveryRow{dynamicRow("pilot.one", "1.0.0", true)}}
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates", "space-1", nil)
	discoveryAPI(store).listTemplates(c)

	body := recorder.Body.String()
	for _, forbidden := range []string{
		"grant", "revision", "audit", "actor", "change_ticket", "canonical_bundle", "blocked_at",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("discovery response leaked %q: %s", forbidden, body)
		}
	}
}

func staticEntry(id cardtmpl.ID, version, visibility string, isDefault bool) cardtmpl.StaticCatalogEntry {
	entry := cardtmpl.StaticCatalogEntry{
		ID: id, Version: version, Owner: "docs", Protocol: cardtmpl.Protocol,
		ContractVersion: "1.1.0", Visibility: visibility,
		ExportHash: "hash-" + string(id) + "-" + version, IsDefault: isDefault,
	}
	if visibility == cardtmpl.CatalogVisibilityPublic {
		entry.ActionContract = &cardtmpl.TemplateActionContract{
			Owner: "docs", ActionType: "access_request.decision",
		}
	}
	return entry
}

func staticDiscoveryAPI(store discoveryStore, entries ...cardtmpl.StaticCatalogEntry) *API {
	api := discoveryAPI(store)
	api.staticEntries = func() []cardtmpl.StaticCatalogEntry { return entries }
	return api
}

// A static template is private unless the composition root published it, and a
// private one needs the caller's Space to hold a discover grant.
func TestStaticPageShowsPublicAndGrantedPrivateOnly(t *testing.T) {
	store := &fakeDiscoveryStore{granted: map[cardtmpl.ID]struct{}{"docs.granted": {}}}
	api := staticDiscoveryAPI(store,
		staticEntry("docs.granted", "1.0.0", cardtmpl.CatalogVisibilityPrivate, true),
		staticEntry("docs.hidden", "1.0.0", cardtmpl.CatalogVisibilityPrivate, true),
		staticEntry("docs.open", "1.0.0", cardtmpl.CatalogVisibilityPublic, false),
	)
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates", "space-1", nil)
	api.listTemplates(c)

	page := decodeListResponse(t, recorder)
	seen := map[string]discoveryListItem{}
	for _, item := range page.Templates {
		seen[item.ID] = item
	}
	if _, hidden := seen["docs.hidden"]; hidden {
		t.Fatalf("an ungranted private template was listed: %+v", page.Templates)
	}
	if len(seen) != 2 {
		t.Fatalf("templates = %+v, want the granted private and the public one", page.Templates)
	}
	if got := seen["docs.granted"]; got.Source != discoverySourceStatic || !got.ActiveForNewSend {
		t.Fatalf("granted static row = %+v", got)
	}
	// A display-only card reports action_contract:null; a card with a callback
	// reports the routing pair. That null is the §9 promise, so it is asserted
	// on the decoded item rather than inferred from the struct.
	if seen["docs.granted"].ActionContract != nil {
		t.Fatalf("private fixture unexpectedly carried a contract: %+v", seen["docs.granted"])
	}
	contract := seen["docs.open"].ActionContract
	if contract == nil || contract.Owner != "docs" || contract.ActionType != "access_request.decision" {
		t.Fatalf("public row action_contract = %+v", contract)
	}
}

// The cursor advances only past rows the caller actually received. If a hidden
// row could move it, the page length would leak how many templates exist that
// this caller may not see.
func TestStaticPageCursorSkipsHiddenRowsWithoutCountingThem(t *testing.T) {
	store := &fakeDiscoveryStore{granted: map[cardtmpl.ID]struct{}{
		"docs.a": {}, "docs.c": {},
	}}
	api := staticDiscoveryAPI(store,
		staticEntry("docs.a", "1.0.0", cardtmpl.CatalogVisibilityPrivate, true),
		staticEntry("docs.b", "1.0.0", cardtmpl.CatalogVisibilityPrivate, true), // hidden
		staticEntry("docs.c", "1.0.0", cardtmpl.CatalogVisibilityPrivate, true),
	)
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates?limit=1", "space-1", nil)
	api.listTemplates(c)

	page := decodeListResponse(t, recorder)
	if len(page.Templates) != 1 || page.Templates[0].ID != "docs.a" || !page.HasMore {
		t.Fatalf("first page = %+v", page)
	}
	cursor, ok := decodeDiscoveryCursor(page.NextCursor, "space-1")
	if !ok || cursor.ID != "docs.a" || cursor.Source != discoverySourceStatic {
		t.Fatalf("cursor = %+v ok=%v, want the last *returned* row", cursor, ok)
	}

	// Resuming lands on the next visible row, and the hidden one never appears.
	next, nextRecorder := newDiscoveryRequest(t,
		"/v1/message/card/templates?limit=1&cursor="+page.NextCursor, "space-1", nil)
	api.listTemplates(next)
	second := decodeListResponse(t, nextRecorder)
	if len(second.Templates) != 1 || second.Templates[0].ID != "docs.c" {
		t.Fatalf("second page = %+v, want docs.c", second.Templates)
	}
}

// A static-source page that fills the limit exactly must not claim another page
// unless the dynamic source really holds one.
func TestStaticPageBoundaryConsultsTheDynamicSource(t *testing.T) {
	entries := []cardtmpl.StaticCatalogEntry{
		staticEntry("docs.open", "1.0.0", cardtmpl.CatalogVisibilityPublic, true),
	}
	empty := &fakeDiscoveryStore{}
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates?limit=1", "space-1", nil)
	staticDiscoveryAPI(empty, entries...).listTemplates(c)
	if page := decodeListResponse(t, recorder); page.HasMore || page.NextCursor != "" {
		t.Fatalf("empty dynamic source still advertised a page: %+v", page)
	}
	if len(empty.listCalls) != 1 || empty.listCalls[0] != 1 {
		t.Fatalf("boundary probe budgets = %v, want a single one-row peek", empty.listCalls)
	}

	stocked := &fakeDiscoveryStore{rows: []DiscoveryRow{dynamicRow("pilot.one", "1.0.0", true)}}
	c2, recorder2 := newDiscoveryRequest(t, "/v1/message/card/templates?limit=1", "space-1", nil)
	staticDiscoveryAPI(stocked, entries...).listTemplates(c2)
	page := decodeListResponse(t, recorder2)
	if !page.HasMore || page.NextCursor == "" {
		t.Fatalf("stocked dynamic source was not advertised: %+v", page)
	}
	cursor, _ := decodeDiscoveryCursor(page.NextCursor, "space-1")
	if cursor.Source != discoverySourceDynamic || cursor.ID != "" {
		t.Fatalf("boundary cursor = %+v, want the top of the dynamic source", cursor)
	}
}

// A grant-probe failure fails the listing closed. Serving only the public rows
// would look like a complete answer to a caller who should have seen more.
func TestStaticPageFailsClosedWhenTheGrantProbeFails(t *testing.T) {
	store := &grantProbeFailureStore{}
	api := staticDiscoveryAPI(store,
		staticEntry("docs.hidden", "1.0.0", cardtmpl.CatalogVisibilityPrivate, true))
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates", "space-1", nil)
	api.listTemplates(c)
	if recorder.Code == http.StatusOK {
		t.Fatalf("a failed grant probe answered OK: %s", recorder.Body.String())
	}
}

type grantProbeFailureStore struct{ fakeDiscoveryStore }

func (s *grantProbeFailureStore) StaticDiscoverGrants(
	context.Context, string, []cardtmpl.ID,
) (map[cardtmpl.ID]struct{}, error) {
	return nil, errors.New("db unavailable")
}

type fakeManagerStore struct {
	fakeDiscoveryStore
	page ManagerTemplatePage
	err  error
}

func (s *fakeManagerStore) ListTemplates(context.Context, string, int) (ManagerTemplatePage, error) {
	if s.err != nil {
		return ManagerTemplatePage{}, s.err
	}
	return s.page, nil
}

// newManagerRequest is newDiscoveryRequest plus the role AuthMiddleware writes
// for a super admin. The manager surface is superAdmin-only; without the role
// every call short-circuits on the permission check and the projection below
// would never be reached.
func newManagerRequest(t *testing.T, target string) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, recorder := newDiscoveryRequest(t, target, "", nil)
	c.Set("role", string(wkhttp.SuperAdmin))
	return c, recorder
}

// A non-superAdmin must not reach the control plane at all: the manager index
// reports blocked and disabled templates across every Space, which is exactly
// the inventory a tenant caller should never see.
func TestManagerListRejectsCallersWhoAreNotSuperAdmin(t *testing.T) {
	for _, role := range []string{"", "admin", "common"} {
		c, recorder := newDiscoveryRequest(t, "/v1/manager/card-templates", "", nil)
		if role != "" {
			c.Set("role", role)
		}
		(&API{discovery: &fakeManagerStore{}}).managerList(c)
		if recorder.Code == http.StatusOK {
			t.Fatalf("role %q reached the manager index: %s", role, recorder.Body.String())
		}
	}
}

// The manager index deliberately shows everything, including blocked and
// disabled templates — hiding rows on the control plane would remove exactly
// what an operator needs during an incident.
func TestManagerListReportsEveryTemplateIncludingBrokenOnes(t *testing.T) {
	store := &fakeManagerStore{page: ManagerTemplatePage{
		Templates: []ManagerTemplateSummary{
			{ID: "docs.a", VersionCount: 3, LatestVersion: "1.2.0",
				ActiveVersion: "1.1.0", ActivationStatus: "active", BlockedVersions: 1},
			{ID: "docs.b", VersionCount: 1, LatestVersion: "0.1.0", ActivationStatus: "disabled"},
		},
		NextCursor: "docs.b",
	}}
	api := &API{discovery: store}
	c, recorder := newManagerRequest(t, "/v1/manager/card-templates")
	api.managerList(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var response managerTemplateListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Templates) != 2 || response.NextCursor != "docs.b" {
		t.Fatalf("response = %+v", response)
	}
	if response.Templates[0].BlockedVersions != 1 || response.Templates[1].ActivationStatus != "disabled" {
		t.Fatalf("manager rows hid diagnostic state: %+v", response.Templates)
	}
}

func TestManagerListRejectsABadLimitAndFailsClosedOnStoreError(t *testing.T) {
	c, recorder := newDiscoveryRequest(t, "/v1/manager/card-templates?limit=999", "", nil)
	(&API{discovery: &fakeManagerStore{}}).managerList(c)
	if recorder.Code == http.StatusOK {
		t.Fatal("an out-of-range limit was accepted")
	}

	failing, failingRecorder := newDiscoveryRequest(t, "/v1/manager/card-templates", "", nil)
	(&API{discovery: &fakeManagerStore{err: errors.New("db unavailable")}}).managerList(failing)
	if failingRecorder.Code == http.StatusOK {
		t.Fatal("a store failure answered OK")
	}
}

// visibleRowStore reports a dynamic row as discoverable. It exists to drive the
// half of B2 the ordinary fakes cannot reach: the point where the discovery
// predicate has already said yes and the runtime catalog is asked to authorize
// the same read.
type visibleRowStore struct {
	fakeDiscoveryStore
	row DiscoveryRow
}

func (s *visibleRowStore) LoadDiscoverable(
	context.Context, string, cardtmpl.ID, string,
) (DiscoveryRow, error) {
	return s.row, nil
}

// The discovery predicate and the runtime authorizer are two different reads,
// and they can disagree — a row visible to the SQL predicate may still be one
// the runtime catalog refuses to resolve for this principal. When they
// disagree, discovery must not be the one that wins: B2 serves the projection
// only when the runtime catalog produced it, never from the listing row alone.
//
// The assertion is deliberately on "not OK" rather than on a specific status.
// Both refusals are correct here (unresolvable → unavailable, unauthorized or
// unknown → the single not-found), and which one a deployment hits depends on
// whether a runtime catalog is installed at all. Pinning the code would be
// pinning the test environment, not the contract.
func TestGetTemplateWillNotServeARowTheRuntimeCatalogCannotAuthorize(t *testing.T) {
	store := &visibleRowStore{row: dynamicRow("pilot.unresolvable", "1.0.0", true)}
	const ref = "pilot.unresolvable@1.0.0"
	c, recorder := newDiscoveryRequest(t, "/v1/message/card/templates/"+ref, "space-1", nil)
	c.Params = gin.Params{{Key: "ref", Value: ref}}
	discoveryAPI(store).getTemplate(c)
	if recorder.Code == http.StatusOK {
		t.Fatalf("B2 served a projection the runtime catalog never produced: %s",
			recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "content_sha256") {
		t.Fatalf("the refusal leaked listing-row state: %s", recorder.Body.String())
	}
}
