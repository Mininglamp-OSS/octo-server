package messages_search

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/olivere/elastic"
)

// visibilityProbe collapses the four MySQL signals filterVisible needs into
// a small surface of plain map / scalar returns. The real implementation
// delegates to message.IService (which exposes those signals via slices of
// types unexported from modules/message — irreducible from a test fake);
// tests substitute a hand-rolled stub that drives the predicates directly.
type visibilityProbe interface {
	// RevokedSet returns the set of message IDs (decimal strings) that are
	// revoked according to message_extra.revoke=1.
	RevokedSet(messageIDs []string) (map[string]struct{}, error)
	// GloballyDeletedSet returns the set of admin / mutual-deleted ids
	// (message_extra.is_deleted=1).
	GloballyDeletedSet(messageIDs []string) (map[string]struct{}, error)
	// UserDeletedSet returns the set of message ids that the given uid has
	// individually deleted (message_user_extra.message_is_deleted=1).
	UserDeletedSet(uid string, messageIDs []string) (map[string]struct{}, error)
	// ChannelOffset returns the user's channel-offset seq for channelID, or
	// 0 when there is no offset record. A non-zero value means messages
	// with messageSeq <= offset have been cleared from the user's view.
	//
	// Retained for backwards compatibility with existing test doubles;
	// production callers should prefer ChannelOffsets for multi-channel
	// batching. filterVisible internally routes through ChannelOffsets
	// with a single-element slice so the two paths behave identically.
	ChannelOffset(uid, channelID string) (uint32, error)
	// ChannelOffsets returns the user's channel-offset seq for each of
	// channelIDs. Missing entries default to 0 (no clear-history record).
	// Fail-closed contract: any DB error returns (nil, err) — filterVisible
	// then propagates to INTERNAL_ERROR without releasing any hits.
	//
	// This is the batch surface the global endpoints rely on so a page whose
	// hits span N rooms only pays 1 MySQL round-trip for offsets, not N.
	ChannelOffsets(uid string, channelIDs []string) (map[string]uint32, error)
}

// messageVisibilityProbe is the production implementation of
// visibilityProbe. It calls into message.IService and translates the
// returned package-private structs into the plain sets / scalars the
// filter wants. Predicate translations mirror modules/search/api.go and
// modules/message/api_channel_files.go::filterMessages so search-side
// visibility stays in lock-step with the read paths.
type messageVisibilityProbe struct {
	svc message.IService
}

func newMessageVisibilityProbe(svc message.IService) visibilityProbe {
	return &messageVisibilityProbe{svc: svc}
}

func (p *messageVisibilityProbe) RevokedSet(ids []string) (map[string]struct{}, error) {
	if len(ids) == 0 {
		return map[string]struct{}{}, nil
	}
	items, err := p.svc.GetRevokedMessages(ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(items))
	for _, e := range items {
		// GetRevokedMessages already restricts to revoke=1 at the DB
		// layer; presence of a row implies revoked.
		out[e.MessageIDStr] = struct{}{}
	}
	return out, nil
}

func (p *messageVisibilityProbe) GloballyDeletedSet(ids []string) (map[string]struct{}, error) {
	if len(ids) == 0 {
		return map[string]struct{}{}, nil
	}
	items, err := p.svc.GetDeletedMessages(ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(items))
	for _, e := range items {
		// IsMutualDeleted mirrors message_extra.is_deleted (admin /
		// mutual-delete). Match modules/search/api.go's predicate.
		if e.IsMutualDeleted == 1 {
			out[e.MessageIDStr] = struct{}{}
		}
	}
	return out, nil
}

func (p *messageVisibilityProbe) UserDeletedSet(uid string, ids []string) (map[string]struct{}, error) {
	if len(ids) == 0 {
		return map[string]struct{}{}, nil
	}
	items, err := p.svc.GetDeletedMessagesWithUID(uid, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(items))
	for _, e := range items {
		if e.MessageIsDeleted == 1 {
			out[e.MessageIDStr] = struct{}{}
		}
	}
	return out, nil
}

