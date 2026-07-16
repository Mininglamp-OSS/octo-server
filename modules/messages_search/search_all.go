package messages_search

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
)

// SearchAllHit is the response shape per A doc §2.4. Either Message or File
// is populated based on ResultType; SortedAt is a flat copy of the inner
// sent_at to make pagination deterministic across mixed result types.
type SearchAllHit struct {
	ResultType string      `json:"result_type"`
	SortedAt   string      `json:"sorted_at"`
	Message    *MessageHit `json:"message,omitempty"`
	File       *FileHit    `json:"file,omitempty"`
}

func init() {
	registerRoute(func(h *Handler, g *wkhttp.RouterGroup) {
		g.POST("/_search_all", h.searchAll)
	})
}

// searchAll is POST /v1/messages/_search_all.
func (h *Handler) searchAll(c *wkhttp.Context) {
	var req SearchAllReq
	if err := c.BindJSON(&req); err != nil {
		respondValidation(c, "body", "invalid JSON")
		return
	}
	req.Keyword = strings.TrimSpace(req.Keyword)
	p := h.principal(c)
	loginUID := p.SubjectUID()

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
	if !h.canReadChannel(c, p, req.ChannelType, req.ChannelID) {
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
			TrackTotalHits(false).
			FetchSourceContext(fileContentSourceExcludes())
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
			h.Warn("OS search_all failed", zap.Error(err))
			responder(c)
			return
		}
		h.Error("messages_search: visibility filter failed", zap.Error(err))
		respondInternal(c)
		return
	}

	items := h.buildSearchAllHits(ctx, filtered, req, loginUID)

	recordAudit(c, "search_all", req.ChannelType, req.ChannelID, req.Keyword, len(items))
	c.Response(envelope(items, hasMore, nextCursor))
}

func buildSearchAllDSL(ctx context.Context, analyzer tokenAnalyzer, stopwordStripEnabled bool, req SearchAllReq, normChannelID, spaceID string) (elastic.Query, error) {
	b := elastic.NewBoolQuery()
	applyChannelAndRevoked(b, normChannelID)
	applySpaceIDScope(b, req.ChannelType, spaceID)
	applyExcludeVirtual(b)
	// Image (2) / video (5) are surfaced ONLY on the empty-keyword browse
	// path. With a keyword, the should[textClause, fileClause] + MSM=1 has no
	// field to hit on media payloads — they would either drop on MSM or
	// surface as zero-relevance noise — so the keyword path keeps the
	// text-bearing whitelist [1, 8, 11, 14]. Browse mode (keyword="") layers
	// media in so the unified feed shows recent photos/videos alongside text.
	allowedTypes := []any{
		payloadTypeText,
		payloadTypeFile,
		payloadTypeMergeForward,
		payloadTypeRichText,
	}
	if req.Keyword == "" {
		allowedTypes = append(allowedTypes, payloadTypeImage, payloadTypeVideo)
	}
	b.Filter(elastic.NewTermsQuery("payload.type", allowedTypes...))
	addCommonFilters(b, req.Filters)
	var analyzeErr error
	if req.Keyword != "" {
		eff, useMSM := req.Keyword, true
		if stopwordStripEnabled {
			// Single `_analyze` roundtrip feeds both Should branches — same
			// keyword, same analyzer, so a second call would be redundant and
			// double the IK-cluster RT cost of every _search_all hit.
			var err error
			eff, useMSM, err = AnalyzeKeyword(ctx, analyzer, req.Keyword)
			analyzeErr = err
		}
		// When stopwordStripEnabled=false: skip _analyze entirely and emit the
		// §4.4 degraded shape (raw keyword + cross_fields + MSM 75%) on both
		// branches — the eff/useMSM defaults above match that contract.
		textClause := buildKeywordClauseFromAnalyzed(eff, useMSM,
			"payload.text.content^3",
			"payload.richText.searchText^3",
			"payload.mergeForward.msgs.searchText",
		)
		fileClause := buildKeywordClauseFromAnalyzed(eff, useMSM,
			"payload.file.name^2",
			"payload.file.caption",
			"payload.file.content",
		)
		b.Should(textClause, fileClause)
		b.MinimumShouldMatch("1")
	}
	return b, analyzeErr
}

func buildSearchAllHighlight() *elastic.Highlight {
	return elastic.NewHighlight().
		PreTags("<mark>").PostTags("</mark>").
		FragmentSize(120).
		NumOfFragments(1).
		Field("payload.text.content").
		Field("payload.richText.searchText").
		Field("payload.mergeForward.msgs.searchText").
		Field("payload.file.name")
}

func (h *Handler) buildSearchAllHits(ctx context.Context, hits []*elastic.SearchHit, req SearchAllReq, loginUID string) []SearchAllHit {
	if len(hits) == 0 {
		return []SearchAllHit{}
	}
	items := make([]SearchAllHit, 0, len(hits))
	senderIDs := make([]string, 0, len(hits))
	for _, hit := range hits {
		var doc Doc
		if err := json.Unmarshal(rawSource(hit.Source), &doc); err != nil {
			h.Warn("messages_search: bad search_all _source skipped", zap.Error(err))
			continue
		}
		hl := map[string][]string(hit.Highlight)
		entry := h.singleSearchAllHit(doc, req.ChannelID, req.ChannelType, hl)
		items = append(items, entry)
		senderIDs = append(senderIDs, doc.From)
		// Forward children: their senders need the same batched name lookup
		// as the outer hit so a 9-msg forward card doesn't trigger 9 extra
		// DB round-trips. payloadType=11 only — file hits never carry
		// inner_messages.
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
	join := h.senderJoin(ctx, uniqUIDs(senderIDs), req.ChannelType, req.ChannelID)
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

// singleSearchAllHit projects a single Doc into the result_type-tagged shape
// _search_all returns. Extracted so unit tests can drive the dispatcher
// without hitting OS.
//
// channelID / channelType are echoed onto the inner MessageHit / FileHit.
// Single-channel callers (/_search_all) pass req.ChannelID / req.ChannelType;
// global callers (/_search_global_messages) pass doc-derived values (for DM
// the peer uid via peerFromFakeChannelID; group/thread the doc.ChannelID
// as-is) so the frontend can jump into the source room from a mixed-room
// unified feed.
func (h *Handler) singleSearchAllHit(doc Doc, channelID string, channelType uint8, hl map[string][]string) SearchAllHit {
	entry := SearchAllHit{SortedAt: msToRFC3339(doc.Timestamp)}
	if payloadType(doc.Payload) == payloadTypeFile {
		fh := h.singleFileHit(doc, channelID, channelType)
		entry.ResultType = "file"
		entry.File = &fh
		entry.SortedAt = fh.SentAt
	} else {
		mh := h.singleMessageHit(doc, channelID, channelType, hl)
		entry.ResultType = "message"
		entry.Message = &mh
		entry.SortedAt = mh.SentAt
	}
	return entry
}
