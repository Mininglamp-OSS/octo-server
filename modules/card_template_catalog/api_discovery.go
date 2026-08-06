package card_template_catalog

// PR-C D5 — B1 (list) and B2 (contract detail).
//
// These are the only card-catalog endpoints an ordinary caller reaches, and
// their whole job is to answer "what can I build against" without answering
// "what exists that I may not see". Three rules carry that:
//
//  1. Everything invisible is invisible the same way. Unknown, private-without-
//     a-grant and blocked all return one localized not-found. A caller that
//     could distinguish them could map the private catalog by probing IDs.
//  2. Discoverability is not permission. A template appearing here says nothing
//     about whether this caller may send or edit it (invariant 5); Bots keep
//     using /v1/bot/card/profile, which is the authoritative send surface.
//  3. Filtering happens before paging, in SQL. See store_discovery.go for why
//     the reverse order leaks a count.
//
// The two sources are paged in sequence — the frozen static Registry first,
// then dynamic rows — rather than merged into one ordering. The cursor names
// which source it is in, so a page boundary never has to reconcile an
// in-memory list against a SQL scan, and adding a dynamic template can never
// shift a static page under a caller mid-iteration.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
)

const (
	discoveryCursorVersion = 1
	discoverySourceStatic  = "static"
	discoverySourceDynamic = "dynamic"
	// discoveryDetailSeparator splits {id}@{version} in the B2 path.
	discoveryDetailSeparator = "@"
)

// discoveryCursor is an opaque, self-describing page position.
//
// It carries the Space it was issued for so that replaying a cursor from
// another Space fails closed rather than resuming a scan whose visibility
// predicate no longer holds. It is not a capability and needs no signature:
// every field is re-validated against the current request, and a forged cursor
// can only move the caller within what they are already allowed to see.
type discoveryCursor struct {
	Version int    `json:"v"`
	Space   string `json:"s"`
	Source  string `json:"src"`
	ID      string `json:"id"`
	Exact   string `json:"ver"`
}

func encodeDiscoveryCursor(cursor discoveryCursor) string {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeDiscoveryCursor rejects anything it cannot fully account for. A cursor
// that is malformed, from another cursor version, or bound to a different Space
// restarts the caller at the beginning instead of silently resuming somewhere
// unintended.
func decodeDiscoveryCursor(encoded, spaceID string) (discoveryCursor, bool) {
	if strings.TrimSpace(encoded) == "" {
		return discoveryCursor{Version: discoveryCursorVersion, Space: spaceID, Source: discoverySourceStatic}, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return discoveryCursor{}, false
	}
	var cursor discoveryCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return discoveryCursor{}, false
	}
	if cursor.Version != discoveryCursorVersion || cursor.Space != spaceID {
		return discoveryCursor{}, false
	}
	if cursor.Source != discoverySourceStatic && cursor.Source != discoverySourceDynamic {
		return discoveryCursor{}, false
	}
	return cursor, true
}

type discoveryActionContract struct {
	Owner      string `json:"owner"`
	ActionType string `json:"action_type"`
}

type discoveryListItem struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Source  string `json:"source"`
	Owner   string `json:"owner,omitempty"`
	// Protocol and ContractVersion let a producer decide compatibility without
	// fetching the full contract.
	Protocol        string `json:"protocol"`
	ContractVersion string `json:"contract_version,omitempty"`
	Visibility      string `json:"visibility"`
	// ActionContract is null for a display-only card. That null is the
	// platform-card-base §9 promise: it tells a producer no callback will ever
	// arrive for this template, which "absent field" could not distinguish
	// from "not populated yet".
	ActionContract *discoveryActionContract `json:"action_contract"`
	// ActiveForNewSend reports that this exact version is what the activation
	// pointer resolves to. It is emphatically not a statement that the caller
	// may send it — send authorization lives behind the Bot profile.
	ActiveForNewSend bool `json:"active_for_new_send"`
}

type discoveryListResponse struct {
	Templates  []discoveryListItem `json:"templates"`
	NextCursor string              `json:"next_cursor,omitempty"`
	HasMore    bool                `json:"has_more"`
}

// discoveryStore is the read surface B1/B2 need. It is an interface so the
// handler tests can drive the visibility matrix without a database.
type discoveryStore interface {
	ListDiscoverable(ctx context.Context, spaceID string, afterID cardtmpl.ID,
		afterVersion string, limit int) ([]DiscoveryRow, bool, error)
	LoadDiscoverable(ctx context.Context, spaceID string, id cardtmpl.ID,
		version string) (DiscoveryRow, error)
	StaticDiscoverGrants(ctx context.Context, spaceID string,
		ids []cardtmpl.ID) (map[cardtmpl.ID]struct{}, error)
}