func (p *messageVisibilityProbe) ChannelOffset(uid, channelID string) (uint32, error) {
	items, err := p.svc.GetChannelOffsetWithUID(uid, []string{channelID})
	if err != nil {
		return 0, err
	}
	for _, o := range items {
		if o.ChannelID == channelID {
			return o.MessageSeq, nil
		}
	}
	return 0, nil
}

// ChannelOffsets is the batch variant used by the global endpoints so a page
// spanning N rooms costs one MySQL round-trip instead of N. The underlying
// message.IService.GetChannelOffsetWithUID already accepts a slice; this is a
// thin re-shape into map[channelID]seq. Missing rows are omitted from the
// map (callers treat "no key" as offset 0, same as ChannelOffset).
func (p *messageVisibilityProbe) ChannelOffsets(uid string, channelIDs []string) (map[string]uint32, error) {
	if len(channelIDs) == 0 {
		return map[string]uint32{}, nil
	}
	items, err := p.svc.GetChannelOffsetWithUID(uid, channelIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]uint32, len(items))
	for _, o := range items {
		out[o.ChannelID] = o.MessageSeq
	}
	return out, nil
}

// msgRef projects a single OS hit into the inputs filterVisible needs to
// decide whether the message is currently visible to the caller. ChannelID
// is reserved for future cross-channel callers (e.g. when the indexer fans
// out into multiple OS docs); for /v1/messages/_search* it is the request's
// channel_id (single channel per request).
//
// Visibles carries the per-message allowlist the authoritative read path
// consults (modules/message/api.go::MsgSyncResp.from). Empty Visibles means
// "no gate" — same fail-open semantics the read path has when the field is
// absent. While the indexer has not yet been updated to write this field
// the gate stays fail-open for legacy docs; see
// docs/messages-search/CONSTRAINTS-2026-06-12.md.
type msgRef struct {
	MessageID  string // canonical decimal-string id (matches message_extra.message_id)
	MessageSeq uint32 // matches channel_offset.message_seq
	ChannelID  string
	Visibles   []string // sender-set allowlist; non-empty => caller must be in it
}

