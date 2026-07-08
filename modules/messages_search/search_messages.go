package messages_search

import (
	"context"
	"encoding/json"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
)

// MessageHit is the response shape per A doc §2.1.
//
// MessageKind / ThumbURL / Width / Height / DurationMs are populated only for
// image (payload.type=2) and video (payload.type=5) hits surfaced by
// /_search_all browse mode (or by /_search_around, which has no type
// whitelist). They mirror MediaHit's renderable fields so the client can
// render a media card directly from a MessageHit without a separate
// projection. All are omitempty so plain text / forward hits keep their wire
// shape unchanged.
type MessageHit struct {
	MessageID       string         `json:"message_id"`
	MessageSeq      int64          `json:"message_seq"`
	MessageKind     string         `json:"message_kind"`
	Snippet         string         `json:"snippet,omitempty"`
	SenderID        string         `json:"sender_id"`
	SenderName      string         `json:"sender_name,omitempty"`
	SenderAvatarURL string         `json:"sender_avatar_url,omitempty"`
	SentAt          string         `json:"sent_at"`
	OuterPreview    *OuterPreview  `json:"outer_preview,omitempty"`
	InnerMessages   []InnerMessage `json:"inner_messages,omitempty"`
	ChannelID       string         `json:"channel_id"`
	// ChannelType is echoed only for the global endpoints (_search_global_*),
	// which return hits from many rooms in a single response — omitempty on the
	// single-channel surfaces keeps the wire shape byte-identical for the
	// legacy _search / _search_all / _search_around callers that pass 0.
	ChannelType     uint8          `json:"channel_type,omitempty"`
	ThumbURL        string         `json:"thumb_url,omitempty"`
	VideoURL        string         `json:"video_url,omitempty"`
	Width           int            `json:"width,omitempty"`
	Height          int            `json:"height,omitempty"`
	DurationMs      int64          `json:"duration_ms,omitempty"`
	// RichText is the typed projection of a payload.type=14 rich-text message,
	// emitted only when the hit is rich-text AND the indexer preserved
	// `_source.payloadRaw`. Older docs (pre-payloadRaw indexer) leave it nil,
	// in which case the client renders the existing snippet/text fallback.
	// Non-richtext hits never carry this field.
	RichText *RichTextDetail `json:"rich_text,omitempty"`
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

	dsl, analyzeErr := buildSearchMessagesDSL(ctx, newOSIKSmartAnalyzer(client), h.cfg.StopwordStripEnabled, req, normID, spaceID)
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
			svc = svc.Highlight(buildSearchMessagesHighlight())
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

// buildSearchMessagesDSL constructs the bool query for /_search. Returns the
// query plus a non-nil error iff the keyword path's `_analyze` call failed and
// the keyword clause degraded to the fallback shape (raw keyword + MSM 75%);
// the query is still safe to use, callers should warn-log the error.
//
// stopwordStripEnabled = false routes through the ops kill switch: skip the
// `_analyze` call and emit the §4.4 degraded shape unconditionally.
func buildSearchMessagesDSL(ctx context.Context, analyzer tokenAnalyzer, stopwordStripEnabled bool, req SearchMessagesReq, normChannelID, spaceID string) (elastic.Query, error) {
	b := elastic.NewBoolQuery()
	var analyzeErr error
	if req.Keyword != "" {
		clause, err := buildKeywordClauseGated(ctx, analyzer, stopwordStripEnabled, req.Keyword,
			"payload.text.content^3",
			"payload.richText.searchText^3",
			"payload.mergeForward.msgs.searchText",
		)
		b.Must(clause)
		analyzeErr = err
	}
	applyChannelAndRevoked(b, normChannelID)
	applySpaceIDScope(b, req.ChannelType, spaceID)
	// Whitelist text (1), mergeForward (11), and richText (14). Media
	// (image/video/voice/gif) and file payloads are reachable through the
	// dedicated /_search_media and /_search_files surfaces — surfacing them
	// on the legacy /_search response was confusing the client UI which can
	// only render text/richText/mergeForward snippets. Sibling endpoints
	// already use the same `terms` shape: /_search_all → [1,8,11,14] in the
	// keyword path and [1,8,11,14,2,5] in browse mode (keyword=""),
	// /_search_media → [2,5], /_search_files → [8].
	b.Filter(elastic.NewTermsQuery("payload.type",
		payloadTypeText,
		payloadTypeMergeForward,
		payloadTypeRichText,
	))
	addCommonFilters(b, req.Filters)
	applySystemMessageHardFilter(b)
	applyExcludeVirtual(b)
	return b, analyzeErr
}

// buildSearchMessagesHighlight returns the standard highlight config for
// /_search responses. Each match returns at most one 120-char fragment.
// Fields mirror the keyword multi_match clause (text / richText / mergeForward)
// — image.caption + file.name are excluded because the endpoint's type
// whitelist [1,11,14] never reaches them.
func buildSearchMessagesHighlight() *elastic.Highlight {
	return elastic.NewHighlight().
		PreTags("<mark>").PostTags("</mark>").
		FragmentSize(120).
		NumOfFragments(1).
		Field("payload.text.content").
		Field("payload.richText.searchText").
		Field("payload.mergeForward.msgs.searchText")
}

// buildMessageHits maps the OS hits into the API response shape and joins
// sender display name + avatar in a single batch. The batch covers both the
// outer message sender and any inner_messages[].sender_id (forward children),
// so a single page incurs at most one MySQL round-trip for names regardless of
// how many forward cards it contains.
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
		mh := h.singleMessageHit(doc, req.ChannelID, req.ChannelType, hl)
		senderIDs = append(senderIDs, mh.SenderID)
		for _, im := range mh.InnerMessages {
			if im.SenderID != "" {
				senderIDs = append(senderIDs, im.SenderID)
			}
		}
		items = append(items, mh)
	}

	if len(items) == 0 {
		return items
	}
	join := h.senderJoin(ctx, uniqUIDs(senderIDs), req.ChannelType, req.ChannelID)
	for i := range items {
		items[i].SenderName = join.Names[items[i].SenderID]
		items[i].SenderAvatarURL = join.Avatars[items[i].SenderID]
		for j := range items[i].InnerMessages {
			if uid := items[i].InnerMessages[j].SenderID; uid != "" {
				items[i].InnerMessages[j].SenderName = join.Names[uid]
			}
		}
	}
	return items
}

