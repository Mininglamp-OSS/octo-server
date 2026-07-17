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
	SenderIDs []string `json:"sender_ids,omitempty"`
	// MemberUID is the legacy single-select "包含成员" field. Superseded by
	// MemberUIDs (multi-select, bug 5) but kept on the wire for backwards
	// compatibility with older clients and to survive a rolling deploy window
	// where a stale frontend can still coexist with a fresh backend. When both
	// fields arrive, MemberUIDs wins; if only MemberUID is set the handler
	// folds it into the plural path via normalizeMemberUIDs.
	MemberUID    string             `json:"member_uid,omitempty"`
	MemberUIDs   []string           `json:"member_uids,omitempty"`
	ChannelIDs   []GlobalChannelRef `json:"channel_ids,omitempty"`
	ChannelTypes []uint8            `json:"channel_types,omitempty"`
	ContentTypes []int              `json:"content_types,omitempty"`
	SentAtFrom   string             `json:"sent_at_from,omitempty"`
	SentAtTo     string             `json:"sent_at_to,omitempty"`
}

// GlobalFileFilters is the file-endpoint-only filter block: the shared base
// (via GlobalSearchFilters) plus file_exts / file_size_min / file_size_max.
type GlobalFileFilters struct {
	SenderIDs []string `json:"sender_ids,omitempty"`
	// MemberUID / MemberUIDs: see GlobalSearchFilters. Same wire contract on
	// the file endpoint.
	MemberUID    string             `json:"member_uid,omitempty"`
	MemberUIDs   []string           `json:"member_uids,omitempty"`
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
	for i, uid := range filters.MemberUIDs {
		if strings.TrimSpace(uid) == "" {
			respondValidationDetails(c, i18n.Details{
				"field":  "filters.member_uids",
				"reason": "empty uid",
				"index":  i,
			})
			return 0, false
		}
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
		MemberUIDs:   filters.MemberUIDs,
		ChannelIDs:   filters.ChannelIDs,
		ChannelTypes: filters.ChannelTypes,
		SentAtFrom:   filters.SentAtFrom,
		SentAtTo:     filters.SentAtTo,
	}
	if _, ok := validateGlobalBaseSharedFields(c, shared); !ok {
		return 0, false
	}
	for i, uid := range filters.MemberUIDs {
		if strings.TrimSpace(uid) == "" {
			respondValidationDetails(c, i18n.Details{
				"field":  "filters.member_uids",
				"reason": "empty uid",
				"index":  i,
			})
			return 0, false
		}
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
	// DoS floor: cap wire-side member_uids before resolveGlobalScope fans out
	// one GetGroupsWithMemberUID per uid. Left uncapped, a caller supplying
	// N=1000 uids triggered N serial DB queries (and, worse, before the
	// per-request search Timeout was even wired in). 50 matches maxSenderIDs
	// as the "obviously reasonable" ceiling for a picker-driven list.
	if len(filters.MemberUIDs) > maxMemberUIDs {
		respondValidationDetails(c, i18n.Details{
			"field":      "filters.member_uids",
			"reason":     "too many",
			"max_length": maxMemberUIDs,
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

// normalizeMemberUIDs collapses the two wire fields (`member_uids` plural,
// `member_uid` legacy singular) into the canonical dedup + self-exclude form
// that resolveGlobalScope consumes. Precedence: plural wins whenever it has any
// entry; otherwise the singular is folded into a single-element slice. Empty
// strings, whitespace-only entries, and the caller's own uid are dropped
// (matches the frontend `filter(uid !== selfUid)` convention so a caller that
// checkboxes themselves gets the same result set as one that did not). A nil
// return means "no member filter".
//
// Both fields are accepted at once (§bug 5): during the rolling deploy an
// older frontend still ships `member_uid` while a newer one ships
// `member_uids`; a mixed request from a mid-refresh browser tab is possible
// and must not double-apply.
func normalizeMemberUIDs(loginUID string, uids []string, single string) []string {
	loginUID = strings.TrimSpace(loginUID)
	raw := uids
	if len(raw) == 0 && strings.TrimSpace(single) != "" {
		raw = []string{single}
	}
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, u := range raw {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if u == loginUID {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
func (h *Handler) resolveGlobalScope(c *wkhttp.Context, loginUID string, channelIDs []GlobalChannelRef, rawMemberUIDs []string, legacyMemberUID string) (osChannelIDs []string, spaceID string, singleFast *channelRef, timings allowlistTimings, ok bool) {
	// spaceID 走 principal（决策十）：真人等价于 GetSpaceID(c)；bot 路由无 SpaceMiddleware
	// 故为空；uk 取 api_key_space_id.
	p := h.principal(c)
	spaceID = p.SpaceID()
	if spaceID == "" && p.RequiresSpaceScope() {
		// DM double-guard is space-dependent; without a Space the guard cannot
		// fire (§6.5). We fail-closed on the DM side identically to
		// resolveP2PSpaceScope so a missing Space cannot silently escape the
		// filter. RequireSpaceID=false is the operator escape hatch — we
		// mirror it here so the two paths behave the same during the v1.9
		// indexer rollout.
		//
		// YUJ-57: only real-user-scoped principals (user / uk) are subject to
		// this gate. Space-less principals (as-bot / OBO) legitimately carry no
		// Space and their allowlist is enumerated by the per-principal readable
		// predicate (IsFriend / grantor allowlist), which already bounds DM
		// visibility without a spaceId term — so they proceed with spaceID=""
		// (applyGlobalDMSpaceScope then emits no DM clause) instead of being
		// blocked here before ever reaching enumerateReadableChannels.
		if h.cfg.RequireSpaceID {
			respondNotFound(c, "channel")
			return nil, "", nil, timings, false
		}
		h.Warn("messages_search: global search without spaceID; OCTO_SEARCH_REQUIRE_SPACE_ID=false escape hatch active",
			zap.String("uid", loginUID))
	}

	// 全局 allowlist 枚举经归一化谓词 enumerateReadableChannels（决策九）：真人语义主体
	// 委托 buildAllowlist（现状不变），as-bot 分支在 #E 落——保证单频道门与 allowlist
	// 共用同一谓词，不各写一套。principalForSubject 优先取路由显式写入的 principal
	//（bot/uk，#B），否则用已解析的 (loginUID, spaceID) 组装真人载体（现网/既有单测行为不变）。
	allowGroup, allowDM, allowThread, allowTimings, err := h.enumerateReadableChannels(c, principalForSubject(c, loginUID, spaceID))
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
		// requestedGroups collects the OS channelId (== group_no) of every
		// channel_ids entry that named a group (channelType=2). A selected
		// group implicitly covers its sub-threads (bug 4), so we fold those in
		// below before intersecting with the allowlist.
		requestedGroups := make(map[string]struct{})
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
			if ref.ChannelType == channelTypeGroup {
				requestedGroups[osID] = struct{}{}
			}
		}
		// Bug 4: selecting a group must search the group body AND all of its
		// sub-threads, mirroring what the frontend's "所在群聊或子区" filter
		// implies. Thread channelIds live in allowSet keyed as the composite
		// `{group_no}____{short_id}` (channelType=5); fold every allowlisted
		// thread whose parent group was requested into the requested set so the
		// terms(channelId) clause spans group + threads. Sourcing them from
		// allowSet (rather than a fresh enumerateThreadsForGroups call) means we
		// inherit the membership gate + the per-group / aggregate thread caps
		// buildAllowlist already applied, with no extra DB round-trip. A thread
		// selected directly (channelType=5) is unaffected: it is not a group, so
		// it never seeds requestedGroups and stays scoped to itself alone.
		if len(requestedGroups) > 0 {
			for osID, ref := range allowSet {
				if ref.ChannelType != channelTypeThread {
					continue
				}
				groupNo, _, err := thread.ParseChannelID(osID)
				if err != nil {
					continue
				}
				if _, ok := requestedGroups[groupNo]; ok {
					requested[osID] = ref
				}
			}
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

	memberUIDs := normalizeMemberUIDs(loginUID, rawMemberUIDs, legacyMemberUID)
	if len(memberUIDs) > 0 {
		memberScope, mErr := h.channelsForMembers(loginUID, memberUIDs, spaceID, allowSet)
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
// Member filter (bug 5, updated): channelsForMembers now folds every thread
// under a surviving shared group into the returned scope (統一 rule: 群 → 群 +
// 其子区). The earlier v1 "thread_member unavailable, so drop all threads"
// behaviour is superseded — threads inherit their parent group's membership
// gate, so no `thread_member` join is required to be correct.
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

// buildBotAllowlist enumerates the as-bot global allowlist (#E / YUJ-52): the
// exact channel set botCanReadChannel (#C/#D) would admit for this bot, so the
// single-channel gate and the global feed stay one predicate (决策九). It is the
// bot-subject counterpart of buildAllowlist — same enumeration skeleton, but the
// subject is botUID and the bot主体语义 strips two real-user-only edges:
//
//   - Groups: GetGroupsWithMemberUID(botUID) gated by ExistMembersActive — the
//     enumeration dual of the ExistMemberActive relation the single-channel group
//     gate (#D) point-evaluates. status!=Normal rows (kicked / group-blacklisted,
//     including the #354 cascade that flips an owner-blacklisted user's in-group
//     bot to non-Normal) drop out for free, so 群级黑名单 is inherited, not re-coded.
//   - DM peers: GetFriends(botUID) — the enumeration dual of IsFriend(botUID, peer)
//     (#C). No bidirectional-blacklist gate (bot 主体 blacklistPolicy=none: bot 无法
//     有意义地拉黑/被拉黑，见 #C), no Space-member union and no bot-in-Space
//     suppression (bot 无 Space；且此处主体本身即 bot，对端由 IsFriend 收口).
//   - Threads: active threads under the active groups, reusing
//     enumerateThreadsForGroups (子区继承父群成员身份，与 #D 一致).
//
// 有界性 (issue 已确认): checkBotOwnership 只允许创建者把自己的 bot 拉进群，
// cascadeRemoveBotsInvitedByUIDTx 在创建者退群时级联移除其 bot，故 bot 群集合 ⊆ 创建者
// 群集合，量级与真人 global 同阶——沿用现有 per-group / 聚合 thread 上限
// (maxThreadsPerGroup / maxTotalThreadChannelIDs)，不为 bot 另设上限；超限降级行为与真人
// 路径完全一致 (enumerateThreadsForGroups 同一实现)。
//
// 归一化 (决策九硬约束): 与 buildAllowlist 的差异纯粹是主体语义 (bot 无 Space、无黑名单)，
// 不在此内联重写任何鉴权规则——所用的 friendship / active-membership 关系与 #C/#D 单频道门
// 同源，故「单频道门放行 ⇔ 出现在 global allowlist」(#G 跨路一致性) 成立。
func (h *Handler) buildBotAllowlist(botUID string) ([]channelRef, []channelRef, []channelRef, allowlistTimings, error) {
	var timings allowlistTimings
	if botUID == "" {
		// Defensive: an empty subject owns no channel. Fail-closed to an empty
		// allowlist rather than risk enumerating an unbounded set.
		return nil, nil, nil, timings, nil
	}

	// Groups: joined groups gated by active membership. No Space filter (bot has
	// no Space) and thus no external-group lookup — both are real-user Space
	// machinery that does not apply to the bot subject.
	groups, err := h.groupService.GetGroupsWithMemberUID(botUID)
	if err != nil {
		return nil, nil, nil, timings, err
	}
	candidateGroupNos := make([]string, 0, len(groups))
	candidateGroupSet := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		if g == nil || g.GroupNo == "" {
			continue
		}
		if _, dup := candidateGroupSet[g.GroupNo]; dup {
			continue
		}
		candidateGroupSet[g.GroupNo] = struct{}{}
		candidateGroupNos = append(candidateGroupNos, g.GroupNo)
	}
	// Active-status gate — mirrors buildAllowlist: drop groups whose group_member
	// row is non-Normal so a kicked / group-blacklisted bot cannot search that
	// group via the global feed. Fail-closed on error (return, don't degrade to
	// an un-gated allowlist).
	activeGroupSet := candidateGroupSet
	if len(candidateGroupNos) > 0 {
		start := time.Now()
		activeNos, gerr := h.groupService.ExistMembersActive(candidateGroupNos, botUID)
		timings.memberActive += time.Since(start)
		if gerr != nil {
			h.Error("messages_search: ExistMembersActive lookup failed; fail-closed on bot group allowlist",
				zap.String("bot_uid", botUID),
				zap.Error(gerr))
			return nil, nil, nil, timings, gerr
		}
		activeGroupSet = make(map[string]struct{}, len(activeNos))
		for _, no := range activeNos {
			activeGroupSet[no] = struct{}{}
		}
	}
	allowGroup := make([]channelRef, 0, len(candidateGroupNos))
	groupNos := make([]string, 0, len(candidateGroupNos))
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

	// DM peers: friend edges only. No blacklist gate (blacklistNone), no
	// Space-member union, no bot-in-Space filter — see the doc comment.
	dmStart := time.Now()
	friends, ferr := h.userService.GetFriends(botUID)
	if ferr != nil {
		return nil, nil, nil, timings, ferr
	}
	allowDM := make([]channelRef, 0, len(friends))
	seenPeer := make(map[string]struct{}, len(friends))
	for _, f := range friends {
		if f == nil || f.UID == "" || f.UID == botUID {
			continue
		}
		if _, dup := seenPeer[f.UID]; dup {
			continue
		}
		seenPeer[f.UID] = struct{}{}
		allowDM = append(allowDM, channelRef{
			OSChannelID: fakeChannelIDFor(botUID, f.UID),
			WireID:      f.UID,
			ChannelType: channelTypePerson,
		})
	}
	timings.dmPeers += time.Since(dmStart)

	// Threads under the active groups — same batch helper + hard caps as the
	// real-user path; threads inherit their parent group's membership gate.
	threadStart := time.Now()
	allowThread := h.enumerateThreadsForGroups(groupNos)
	timings.threadEnum += time.Since(threadStart)

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

// channelsForMembers resolves the "包含成员" (member_uids) filter into the set
// of OS channelIds where the caller AND every named member can co-inhabit:
//
//   - Groups (channelType=2): intersect across members — a group survives only
//     when every named member is in it. The統一 rule from the design doc means
//     each surviving group also folds in its sub-threads from allowSet
//     (channelType=5, composite `{group_no}____{short_id}`); threads inherit
//     their parent group's membership, so no per-thread membership query is
//     needed. This flips the v1 behaviour (which explicitly dropped threads
//     from the member filter) so a caller filtering by member now sees thread
//     hits under groups the member is in.
//   - DM (channelType=1): only meaningful with exactly one named member and
//     only when the caller has a DM entry for that member. Multi-member DMs
//     do not exist on this product, so len(memberUIDs) > 1 drops the DM
//     branch entirely — matching the "AND across members" semantics.
//
// self-uid was already stripped by normalizeMemberUIDs so the loop can assume
// every entry is a real other-party candidate. Returned map is keyed by OS
// channelId; values mirror the allowSet entry so wire-id / channelType round-
// trip unchanged.
func (h *Handler) channelsForMembers(loginUID string, memberUIDs []string, spaceID string, allowSet map[string]channelRef) (map[string]channelRef, error) {
	out := make(map[string]channelRef)
	if len(memberUIDs) == 0 {
		return out, nil
	}
	// Step 1 — intersect group memberships across every named member.
	// groupIntersect holds the group_nos where every named member is in.
	// Seeded from the first member so subsequent members can prune only.
	var groupIntersect map[string]struct{}
	for i, member := range memberUIDs {
		memberGroups, err := h.groupService.GetGroupsWithMemberUID(member)
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
		if i == 0 {
			groupIntersect = memberSet
			continue
		}
		for gn := range groupIntersect {
			if _, ok := memberSet[gn]; !ok {
				delete(groupIntersect, gn)
			}
		}
		if len(groupIntersect) == 0 {
			break
		}
	}
	// Step 2 — walk allowSet once, keeping:
	//   - group entries whose group_no is in groupIntersect;
	//   - thread entries under those same groups (統一 rule: 群 → 群 + 其子区);
	//   - the DM entry for the sole named member (single-member case only).
	singleMember := ""
	if len(memberUIDs) == 1 {
		singleMember = memberUIDs[0]
	}
	for id, ref := range allowSet {
		switch ref.ChannelType {
		case channelTypeGroup:
			if _, ok := groupIntersect[ref.OSChannelID]; ok {
				out[id] = ref
			}
		case channelTypeThread:
			groupNo, _, err := thread.ParseChannelID(ref.OSChannelID)
			if err != nil {
				continue
			}
			if _, ok := groupIntersect[groupNo]; ok {
				out[id] = ref
			}
		case channelTypePerson:
			if singleMember != "" && ref.WireID == singleMember {
				out[id] = ref
			}
		}
	}
	// spaceID is unused: the caller has already narrowed allowSet to the
	// current Space in resolveGlobalScope (via buildAllowlist), so the ∩
	// against allowSet here inherits the Space filter transitively.
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