// filterVisible is the search-side analogue of message.filterMessages. It
// rejects hits the caller must NOT see based on the same five signals the
// /messages and /channel_files read paths consult:
//
//  1. message_extra.revoke=1                     (sender-revoked)
//  2. message_extra.is_deleted=1                 (admin / mutual-deleted)
//  3. message_user_extra.message_is_deleted=1    (current user deleted)
//  4. channel_offset.message_seq >= hit.seq      (current user cleared chat)
//  5. payload.visibles whitelist                 (loginUID not in allowlist)
//
// 1–4 are MySQL-resident (probe roundtrips); 5 is read directly off the OS
// hit (msgRef.Visibles). Empty Visibles means "no gate" — same fail-open
// contract the read path has when the field is absent on a message. Until
// the indexer writes this field explicitly, legacy docs land here as if no
// gate were set; see docs/messages-search/CONSTRAINTS-2026-06-12.md (D24).
//
// We deliberately do NOT gate on `payload.expire` even though the read
// path has an expire branch: per CONSTRAINTS-2026-06-12 D25 the field has
// no per-message write path in octo-server, so any gate on it would defend
// a non-existent risk and only build false confidence.
//
// Fail-closed contract: any DB error returns (nil, err) and the caller MUST
// surface INTERNAL_ERROR rather than fall through to the OS hits. This is
// load-bearing — the four DB signals are the reason the read path keeps a
// MySQL round-trip on the hot path; OS only sees a stale `revoked` field
// (and nothing else), so post-filter is the only place these are applied
// for search.
//
// Empty refs is a no-op (no DB calls). Duplicate MessageIDs collapse so the
// IN list stays bounded by unique ids on the page.
func (h *Handler) filterVisible(ctx context.Context, loginUID, channelID string, refs []msgRef) (map[string]struct{}, error) {
	_ = ctx // current probe methods don't take a ctx; keep parameter for future plumbing
	if len(refs) == 0 {
		return map[string]struct{}{}, nil
	}

	uniqueIDs := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		if r.MessageID == "" {
			continue
		}
		if _, ok := seen[r.MessageID]; ok {
			continue
		}
		seen[r.MessageID] = struct{}{}
		uniqueIDs = append(uniqueIDs, r.MessageID)
	}

	revokedSet, err := h.visibility.RevokedSet(uniqueIDs)
	if err != nil {
		return nil, fmt.Errorf("filterVisible: RevokedSet: %w", err)
	}
	deletedSet, err := h.visibility.GloballyDeletedSet(uniqueIDs)
	if err != nil {
		return nil, fmt.Errorf("filterVisible: GloballyDeletedSet: %w", err)
	}
	userDeletedSet, err := h.visibility.UserDeletedSet(loginUID, uniqueIDs)
	if err != nil {
		return nil, fmt.Errorf("filterVisible: UserDeletedSet: %w", err)
	}
	// Multi-channel channel-offset lookup (§8.2 generalisation). Each hit is
	// gated by its own room's clear-history watermark instead of a single
	// per-request channel. Two invariants preserve legacy behaviour and let
	// fail-closed still bite:
	//
	//   1. refs whose ChannelID is empty fall back to the request-level
	//      `channelID` parameter. All single-channel callers (search_messages,
	//      search_all, search_files, search_around) invoke projectDocRef
	//      today, which stamps the request channel_id on every ref — so the
	//      set of channels queried collapses to {channelID}, keeping the
	//      round-trip shape byte-identical to the legacy path.
	//   2. When ChannelOffsets fails wholesale we surface the error (same
	//      fail-closed semantics as the other three signals). A channel with
	//      no offset row is not an error — the map simply omits the key and
	//      we treat it as offset 0 (mirrors ChannelOffset's contract).
	channelSet := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		if r.ChannelID != "" {
			channelSet[r.ChannelID] = struct{}{}
		} else if channelID != "" {
			channelSet[channelID] = struct{}{}
		}
	}
	channelList := make([]string, 0, len(channelSet))
	for id := range channelSet {
		channelList = append(channelList, id)
	}
	offsets, err := h.visibility.ChannelOffsets(loginUID, channelList)
	if err != nil {
		return nil, fmt.Errorf("filterVisible: ChannelOffsets: %w", err)
	}

	keep := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		if r.MessageID == "" {
			continue
		}
		if _, bad := revokedSet[r.MessageID]; bad {
			continue
		}
		if _, bad := deletedSet[r.MessageID]; bad {
			continue
		}
		if _, bad := userDeletedSet[r.MessageID]; bad {
			continue
		}
		effectiveChannel := r.ChannelID
		if effectiveChannel == "" {
			effectiveChannel = channelID
		}
		offsetSeq := offsets[effectiveChannel]
		if offsetSeq > 0 && r.MessageSeq <= offsetSeq {
			continue
		}
		if len(r.Visibles) > 0 {
			inList := false
			for _, uid := range r.Visibles {
				if uid == loginUID {
					inList = true
					break
				}
			}
			if !inList {
				continue
			}
		}
		keep[r.MessageID] = struct{}{}
	}
	return keep, nil
}

// Oversample-and-resume tuning. OS may return up to oversampleMultiplier ×
// pageSize hits per round so a high filter-rejection rate still has a shot
// at filling the page in a single round. After loopBudget rounds we stop
// pulling and let the client pull again with the cursor we hand back —
// bounded work per request, but we never silently truncate to <pageSize
// when more visible hits exist.
const (
	oversampleMultiplier = 3
	loopBudget           = 3
)

// osQueryFn runs one OpenSearch round. searchAfter carries the previous
// round's last hit's `Sort` array (or the decoded request cursor on the
// first round); size is the per-round fetch ceiling.
type osQueryFn func(searchAfter []any, size int) ([]*elastic.SearchHit, error)

// projectFn maps a raw OS hit into the visibility check inputs. Returning
// ok=false drops the hit (e.g. unparseable _source) without aborting the
// page.
type projectFn func(hit *elastic.SearchHit) (msgRef, bool)

