package messages_search

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
)

// SearchGlobalMessagesReq is the request body for
// POST /v1/messages/_search_global_messages (§7.1). Same shape as
// SearchMessagesReq minus channel_type/channel_id, plus the global-only
// filter block.
type SearchGlobalMessagesReq struct {
	Keyword  string              `json:"keyword,omitempty"`
	Filters  GlobalSearchFilters `json:"filters,omitempty"`
	Sort     string              `json:"sort,omitempty"`
	PageSize int                 `json:"page_size,omitempty"`
	Cursor   string              `json:"cursor,omitempty"`
}

func init() {
	registerRoute(func(h *Handler, g *wkhttp.RouterGroup) {
		g.POST("/_search_global_messages", h.searchGlobalMessages)
	})
}

// searchGlobalMessages is POST /v1/messages/_search_global_messages — the
// unified message + file feed spanning every room the caller can currently
// read (§5). Reuses buildSearchAllDSL's keyword / type-whitelist logic and
// swaps the single-channel term for a terms(channelId, allowlist ∩
// channel_ids) filter (§5 step 5).
//
// Single-channel fast path: when the resolved scope collapses to exactly one
// room the request is re-dispatched to the existing /_search_all handler so
// the checkChannelAccess gate and Routing(normID) optimisation still fire
// (§5 step 4).
func (h *Handler) searchGlobalMessages(c *wkhttp.Context) {
	var req SearchGlobalMessagesReq
	if err := c.BindJSON(&req); err != nil {
		respondValidation(c, "body", "invalid JSON")
		return
	}
	req.Keyword = strings.TrimSpace(req.Keyword)
	loginUID := c.GetLoginUID()

	if !validateKeywordOptional(c, req.Keyword) {
		return
	}
	if !validateSearchNotEmptyGlobal(c, req.Keyword, req.Filters) {
		return
	}
	pageSize, ok := validateGlobalBase(c, h.cfg, req.Sort, req.Cursor, req.Filters, req.PageSize, req.Keyword != "")
	if !ok {
		return
	}

	osChannelIDs, spaceID, singleFast, ok := h.resolveGlobalScope(c, loginUID, req.Filters.ChannelIDs, req.Filters.MemberUID)
	if !ok {
		return
	}
	if singleFast != nil {
		h.dispatchSingleAll(c, req, *singleFast, loginUID)
		return
	}
	if len(osChannelIDs) == 0 {
		recordAudit(c, "search_global_messages", 0, "", req.Keyword, 0)
		c.Response(envelope([]SearchAllHit{}, false, ""))
		return
	}

	client, err := ESClient(h.cfg)
	if err != nil {
		h.Error("ESClient init failed", zap.Error(err))
		respondUpstream(c)
		return
	}

	isRelevance := req.Sort == "relevance"
	initialAfter, ok := decodeCursorAsSearchAfter(h.cfg, req.Cursor, isRelevance)
	if !ok {
		respondValidation(c, "cursor", "malformed")
		return
	}
	priorDepth, ok := h.resolveCursorDepth(c, req.Cursor, pageSize)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.Timeout)
	defer cancel()

	dsl, analyzeErr := buildGlobalMessagesDSL(ctx, newOSIKSmartAnalyzer(client), h.cfg.StopwordStripEnabled, req, osChannelIDs, spaceID)
	if analyzeErr != nil {
		h.Warn("messages_search: _analyze fallback (degraded keyword clause)", zap.Error(analyzeErr))
	}

	osQuery := func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		// Deliberately no Routing(...) on the global surface (§11.1): the
		// query spans many channelIds and there is no single routing key.
		// Single-shard indexes make this a no-op today; a future re-sharding
		// rollout is where scatter-gather cost shows up and the single-channel
		// fast path continues to keep hot-path queries pinned to one shard.
		svc := client.Search().
			Index(h.cfg.OSReadAlias).
			Query(dsl).
			Size(size).
			TrackTotalHits(false)
		if req.Keyword != "" {
			svc = svc.Highlight(buildSearchAllHighlight())
		}
		svc = applySort(svc, req.Sort)
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

	filtered, hasMore, nextCursor, err := h.paginateWithFilterDepth(
		ctx, loginUID, "", pageSize, priorDepth, initialAfter, isRelevance, osQuery, projectDocRef("", loginUID),
	)
	if err != nil {
		if responder := classifyOSError(err); responder != nil {
			h.Warn("OS search_global_messages failed", zap.Error(err))
			responder(c)
			return
		}
		h.Error("messages_search: visibility filter failed", zap.Error(err))
		respondInternal(c)
		return
	}

	items := h.buildGlobalSearchAllHits(ctx, filtered, loginUID)

	recordAudit(c, "search_global_messages", 0, "", req.Keyword, len(items))
	c.Response(envelope(items, hasMore, nextCursor))
}