// singleMessageHit projects a single Doc into a MessageHit. Extracted so unit
// tests can drive the field mapping (kind / snippet / outer_preview) without
// standing up a full search loop, and so search_all can reuse it.
//
// channelID / channelType are the values echoed on the wire. Single-channel
// callers pass req.ChannelID / req.ChannelType so the response mirrors the
// request. Global callers pass doc-derived values: for DM (channelType=1) the
// caller must reverse the OS fakeChannelID back to the peer uid via
// peerFromFakeChannelID; for group/thread the doc.ChannelID is echoed as-is.
// A zero channelType (legacy call sites) is omitted on the wire via omitempty
// so the single-channel response shape stays byte-identical.
func (h *Handler) singleMessageHit(doc Doc, channelID string, channelType uint8, hl map[string][]string) MessageHit {
	// Prefer the keyword highlight fragment; on the empty-keyword browse path
	// no highlight is requested, so fall back to the raw payload text so the
	// hit still carries readable content (A-doc §2.1).
	snippet := pickSnippet(hl)
	if snippet == "" {
		snippet = fallbackSnippet(doc.Payload)
	}
	mh := MessageHit{
		MessageID:     strconv.FormatInt(doc.MessageID, 10),
		MessageSeq:    int64(doc.MessageSeq),
		MessageKind:   classifyKind(doc.Payload),
		Snippet:       snippet,
		SenderID:      doc.From,
		SentAt:        msToRFC3339(doc.Timestamp),
		OuterPreview:  buildOuterPreview(doc.Payload),
		InnerMessages: buildInnerMessages(doc.Payload),
		ChannelID:     encodeChannelID(channelID),
		ChannelType:   channelType,
	}
	applyMediaProjection(&mh, doc.Payload)
	// Rich-text projection runs as an additive layer: message_kind stays "text"
	// (the swagger enum is locked at ["text","forward"]) and snippet remains
	// the highlight/text fallback. A non-nil rich_text signals the client to
	// render via the structured pipeline; nil falls back to snippet.
	if payloadType(doc.Payload) == payloadTypeRichText {
		mh.RichText = buildRichTextDetail(doc.PayloadRaw)
	}
	// card-message-protocol P1：type-17 命中的响应侧投影 = 遮蔽后的 plain
	// （单一执法点 DisplayTextFor：bot/webhook sender → 权威 plain；否则
	// [卡片]，round-3 P1-2）。message_kind 维持 "text"（swagger 枚举已锁），
	// snippet 即投影文本。
	if payloadType(doc.Payload) == payloadTypeCard {
		mh.Snippet = cardmsg.DisplayTextFor(h.cardTrust.Trusted(doc.From), doc.PayloadRaw)
	}
	return mh
}

// applyMediaProjection mirrors MediaHit's renderable fields onto a MessageHit
// for image (payload.type=2) and video (payload.type=5) hits. Image surfaces
// payload.image.url as ThumbURL (v1.8 mapping has no separate thumb URL field;
// the original URL is always renderable and the client can apply CDN sizing
// parameters); video surfaces payload.video.cover and the second→ms duration
// conversion. Both use omitempty fields, so plain text / forward / file hits
// keep their existing wire shape byte-identical.
func applyMediaProjection(mh *MessageHit, p *Payload) {
	if p == nil || p.MergeForward != nil {
		return
	}
	switch payloadType(p) {
	case payloadTypeImage:
		if img := imagePayloadOf(p); img != nil {
			mh.ThumbURL = img.URL
			mh.Width = img.Width
			mh.Height = img.Height
		}
	case payloadTypeVideo:
		if vid := videoPayloadOf(p); vid != nil {
			mh.ThumbURL = vid.Cover
			mh.VideoURL = vid.URL
			mh.Width = vid.Width
			mh.Height = vid.Height
			if vid.Second > 0 {
				mh.DurationMs = int64(vid.Second) * 1000
			}
		}
	}
}