// paginateWithFilter runs the oversample-and-resume loop around an OS
// search and applies filterVisible to each round.
//
// Cursor protocol is unchanged: the returned next_cursor encodes the OS
// hit at which the next page should resume — either collected[pageSize-1]
// when we filled the page, or the last hit examined on the final round
// when the budget was exhausted.
//
// When the cursor cannot be encoded (missing Sort / messageId — e.g. an
// unparseable _source on the anchor) we collapse to (hasMore=false,
// nextCursor="") rather than emit a half-valid cursor. The page may be
// short but the next request is well-defined: the caller stops paging.
func (h *Handler) paginateWithFilter(
	ctx context.Context,
	loginUID, channelID string,
	pageSize int,
	initialSearchAfter []any,
	isRelevanceSort bool,
	osQuery osQueryFn,
	project projectFn,
) ([]*elastic.SearchHit, bool, string, error) {
	return h.paginateWithFilterDepth(ctx, loginUID, channelID, pageSize, 0, initialSearchAfter, isRelevanceSort, osQuery, project)
}

// paginateWithFilterDepth is paginateWithFilter plus the cumulative result
// depth already served before this page (decoded from the request cursor). The
// returned next_cursor encodes priorDepth + len(returned page) so the
// max-pagination-depth cap (enforced in the handler via decodeCursorWithDepth)
// is measured on the cumulative count, not page_size — shrinking/growing
// page_size between requests cannot walk past the cap.
func (h *Handler) paginateWithFilterDepth(
	ctx context.Context,
	loginUID, channelID string,
	pageSize int,
	priorDepth int64,
	initialSearchAfter []any,
	isRelevanceSort bool,
	osQuery osQueryFn,
	project projectFn,
) ([]*elastic.SearchHit, bool, string, error) {
	collected := make([]*elastic.SearchHit, 0, pageSize)
	searchAfter := initialSearchAfter
	fetchSize := pageSize * oversampleMultiplier

	var (
		anchorHit *elastic.SearchHit // anchor for next-page cursor
		moreInOS  bool               // OS still has results past anchor
	)

	for round := 0; round < loopBudget; round++ {
		hits, err := osQuery(searchAfter, fetchSize)
		if err != nil {
			return nil, false, "", err
		}
		if len(hits) == 0 {
			anchorHit = nil
			moreInOS = false
			break
		}

		// OS may have results behind this round if it returned a full page.
		moreInOS = len(hits) >= fetchSize

		refs := make([]msgRef, len(hits))
		filterInput := make([]msgRef, 0, len(hits))
		for i, hit := range hits {
			r, ok := project(hit)
			if !ok {
				continue
			}
			refs[i] = r
			filterInput = append(filterInput, r)
		}
		keep, err := h.filterVisible(ctx, loginUID, channelID, filterInput)
		if err != nil {
			return nil, false, "", err
		}

		filledThisRound := false
		for i, hit := range hits {
			r := refs[i]
			if r.MessageID == "" {
				continue
			}
			if _, ok := keep[r.MessageID]; !ok {
				continue
			}
			collected = append(collected, hit)
			if len(collected) == pageSize {
				anchorHit = hit
				if i < len(hits)-1 {
					moreInOS = true // remaining hits inside this round
				}
				filledThisRound = true
				break
			}
		}
		if filledThisRound {
			break
		}

		// Round consumed without filling the page. Anchor on OS last hit so
		// the next round / page resumes there, then either continue (if OS
		// still has results) or stop.
		anchorHit = hits[len(hits)-1]
		if !moreInOS {
			break
		}
		// Rebuild search_after from the typed _source rather than reusing
		// anchorHit.Sort. JSON-decoded sort values are float64, which rounds
		// snowflake messageId tiebreakers above 2^53 and corrupts the
		// resume boundary at timestamp ties. Mirrors the typed-source
		// policy in computeCursorPagination so internal round-refill and
		// external cursor share one full-precision id source.
		nextSA, ok := buildSearchAfterFromHit(anchorHit, isRelevanceSort)
		if !ok {
			break
		}
		searchAfter = nextSA
	}

	hasMore := moreInOS && anchorHit != nil
	nextCursor := ""
	if hasMore {
		ts, _, score := extractSortValues(anchorHit.Sort, isRelevanceSort)
		msgID, subSeq := lastHitMessageIDAndSubSeq(anchorHit)
		if ts == 0 || msgID == 0 {
			// Spec v4.2 §1.4 requires a non-empty cursor when has_more=true;
			// rather than break the contract, drop has_more so paging stops
			// cleanly. Caller can retry the request to get fresher state.
			hasMore = false
		} else {
			// Carry the cumulative depth so the next request's cap check sees
			// the running total, independent of the page_size used to get here.
			nextDepth := priorDepth + int64(len(collected))
			nextCursor = encodeCursorWithDepth(h.cfg, ts, msgID, score, subSeq, nextDepth)
		}
	}
	return collected, hasMore, nextCursor, nil
}

