package messages_search

import (
	"context"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
)

// AroundResult is the response envelope for POST /v1/messages/_search_around.
// The window is returned in chronological (time_asc) order: `before` (older
// than the anchor, oldest-first), then `anchor`, then `after` (newer than the
// anchor, oldest-first). has_more_before / has_more_after tell the client
// whether paging further in each direction would yield more visible messages.
type AroundResult struct {
	Before        []MessageHit `json:"before"`
	Anchor        MessageHit   `json:"anchor"`
	After         []MessageHit `json:"after"`
	HasMoreBefore bool         `json:"has_more_before"`
	HasMoreAfter  bool         `json:"has_more_after"`
}

func init() {
	registerRoute(func(h *Handler, g *wkhttp.RouterGroup) {
		g.POST("/_search_around", h.searchAround)
	})
}

// searchAround is POST /v1/messages/_search_around.
//
// Security contract (YUJ-4662 §4 step 3 / V8-b, STOP #3): the anchor is
// pre-validated through the SAME gates as a normal read before any context is
// fetched —
//
//  1. checkChannelAccess: caller must be able to read the channel at all.
//  2. resolveP2PSpaceScope: p2p anchor is scoped to the caller's Space, so a
//     cross-Space anchor cannot be located (the spaceId term filter on the
//     anchor lookup returns nothing → NOT_FOUND).
//  3. filterVisible on the anchor doc itself: a revoked / deleted /
//     not-in-visibles / cleared-by-offset anchor is NOT_FOUND.
//
// Only after the anchor passes all three do we fetch the surrounding window.
// We never pull context around an anchor the caller could not see — that would
// be both a disclosure (the neighbours) and a probing oracle (existence of the
// anchor). All window hits go through the same filterVisible post-filter.
func (h *Handler) searchAround(c *wkhttp.Context) {
	var req SearchAroundReq
	if err := c.BindJSON(&req); err != nil {
		respondValidation(c, "body", "invalid JSON")
		return
	}
	loginUID := c.GetLoginUID()

	// Reuse the shared field validation (channel form, page_size, filters).
	// No sort/cursor on this endpoint; relevance is not applicable.
	pageSize, ok := validateBase(c, h.cfg, req.ChannelType, req.ChannelID, "", "", req.Filters, req.PageSize, false)
	if !ok {
		return
	}
	anchorID, perr := strconv.ParseInt(req.AnchorMessageID, 10, 64)
	if perr != nil || anchorID <= 0 {
		respondValidation(c, "anchor_message_id", "must be a positive integer id")
		return
	}
	if !h.checkChannelAccess(c, req.ChannelType, req.ChannelID, loginUID) {
		return
	}
	spaceID, ok := h.resolveP2PSpaceScope(c, req.ChannelType, loginUID)
	if !ok {
		return
	}

	client, err := ESClient(h.cfg)
	if err != nil {
		h.Error("ESClient init failed", zap.Error(err))
		respondUpstream(c)
		return
	}

	normID := normalizedChannelID(req.ChannelType, req.ChannelID, loginUID)

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.Timeout)
	defer cancel()

	// --- 1. Locate + pre-validate the anchor (single doc). ---
	anchorHit, err := h.fetchAnchor(ctx, client, req, normID, spaceID, loginUID, anchorID)
	if err != nil {
		if responder := classifyOSError(err); responder != nil {
			h.Warn("OS around anchor fetch failed", zap.Error(err))
			responder(c)
			return
		}
		h.Error("messages_search: around anchor visibility check failed", zap.Error(err))
		respondInternal(c)
		return
	}
	if anchorHit == nil {
		// Anchor not present, cross-Space, or filtered out by visibility — all
		// collapse to NOT_FOUND so the failure path is not an existence oracle.
		respondNotFound(c, "channel")
		return
	}

	anchorSort, ok := buildSearchAfterFromHit(anchorHit, false)
	if !ok {
		// A located anchor whose _source/sort can't yield a resume key is
		// unusable for windowing; treat as not found rather than emit a
		// half-window.
		respondNotFound(c, "channel")
		return
	}

	// --- 2. Fetch the two directions around the anchor. ---
	// `after` (newer): time_asc, search_after = anchor sort tuple.
	afterHits, hasMoreAfter, err := h.aroundDirection(ctx, client, req, normID, spaceID, loginUID, pageSize, anchorSort, false)
	if err != nil {
		h.respondAroundErr(c, err, "after")
		return
	}
	// `before` (older): time_desc (reversed), search_after = anchor sort tuple
	// in the reversed sort space. Re-reversed to chronological order below.
	beforeDesc, hasMoreBefore, err := h.aroundDirection(ctx, client, req, normID, spaceID, loginUID, pageSize, anchorSort, true)
	if err != nil {
		h.respondAroundErr(c, err, "before")
		return
	}
	beforeHits := reverseHits(beforeDesc)

	// --- 3. Project + sender-join the whole window in one batch. ---
	window := make([]*elastic.SearchHit, 0, len(beforeHits)+1+len(afterHits))
	window = append(window, beforeHits...)
	window = append(window, anchorHit)
	window = append(window, afterHits...)
	hits := h.buildMessageHits(ctx, window, SearchMessagesReq{
		ChannelType: req.ChannelType,
		ChannelID:   req.ChannelID,
	}, loginUID)

	result := splitAroundWindow(hits, len(beforeHits), len(afterHits), hasMoreBefore, hasMoreAfter)

	recordAudit(c, "search_around", req.ChannelType, req.ChannelID, "", len(hits))
	c.Response(result)
}

