package messages_search

import (
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
)

// GlobalChannelRef is the request shape for a filters.channel_ids entry on the
// global endpoints. Kept as an exported alias so both request payloads
// (SearchGlobalMessagesReq / SearchGlobalFilesReq) share one type without
// pulling in each other's package.
type GlobalChannelRef struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
}

// GlobalSearchFilters is the shared filter block for the two global endpoints.
// A superset of SearchFilters (sender_ids / sent_at_from / sent_at_to) plus
// the four global-only dimensions: member_uid, channel_ids, channel_types,
// content_types.
//
// content_types is meaningful only for _search_global_messages (the mixed
// message+file stream). _search_global_files hardlocks payload.type=8 so it
// ignores content_types entirely; file_exts / file_size_min / file_size_max
// live in SearchGlobalFilesFilters instead.
type GlobalSearchFilters struct {
	SenderIDs    []string           `json:"sender_ids,omitempty"`
	MemberUID    string             `json:"member_uid,omitempty"`
	ChannelIDs   []GlobalChannelRef `json:"channel_ids,omitempty"`
	ChannelTypes []uint8            `json:"channel_types,omitempty"`
	ContentTypes []int              `json:"content_types,omitempty"`
	SentAtFrom   string             `json:"sent_at_from,omitempty"`
	SentAtTo     string             `json:"sent_at_to,omitempty"`
}

// GlobalFileFilters is the file-endpoint-only filter block: the shared base
// (via GlobalSearchFilters) plus file_exts / file_size_min / file_size_max.
type GlobalFileFilters struct {
	SenderIDs    []string           `json:"sender_ids,omitempty"`
	MemberUID    string             `json:"member_uid,omitempty"`
	ChannelIDs   []GlobalChannelRef `json:"channel_ids,omitempty"`
	ChannelTypes []uint8            `json:"channel_types,omitempty"`
	FileExts     []string           `json:"file_exts,omitempty"`
	FileSizeMin  int64              `json:"file_size_min,omitempty"`
	FileSizeMax  int64              `json:"file_size_max,omitempty"`
	SentAtFrom   string             `json:"sent_at_from,omitempty"`
	SentAtTo     string             `json:"sent_at_to,omitempty"`
}

// baseFilters projects GlobalSearchFilters into the SearchFilters shape that
// addCommonFilters consumes (sender_ids + sent_at window). Global-only fields
// are applied separately by the global DSL builders.
func (f GlobalSearchFilters) baseFilters() SearchFilters {
	return SearchFilters{
		SenderIDs:  f.SenderIDs,
		SentAtFrom: f.SentAtFrom,
		SentAtTo:   f.SentAtTo,
	}
}

// baseFilters mirrors GlobalSearchFilters.baseFilters for the file-endpoint
// filter block.
func (f GlobalFileFilters) baseFilters() SearchFilters {
	return SearchFilters{
		SenderIDs:  f.SenderIDs,
		SentAtFrom: f.SentAtFrom,
		SentAtTo:   f.SentAtTo,
	}
}

// channelRef is the shared representation of a single (id, type) channel
// resolved into an OS channelId. Used by resolveGlobalScope to describe the
// single-channel fast path.
type channelRef struct {
	OSChannelID string // normalised OS `channelId` value
	WireID      string // channel_id echoed to the client (peer uid for DM)
	ChannelType uint8
}

// Thread-enumeration hard limits for the global allowlist.
//
//   - maxThreadsPerGroup caps how many active threads from a single group we
//     fold into the terms(channelId, allowlist) clause. Beyond this the group
//     downgrades to a "group-only" allowlist entry (its group channel still
//     works, its thread hits get hard-dropped, no 500). Keeps a
//     runaway-thread group from blowing the OS terms clause.
//   - maxTotalThreadChannelIDs is the global aggregate cap across ALL groups
//     in one request. OpenSearch's `indices.query.bool.max_clause_count`
//     default is 4096 (Elasticsearch 7.x); we stay well below that so the
//     rest of the DSL (group + DM + sender filters + ...) has room. On the
//     first group that pushes us past this cap, we skip its threads (WARN)
//     and keep serving the request.
//
// Both are intentionally conservative first cuts. Tune later once we have
// prod telemetry on real thread-count distributions.
const (
	maxThreadsPerGroup       = 200
	maxTotalThreadChannelIDs = 2000
)

// contentTypeSet is the union of every payload.type the indexer emits.
// validate.contentTypesAllowed uses it to reject unknown values.
var contentTypeSet = map[int]struct{}{
	payloadTypeText:         {},
	payloadTypeImage:        {},
	payloadTypeVideo:        {},
	payloadTypeFile:         {},
	payloadTypeMergeForward: {},
	payloadTypeRichText:     {},
}

// validChannelTypesGlobal permits person(1)/group(2)/thread(5). Threads are
// included so a user selecting a thread `channel_id` isn't excluded by a
// mismatched channel_types filter (per §7.3).
func validChannelTypesGlobal(types []uint8) bool {
	for _, t := range types {
		if !validChannelType(t) {
			return false
		}
	}
	return true
}