// buildGlobalMessagesDSL is the multi-channel variant of buildSearchAllDSL.
// Layers: terms(channelId, scope) + MustNot(revoked); type whitelist
// (keyword: [1,8,11,14]; browse: + [2,5]); addCommonFilters (sender/time);
// applySystemMessageHardFilter + applyExcludeVirtual; content_types (∩
// whitelist); channel_types (terms(channelType)); DM Space double-guard.
// Keyword should(text,file) + MSM=1 identical to the single-channel path.
func buildGlobalMessagesDSL(ctx context.Context, analyzer tokenAnalyzer, stopwordStripEnabled bool, req SearchGlobalMessagesReq, osChannelIDs []string, spaceID string) (elastic.Query, error) {
	b := elastic.NewBoolQuery()
	// Replace the single-channel `term channelId` with a terms filter over
	// the allowlist intersection. MustNot(revoked=true) mirrors
	// applyChannelAndRevoked so we keep the OS-side pre-filter that helps
	// filterVisible skip already-flagged rows.
	channelTerms := make([]any, 0, len(osChannelIDs))
	for _, id := range osChannelIDs {
		channelTerms = append(channelTerms, id)
	}
	b.Filter(elastic.NewTermsQuery("channelId", channelTerms...))
	b.MustNot(elastic.NewTermQuery("revoked", true))

	applyExcludeVirtual(b)
	applySystemMessageHardFilter(b)

	allowedTypes := []int{
		payloadTypeText,
		payloadTypeFile,
		payloadTypeMergeForward,
		payloadTypeRichText,
	}
	if req.Keyword == "" {
		allowedTypes = append(allowedTypes, payloadTypeImage, payloadTypeVideo)
	}
	// content_types: intersect with the hard whitelist. Empty means "no
	// narrowing" — the whitelist alone applies. Image/video pass through only
	// on the empty-keyword browse branch (they're absent from allowedTypes
	// otherwise, so the intersection drops them by construction).
	effectiveTypes := allowedTypes
	if len(req.Filters.ContentTypes) > 0 {
		allowedSet := make(map[int]struct{}, len(allowedTypes))
		for _, t := range allowedTypes {
			allowedSet[t] = struct{}{}
		}
		effectiveTypes = effectiveTypes[:0]
		for _, t := range req.Filters.ContentTypes {
			if _, ok := allowedSet[t]; ok {
				effectiveTypes = append(effectiveTypes, t)
			}
		}
	}
	if len(effectiveTypes) == 0 {
		// content_types requested no reachable payload types (e.g. only [2] on
		// the keyword path where media is not surfaced). Match zero docs
		// rather than silently drop the filter — the caller asked for a set
		// that produces no hits, so return an empty result. MatchNone is the
		// intent-explicit shape; a "type=-1" sentinel would rely on the
		// mapping never legitimising -1 in the future.
		b.Filter(elastic.NewMatchNoneQuery())
	} else {
		payloadTerms := make([]any, 0, len(effectiveTypes))
		for _, t := range effectiveTypes {
			payloadTerms = append(payloadTerms, t)
		}
		b.Filter(elastic.NewTermsQuery("payload.type", payloadTerms...))
	}

	addCommonFilters(b, req.Filters.baseFilters())

	if len(req.Filters.ChannelTypes) > 0 {
		typeTerms := make([]any, 0, len(req.Filters.ChannelTypes))
		for _, t := range req.Filters.ChannelTypes {
			typeTerms = append(typeTerms, t)
		}
		b.Filter(elastic.NewTermsQuery("channelType", typeTerms...))
	}

	applyGlobalDMSpaceScope(b, spaceID)

	var analyzeErr error
	if req.Keyword != "" {
		eff, useMSM := req.Keyword, true
		if stopwordStripEnabled {
			var err error
			eff, useMSM, err = AnalyzeKeyword(ctx, analyzer, req.Keyword)
			analyzeErr = err
		}
		textClause := buildKeywordClauseFromAnalyzed(eff, useMSM,
			"payload.text.content^3",
			"payload.richText.searchText^3",
			"payload.mergeForward.msgs.searchText",
		)
		fileClause := buildKeywordClauseFromAnalyzed(eff, useMSM,
			"payload.file.name^2",
			"payload.file.caption",
		)
		b.Should(textClause, fileClause)
		b.MinimumShouldMatch("1")
	}
	return b, analyzeErr
}