// fetchAnchor looks up the single anchor doc by (channelId, messageId), applies
// the same Space scope as the window, and runs filterVisible on it. Returns
// (nil, nil) — the "not visible / not found" signal — when the doc is missing,
// out of Space, or filtered out; (hit, nil) when the anchor is visible.
//
// The lookup applies the SAME structural negations the window query uses
// (revoked + cmd exclusion) so the anchor is held to the exact visibility
// contract of the stream it anchors: a command message (payload.type=99) is
// never a valid anchor, mirroring buildAroundDSL's MustNot(cmd). filterVisible
// then layers the MySQL-resident gates (revoke/delete/offset/visibles) on top.
func (h *Handler) fetchAnchor(ctx context.Context, client *elastic.Client, req SearchAroundReq, normID, spaceID, loginUID string, anchorID int64) (*elastic.SearchHit, error) {
	b := buildAnchorDSL(req, normID, spaceID, anchorID)

	svc := client.Search().
		Index(h.cfg.OSReadAlias).
		Routing(normID).
		Query(b).
		Size(1).
		TrackTotalHits(false)
	svc = applySort(svc, "time_asc")
	res, err := svc.Do(ctx)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Hits == nil || len(res.Hits.Hits) == 0 {
		return nil, nil
	}
	hit := res.Hits.Hits[0]

	ref, ok := projectDocRef(req.ChannelID)(hit)
	if !ok {
		return nil, nil
	}
	keep, err := h.filterVisible(ctx, loginUID, req.ChannelID, []msgRef{ref})
	if err != nil {
		return nil, err
	}
	if _, visible := keep[ref.MessageID]; !visible {
		return nil, nil
	}
	return hit, nil
}

// aroundDirection fetches one side of the window. reversed=false walks newer
// (time_asc), reversed=true walks older (time_desc). Both start at the anchor's
// sort tuple via search_after and reuse paginateWithFilter's oversample-补页
// loop so post-filter losses still fill the page. The returned hits are in the
// query's own sort order (caller re-reverses the `before` slice).
func (h *Handler) aroundDirection(ctx context.Context, client *elastic.Client, req SearchAroundReq, normID, spaceID, loginUID string, pageSize int, anchorSort []any, reversed bool) ([]*elastic.SearchHit, bool, error) {
	dsl := buildAroundDSL(req, normID, spaceID)
	sortMode := "time_asc"
	if reversed {
		sortMode = "time_desc"
	}
	osQuery := func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		svc := client.Search().
			Index(h.cfg.OSReadAlias).
			Routing(normID).
			Query(dsl).
			Size(size).
			TrackTotalHits(false)
		svc = applySort(svc, sortMode)
		if len(searchAfter) > 0 {
			svc = svc.SearchAfter(searchAfter...)
		}
		res, qerr := svc.Do(ctx)
		if qerr != nil {
			return nil, qerr
		}
		if res == nil || res.Hits == nil {
			return nil, nil
		}
		return res.Hits.Hits, nil
	}
	hits, hasMore, _, err := h.paginateWithFilterDepth(
		ctx, loginUID, req.ChannelID, pageSize, 0, anchorSort, false, osQuery, projectDocRef(req.ChannelID),
	)
	return hits, hasMore, err
}

