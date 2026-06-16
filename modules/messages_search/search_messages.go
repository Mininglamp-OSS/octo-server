package messages_search

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
)

// MessageHit is the response shape per A doc §2.1.
type MessageHit struct {
	MessageID       string        `json:"message_id"`
	MessageSeq      int64         `json:"message_seq"`
	MessageKind     string        `json:"message_kind"`
	Snippet         string        `json:"snippet,omitempty"`
	SenderID        string        `json:"sender_id"`
	SenderName      string        `json:"sender_name,omitempty"`
	SenderAvatarURL string        `json:"sender_avatar_url,omitempty"`
	SentAt          string        `json:"sent_at"`
	OuterPreview    *OuterPreview `json:"outer_preview,omitempty"`
	ChannelID       string        `json:"channel_id"`
}

func init() {
	registerRoute(func(h *Handler, g *wkhttp.RouterGroup) {
		g.POST("/_search", h.searchMessages)
	})
}

// searchMessages is POST /v1/messages/_search.
func (h *Handler) searchMessages(c *wkhttp.Context) {
	var req SearchMessagesReq
	if err := c.BindJSON(&req); err != nil {
		respondValidation(c, "body", "invalid JSON")
		return
	}
	req.Keyword = strings.TrimSpace(req.Keyword)
	loginUID := c.GetLoginUID()

	if !validateKeywordRequired(c, req.Keyword) {
		return
	}
	pageSize, ok := validateBase(c, h.cfg, req.ChannelType, req.ChannelID, req.Sort, req.Cursor, req.Filters, req.PageSize, true)
	if !ok {
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
	dsl := buildSearchMessagesDSL(req, normID, spaceID)
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

	osQuery := func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		svc := client.Search().
			Index(h.cfg.OSReadAlias).
			Routing(normID).
			Query(dsl).
			Highlight(buildSearchMessagesHighlight()).
			Size(size).
			TrackTotalHits(false)
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
		ctx, loginUID, req.ChannelID, pageSize, priorDepth, initialAfter, isRelevance, osQuery, projectDocRef(req.ChannelID),
	)
	if err != nil {
		if responder := classifyOSError(err); responder != nil {
			h.Warn("OS search failed", zap.Error(err))
			responder(c)
			return
		}
		// filterVisible failures fall through to here; fail-closed with INTERNAL.
		h.Error("messages_search: visibility filter failed", zap.Error(err))
		respondInternal(c)
		return
	}

	items := h.buildMessageHits(ctx, filtered, req, loginUID)

	recordAudit(c, "search_messages", req.ChannelType, req.ChannelID, req.Keyword, len(items))
	c.Response(envelope(items, hasMore, nextCursor))
}

// buildSearchMessagesDSL constructs the bool query for /_search.
func buildSearchMessagesDSL(req SearchMessagesReq, normChannelID, spaceID string) elastic.Query {
	b := elastic.NewBoolQuery()
	b.Must(elastic.NewMultiMatchQuery(req.Keyword,
		"payload.text.content^3",
		"payload.image.caption", "payload.image.name",
		"payload.file.caption", "payload.file.name",
		"payload.mergeForward.msgs.searchText",
	))
	applyChannelAndRevoked(b, normChannelID)
	applySpaceIDScope(b, req.ChannelType, spaceID)
	addCommonFilters(b, req.Filters)
	b.MustNot(elastic.NewTermQuery("payload.type", payloadTypeCmd))
	return b
}

// buildSearchMessagesHighlight returns the standard highlight config for
// /_search responses. Each match returns at most one 120-char fragment.
func buildSearchMessagesHighlight() *elastic.Highlight {
	return elastic.NewHighlight().
		PreTags("<mark>").PostTags("</mark>").
		FragmentSize(120).
		NumOfFragments(1).
		Field("payload.text.content").
		Field("payload.mergeForward.msgs.searchText").
		Field("payload.image.caption").
		Field("payload.file.name")
}

// buildMessageHits maps the OS hits into the API response shape and joins
// sender display name + avatar in a single batch.
func (h *Handler) buildMessageHits(ctx context.Context, hits []*elastic.SearchHit, req SearchMessagesReq, loginUID string) []MessageHit {
	if len(hits) == 0 {
		return []MessageHit{}
	}
	items := make([]MessageHit, 0, len(hits))
	senderIDs := make([]string, 0, len(hits))
	for _, hit := range hits {
		var doc Doc
		if err := json.Unmarshal(rawSource(hit.Source), &doc); err != nil {
			h.Warn("messages_search: bad _source skipped", zap.Error(err))
			continue
		}
		hl := map[string][]string(hit.Highlight)
		items = append(items, h.singleMessageHit(doc, req.ChannelID, hl))
		senderIDs = append(senderIDs, doc.From)
	}

	if len(items) == 0 {
		return items
	}
	join := h.senderJoin(ctx, uniqUIDs(senderIDs), req.ChannelType, req.ChannelID)
	for i := range items {
		items[i].SenderName = join.Names[items[i].SenderID]
		items[i].SenderAvatarURL = join.Avatars[items[i].SenderID]
	}
	return items
}

// singleMessageHit projects a single Doc into a MessageHit. Extracted so unit
// tests can drive the field mapping (kind / snippet / outer_preview) without
// standing up a full search loop, and so search_all can reuse it.
func (h *Handler) singleMessageHit(doc Doc, reqChannelID string, hl map[string][]string) MessageHit {
	return MessageHit{
		MessageID:    strconv.FormatInt(doc.MessageID, 10),
		MessageSeq:   int64(doc.MessageSeq),
		MessageKind:  classifyKind(doc.Payload),
		Snippet:      pickSnippet(hl),
		SenderID:     doc.From,
		SentAt:       msToRFC3339(doc.Timestamp),
		OuterPreview: buildOuterPreview(doc.Payload),
		ChannelID:    encodeChannelID(reqChannelID),
	}
}