// validateGlobalBase is the field-shape validator for the two global endpoints,
// derived from validateBase but without the single-channel channel_id/type
// gate. It returns the normalised page_size + ok, mirroring validateBase's
// contract; on ok=false a response was already written.
//
// New global-only validations layered on top:
//   - channel_types ⊆ {1,2,5}
//   - content_types ⊆ contentTypeSet (only meaningful for the message endpoint;
//     the file endpoint validator wraps this so an empty slice is fine)
//   - channel_ids[].channel_type ∈ {1,2,5} + id non-empty
//   - member_uid must be a non-empty string when set (self-uid is IGNORED at
//     resolveGlobalScope, not rejected here)
func validateGlobalBase(c *wkhttp.Context, cfg SearchConfig, sort, cursor string, filters GlobalSearchFilters, pageSize int, allowRelevance bool) (int, bool) {
	if _, ok := validateGlobalBaseSharedFields(c, filters); !ok {
		return 0, false
	}
	if strings.TrimSpace(filters.MemberUID) == "" && filters.MemberUID != "" {
		respondValidation(c, "filters.member_uid", "must be a non-empty uid")
		return 0, false
	}
	return validateSortCursorPage(c, cfg, sort, cursor, pageSize, allowRelevance)
}

// validateGlobalFileBase mirrors validateGlobalBase for the file endpoint.
// Adds file_exts (must be in the enum) + file_size_min ≤ file_size_max.
// content_types is not accepted on this endpoint.
func validateGlobalFileBase(c *wkhttp.Context, cfg SearchConfig, sort, cursor string, filters GlobalFileFilters, pageSize int, allowRelevance bool) (int, bool) {
	// Delegate the shared subset by projecting into the message-side struct.
	shared := GlobalSearchFilters{
		SenderIDs:    filters.SenderIDs,
		MemberUID:    filters.MemberUID,
		ChannelIDs:   filters.ChannelIDs,
		ChannelTypes: filters.ChannelTypes,
		SentAtFrom:   filters.SentAtFrom,
		SentAtTo:     filters.SentAtTo,
	}
	if _, ok := validateGlobalBaseSharedFields(c, shared); !ok {
		return 0, false
	}
	for i, ext := range filters.FileExts {
		norm := strings.ToLower(strings.TrimSpace(ext))
		if norm == "" || !isKnownFileExt(norm) {
			respondValidationDetails(c, i18n.Details{
				"field":  "filters.file_exts",
				"reason": "unknown extension",
				"index":  i,
			})
			return 0, false
		}
	}
	if filters.FileSizeMin < 0 || filters.FileSizeMax < 0 {
		respondValidation(c, "filters.file_size", "must be non-negative")
		return 0, false
	}
	if filters.FileSizeMax > 0 && filters.FileSizeMin > filters.FileSizeMax {
		respondValidation(c, "filters.file_size", "file_size_min must be <= file_size_max")
		return 0, false
	}
	return validateSortCursorPage(c, cfg, sort, cursor, pageSize, allowRelevance)
}

// validateGlobalBaseSharedFields is the pure-validation subset shared between
// the two global endpoint validators (everything except sort/cursor/page and
// file_exts/file_size). Returns (unused, ok); on ok=false a response was
// already written. Kept package-private since callers should pick the specific
// entry point (validateGlobalBase or validateGlobalFileBase).
func validateGlobalBaseSharedFields(c *wkhttp.Context, filters GlobalSearchFilters) (int, bool) {
	if len(filters.SenderIDs) > maxSenderIDs {
		respondValidationDetails(c, i18n.Details{
			"field":      "filters.sender_ids",
			"reason":     "too many",
			"max_length": maxSenderIDs,
		})
		return 0, false
	}
	if !validateSentAtWindow(c, filters.SentAtFrom, filters.SentAtTo) {
		return 0, false
	}
	if !validChannelTypesGlobal(filters.ChannelTypes) {
		respondValidation(c, "filters.channel_types", "must be a subset of {1,2,5}")
		return 0, false
	}
	for _, t := range filters.ContentTypes {
		if _, ok := contentTypeSet[t]; !ok {
			respondValidation(c, "filters.content_types", "unknown payload type")
			return 0, false
		}
	}
	for i, ref := range filters.ChannelIDs {
		if strings.TrimSpace(ref.ChannelID) == "" {
			respondValidationDetails(c, i18n.Details{
				"field":  "filters.channel_ids",
				"reason": "empty channel_id",
				"index":  i,
			})
			return 0, false
		}
		if !validChannelType(ref.ChannelType) {
			respondValidationDetails(c, i18n.Details{
				"field":  "filters.channel_ids",
				"reason": "invalid channel_type",
				"index":  i,
			})
			return 0, false
		}
	}
	return 0, true
}

// validateSentAtWindow parses filters.sent_at_from/to and ensures the window
// is ordered. Empty bounds are accepted. On error a validation response is
// already written; caller returns false.
func validateSentAtWindow(c *wkhttp.Context, from, to string) bool {
	fromTs, fromOK := int64(0), from == ""
	toTs, toOK := int64(0), to == ""
	if from != "" {
		fromTs, fromOK = parseSentAt(from, true)
		if !fromOK {
			respondValidation(c, "filters.sent_at_from", "invalid time format")
			return false
		}
	}
	if to != "" {
		toTs, toOK = parseSentAt(to, false)
		if !toOK {
			respondValidation(c, "filters.sent_at_to", "invalid time format")
			return false
		}
	}
	if from != "" && to != "" && fromTs > toTs {
		respondValidation(c, "filters", "sent_at_from must be <= sent_at_to")
		return false
	}
	return true
}