// resolveCursorDepth decodes the cumulative result depth carried in the
// request cursor and enforces the max-pagination-depth cap. The bool return is
// "continue": false means a DEPTH_EXCEEDED response was already written and the
// handler must abort. An empty cursor (first page) has depth 0.
//
// The cap is checked against the cumulative depth INCLUDING the page about to
// be served (priorDepth + pageSize), NOT page_size alone, so a caller sitting
// just under the cap cannot pick a larger page_size to read past it: a request
// whose window would cross maxPaginationDepth is rejected before any OS
// round-trip.
//
// Integrity note (codex P2): the depth is HMAC-signed into the cursor, so a
// well-behaved deployment (OCTO_SEARCH_CURSOR_HMAC set) cannot have its depth
// forged back to 0. When the secret is UNSET the cursor falls back to the
// published default key (api.go New() logs a loud WARN at startup), which makes
// BOTH the depth AND the search_after position forgeable — but forging depth
// buys nothing an attacker doesn't already get by forging search_after
// directly: every page still passes the per-request channel-access +
// filterVisible gates and the bounded oversample loop (loopBudget rounds), so
// the depth cap is a resource-protection limit, not a security boundary. The
// correct hardening is to require the per-deployment secret in production
// (already surfaced as a startup WARN); we deliberately do not reject
// depth-carrying cursors under the default key here because that would break
// the existing time_*/relevance cursors that share the same signing path.
func (h *Handler) resolveCursorDepth(c *wkhttp.Context, cursor string, pageSize int) (int64, bool) {
	var depth int64
	if cursor != "" {
		_, _, _, d, _, err := decodeCursorWithDepth(h.cfg, cursor)
		if err != nil {
			// Mirrors decodeCursorAsSearchAfter's contract: malformed cursors
			// map to VALIDATION_ERROR(field=cursor) upstream. Callers already
			// validate the cursor via validateBase, so this is defense-in-depth.
			respondValidation(c, "cursor", "malformed")
			return 0, false
		}
		depth = d
	}
	// Reject when this request's window would carry the cumulative depth past
	// the cap. Using pageSize (not the post-filter count) keeps the check
	// page_size-driven and pre-OS, so it cannot be bypassed by choosing a
	// larger page near the boundary.
	if depth+int64(pageSize) > maxPaginationDepth {
		respondDepthExceeded(c)
		return 0, false
	}
	return depth, true
}

