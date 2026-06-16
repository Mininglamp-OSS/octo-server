package messages_search

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
)

// FileHit is the response shape per A doc §2.3.
//
// preview_url is always nil this release: business payload doesn't supply a
// preview link and indexer doesn't carry one in the document. A doc v4.2 §2.3
// permits the field to be null when previewing isn't available. channel_id is
// not on the spec table (the request channel is implicit) so we don't return
// it.
type FileHit struct {
	MessageID       string  `json:"message_id"`
	MessageSeq      int64   `json:"message_seq"`
	FileName        string  `json:"file_name"`
	FileSizeBytes   int64   `json:"file_size_bytes,omitempty"`
	FileExt         string  `json:"file_ext,omitempty"`
	DownloadURL     string  `json:"download_url,omitempty"`
	PreviewURL      *string `json:"preview_url"`
	SenderID        string  `json:"sender_id"`
	SenderName      string  `json:"sender_name,omitempty"`
	SenderAvatarURL string  `json:"sender_avatar_url,omitempty"`
	SentAt          string  `json:"sent_at"`
}

func init() {
	registerRoute(func(h *Handler, g *wkhttp.RouterGroup) {
		g.POST("/_search_files", h.searchFiles)
	})
}

// searchFiles is POST /v1/messages/_search_files.
func (h *Handler) searchFiles(c *wkhttp.Context) {
	var req SearchFilesReq
	if err := c.BindJSON(&req); err != nil {
		respondValidation(c, "body", "invalid JSON")
		return
	}
	req.Keyword = strings.TrimSpace(req.Keyword)
	loginUID := c.GetLoginUID()

	if !validateKeywordOptional(c, req.Keyword) {
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
	dsl := buildSearchFilesDSL(req, normID, spaceID)
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
			h.Warn("OS search files failed", zap.Error(err))
			responder(c)
			return
		}
		h.Error("messages_search: visibility filter failed", zap.Error(err))
		respondInternal(c)
		return
	}

	items := h.buildFileHits(ctx, filtered, req, loginUID)

	recordAudit(c, "search_files", req.ChannelType, req.ChannelID, req.Keyword, len(items))
	c.Response(envelope(items, hasMore, nextCursor))
}

func buildSearchFilesDSL(req SearchFilesReq, normChannelID, spaceID string) elastic.Query {
	b := elastic.NewBoolQuery()
	applyChannelAndRevoked(b, normChannelID)
	applySpaceIDScope(b, req.ChannelType, spaceID)
	b.Filter(elastic.NewTermQuery("payload.type", payloadTypeFile))
	addCommonFilters(b, req.Filters)
	if req.Keyword != "" {
		b.Must(elastic.NewMultiMatchQuery(req.Keyword,
			"payload.file.name^2",
			"payload.file.caption",
		))
	}
	return b
}

func (h *Handler) buildFileHits(ctx context.Context, hits []*elastic.SearchHit, req SearchFilesReq, loginUID string) []FileHit {
	if len(hits) == 0 {
		return []FileHit{}
	}
	items := make([]FileHit, 0, len(hits))
	senderIDs := make([]string, 0, len(hits))
	for _, hit := range hits {
		var doc Doc
		if err := json.Unmarshal(rawSource(hit.Source), &doc); err != nil {
			h.Warn("messages_search: bad file _source skipped", zap.Error(err))
			continue
		}
		items = append(items, h.singleFileHit(doc))
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

// singleFileHit projects a single Doc into a FileHit. Extracted so unit tests
// can assert ext fallback / preview_url null without going through ES.
func (h *Handler) singleFileHit(doc Doc) FileHit {
	fp := filePayloadOf(doc.Payload)
	fh := FileHit{
		MessageID:  strconv.FormatInt(doc.MessageID, 10),
		MessageSeq: int64(doc.MessageSeq),
		SenderID:   doc.From,
		SentAt:     msToRFC3339(doc.Timestamp),
		PreviewURL: nil,
	}
	if fp != nil {
		fh.FileName = fp.Name
		fh.FileSizeBytes = fp.SizeBytes
		fh.FileExt = resolveFileExt(fp)
		fh.DownloadURL = fp.URL
	}
	return fh
}

// resolveFileExt prefers the indexed payload.file.extension, which the indexer
// stores verbatim from the business payload (no case folding — see v1.8 OS
// mapping). Old documents predate the field; for those we fall back to
// splitting the filename, again without case folding so the API surface is
// consistent across the two paths. Invariant: never returns the leading dot.
func resolveFileExt(f *FilePayload) string {
	if f.Ext != "" {
		return f.Ext
	}
	if f.Name == "" {
		return ""
	}
	ext := filepath.Ext(f.Name)
	if ext == "" {
		return ""
	}
	return strings.TrimPrefix(ext, ".")
}

func filePayloadOf(p *Payload) *FilePayload {
	if p == nil {
		return nil
	}
	return p.File
}