// buildAnchorDSL builds the single-doc anchor lookup. It applies the same
// structural visibility negations as the window (revoked + cmd exclusion) plus
// the Space scope, so the anchor is held to the exact contract of the stream it
// anchors — a command (payload.type=99) or revoked doc can never be a valid
// anchor, mirroring buildAroundDSL. MySQL-resident gates are layered on top by
// filterVisible in fetchAnchor.
func buildAnchorDSL(req SearchAroundReq, normChannelID, spaceID string, anchorID int64) *elastic.BoolQuery {
	b := elastic.NewBoolQuery()
	b.Filter(elastic.NewTermQuery("channelId", normChannelID))
	b.Filter(elastic.NewTermQuery("messageId", anchorID))
	applySpaceIDScope(b, req.ChannelType, spaceID)
	b.MustNot(elastic.NewTermQuery("revoked", true))
	b.MustNot(elastic.NewTermQuery("payload.type", payloadTypeCmd))
	return b
}

// buildAroundDSL is the window query (no keyword, no payload.type filter): the
// full visible message stream of the channel, scoped to Space for p2p, with the
// standard revoked/cmd negations. The anchor itself is excluded so it is not
// duplicated into either wing.
func buildAroundDSL(req SearchAroundReq, normChannelID, spaceID string) elastic.Query {
	b := elastic.NewBoolQuery()
	applyChannelAndRevoked(b, normChannelID)
	applySpaceIDScope(b, req.ChannelType, spaceID)
	addCommonFilters(b, req.Filters)
	b.MustNot(elastic.NewTermQuery("payload.type", payloadTypeCmd))
	return b
}

// splitAroundWindow re-slices the joined window (built in before+anchor+after
// order) back into the AroundResult shape.
func splitAroundWindow(hits []MessageHit, beforeN, afterN int, hasMoreBefore, hasMoreAfter bool) AroundResult {
	res := AroundResult{
		Before:        []MessageHit{},
		After:         []MessageHit{},
		HasMoreBefore: hasMoreBefore,
		HasMoreAfter:  hasMoreAfter,
	}
	// buildMessageHits may drop unparseable hits, so clamp the indices to the
	// surviving slice rather than trusting beforeN/afterN blindly.
	if beforeN > len(hits) {
		beforeN = len(hits)
	}
	res.Before = append(res.Before, hits[:beforeN]...)
	rest := hits[beforeN:]
	if len(rest) == 0 {
		return res
	}
	res.Anchor = rest[0]
	res.After = append(res.After, rest[1:]...)
	return res
}

// reverseHits reverses a hit slice in place-safe fashion (returns a new order)
// so a time_desc `before` wing can be presented oldest-first.
func reverseHits(in []*elastic.SearchHit) []*elastic.SearchHit {
	out := make([]*elastic.SearchHit, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}

// respondAroundErr maps a window-fetch error to the right response.
func (h *Handler) respondAroundErr(c *wkhttp.Context, err error, side string) {
	if responder := classifyOSError(err); responder != nil {
		h.Warn("OS around window fetch failed", zap.String("side", side), zap.Error(err))
		responder(c)
		return
	}
	h.Error("messages_search: around window visibility filter failed", zap.String("side", side), zap.Error(err))
	respondInternal(c)
}