// validateSortCursorPage checks the sort enum, cursor signature and page_size
// range. Returns the normalised page_size. Extracted from validateBase so the
// global validators can compose it after their global-only checks.
func validateSortCursorPage(c *wkhttp.Context, cfg SearchConfig, sort, cursor string, pageSize int, allowRelevance bool) (int, bool) {
	switch sort {
	case "", "time_desc", "time_asc":
	case "relevance":
		if !allowRelevance {
			respondValidation(c, "sort", "relevance is not supported on this endpoint")
			return 0, false
		}
	default:
		respondValidation(c, "sort", "must be time_desc, time_asc, or relevance")
		return 0, false
	}
	if pageSize != 0 && (pageSize < minPageSize || pageSize > maxPageSize) {
		respondValidationDetails(c, i18n.Details{
			"field":      "page_size",
			"reason":     "out of range",
			"max_length": maxPageSize,
		})
		return 0, false
	}
	if cursor != "" {
		if _, _, _, _, err := decodeCursor(cfg, cursor); err != nil {
			respondValidation(c, "cursor", "malformed cursor")
			return 0, false
		}
	}
	page := pageSize
	if page == 0 {
		page = defaultPage
	}
	return page, true
}

// applyGlobalDMSpaceScope is the DM-only variant of applySpaceIDScope for the
// global endpoints. Because a single global query mixes DM + group + thread
// documents we cannot short-circuit on channelType — instead we require the
// spaceId term only on the DM (channelType=1) side.
//
// DSL shape (per §6.5):
//
//	should(
//	  mustNot(term channelType=1),                // group/thread: no spaceId gate
//	  filter([ term channelType=1, term spaceId=X ]) // DM: must match Space
//	) + minimumShouldMatch(1)
//
// When spaceID is empty we do NOT emit the clause. Caller is expected to have
// already fail-closed via RequireSpaceID before calling this — see the
// resolveGlobalScope contract.
func applyGlobalDMSpaceScope(b *elastic.BoolQuery, spaceID string) {
	if spaceID == "" {
		return
	}
	nonDM := elastic.NewBoolQuery().MustNot(elastic.NewTermQuery("channelType", channelTypePerson))
	dmInSpace := elastic.NewBoolQuery().
		Filter(elastic.NewTermQuery("channelType", channelTypePerson)).
		Filter(elastic.NewTermQuery("spaceId", spaceID))
	scope := elastic.NewBoolQuery().
		Should(nonDM, dmInSpace).
		MinimumShouldMatch("1")
	b.Filter(scope)
}

// resolveGlobalScope resolves the caller's per-request channel scope: the
// intersection of their allowlist (all rooms they can currently read within
// the requested Space) with the request's optional channel_ids / member_uid
// filters. It also computes the DM Space-scoping term the DSL must attach
// (§6.5 double-guard).
//
// The single-channel fast path (singleFast != nil) fires when the resolved
// scope collapses to exactly one channelId — the caller should re-dispatch
// to the corresponding single-channel handler (which runs the full
// checkChannelAccess gate) instead of taking the allowlist path.
//
// Returns (channelIDs, spaceID, singleFast, ok). On ok=false a response was
// already written (SEARCH_DISABLED / RequireSpaceID fail-close / DB error).
// Empty channelIDs with ok=true means the caller has no readable rooms in
// this Space OR the channel_ids/member_uid intersection is empty — the
// handler should return an empty envelope without touching OS.
func (h *Handler) resolveGlobalScope(c *wkhttp.Context, loginUID string, channelIDs []GlobalChannelRef, memberUID string) (osChannelIDs []string, spaceID string, singleFast *channelRef, timings allowlistTimings, ok bool) {
	spaceID = strings.TrimSpace(spacepkg.GetSpaceID(c))
	if spaceID == "" {
		// DM double-guard is space-dependent; without a Space the guard cannot
		// fire (§6.5). We fail-closed on the DM side identically to
		// resolveP2PSpaceScope so a missing Space cannot silently escape the
		// filter. RequireSpaceID=false is the operator escape hatch — we
		// mirror it here so the two paths behave the same during the v1.9
		// indexer rollout.
		if h.cfg.RequireSpaceID {
			respondNotFound(c, "channel")
			return nil, "", nil, timings, false
		}
		h.Warn("messages_search: global search without spaceID; OCTO_SEARCH_REQUIRE_SPACE_ID=false escape hatch active",
			zap.String("uid", loginUID))
	}

	allowGroup, allowDM, allowThread, allowTimings, err := h.buildAllowlist(c, loginUID, spaceID)
	timings = allowTimings
	if err != nil {
		h.Error("messages_search: allowlist build failed", zap.Error(err))
		respondInternal(c)
		return nil, "", nil, timings, false
	}
	allowSet := make(map[string]channelRef, len(allowGroup)+len(allowDM)+len(allowThread))
	for _, r := range allowGroup {
		allowSet[r.OSChannelID] = r
	}
	for _, r := range allowDM {
		allowSet[r.OSChannelID] = r
	}
	for _, r := range allowThread {
		allowSet[r.OSChannelID] = r
	}

	scope := allowSet
	if len(channelIDs) > 0 {
		requested := make(map[string]channelRef, len(channelIDs))
		for _, ref := range channelIDs {
			id := strings.TrimSpace(ref.ChannelID)
			if id == "" {
				continue
			}
			var osID, wireID string
			switch ref.ChannelType {
			case channelTypePerson:
				osID = fakeChannelIDFor(loginUID, id)
				wireID = id
			default:
				osID = id
				wireID = id
			}
			requested[osID] = channelRef{OSChannelID: osID, WireID: wireID, ChannelType: ref.ChannelType}
		}
		// Allowlist ∩ requested — anything the caller cannot read is silently
		// dropped (per §6.3, an unreachable channel_id is NOT a rejection).
		intersect := make(map[string]channelRef, len(requested))
		for id, ref := range requested {
			if _, present := allowSet[id]; present {
				intersect[id] = ref
			}
		}
		scope = intersect
	}

	memberUID = strings.TrimSpace(memberUID)
	if memberUID != "" && memberUID != loginUID {
		memberScope, mErr := h.channelsForMember(loginUID, memberUID, spaceID, allowSet)
		if mErr != nil {
			h.Error("messages_search: member-scope resolution failed", zap.Error(mErr))
			respondInternal(c)
			return nil, "", nil, timings, false
		}
		// scope ∩ memberScope
		intersect := make(map[string]channelRef, len(scope))
		for id, ref := range scope {
			if _, present := memberScope[id]; present {
				intersect[id] = ref
			}
		}
		scope = intersect
	}

	if len(scope) == 0 {
		return nil, spaceID, nil, timings, true
	}

	osChannelIDs = make([]string, 0, len(scope))
	for id := range scope {
		osChannelIDs = append(osChannelIDs, id)
	}
	osChannelIDs = util.RemoveRepeatedElement(osChannelIDs)

	if len(scope) == 1 {
		for _, ref := range scope {
			r := ref
			singleFast = &r
		}
	}
	return osChannelIDs, spaceID, singleFast, timings, true
}