// buildGlobalSearchAllHits is buildSearchAllHits adapted to derive the wire
// channel_id / channel_type from the doc itself (DM peer reversal) rather
// than the request. senderJoin is called with a NEIGHBOURHOOD-less
// (channelType=0, channelID="") signature so it degrades to a plain global
// user lookup — per §5 step 7 we deliberately skip group-remark scoping
// because a single global response mixes hits from many rooms.
func (h *Handler) buildGlobalSearchAllHits(ctx context.Context, hits []*elastic.SearchHit, loginUID string) []SearchAllHit {
	if len(hits) == 0 {
		return []SearchAllHit{}
	}
	items := make([]SearchAllHit, 0, len(hits))
	senderIDs := make([]string, 0, len(hits))
	for _, hit := range hits {
		var doc Doc
		if err := json.Unmarshal(rawSource(hit.Source), &doc); err != nil {
			h.Warn("messages_search: bad global _source skipped", zap.Error(err))
			continue
		}
		wireID, wireType := wireChannelFromDoc(doc, loginUID)
		hl := map[string][]string(hit.Highlight)
		entry := h.singleSearchAllHit(doc, wireID, wireType, hl)
		items = append(items, entry)
		senderIDs = append(senderIDs, doc.From)
		if entry.Message != nil {
			for _, im := range entry.Message.InnerMessages {
				if im.SenderID != "" {
					senderIDs = append(senderIDs, im.SenderID)
				}
			}
		}
	}
	if len(items) == 0 {
		return items
	}
	join := h.senderJoin(ctx, uniqUIDs(senderIDs), 0, "")
	for i := range items {
		switch items[i].ResultType {
		case "message":
			if items[i].Message != nil {
				items[i].Message.SenderName = join.Names[items[i].Message.SenderID]
				items[i].Message.SenderAvatarURL = join.Avatars[items[i].Message.SenderID]
				for j := range items[i].Message.InnerMessages {
					if uid := items[i].Message.InnerMessages[j].SenderID; uid != "" {
						items[i].Message.InnerMessages[j].SenderName = join.Names[uid]
					}
				}
			}
		case "file":
			if items[i].File != nil {
				items[i].File.SenderName = join.Names[items[i].File.SenderID]
				items[i].File.SenderAvatarURL = join.Avatars[items[i].File.SenderID]
			}
		}
	}
	return items
}

// dispatchSingleAll forwards a global-messages request whose resolved scope
// collapsed to exactly one room back onto the single-channel /_search_all
// handler (§5 step 4). The single-channel path runs checkChannelAccess and
// gets Routing(normID) — both worth preserving on the hot path.
//
// This synthesises a SearchAllReq from the global request. member_uid /
// channel_ids / channel_types are already satisfied by the scope collapse, so
// they're dropped; content_types passes through as SearchFilters doesn't
// carry it and search_all's DSL isn't set up to consume it. This means a
// single-channel dispatch quietly ignores content_types — an acceptable v1
// tradeoff because a caller whose scope is one channel wanting to further
// narrow by content_types would today also see the same result set (the
// hard whitelist is what matters, and the fast path applies it).
func (h *Handler) dispatchSingleAll(c *wkhttp.Context, req SearchGlobalMessagesReq, target channelRef, loginUID string) {
	inner := SearchAllReq{
		ChannelType: target.ChannelType,
		ChannelID:   target.WireID,
		Keyword:     req.Keyword,
		Sort:        req.Sort,
		PageSize:    req.PageSize,
		Cursor:      req.Cursor,
		Filters: SearchFilters{
			SenderIDs:  req.Filters.SenderIDs,
			SentAtFrom: req.Filters.SentAtFrom,
			SentAtTo:   req.Filters.SentAtTo,
		},
	}
	// Preserve authorisation shape by explicitly running checkChannelAccess
	// here — the caller already resolved the room via the allowlist gate but
	// the single-channel path assumes this check has already happened.
	if !h.checkChannelAccess(c, inner.ChannelType, inner.ChannelID, loginUID) {
		return
	}
	// Rebuild the wire request context by rewriting BindJSON's already-
	// consumed body — the single-channel handlers rebind. Simpler: call the
	// core logic directly by re-running the search_all body with the
	// synthesised inner req.
	h.searchAllForGlobalFastPath(c, inner, loginUID)
}