// decodeCursorAsSearchAfter rebuilds the OpenSearch SearchAfter tuple from
// a cursor string. Returns (nil, true) when cursor is empty (first page).
// On structural / signature failure, returns (nil, false) so the handler
// can map to VALIDATION_ERROR(field=cursor) — same surface as the legacy
// per-handler decode path that this consolidates.
//
// The trailing subSeq element (Part B virtual-doc tiebreaker) is appended
// for both sort modes — pre-Part-B cursors decode to subSeq=0 and the
// emitted tuple is search_after-exclusive, so legacy cursors keep working.
// Whether same-(ts,msgID) virtual children resume on the next page is
// sort-direction dependent (time_asc surfaces them; time_desc/relevance DESC
// skips subSeq>=1 children) — acceptable because legacy cursors only live in
// the deploy-transition window and the next fresh query self-heals. See the
// Cursor.SubSeq doc in cursor.go for the full direction analysis.
func decodeCursorAsSearchAfter(cfg SearchConfig, cursor string, isRelevanceSort bool) ([]any, bool) {
	if cursor == "" {
		return nil, true
	}
	ts, msgID, score, subSeq, err := decodeCursor(cfg, cursor)
	if err != nil {
		return nil, false
	}
	if isRelevanceSort {
		if score == nil {
			return nil, false // stale cursor format
		}
		return []any{ts, *score, msgID, subSeq}, true
	}
	return []any{ts, msgID, subSeq}, true
}

// projectDocRef returns a projectFn that pulls (messageId, messageSeq,
// channelId) from a hit's typed _source. Every ref carries the doc's OWN
// channelId — filterVisible then buckets refs per channel and consults each
// room's clear-history offset independently (§8.2 multi-channel
// generalisation). Single-channel callers pass their request `channel_id`
// as reqChannelID so this stays a defensive fallback if the doc's channelId
// is ever missing (indexer contract says it's always populated). Hits with
// unparseable _source fail-soft: project returns ok=false and the loop drops
// them — same behaviour as the legacy buildXxxHits path.
//
// DM key alignment: the `channel_offset` MySQL table is keyed by peer uid for
// DMs, while OS stores the fakeChannelID ("uidA@uidB"). If we handed the raw
// fake id to ChannelOffsets the lookup would always miss and the caller's
// clear-history watermark would silently stop applying to search hits. To
// keep the search-side channel_offset gate in lockstep with the read path we
// reverse the fake id back to the peer uid here for DMs — the resulting
// msgRef.ChannelID is exactly the key `channel_offset` uses. Non-DMs pass
// through unchanged.
func projectDocRef(reqChannelID, loginUID string) projectFn {
	return func(hit *elastic.SearchHit) (msgRef, bool) {
		if hit == nil {
			return msgRef{}, false
		}
		var d Doc
		if err := json.Unmarshal(rawSource(hit.Source), &d); err != nil {
			return msgRef{}, false
		}
		if d.MessageID == 0 {
			return msgRef{}, false
		}
		// Visibility key is the parent messageId for virtual sub-documents
		// (Part B rich-text derivatives, see richtext-virtual-docs-octo-server-dev.md
		// §1 / §4). Revoke / delete / channel-offset / visibles state lives on
		// the parent's row in MySQL; the child has no row of its own. coalesce
		// covers both cases: plain docs (Virtual=false, ParentMessageID=nil)
		// keep the existing behaviour; virtual sub-docs route to the parent's
		// id. Per indexer contract MessageID itself is also the parent's value
		// on virtual sub-docs, so the coalesce is a safety belt — if indexer
		// ever lets these diverge (e.g. swaps to a composite child id) the
		// reader still routes visibility correctly.
		//
		// Note: msgRef.MessageID is the visibility-query key, NOT the wire
		// `message_id` on the response (response projection is buildXxxHits,
		// which transparently passes through doc.messageId per Part B's
		// "zero front-end change" contract).
		visKey := d.MessageID
		if d.Virtual && d.ParentMessageID != nil && *d.ParentMessageID != 0 {
			visKey = *d.ParentMessageID
		}
		channelID := d.ChannelID
		if channelID == "" {
			channelID = reqChannelID
		}
		// DM channel_offset lookup key alignment: reverse fakeChannelID → peer
		// uid so ChannelOffsets(uid, [peer_uid]) actually hits the row.
		if uint8(d.ChannelType) == channelTypePerson && loginUID != "" && channelID != "" {
			channelID = peerFromFakeChannelID(channelID, loginUID)
		}
		return msgRef{
			MessageID:  strconv.FormatInt(visKey, 10),
			MessageSeq: uint32(d.MessageSeq),
			ChannelID:  channelID,
			Visibles:   d.Visibles,
		}, true
	}
}