// listTemplates handles B1.
func (a *API) listTemplates(c *wkhttp.Context) {
	store, ok := a.discoveryStore()
	if !ok {
		respondCatalogUnavailable(c)
		return
	}
	spaceID := space.GetSpaceID(c)
	cursor, valid := decodeDiscoveryCursor(strings.TrimSpace(c.Query("cursor")), spaceID)
	if !valid {
		respondCatalogRequestInvalid(c, errors.New("cursor is not valid for this Space"))
		return
	}
	limit, valid := discoveryPageLimit(c.Query("limit"))
	if !valid {
		respondCatalogRequestInvalid(c, errors.New("limit is out of range"))
		return
	}

	ctx := c.Request.Context()
	page := discoveryListResponse{Templates: []discoveryListItem{}}
	if cursor.Source == discoverySourceStatic {
		items, next, err := a.staticPage(ctx, store, spaceID, cursor, limit)
		if err != nil {
			respondCatalogUnavailable(c)
			return
		}
		page.Templates = append(page.Templates, items...)
		if next != nil {
			page.NextCursor, page.HasMore = encodeDiscoveryCursor(*next), true
			writeDiscoveryCacheHeaders(c, spaceID)
			c.Response(page)
			return
		}
		// The static source is exhausted; continue into dynamic from the top.
		cursor = discoveryCursor{Version: discoveryCursorVersion, Space: spaceID, Source: discoverySourceDynamic}
	}

	remaining := limit - len(page.Templates)
	if remaining <= 0 {
		// Static filled the page exactly, so whether another page exists is a
		// question about the dynamic source, not something to assume.
		next, more, err := dynamicPageFollows(ctx, store, spaceID)
		if err != nil {
			respondCatalogUnavailable(c)
			return
		}
		page.NextCursor, page.HasMore = next, more
		writeDiscoveryCacheHeaders(c, spaceID)
		c.Response(page)
		return
	}
	rows, hasMore, err := store.ListDiscoverable(ctx, spaceID,
		cardtmpl.ID(cursor.ID), cursor.Exact, remaining)
	if err != nil {
		respondCatalogUnavailable(c)
		return
	}
	for _, row := range rows {
		page.Templates = append(page.Templates, dynamicListItem(row))
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor = encodeDiscoveryCursor(discoveryCursor{
			Version: discoveryCursorVersion, Space: spaceID, Source: discoverySourceDynamic,
			ID: string(last.ID), Exact: last.Version,
		})
		page.HasMore = true
	}
	writeDiscoveryCacheHeaders(c, spaceID)
	c.Response(page)
}

// staticPage returns the frozen templates visible to this caller, starting
// after the cursor. A non-nil next cursor means the static source itself has
// more rows; nil means the caller should move on to dynamic.
func (a *API) staticPage(
	ctx context.Context,
	store discoveryStore,
	spaceID string,
	cursor discoveryCursor,
	limit int,
) ([]discoveryListItem, *discoveryCursor, error) {
	entries := a.staticCatalog()
	if len(entries) == 0 {
		return nil, nil, nil
	}
	granted, err := store.StaticDiscoverGrants(ctx, spaceID, privateTemplateIDs(entries))
	if err != nil {
		return nil, nil, err
	}

	items := make([]discoveryListItem, 0, limit)
	for _, entry := range entries {
		if !afterStaticCursor(entry, cursor) {
			continue
		}
		if entry.Visibility != cardtmpl.CatalogVisibilityPublic {
			if _, ok := granted[entry.ID]; !ok {
				continue
			}
		}
		if len(items) == limit {
			// A visible row exists beyond this page, so hand back a cursor
			// pointing at the last row we actually returned. Skipped rows never
			// move the cursor — that is what keeps a hidden template from
			// changing the shape of anyone else's pagination.
			return items, &discoveryCursor{
				Version: discoveryCursorVersion, Space: spaceID, Source: discoverySourceStatic,
				ID: string(items[len(items)-1].ID), Exact: items[len(items)-1].Version,
			}, nil
		}
		items = append(items, staticListItem(entry))
	}
	return items, nil, nil
}