// buildAllowlist enumerates every OS channelId the caller can read within the
// given Space: joined groups + DM peers + active threads under those joined
// groups. DM peers come from both the caller's friend list AND the current
// Space's member list (§6.2) — see enumerateDMPeers for the union +
// bot-in-space semantics. Threads are enumerated per-group via
// enumerateThreadsForGroups (batch IN query over the joined group set), and
// are subject to two hard caps (maxThreadsPerGroup / maxTotalThreadChannelIDs)
// so a runaway thread population downgrades gracefully to "group only" rather
// than blowing the OS terms clause.
//
// Thread coverage — v1 scope (YUJ-10, supersedes the stage3 v1.1 defer):
//
//   - Thread messages surface on BOTH the unfiltered global stream AND when a
//     request narrows via filters.channel_ids with channel_type=5. In the
//     terms(channelId, allowlist) DSL that gates every global query, thread
//     channelIDs (composite `{group_no}____{short_id}`, per
//     thread.BuildChannelID) sit alongside group + DM channelIDs.
//   - Rationale: single batch IN query bounded by joined groups (typically
//     < 1k for even heavy users; each with 5–15 quantile active threads)
//     stays well inside OpenSearch's `indices.query.bool.max_clause_count`.
//     Threads share their parent group's membership so no extra auth check
//     is needed — group membership already gates thread reachability.
//   - Soft-fail: if QueryNonDeletedShortIDsByGroupNos errors, threads are
//     dropped from the allowlist but the group + DM parts still serve
//     (WARN log; the whole request does NOT 500). Same policy as the
//     external-group / space_member soft-fails elsewhere in this helper.
//   - Hard caps: per-group threads capped at maxThreadsPerGroup; total
//     thread channelIDs across all groups capped at maxTotalThreadChannelIDs.
//     Beyond either cap we WARN and skip further thread ids for that group
//     (or the whole request) — the group's own message hits still surface,
//     only its thread hits get hard-dropped for this request.
//   - Visibility: archived threads are INCLUDED (contract is "reject deleted,
//     allow archived" — matches single-channel search + message read; see
//     thread.DB.QueryNonDeletedShortIDsByGroupNos). Deleted threads are NOT
//     surfaced. Aligning global search with the rest of the system was RC
//     blocker on PR #553.
//
// Known v1 gap: channelsForMember (the member_uid "包含成员" filter) still
// scopes to group + DM only; it explicitly DROPS every thread entry from
// the allowlist when member_uid is set (see channelsForMember below,
// `case channelTypeThread:` is a no-op). A caller filtering by member_uid
// therefore sees zero thread hits, regardless of whether the named member
// posted in any thread. This is fail-closed (more-restrictive than a
// hypothetical thread_member intersection), safe on the security axis,
// but it is a real UX gap the API contract should surface. Tightening
// this requires a `thread_member` join and is deferred to v1.1; the
// tradeoff is documented on channelsForMember.
func (h *Handler) buildAllowlist(_ *wkhttp.Context, loginUID, spaceID string) ([]channelRef, []channelRef, []channelRef, allowlistTimings, error) {
	var timings allowlistTimings
	groups, err := h.groupService.GetGroupsWithMemberUID(loginUID)
	if err != nil {
		return nil, nil, nil, timings, err
	}
	externalLookup := h.externalGroupFn
	if externalLookup == nil {
		externalLookup = group.NewDB(h.ctx).QueryExternalGroupNosForUser
	}
	externalGroupMap, extErr := externalLookup(loginUID)
	if extErr != nil {
		// Same soft-fail as modules/search/api.go — external groups become
		// invisible for this request but the rest of the allowlist proceeds.
		h.Warn("messages_search: external-group lookup failed; external groups will be hidden",
			zap.Error(extErr))
		externalGroupMap = map[string]string{}
	}
	// Space-filter FIRST so the active-status gate below only pays the DB round-trip
	// on rooms the caller could otherwise see. Preserve enumeration order for
	// deterministic OS-terms output.
	candidateGroupNos := make([]string, 0, len(groups))
	candidateGroupSet := make(map[string]struct{}, len(groups))
	groupNos := make([]string, 0, len(groups))
	for _, g := range groups {
		if g == nil {
			continue
		}
		if spaceID != "" && !shouldIncludeGroupForSpaceLocal(g.SpaceID, spaceID, g.GroupNo, externalGroupMap) {
			continue
		}
		if _, dup := candidateGroupSet[g.GroupNo]; dup {
			continue
		}
		candidateGroupSet[g.GroupNo] = struct{}{}
		candidateGroupNos = append(candidateGroupNos, g.GroupNo)
	}
	// Access-control gate: drop groups whose group_member row is non-Normal
	// (status=Blacklist / future non-active states). Mirrors what the single-
	// channel path enforces via checkGroupAccess -> ExistMemberActive, so a
	// group-blacklisted member cannot search that group via the global feed
	// (YUJ-11 RC blocker #1). Fail-closed on error: return the error instead
	// of degrading to the un-gated allowlist.
	activeGroupSet := candidateGroupSet
	if len(candidateGroupNos) > 0 {
		start := time.Now()
		activeNos, gerr := h.groupService.ExistMembersActive(candidateGroupNos, loginUID)
		timings.memberActive += time.Since(start)
		if gerr != nil {
			h.Error("messages_search: ExistMembersActive lookup failed; fail-closed on group allowlist",
				zap.String("login_uid", loginUID),
				zap.Error(gerr))
			return nil, nil, nil, timings, gerr
		}
		activeGroupSet = make(map[string]struct{}, len(activeNos))
		for _, no := range activeNos {
			activeGroupSet[no] = struct{}{}
		}
	}
	allowGroup := make([]channelRef, 0, len(candidateGroupNos))
	for _, no := range candidateGroupNos {
		if _, ok := activeGroupSet[no]; !ok {
			continue
		}
		allowGroup = append(allowGroup, channelRef{
			OSChannelID: no,
			WireID:      no,
			ChannelType: channelTypeGroup,
		})
		groupNos = append(groupNos, no)
	}

	// DM peers: friend list ∪ Space members, minus bots not in the current
	// Space. Mirrors the filtering in modules/search/api.go so the two
	// surfaces converge on the same DM candidate set.
	dmStart := time.Now()
	dmPeers, dmBlacklistMs, dmErr := h.enumerateDMPeersTimed(loginUID, spaceID)
	timings.dmPeers += time.Since(dmStart) - dmBlacklistMs
	timings.blacklist += dmBlacklistMs
	if dmErr != nil {
		return nil, nil, nil, timings, dmErr
	}
	allowDM := make([]channelRef, 0, len(dmPeers))
	for _, peer := range dmPeers {
		if peer == "" || peer == loginUID {
			continue
		}
		allowDM = append(allowDM, channelRef{
			OSChannelID: fakeChannelIDFor(loginUID, peer),
			WireID:      peer,
			ChannelType: channelTypePerson,
		})
	}

	// Threads under the joined groups — single batch IN query. Soft-fail:
	// on error we log + serve group/DM only so a MySQL blip on the thread
	// side doesn't sink the whole global endpoint.
	threadStart := time.Now()
	allowThread := h.enumerateThreadsForGroups(groupNos)
	timings.threadEnum += time.Since(threadStart)

	// Kept as separate slices only for readability at the call site; the
	// caller flattens all three into a single set.
	return allowGroup, allowDM, allowThread, timings, nil
}

