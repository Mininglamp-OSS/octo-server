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
// permits the field to be null when previewing isn't available.
//
// channel_id / channel_type are omitempty because single-channel callers
// (_search_files) historically didn't return them (the request channel is
// implicit). Global callers (_search_global_files, _search_global_messages
// via SearchAllHit.file) MUST populate both so the frontend has a route to
// jump into the source room; for DM the channel_id is the peer uid, not the
// OS fakeChannelID (see peerFromFakeChannelID in channel.go).
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
	ChannelID       string  `json:"channel_id,omitempty"`
	ChannelType     uint8   `json:"channel_type,omitempty"`

	// NameHighlight is the file name with keyword matches wrapped in <mark>,
	// taken from the OS highlight response (payload.file.name). Empty on the
	// browse path (keyword="") or when the match came only from body/caption.
	// The client renders this in place of FileName when present, otherwise
	// falls back to client-side highlighting of FileName.
	NameHighlight string `json:"name_highlight,omitempty"`
	// ContentSnippet is a single <mark>-wrapped fragment of the Tika-extracted
	// file body (payload.file.content) around the matched term. This is the
	// only signal explaining WHY a body-only hit matched — the raw content is
	// excluded from _source (30–100 KB), but the highlighter reconstructs the
	// fragment from the inverted index. Empty when the body didn't match.
	ContentSnippet string `json:"content_snippet,omitempty"`
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
	p := h.principal(c)
	loginUID := p.SubjectUID()

	if !validateKeywordOptional(c, req.Keyword) {
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

	dsl, analyzeErr := buildSearchFilesDSL(ctx, newOSIKSmartAnalyzer(client), h.cfg.StopwordStripEnabled, req, normID, spaceID)
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
			svc = svc.Highlight(buildSearchFilesHighlight())
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

func buildSearchFilesDSL(ctx context.Context, analyzer tokenAnalyzer, stopwordStripEnabled bool, req SearchFilesReq, normChannelID, spaceID string) (elastic.Query, error) {
	b := elastic.NewBoolQuery()
	applyChannelAndRevoked(b, normChannelID)
	applySpaceIDScope(b, req.ChannelType, spaceID)
	b.Filter(elastic.NewTermQuery("payload.type", payloadTypeFile))
	addCommonFilters(b, req.Filters)
	var analyzeErr error
	if req.Keyword != "" {
		clause, err := buildKeywordClauseGated(ctx, analyzer, stopwordStripEnabled, req.Keyword,
			"payload.file.name^2",
			"payload.file.caption",
			// content: full extracted file body from Tika (indexer v1.12).
			// Default weight ^1 lets name/caption matches still outrank a
			// pure-body hit. See docs/messages-search/v1.12-file-content-mapping-integration.md.
			"payload.file.content",
		)
		b.Must(clause)
		analyzeErr = err
	}
	return b, analyzeErr
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
		items = append(items, h.singleFileHit(doc, req.ChannelID, req.ChannelType, map[string][]string(hit.Highlight)))
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
//
// channelID / channelType are echoed on the wire (both omitempty). Global
// callers must pass doc-derived values — for DM (channelType=1) the peer uid
// via peerFromFakeChannelID; for group/thread the doc.ChannelID as-is.
// Single-channel callers pass req.ChannelID / req.ChannelType so the response
// mirrors the request.
func (h *Handler) singleFileHit(doc Doc, channelID string, channelType uint8, hl map[string][]string) FileHit {
	fp := filePayloadOf(doc.Payload)
	fh := FileHit{
		MessageID:   strconv.FormatInt(doc.MessageID, 10),
		MessageSeq:  int64(doc.MessageSeq),
		SenderID:    doc.From,
		SentAt:      msToRFC3339(doc.Timestamp),
		PreviewURL:  nil,
		ChannelID:   channelID,
		ChannelType: channelType,
	}
	if fp != nil {
		fh.FileName = fp.Name
		fh.FileSizeBytes = fp.SizeBytes
		fh.FileExt = resolveFileExt(fp)
		fh.DownloadURL = fp.URL
	}
	fh.NameHighlight = pickFileNameHighlight(hl)
	fh.ContentSnippet = pickFileContentSnippet(hl)
	return fh
}

// buildSearchFilesHighlight returns the highlight config for the file-search
// endpoints. Two fields mirror the keyword multi_match clause: payload.file.name
// (mapped onto NameHighlight, no fragmenting — the whole name is short) and
// payload.file.content (a single 120-char fragment around the matched body term,
// mapped onto ContentSnippet). caption is intentionally omitted: it is rarely
// populated and adds a third precedence branch with no UI slot. Fragments are
// drawn from the inverted index, so fileContentSourceExcludes (which drops the
// raw body from _source) does not affect this — see dsl.go:fileContentSourceExcludes.
func buildSearchFilesHighlight() *elastic.Highlight {
	return elastic.NewHighlight().
		PreTags("<mark>").PostTags("</mark>").
		// Encoder("html") escapes uploader-controlled source text (file.name is
		// fully user-controlled; file.content is Tika-extracted uploader body)
		// before the highlighter inserts <mark>/</mark>, so a malicious name
		// like `<img src=x onerror=alert(1)>.pdf` comes back as
		// `&lt;img src=x onerror=alert(1)&gt;.pdf` with only <mark> as live
		// markup. Clients decode the entities into text nodes; there is no
		// path for the injected HTML to execute. Without this, OpenSearch's
		// default encoder is a pass-through — a stored-XSS vector via any
		// uploader name/body token that overlaps a searcher's keyword.
		Encoder("html").
		Fields(
			// NumberOfFragments(0) returns the whole field with matches marked
			// in-place — correct for a short file name where we want the full
			// string, not a fragment window.
			elastic.NewHighlighterField("payload.file.name").NumOfFragments(0),
			elastic.NewHighlighterField("payload.file.content").FragmentSize(120).NumOfFragments(1),
		)
}

// pickFileNameHighlight returns the marked file name fragment, or "" when the
// name field didn't match (body-only hit or browse path).
func pickFileNameHighlight(hl map[string][]string) string {
	if hl == nil {
		return ""
	}
	if frags, ok := hl["payload.file.name"]; ok && len(frags) > 0 && frags[0] != "" {
		return frags[0]
	}
	return ""
}

// pickFileContentSnippet returns the marked body fragment, or "" when the body
// didn't match. This is the "why did this file match" signal for hits whose
// keyword only appears in the extracted content, not the name.
func pickFileContentSnippet(hl map[string][]string) string {
	if hl == nil {
		return ""
	}
	if frags, ok := hl["payload.file.content"]; ok && len(frags) > 0 && frags[0] != "" {
		return frags[0]
	}
	return ""
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