// dynamicPageFollows peeks at the dynamic source with a budget of one row.
//
// The alternative — declaring has_more:true because the static source happened
// to fill the page — costs every caller iterating this boundary a guaranteed
// empty round trip, and, worse, teaches them that has_more is not something
// they can act on anywhere else either.
func dynamicPageFollows(
	ctx context.Context,
	store discoveryStore,
	spaceID string,
) (cursor string, hasMore bool, err error) {
	peek, _, err := store.ListDiscoverable(ctx, spaceID, "", "", 1)
	if err != nil {
		return "", false, err
	}
	if len(peek) == 0 {
		return "", false, nil
	}
	return encodeDiscoveryCursor(discoveryCursor{
		Version: discoveryCursorVersion, Space: spaceID, Source: discoverySourceDynamic,
	}), true, nil
}

// privateTemplateIDs is the deduplicated set of static template IDs that need a
// discover-grant probe. Keying by ID rather than by exact version matters: a
// card registered at three versions would otherwise take three slots in the
// bounded IN-list and could push a genuinely granted template past the cut,
// where it is silently treated as ungranted.
func privateTemplateIDs(entries []cardtmpl.StaticCatalogEntry) []cardtmpl.ID {
	ids := make([]cardtmpl.ID, 0, len(entries))
	seen := make(map[cardtmpl.ID]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Visibility == cardtmpl.CatalogVisibilityPublic {
			continue
		}
		if _, done := seen[entry.ID]; done {
			continue
		}
		seen[entry.ID] = struct{}{}
		ids = append(ids, entry.ID)
	}
	return ids
}

func afterStaticCursor(entry cardtmpl.StaticCatalogEntry, cursor discoveryCursor) bool {
	if cursor.ID == "" {
		return true
	}
	if string(entry.ID) != cursor.ID {
		return string(entry.ID) > cursor.ID
	}
	return entry.Version > cursor.Exact
}

func staticListItem(entry cardtmpl.StaticCatalogEntry) discoveryListItem {
	item := discoveryListItem{
		ID: string(entry.ID), Version: entry.Version, Source: discoverySourceStatic,
		Owner: entry.Owner, Protocol: entry.Protocol,
		ContractVersion: entry.ContractVersion, Visibility: entry.Visibility,
		// A static template has no activation row of its own here; the default
		// version registered in the frozen Registry is what a static new send
		// resolves to.
		ActiveForNewSend: entry.IsDefault,
	}
	if entry.ActionContract != nil {
		item.ActionContract = &discoveryActionContract{
			Owner: entry.ActionContract.Owner, ActionType: entry.ActionContract.ActionType,
		}
	}
	return item
}

func dynamicListItem(row DiscoveryRow) discoveryListItem {
	return discoveryListItem{
		ID: string(row.ID), Version: row.Version, Source: discoverySourceDynamic,
		Owner: row.Owner, Protocol: row.Protocol,
		ContractVersion: row.ContractVersion, Visibility: row.Visibility,
		ActiveForNewSend: row.ActiveForNewSend,
		// The action contract of a dynamic template lives in its compiled
		// projection, which B1 does not load — B1 is a metadata listing, and
		// compiling every row to fill one field would make a list page cost as
		// much as N detail requests. Callers read it from B2.
	}
}