// enumerateThreadsForGroups fans a single batch query against the thread
// table and turns every (groupNo, shortID) row into a composite
// `{groupNo}____{shortID}` channelRef with channelType=5. Bounded by two
// caller-side hard caps (maxThreadsPerGroup / maxTotalThreadChannelIDs) so
// a runaway group cannot alone blow the OS terms clause.
//
// The DB query itself is bounded **per group** via a UNION ALL of
// per-group `LIMIT` subqueries (thread.NonDeletedByGroupNosPerGroupHardLimit
// = 201), so a single fat group with tens of thousands of non-deleted
// threads no longer starves the DB budget of other groups (RC 3 on PR
// #553). Previous revisions used one global `ORDER BY group_no, short_id
// LIMIT 2500` which let a group sorting early consume the entire budget;
// every other group then returned zero rows and — combined with the
// caller's `continue` on the fat group's per-group cap — zeroed out
// thread coverage across the whole request. The per-group LIMIT eliminates
// that failure mode by construction, and stays portable (MySQL 5.7 /
// 8.0 / MariaDB) by avoiding the 8.0-only window function.
//
// Visibility: threads with status != deleted (i.e. active OR archived) are
// included, aligning with single-channel search + message read semantics.
// See modules/thread/db.go::QueryNonDeletedShortIDsByGroupNos.
//
// Returns an empty slice on error (WARN logged) — the caller then serves
// group + DM only, matching the external-group / space_member soft-fail
// policy on the same helper.
func (h *Handler) enumerateThreadsForGroups(groupNos []string) []channelRef {
	if len(groupNos) == 0 {
		return nil
	}
	enum := h.threadEnumFn
	if enum == nil {
		enum = thread.NewDB(h.ctx).QueryNonDeletedShortIDsByGroupNos
	}
	byGroup, err := enum(groupNos)
	if err != nil {
		h.Warn("messages_search: thread enumeration failed; thread hits will be hidden for this request",
			zap.Error(err))
		return nil
	}
	// Deterministic iteration — range over groupNos, not the map, so the
	// hard-cap downgrade is reproducible across runs and easy to reason
	// about in tests.
	out := make([]channelRef, 0)
	total := 0
	for _, gn := range groupNos {
		shortIDs := byGroup[gn]
		if len(shortIDs) == 0 {
			continue
		}
		// Count non-blank rows once and reuse for BOTH cap checks. Production
		// QueryNonDeletedShortIDsByGroupNos already strips blank shortIDs at
		// the SQL parse boundary, but the test seam threadEnumFn (and any
		// future alternative backend) is not required to — keep the caps
		// consistent by looking through blanks in both.
		nonBlank := 0
		for _, sid := range shortIDs {
			if sid != "" {
				nonBlank++
			}
		}
		if nonBlank == 0 {
			continue
		}
		if nonBlank > maxThreadsPerGroup {
			h.Warn("messages_search: thread count per group exceeds cap; downgrading to group-only for this request",
				zap.String("group_no", gn),
				zap.Int("thread_count", nonBlank),
				zap.Int("cap", maxThreadsPerGroup))
			continue
		}
		if total+nonBlank > maxTotalThreadChannelIDs {
			h.Warn("messages_search: total thread channelIDs would exceed global cap; skipping remaining groups",
				zap.String("skipped_group_no", gn),
				zap.Int("running_total", total),
				zap.Int("would_add", nonBlank),
				zap.Int("cap", maxTotalThreadChannelIDs))
			break
		}
		for _, sid := range shortIDs {
			if sid == "" {
				continue
			}
			id := thread.BuildChannelID(gn, sid)
			out = append(out, channelRef{
				OSChannelID: id,
				WireID:      id,
				ChannelType: channelTypeThread,
			})
		}
		total += nonBlank
	}
	return out
}

