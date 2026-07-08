package messages_search

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
)

// SearchGlobalFilesReq is the request body for
// POST /v1/messages/_search_global_files (§7.2). Global variant of
// SearchFilesReq — no channel_type/channel_id, adds file_exts / file_size /
// channel_ids / channel_types / member_uid filters.
type SearchGlobalFilesReq struct {
	Keyword  string            `json:"keyword,omitempty"`
	Filters  GlobalFileFilters `json:"filters,omitempty"`
	Sort     string            `json:"sort,omitempty"`
	PageSize int               `json:"page_size,omitempty"`
	Cursor   string            `json:"cursor,omitempty"`
}

func init() {
	registerRoute(func(h *Handler, g *wkhttp.RouterGroup) {
		g.POST("/_search_global_files", h.searchGlobalFiles)
	})
}

// searchGlobalFiles is POST /v1/messages/_search_global_files — the file-only
// global feed. Filters are additive on top of the hard payload.type=8 lock.
// Single-channel fast path re-dispatches to /_search_files.
//
// keyword is intentionally optional: the WeChat-work "聊天文件" tab opens on a
// "recent files across all rooms" browse view, and that path is a legitimate
// no-keyword request (the shape lock plus the allowlist / space-scope filters
// still bound the result set). We do NOT layer validateSearchNotEmpty on this
// endpoint for that reason.
func (h *Handler) searchGlobalFiles(c *wkhttp.Context) {
	var req SearchGlobalFilesReq
	if err := c.BindJSON(&req); err != nil {
		respondValidation(c, "body", "invalid JSON")
		return
	}
	req.Keyword = strings.TrimSpace(req.Keyword)
	loginUID := c.GetLoginUID()

	if !validateKeywordOptional(c, req.Keyword) {
		return
	}
	pageSize, ok := validateGlobalFileBase(c, h.cfg, req.Sort, req.Cursor, req.Filters, req.PageSize, req.Keyword != "")
	if !ok {
		return
	}

	osChannelIDs, spaceID, singleFast, ok := h.resolveGlobalScope(c, loginUID, req.Filters.ChannelIDs, req.Filters.MemberUID)
	if !ok {
		return
	}
	if singleFast != nil {
		h.dispatchSingleFiles(c, req, *singleFast, loginUID, pageSize)
		return
	}
	if len(osChannelIDs) == 0 {
		recordAudit(c, "search_global_files", 0, "", req.Keyword, 0)
		c.Response(envelope([]FileHit{}, false, ""))
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

	dsl, analyzeErr := buildGlobalFilesDSL(ctx, newOSIKSmartAnalyzer(client), h.cfg.StopwordStripEnabled, req, osChannelIDs, spaceID)
	if analyzeErr != nil {
		h.Warn("messages_search: _analyze fallback (degraded keyword clause)", zap.Error(analyzeErr))
	}

	osQuery := func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		svc := client.Search().
			Index(h.cfg.OSReadAlias).
			Query(dsl).
			Size(size).
			TrackTotalHits(false).
			FetchSourceContext(fileContentSourceExcludes())
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
			h.Warn("OS search_global_files failed", zap.Error(err))
			responder(c)
			return
		}
		h.Error("messages_search: visibility filter failed", zap.Error(err))
		respondInternal(c)
		return
	}

	items := h.buildGlobalFileHits(ctx, filtered, loginUID)
	recordAudit(c, "search_global_files", 0, "", req.Keyword, len(items))
	c.Response(envelope(items, hasMore, nextCursor))
}

// buildGlobalFilesDSL is the multi-channel variant of buildSearchFilesDSL.
// Layers: terms(channelId, scope); MustNot(revoked); Filter(payload.type=8);
// keyword → Must(fileClause); file_exts → terms(payload.file.extension) with
// lowercase normalisation to match indexer keyword storage; file_size range;
// channel_types filter; addCommonFilters; DM Space double-guard.
func buildGlobalFilesDSL(ctx context.Context, analyzer tokenAnalyzer, stopwordStripEnabled bool, req SearchGlobalFilesReq, osChannelIDs []string, spaceID string) (elastic.Query, error) {
	b := elastic.NewBoolQuery()
	channelTerms := make([]any, 0, len(osChannelIDs))
	for _, id := range osChannelIDs {
		channelTerms = append(channelTerms, id)
	}
	b.Filter(elastic.NewTermsQuery("channelId", channelTerms...))
	b.MustNot(elastic.NewTermQuery("revoked", true))

	b.Filter(elastic.NewTermQuery("payload.type", payloadTypeFile))

	if len(req.Filters.FileExts) > 0 {
		terms := make([]any, 0, len(req.Filters.FileExts))
		seen := make(map[string]struct{}, len(req.Filters.FileExts))
		for _, ext := range req.Filters.FileExts {
			norm := strings.ToLower(strings.TrimSpace(ext))
			if norm == "" {
				continue
			}
			if _, dup := seen[norm]; dup {
				continue
			}
			seen[norm] = struct{}{}
			terms = append(terms, norm)
		}
		if len(terms) > 0 {
			b.Filter(elastic.NewTermsQuery("payload.file.extension", terms...))
		}
	}

	if req.Filters.FileSizeMin > 0 || req.Filters.FileSizeMax > 0 {
		rng := elastic.NewRangeQuery("payload.file.size")
		if req.Filters.FileSizeMin > 0 {
			rng = rng.Gte(req.Filters.FileSizeMin)
		}
		if req.Filters.FileSizeMax > 0 {
			rng = rng.Lte(req.Filters.FileSizeMax)
		}
		b.Filter(rng)
	}

	if len(req.Filters.ChannelTypes) > 0 {
		typeTerms := make([]any, 0, len(req.Filters.ChannelTypes))
		for _, t := range req.Filters.ChannelTypes {
			typeTerms = append(typeTerms, t)
		}
		b.Filter(elastic.NewTermsQuery("channelType", typeTerms...))
	}

	addCommonFilters(b, req.Filters.baseFilters())

	applyGlobalDMSpaceScope(b, spaceID)

	var analyzeErr error
	if req.Keyword != "" {
		clause, err := buildKeywordClauseGated(ctx, analyzer, stopwordStripEnabled, req.Keyword,
			"payload.file.name^2",
			"payload.file.caption",
			"payload.file.content",
		)
		b.Must(clause)
		analyzeErr = err
	}
	return b, analyzeErr
}