// getTemplate handles B2.
func (a *API) getTemplate(c *wkhttp.Context) {
	store, ok := a.discoveryStore()
	if !ok {
		respondCatalogUnavailable(c)
		return
	}
	id, version, ok := parseDiscoveryRef(c.Param("ref"))
	if !ok {
		respondCatalogNotFound(c)
		return
	}
	spaceID := space.GetSpaceID(c)
	ctx := c.Request.Context()

	if entry, found := a.staticEntry(id, version); found {
		if entry.Visibility != cardtmpl.CatalogVisibilityPublic {
			granted, err := store.StaticDiscoverGrants(ctx, spaceID, []cardtmpl.ID{id})
			if err != nil {
				respondCatalogUnavailable(c)
				return
			}
			if _, ok := granted[id]; !ok {
				respondCatalogNotFound(c)
				return
			}
		}
		export := a.staticExport(id, version)
		if export == nil {
			respondCatalogNotFound(c)
			return
		}
		a.writeExport(c, spaceID, entry.Visibility, entry.ExportHash, export)
		return
	}

	// One authorized read decides everything about this response — that the
	// caller may see the template, what its contract says, and which validator
	// and cache directive describe it.
	//
	// An earlier revision asked twice: the discovery predicate for visibility
	// and the ETag source, then the runtime catalog for the projection. Two
	// independent reads mean two instants, and a grant that moved between them
	// produced either a 404 for a template the caller could see a millisecond
	// earlier or a response whose headers described a snapshot the body did not
	// come from. That is precisely the torn read the one-snapshot resolver
	// exists to eliminate, so B2 no longer performs it.
	//
	// Dropping the predicate read cannot widen what is served: the authorizer
	// applies the same visibility-or-grant rule *plus* the owner allowlist, and
	// rejects blocked and unknown artifacts on its own.
	meta, err := a.discoveryMeta(ctx, spaceID, id, version)
	if err != nil {
		if errors.Is(err, cardtmpl.ErrTemplateUnknown) ||
			errors.Is(err, cardtmpl.ErrRuntimeCatalogNotAuthorized) ||
			errors.Is(err, cardtmpl.ErrRuntimeCatalogBlocked) {
			respondCatalogNotFound(c)
			return
		}
		respondCatalogUnavailable(c)
		return
	}
	export := meta.Export()
	if export == nil {
		respondCatalogNotFound(c)
		return
	}
	// The validator is the projection's own deterministic digest, which is the
	// one hash guaranteed to describe the bytes being written. Two artifacts
	// that project identically therefore share a validator — correct for a
	// cache, since the responses are byte-identical. Visibility likewise comes
	// from the projection, which fails closed to private.
	a.writeExport(c, spaceID, export.Visibility, export.Hash, export)
}

// writeExport emits the projection with conditional-request and cache headers.
func (a *API) writeExport(
	c *wkhttp.Context,
	spaceID, visibility, etagSource string,
	export *cardtmpl.SafeExport,
) {
	writeDiscoveryCacheHeaders(c, spaceID)
	if visibility != cardtmpl.CatalogVisibilityPublic {
		// A private contract must not be retained by a shared cache even when
		// the caller is entitled to it — the next caller through that cache may
		// not be.
		c.Header("Cache-Control", "private, no-cache")
	}
	if etagSource == "" {
		etagSource = export.Hash
	}
	etag := `"` + etagSource + `"`
	c.Header("ETag", etag)
	if matchesETag(c.GetHeader("If-None-Match"), etag) {
		// Flush explicitly. c.Status only records the code and leaves the write
		// to whenever the framework gets around to it, which makes "304 with no
		// body" depend on the caller's plumbing rather than on this handler.
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return
	}
	c.Response(export)
}