// searchAllForGlobalFastPath is a body-less variant of h.searchAll that
// accepts a pre-parsed SearchAllReq. Kept private and marked as
// fast-path-only so the wire contract stays owned by h.searchAll.
func (h *Handler) searchAllForGlobalFastPath(c *wkhttp.Context, req SearchAllReq, loginUID string) {
	if !validateKeywordOptional(c, req.Keyword) {
		return
	}
	if !validateSearchNotEmpty(c, req.Keyword, req.Filters) {
		return
	}
	pageSize, ok := validateBase(c, h.cfg, req.ChannelType, req.ChannelID, req.Sort, req.Cursor, req.Filters, req.PageSize, req.Keyword != "")
	if !ok {
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
	isRelevance := req.Sort == "relevance"

	initialAfter, ok := decodeCursorAsSearchAfter(h.cfg, req.Cursor, isRelevance)
	if !ok {
		respondValidation(c, "cursor", "malformed")
		return
	}
	priorDepth, ok := h.resolveCursorDepth(c, req.Cursor, pageSize)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.Timeout)
	defer cancel()

	dsl, analyzeErr := buildSearchAllDSL(ctx, newOSIKSmartAnalyzer(client), h.cfg.StopwordStripEnabled, req, normID, spaceID)
	if analyzeErr != nil {
		h.Warn("messages_search: _analyze fallback (degraded keyword clause)", zap.Error(analyzeErr))
	}

	osQuery := func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		svc := client.Search().
			Index(h.cfg.OSReadAlias).
			Routing(normID).
			Query(dsl).
			Size(size).
			TrackTotalHits(false)
		if req.Keyword != "" {
			svc = svc.Highlight(buildSearchAllHighlight())
		}
		svc = applySort(svc, req.Sort)
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
	filtered, hasMore, nextCursor, err := h.paginateWithFilterDepth(
		ctx, loginUID, req.ChannelID, pageSize, priorDepth, initialAfter, isRelevance, osQuery, projectDocRef(req.ChannelID, loginUID),
	)
	if err != nil {
		if responder := classifyOSError(err); responder != nil {
			h.Warn("OS search_all fast-path failed", zap.Error(err))
			responder(c)
			return
		}
		h.Error("messages_search: fast-path visibility filter failed", zap.Error(err))
		respondInternal(c)
		return
	}

	items := h.buildSearchAllHits(ctx, filtered, req, loginUID)
	recordAudit(c, "search_global_messages_fast", req.ChannelType, req.ChannelID, req.Keyword, len(items))
	c.Response(envelope(items, hasMore, nextCursor))
}

// validateSearchNotEmptyGlobal is the empty-search guard for the global
// message endpoint. Mirrors validateSearchNotEmpty but consults the global
// filter shape (which additionally recognises channel_ids / channel_types /
// content_types / member_uid as "effective" filters).
func validateSearchNotEmptyGlobal(c *wkhttp.Context, keyword string, filters GlobalSearchFilters) bool {
	if keyword != "" {
		return true
	}
	if hasEffectiveFilters(filters.baseFilters()) {
		return true
	}
	if len(filters.ChannelIDs) > 0 {
		return true
	}
	if len(filters.ChannelTypes) > 0 {
		return true
	}
	if len(filters.ContentTypes) > 0 {
		return true
	}
	if strings.TrimSpace(filters.MemberUID) != "" {
		return true
	}
	respondValidation(c, "keyword", "keyword or at least one filter is required")
	return false
}