// enumerateDMPeers returns the peer UIDs whose DM the caller is allowed to
// see in the current Space. Peers come from TWO sources (§6.2):
//
//  1. The caller's friend list (always).
//  2. Same-Space members (only when spaceID != ""), matching the legacy
//     `/v1/search/global` behaviour in modules/search/api.go — the caller
//     may DM anyone in the same Space, not just their friends.
//
// The two sources are unioned and de-duplicated. Non-Space (empty spaceID)
// deployments keep the friend-only fallback.
//
// Bot handling: a bot the caller is talking to must be a member of the
// current Space (or a SystemBot) to be visible — matches
// modules/search/api.go's cross-check via spacepkg.CheckBotsInSpace so a
// non-space bot cannot leak into search results through the DM path.
//
// Soft-fail: an error reading space_member degrades to "friends only" with a
// WARN log — aligns with the external-group soft-fail behaviour in
// buildAllowlist so a MySQL blip on one edge doesn't sink the whole request.
func (h *Handler) enumerateDMPeers(loginUID, spaceID string) ([]string, error) {
	friends, err := h.userService.GetFriends(loginUID)
	if err != nil {
		return nil, err
	}
	peers := make([]string, 0, len(friends))
	for _, f := range friends {
		if f == nil || f.UID == "" || f.UID == loginUID {
			continue
		}
		peers = append(peers, f.UID)
	}
	if spaceID != "" {
		members, mErr := h.fetchSpaceMemberUIDs(spaceID, loginUID)
		if mErr != nil {
			// Same fail-soft as the external-group lookup: rather than sink
			// the request, degrade to the friends-only allowlist. Non-friend
			// Space DMs will not surface for this request; the friend edge
			// still works.
			h.Warn("messages_search: space_member enumeration failed; falling back to friends-only DM allowlist",
				zap.Error(mErr))
		} else {
			peers = append(peers, members...)
		}
	}
	peers = util.RemoveRepeatedElement(peers)
	// Bidirectional blacklist gate: mirror single-channel checkP2PAccess (authz.go
	// Step 3) so a DM peer who has blacklisted the caller, or whom the caller has
	// blacklisted, does NOT appear in the global allowlist. Runs BEFORE the
	// bot/Space gate and BEFORE the empty-spaceID early return so friend-only
	// deployments still enforce it (YUJ-11 RC blocker #2).
	peers = h.filterBlacklistedDMPeers(loginUID, peers)
	if spaceID == "" {
		return peers, nil
	}
	// Apply the bot-in-space gate. Non-bot DMs need no extra work — Space
	// containment is already implied by the friend / space_member gate that
	// picked them (the caller's Space middleware would have rejected the
	// request otherwise). Bots have no space_member row so we check them
	// explicitly against CheckBotsInSpace.
	if len(peers) == 0 {
		return peers, nil
	}
	return h.applyDMBotFilter(spaceID, peers)
}