// buildGlobalFileHits is buildFileHits adapted to derive channel context from
// each doc. Same senderJoin degradation to global user names — group remarks
// aren't scoped in a multi-room feed.
func (h *Handler) buildGlobalFileHits(ctx context.Context, hits []*elastic.SearchHit, loginUID string) []FileHit {
	if len(hits) == 0 {
		return []FileHit{}
	}
	items := make([]FileHit, 0, len(hits))
	senderIDs := make([]string, 0, len(hits))
	for _, hit := range hits {
		var doc Doc
		if err := json.Unmarshal(rawSource(hit.Source), &doc); err != nil {
			h.Warn("messages_search: bad global file _source skipped", zap.Error(err))
			continue
		}
		wireID, wireType := wireChannelFromDoc(doc, loginUID)
		items = append(items, h.singleFileHit(doc, wireID, wireType))
		senderIDs = append(senderIDs, doc.From)
	}
	if len(items) == 0 {
		return items
	}
	join := h.senderJoin(ctx, uniqUIDs(senderIDs), 0, "")
	for i := range items {
		items[i].SenderName = join.Names[items[i].SenderID]
		items[i].SenderAvatarURL = join.Avatars[items[i].SenderID]
	}
	return items
}

// dispatchSingleFiles re-dispatches a global-files request whose scope
// collapsed to one room onto the single-channel /_search_files handler,
// preserving checkChannelAccess + Routing(normID). Global-only filters that
// have no representation on SearchFilesReq (file_exts, file_size,
// channel_ids, channel_types, member_uid) are handled by re-running the
// global DSL directly on the fast-path body: file_exts / file_size still
// need to apply. So instead of forwarding to h.searchFiles (which would drop
// those), we re-scope this handler's own query to the single channel.
func (h *Handler) dispatchSingleFiles(c *wkhttp.Context, req SearchGlobalFilesReq, target channelRef, loginUID string, pageSize int) {
	// Access gate mirrors the single-channel path.
	if !h.checkChannelAccess(c, target.ChannelType, target.WireID, loginUID) {
		return
	}
	// Fast path: run the same global DSL with a scope of one channel. This
	// keeps file_exts / file_size / channel_types honoured; the only thing
	// we lose vs a full /_search_files call is the OS Routing(normID) hint,
	// which under the current single-shard index is a no-op anyway. See
	// §11.1: routing is a preservation for the day the index is re-sharded,
	// and the /_search_files path continues to use it.
	//
	// pageSize is passed in from the caller — the outer handler already ran
	// validateGlobalFileBase to normalise it, so we don't repeat that work
	// here.
	client, err := ESClient(h.cfg)
	if err != nil {
		h.Error("ESClient init failed", zap.Error(err))
		respondUpstream(c)
		return
	}
	normID := normalizedChannelID(target.ChannelType, target.WireID, loginUID)
	spaceID, ok := h.resolveP2PSpaceScope(c, target.ChannelType, loginUID)
	if !ok {
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
	dsl, analyzeErr := buildGlobalFilesDSL(ctx, newOSIKSmartAnalyzer(client), h.cfg.StopwordStripEnabled, req, []string{normID}, spaceID)
	if analyzeErr != nil {
		h.Warn("messages_search: _analyze fallback (degraded keyword clause)", zap.Error(analyzeErr))
	}
	osQuery := func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		svc := client.Search().
			Index(h.cfg.OSReadAlias).
			Routing(normID).
			Query(dsl).
			Size(size).
			TrackTotalHits(false).
			FetchSourceContext(fileContentSourceExcludes())
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
		ctx, loginUID, normID, pageSize, priorDepth, initialAfter, isRelevance, osQuery, projectDocRef(normID, loginUID),
	)
	if err != nil {
		if responder := classifyOSError(err); responder != nil {
			h.Warn("OS search_files fast-path failed", zap.Error(err))
			responder(c)
			return
		}
		h.Error("messages_search: fast-path visibility filter failed", zap.Error(err))
		respondInternal(c)
		return
	}
	items := h.buildGlobalFileHits(ctx, filtered, loginUID)
	recordAudit(c, "search_global_files_fast", target.ChannelType, target.WireID, req.Keyword, len(items))
	c.Response(envelope(items, hasMore, nextCursor))
}