// matchesETag implements the subset of If-None-Match this endpoint needs:
// a comma-separated list of entity tags, or "*". Weak validators are compared
// by their opaque part, since the projection is byte-identical either way.
func matchesETag(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

func writeDiscoveryCacheHeaders(c *wkhttp.Context, spaceID string) {
	if spaceID != "" {
		c.Header("Cache-Control", "private, no-cache")
		return
	}
	c.Header("Cache-Control", "no-cache")
}

// parseDiscoveryRef splits the {id}@{version} path segment. Both halves are
// required: a bare ID would have to resolve through the activation pointer,
// and a discovery read that followed the pointer would report a different
// contract from one moment to the next.
func parseDiscoveryRef(ref string) (cardtmpl.ID, string, bool) {
	ref = strings.TrimSpace(ref)
	at := strings.LastIndex(ref, discoveryDetailSeparator)
	if at <= 0 || at == len(ref)-1 {
		return "", "", false
	}
	id, version := strings.TrimSpace(ref[:at]), strings.TrimSpace(ref[at+1:])
	if id == "" || version == "" {
		return "", "", false
	}
	return cardtmpl.ID(id), version, true
}

func discoveryPageLimit(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultDiscoveryPageSize, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maxDiscoveryPageSize {
		return 0, false
	}
	return limit, true
}

// managerTemplateStore is the operator index read. Separate from
// discoveryStore because the two answer different questions: this one shows
// everything, that one shows what a caller may see.
type managerTemplateStore interface {
	ListTemplates(ctx context.Context, after string, limit int) (ManagerTemplatePage, error)
}

type managerTemplateItem struct {
	ID               string `json:"id"`
	VersionCount     int    `json:"version_count"`
	LatestVersion    string `json:"latest_version,omitempty"`
	ActiveVersion    string `json:"active_version,omitempty"`
	ActivationStatus string `json:"activation_status,omitempty"`
	BlockedVersions  int    `json:"blocked_versions"`
}

type managerTemplateListResponse struct {
	Templates  []managerTemplateItem `json:"templates"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

// managerList handles GET /v1/manager/card-templates. It is the read-only
// index the control plane was missing: every other manager route needs a
// template ID the operator already knows, which makes a freshly published or
// half-rolled-back template hard to find.
func (a *API) managerList(c *wkhttp.Context) {
	if !a.requireSuperAdmin(c) {
		return
	}
	store, ok := a.managerTemplateStore()
	if !ok {
		respondCatalogUnavailable(c)
		return
	}
	limit, valid := discoveryPageLimit(c.Query("limit"))
	if !valid {
		respondCatalogRequestInvalid(c, errors.New("limit is out of range"))
		return
	}
	page, err := store.ListTemplates(c.Request.Context(), strings.TrimSpace(c.Query("cursor")), limit)
	if err != nil {
		respondCatalogUnavailable(c)
		return
	}
	response := managerTemplateListResponse{
		Templates:  make([]managerTemplateItem, 0, len(page.Templates)),
		NextCursor: page.NextCursor,
	}
	for _, summary := range page.Templates {
		response.Templates = append(response.Templates, managerTemplateItem{
			ID: string(summary.ID), VersionCount: summary.VersionCount,
			LatestVersion: summary.LatestVersion, ActiveVersion: summary.ActiveVersion,
			ActivationStatus: summary.ActivationStatus, BlockedVersions: summary.BlockedVersions,
		})
	}
	c.Response(response)
}

// managerTemplateStore resolves the operator index reader. It prefers the test
// seam for the same reason discoveryStore does: a handler test should not have
// to stand up the publish store to assert a projection.
func (a *API) managerTemplateStore() (managerTemplateStore, bool) {
	if a == nil {
		return nil, false
	}
	if a.discovery != nil {
		if store, ok := a.discovery.(managerTemplateStore); ok {
			return store, true
		}
	}
	store, ok := a.store.(managerTemplateStore)
	return store, ok
}

// discoveryStore returns the read surface, or false when this API has no store
// that implements it. Failing closed matters more than degrading: a listing
// that silently returned only static templates would look like a complete
// answer.
func (a *API) discoveryStore() (discoveryStore, bool) {
	if a == nil {
		return nil, false
	}
	if a.discovery != nil {
		return a.discovery, true
	}
	store, ok := a.store.(discoveryStore)
	return store, ok
}

// staticCatalog lists the frozen Registry. An API constructed without one (as
// some focused tests are) simply has no static half.
//
// staticEntries is a test seam. The frozen Registry is built from embedded
// template assets that this package does not carry, so without it the static
// half of pagination — including the rule that a hidden row must not advance
// the cursor — would have no coverage at all.
func (a *API) staticCatalog() []cardtmpl.StaticCatalogEntry {
	if a == nil {
		return nil
	}
	if a.staticEntries != nil {
		return a.staticEntries()
	}
	if a.registry == nil {
		return nil
	}
	return a.registry.StaticCatalog()
}

func (a *API) staticEntry(id cardtmpl.ID, version string) (cardtmpl.StaticCatalogEntry, bool) {
	for _, entry := range a.staticCatalog() {
		if entry.ID == id && entry.Version == version {
			return entry, true
		}
	}
	return cardtmpl.StaticCatalogEntry{}, false
}

func (a *API) staticExport(id cardtmpl.ID, version string) *cardtmpl.SafeExport {
	if a == nil || a.registry == nil {
		return nil
	}
	return a.registry.ExportFor(id, version)
}

// discoveryMeta resolves a dynamic template's compiled metadata through the
// runtime catalog, so the projection served here is the same object the
// renderer would use and the read passes the same one-snapshot authorization
// every other dynamic access does.
func (a *API) discoveryMeta(
	ctx context.Context,
	spaceID string,
	id cardtmpl.ID,
	version string,
) (cardtmpl.TemplateMeta, error) {
	catalog := cardtmpl.DefaultCatalog()
	if catalog == nil {
		return cardtmpl.TemplateMeta{}, cardtmpl.ErrRuntimeCatalogUnavailable
	}
	return catalog.MetaExact(ctx, cardtmpl.CatalogExactRequest{
		Access: cardtmpl.CatalogAccess{
			Purpose: cardtmpl.CatalogPurposeDiscover,
			Principal: cardtmpl.CatalogPrincipal{
				// A Space principal's grants are always global-scoped (D2), so
				// the identity is the Space itself and the scope stays empty.
				Kind: cardtmpl.CatalogPrincipalSpace, ID: spaceID,
			},
		},
		ID: id, Version: version,
	})
}