// enumerateDMPeersTimed is a timing-aware wrapper around enumerateDMPeers.
// blacklistDur reports the wall-clock time spent inside
// ExistBlacklistsBoth so the caller can bucket it under a dedicated
// audit field (audit's blacklist_ms). The rest of the time — GetFriends,
// fetchSpaceMemberUIDs, bot filter — is attributed to the dm_peers bucket
// by the caller subtracting blacklistDur from the wall-clock delta.
func (h *Handler) enumerateDMPeersTimed(loginUID, spaceID string) ([]string, time.Duration, error) {
	startToken := time.Now()
	_ = startToken // reserved: measure sub-phases in enumerateDMPeers itself
	// We instrument the sole blacklist call via an atomic
	// counter-style capture. Simplest option: temporarily swap in a
	// shim on filterBlacklistedDMPeers is intrusive; instead we
	// re-implement enumerateDMPeers here with the timing hook inlined
	// so the production path stays a single method. The two must stay
	// semantically equivalent — tests for enumerateDMPeers exercise
	// the plain method (below) directly.
	var blacklistDur time.Duration
	friends, err := h.userService.GetFriends(loginUID)
	if err != nil {
		return nil, 0, err
	}
	peers := make([]string, 0, len(friends))
	for _, f := range friends {
		if f == nil || f.UID == "" || f.UID == loginUID {
			continue
		}
		peers = append(peers, f.UID)
	}
	if spaceID != "" {
		members, mErr := h.fetchSpaceMemberUIDs(spaceID, loginUID)
		if mErr != nil {
			h.Warn("messages_search: space_member enumeration failed; falling back to friends-only DM allowlist",
				zap.Error(mErr))
		} else {
			peers = append(peers, members...)
		}
	}
	peers = util.RemoveRepeatedElement(peers)

	// Time only the bidirectional blacklist gate — the piece YUJ-27
	// batched from 2N per-peer round-trips down to 1 IN query.
	blStart := time.Now()
	peers = h.filterBlacklistedDMPeers(loginUID, peers)
	blacklistDur = time.Since(blStart)

	if spaceID == "" {
		return peers, blacklistDur, nil
	}
	if len(peers) == 0 {
		return peers, blacklistDur, nil
	}
	filtered, err := h.applyDMBotFilter(spaceID, peers)
	return filtered, blacklistDur, err
}

// filterBlacklistedDMPeers drops peers involved in a bidirectional-blacklist
// edge with loginUID. Fail-closed on error: on batch-lookup failure the
// whole peer set is dropped for this request (mirrors the previous per-peer
// behaviour which skipped the offending peer; a single batch call means one
// error affects every peer at once, so fail-closed = drop them all rather
// than silently downgrade to an un-gated allowlist).
//
// Perf (YUJ-27): previously this function did 2N MySQL round-trips (one
// ExistBlacklist per direction per peer). For a user with a few hundred
// friends / same-Space members that was the dominant latency source on
// _search_global_messages / _search_global_files (~4.5s end-to-end even
// though OS itself served in <20ms). We now issue a single IN-based batch
// via userService.ExistBlacklistsBoth, which preserves the exact
// bidirectional semantics of the per-pair check while collapsing 2N
// round-trips to 1.
func (h *Handler) filterBlacklistedDMPeers(loginUID string, peers []string) []string {
	if len(peers) == 0 {
		return peers
	}
	blockedByMe, blockedByPeer, err := h.userService.ExistBlacklistsBoth(loginUID, peers)
	if err != nil {
		// Fail-closed: hide every candidate DM rather than leak a
		// blacklisted one when the blacklist table is unreachable. Matches
		// the intent of the previous per-peer fail-closed branch (skip the
		// offender); with a single batch call an error affects the whole
		// set at once, so drop the whole set.
		h.Error("messages_search: ExistBlacklistsBoth failed; dropping all DM peers fail-closed",
			zap.String("login_uid", loginUID),
			zap.Int("peer_count", len(peers)),
			zap.Error(err))
		return nil
	}
	out := make([]string, 0, len(peers))
	for _, peer := range peers {
		if peer == "" || peer == loginUID {
			continue
		}
		if blockedByMe[peer] || blockedByPeer[peer] {
			continue
		}
		out = append(out, peer)
	}
	return out
}

// applyDMBotFilter runs the bot-in-Space suppression on the DM peer set —
// production hits h.ctx.DB() via spacepkg helpers; tests inject dmBotFilterFn
// so enumerateDMPeers is testable without a real MySQL connection.
func (h *Handler) applyDMBotFilter(spaceID string, peers []string) ([]string, error) {
	if h.dmBotFilterFn != nil {
		return h.dmBotFilterFn(spaceID, peers)
	}
	botSet, err := spacepkg.GetBotUIDs(h.ctx.DB(), peers)
	if err != nil {
		// Same fail-soft as modules/search/api.go: skip bot filtering rather
		// than break the whole DM enumeration.
		h.Warn("messages_search: global DM bot enumeration failed; bot Space filter skipped", zap.Error(err))
		return peers, nil
	}
	if len(botSet) == 0 {
		return peers, nil
	}
	botInSpace, err := spacepkg.CheckBotsInSpace(h.ctx.DB(), spaceID, botSet)
	if err != nil {
		h.Warn("messages_search: bot-in-space check failed; bot Space filter skipped", zap.Error(err))
		return peers, nil
	}
	filtered := make([]string, 0, len(peers))
	for _, p := range peers {
		if botSet[p] && !botInSpace[p] {
			continue
		}
		filtered = append(filtered, p)
	}
	return filtered, nil
}

// fetchSpaceMemberUIDs dispatches to a swappable member-fetch function.
// Production returns queryDMSpaceMemberUIDs (raw SQL); tests inject a stub via
// the spaceMembersFn override so enumerateDMPeers can be exercised without a
// real MySQL connection.
func (h *Handler) fetchSpaceMemberUIDs(spaceID, loginUID string) ([]string, error) {
	if h.spaceMembersFn != nil {
		return h.spaceMembersFn(spaceID, loginUID)
	}
	return h.queryDMSpaceMemberUIDs(spaceID, loginUID)
}

// queryDMSpaceMemberUIDs returns the active members of spaceID, minus the
// caller. Mirrors modules/search/api.go's raw SQL over space_member so the two
// surfaces converge on the same candidate set — the Space allowlist is the same
// wherever it is consulted. The query is bounded by (space_id, status) which
// both live in the space_member primary key covering index.
func (h *Handler) queryDMSpaceMemberUIDs(spaceID, loginUID string) ([]string, error) {
	var rows []struct {
		UID string `db:"uid"`
	}
	_, err := h.ctx.DB().SelectBySql(
		"SELECT uid FROM space_member WHERE space_id=? AND status=1 AND uid<>?",
		spaceID, loginUID,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.UID == "" {
			continue
		}
		out = append(out, r.UID)
	}
	return out, nil
}

// channelsForMember resolves the "包含成员" (member_uid) filter into the set of
// OS channelIds where BOTH the caller and the named member are reachable
// together: the caller's allowlist ∩ (groups memberUID is in ∪ DM with
// memberUID). Returned map keys are the OS channelId; values mirror the
// allowSet entry so the caller can preserve wire-id / channelType.
//
// v1 thread scope (YUJ-10): this helper filters ONLY group + DM entries by
// memberUID's own membership. Thread entries in allowSet are dropped from the
// returned scope entirely — v1 does NOT resolve `thread_member` for the named
// member (would require an extra IN join and a schema pass). A caller that
// sets member_uid therefore sees group + DM hits that co-inhabit memberUID,
// but NO thread hits at all. This is a deliberate v1 simplification: the
// primary YUJ-10 requirement is "thread hits appear in the global feed";
// scoping them by an arbitrary member is a v1.1 follow-up (see the design
// doc §6.2 known-limitations note).
func (h *Handler) channelsForMember(loginUID, memberUID, spaceID string, allowSet map[string]channelRef) (map[string]channelRef, error) {
	out := make(map[string]channelRef)
	memberGroups, err := h.groupService.GetGroupsWithMemberUID(memberUID)
	if err != nil {
		return nil, err
	}
	memberSet := make(map[string]struct{}, len(memberGroups))
	for _, g := range memberGroups {
		if g == nil {
			continue
		}
		memberSet[g.GroupNo] = struct{}{}
	}
	for id, ref := range allowSet {
		switch ref.ChannelType {
		case channelTypeGroup:
			if _, ok := memberSet[ref.OSChannelID]; ok {
				out[id] = ref
			}
		case channelTypePerson:
			if ref.WireID == memberUID {
				// The DM with memberUID is trivially the "shared channel"
				// between caller and member.
				out[id] = ref
			}
		case channelTypeThread:
			// Intentionally dropped for v1 — see helper doc-comment above.
			// Explicit case so a future maintainer adding a default: arm
			// sees the drop is intentional and not a forgotten branch.
		}
	}
	// spaceID is unused: the caller has already narrowed allowSet to the
	// current Space in resolveGlobalScope (via buildAllowlist), so the ∩
	// against allowSet here inherits the Space filter transitively. The
	// parameter is kept on the signature both for symmetry with the other
	// scope helpers and so a future cross-Space feature (e.g. surfacing DMs
	// from Spaces the member belongs to but the caller does not) has an
	// obvious pivot without changing the call site.
	_ = spaceID
	return out, nil
}

// shouldIncludeGroupForSpaceLocal mirrors modules/search/api.go's
// shouldIncludeGroupForSpace so this package doesn't take a dependency on
// modules/search. Keep in sync — the two callers are the only ones and the
// dependency direction (messages_search → search) is otherwise avoided.
func shouldIncludeGroupForSpaceLocal(groupSpaceID, searchSpaceID, groupNo string, externalGroupMap map[string]string) bool {
	if searchSpaceID == "" {
		return false
	}
	if groupSpaceID == searchSpaceID {
		return true
	}
	if sourceSpace, ok := externalGroupMap[groupNo]; ok && sourceSpace == searchSpaceID {
		return true
	}
	return false
}

// wireChannelFromDoc derives the wire (channel_id, channel_type) pair the
// global response should carry for a hit. Global hits carry doc.ChannelID =
// the OS channelId (fakeChannelID for DM, plain groupNo / composite thread id
// for the others); DM must be reversed back to the peer uid so the frontend
// can jump to the DM by uid (§9.1 NEW-A). Group and thread channel_ids are
// echoed unchanged.
func wireChannelFromDoc(doc Doc, loginUID string) (string, uint8) {
	channelType := uint8(doc.ChannelType)
	if channelType == channelTypePerson {
		return peerFromFakeChannelID(doc.ChannelID, loginUID), channelType
	}
	return doc.ChannelID, channelType
}
