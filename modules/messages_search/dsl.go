package messages_search

import (
	"encoding/json"

	"github.com/olivere/elastic"
)

// rawSource turns olivere/elastic's *json.RawMessage hit field into the []byte
// json.Unmarshal expects. Returns nil for missing sources so the caller's
// Unmarshal fails fast and the bad row is skipped (rather than panicking on
// a nil deref).
func rawSource(s *json.RawMessage) []byte {
	if s == nil {
		return nil
	}
	return []byte(*s)
}

// addCommonFilters layers the optional filter clauses (sender, time window)
// shared across all four endpoints onto a *BoolQuery.
func addCommonFilters(b *elastic.BoolQuery, filters SearchFilters) {
	if len(filters.SenderIDs) > 0 {
		terms := make([]any, 0, len(filters.SenderIDs))
		for _, s := range filters.SenderIDs {
			if s != "" {
				terms = append(terms, s)
			}
		}
		if len(terms) > 0 {
			b.Filter(elastic.NewTermsQuery("from", terms...))
		}
	}
	if filters.SentAtFrom != "" || filters.SentAtTo != "" {
		rng := elastic.NewRangeQuery("timestamp")
		if from, ok := parseSentAt(filters.SentAtFrom, true); ok {
			rng = rng.Gte(from)
		}
		if to, ok := parseSentAt(filters.SentAtTo, false); ok {
			rng = rng.Lte(to)
		}
		b.Filter(rng)
	}
}

// applyChannelAndRevoked adds the channel-routing filter and the standard
// revoked / cmd negation clauses every endpoint shares.
//
// The `MustNot Term(revoked=true)` clause is a best-effort optimisation, NOT
// a security boundary: indexer v1.8 only sets `revoked` on the initial index
// write, so any message revoked AFTER initial indexing keeps `revoked=false`
// in OS until a partial-update job lands (tracked in the v1.10 mapping
// follow-up). The authoritative check is the post-filter in
// visibility.filterVisible — that consults `message_extra.revoke=1` from
// MySQL on every hit, which is the same source of truth the read paths use
// (see modules/message/api_channel_files.go::filterMessages). Keeping the
// native term here still trims the page that hits the post-filter for the
// docs OS *does* have flagged correctly.
func applyChannelAndRevoked(b *elastic.BoolQuery, normChannelID string) {
	b.Filter(elastic.NewTermQuery("channelId", normChannelID))
	b.MustNot(elastic.NewTermQuery("revoked", true))
}

// applySystemMessageHardFilter layers the indexer-spec §2.4 "搜索硬过滤"
// contract for control-plane / system payload types onto a bool query. Per
// ~/Projects/_refs/wukongim-message-indexer/docs/specs/2026-06-04-v1.6-decisions.md §2.4:
//
//	must_not:
//	  - { term:  { payload.type: 99 } }                            # Cmd
//	  - { range: { payload.type: { gte: 1000, lte: 2000 } } }      # FriendApply..Tip
//
// Both /_search_messages (the legacy keyword/browse surface) and
// /_search_around (anchor + window) need this — without it the empty-keyword
// browse path and the around window both surface GroupCreate / Tip /
// FriendApply events that the user never authored.
//
// Scope: this helper covers ONLY the payload.type negations. The `revoked`
// negation is a separate concern (message-level state, not control-plane
// type) and lives in applyChannelAndRevoked; search/browse paths should call
// both.
//
// Note (RTC 9989-9999): the spec also reserves a Webhook/RTC range that is
// not yet pinned by the indexer owners. Once §2.3 is finalised this helper
// should add the matching `must_not` clause so both endpoints pick it up at
// the same time.
func applySystemMessageHardFilter(b *elastic.BoolQuery) {
	b.MustNot(elastic.NewTermQuery("payload.type", payloadTypeCmd))
	b.MustNot(elastic.NewRangeQuery("payload.type").Gte(payloadTypeSystemMin).Lte(payloadTypeSystemMax))
}

// applyExcludeVirtual hangs `must_not(virtual=true)` on the text-side DSL so
// virtual media/file sub-documents derived from rich-text messages (Part B,
// payload.type=2/5/8) cannot surface as standalone messages in /_search,
// /_search_all, or /_search_around. Until Part B lands the `virtual` field
// does not exist in the mapping and this term matches zero documents — a
// no-op for current corpora, byte-different only in DSL pin tests. The
// media/file endpoints (/_search_media, /_search_files) deliberately omit
// this filter so their virtual children remain reachable.
func applyExcludeVirtual(b *elastic.BoolQuery) {
	b.MustNot(elastic.NewTermQuery("virtual", true))
}

// applySpaceIDScope layers the per-Space term filter onto the bool query for
// p2p (channel_type=1) search only. Group and thread searches are already
// space-scoped at the channel level (channel_id encodes the parent space) so
// adding a redundant term filter would only mask indexer-mapping mismatches.
//
// p2p docs in OS carry `spaceId` from indexer v1.9 (mirrors
// payload.space_id). Until that mapping is rolled out and the existing
// corpus is backfilled, doc.spaceId is missing on legacy rows and the term
// filter matches nothing — that is the intended fail-closed behaviour while
// the dependency is satisfied. See PRD-CONSTRAINTS / "Person/DM Space
// Isolation".
//
// Empty spaceID is a no-op here: the handler is responsible for either
// rejecting the request (RequireSpaceID=true) or knowingly skipping the
// scope filter (RequireSpaceID=false). We never silently include p2p hits
// for an unknown Space.
func applySpaceIDScope(b *elastic.BoolQuery, channelType uint8, spaceID string) {
	if channelType != channelTypePerson {
		return
	}
	if spaceID == "" {
		return
	}
	b.Filter(elastic.NewTermQuery("spaceId", spaceID))
}

// applySort returns a SearchService with the requested sort applied.
//   - time_desc (default): timestamp desc + messageId desc + subSeq desc
//   - time_asc:            timestamp asc  + messageId asc  + subSeq asc
//   - relevance:           timestamp desc + _score desc + messageId desc + subSeq desc
//
// `relevance` is rejected upstream by the validator for endpoints (e.g.
// _search_media) where no keyword is involved.
//
// The trailing `subSeq` tiebreaker is the Part B fix for virtual sub-documents
// (rich-text-derived media/file children) that share (timestamp, messageId)
// with their parent and their siblings: without it, OS's exclusive
// search_after at a (ts, msgID) boundary silently drops the remaining
// same-tuple siblings. Indexer writes subSeq=0 on plain/parent docs and
// 1..N on virtual children, so the tuple becomes globally unique. Legacy
// docs without the field deserialise to 0 — same value as plain docs, so
// the sort stays stable before the indexer field ships.
//
// subSeq carries UnmappedType("long") + Missing(0): the field is absent from
// the OpenSearch mapping until the indexer rollout lands, and a sort on an
// unmapped field hard-fails with query_shard_exception ("No mapping found for
// [subSeq]"). Without this, a reader-first deploy (the documented rollout
// order) or a rollback would 400 every /_search* call. unmapped_type makes OS
// treat the missing field as a long, Missing(0) pins absent values to the same
// 0 the plain/parent convention uses — no behaviour change once mapping exists.
func applySort(s *elastic.SearchService, sort string) *elastic.SearchService {
	return s.SortBy(searchSorters(sort)...)
}

// searchSorters returns the ordered sort keys applySort applies. Extracted so
// tests can pin the exact wire shape (field order + the subSeq unmapped_type /
// missing guards) against the production builders rather than a hand-rebuilt
// copy.
func searchSorters(sort string) []elastic.Sorter {
	// subSeq always carries unmapped_type+missing — see applySort doc above.
	subSeqAsc := elastic.NewFieldSort("subSeq").Asc().UnmappedType("long").Missing(0)
	subSeqDesc := elastic.NewFieldSort("subSeq").Desc().UnmappedType("long").Missing(0)
	switch sort {
	case "time_asc":
		return []elastic.Sorter{
			elastic.NewFieldSort("timestamp").Asc(),
			elastic.NewFieldSort("messageId").Asc(),
			subSeqAsc,
		}
	case "relevance":
		return []elastic.Sorter{
			elastic.NewFieldSort("timestamp").Desc(),
			elastic.NewScoreSort(),
			elastic.NewFieldSort("messageId").Desc(),
			subSeqDesc,
		}
	default:
		return []elastic.Sorter{
			elastic.NewFieldSort("timestamp").Desc(),
			elastic.NewFieldSort("messageId").Desc(),
			subSeqDesc,
		}
	}
}

// pickSnippet selects the most informative highlight fragment for a hit.
// Priority follows A doc §2.1: text content first, then rich-text search-text,
// then merge-forward child search-text, then image caption, then file name.
// The /_search type whitelist [1,11,14] never delivers image/file docs, but
// /_search_around (no whitelist) does — keep those branches so an image/file
// hit in the around window still surfaces a snippet. Returns "" when no field
// highlighted.
func pickSnippet(h map[string][]string) string {
	if h == nil {
		return ""
	}
	for _, field := range []string{
		"payload.text.content",
		"payload.richText.searchText",
		"payload.mergeForward.msgs.searchText",
		"payload.image.caption",
		"payload.file.name",
	} {
		if frags, ok := h[field]; ok && len(frags) > 0 && frags[0] != "" {
			return frags[0]
		}
	}
	return ""
}

// snippetWindow caps a fallback snippet's rune length. Matches the keyword
// path's highlight FragmentSize(120) so empty-keyword browse hits and keyword
// hits surface a comparably sized preview.
const snippetWindow = 120

// fallbackSnippet derives a plain-text snippet straight from the doc payload
// when the keyword highlight produced nothing — the empty-keyword browse case,
// where no Highlight is requested at all (highlight is only attached when
// keyword != "", see search_messages.go / search_all.go). Without this a
// browse hit comes back with snippet="" and the client has no message text to
// render, violating the A-doc §2.1 contract that every message hit carries
// content.
//
// Field priority mirrors pickSnippet (text → rich-text → merge-forward child →
// image caption → file name) so the fallback and the highlighted path agree on
// which field represents the message. The /_search type whitelist [1,11,14]
// never delivers image/file docs, but /_search_around (no whitelist) does, so
// keeping the image/file branches preserves the snippet for media messages in
// the around window. Returns "" only when the payload has no textual
// projection at all (e.g. a bare voice/video doc), leaving snippet
// omitted exactly as before.
func fallbackSnippet(p *Payload) string {
	if p == nil {
		return ""
	}
	if p.Text != nil && p.Text.Content != "" {
		return truncateRunes(p.Text.Content, snippetWindow)
	}
	if p.RichText != nil && p.RichText.SearchText != "" {
		return truncateRunes(p.RichText.SearchText, snippetWindow)
	}
	if p.MergeForward != nil {
		for _, m := range p.MergeForward.Msgs {
			if m.SearchText != "" {
				return truncateRunes(m.SearchText, snippetWindow)
			}
		}
	}
	if p.Image != nil && p.Image.Caption != "" {
		return truncateRunes(p.Image.Caption, snippetWindow)
	}
	if p.File != nil && p.File.Name != "" {
		return truncateRunes(p.File.Name, snippetWindow)
	}
	// Video has no text-bearing field in the v1.8 indexer projection
	// (VideoPayload carries url/cover/width/height/second only — see
	// source.go), so there is nothing to clip into a snippet. Browse-mode
	// /_search_all and /_search_around now surface the renderable bits
	// (thumb_url, duration_ms, dimensions) directly on MessageHit via
	// applyMediaProjection — the empty snippet here is intentional, not a
	// missed branch.
	return ""
}

// truncateRunes clips s to at most n runes, appending "…" when it had to cut.
// Rune-based so it never splits a multi-byte UTF-8 codepoint — CJK content is
// the common case here.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// extractSortValues pulls (timestamp, messageId, score?) from a search hit's
// `sort` array so the next-page cursor can be encoded. The shape depends on
// the requested sort:
//   - time_desc / time_asc: sort = [timestamp, messageId]      → score=nil
//   - relevance:            sort = [timestamp, _score, messageId] → score non-nil
//
// Returns zeros / nil when the hit is missing sort values, which means the
// caller has already exhausted the page or requested an inconsistent sort.
func extractSortValues(sort []any, isRelevance bool) (int64, int64, *float64) {
	if isRelevance {
		if len(sort) < 3 {
			return 0, 0, nil
		}
		ts := numericTo64(sort[0])
		score := numericToFloat(sort[1])
		msgID := numericTo64(sort[2])
		return ts, msgID, &score
	}
	if len(sort) < 2 {
		return 0, 0, nil
	}
	return numericTo64(sort[0]), numericTo64(sort[1]), nil
}

// numericTo64 squashes the variety of numeric shapes JSON unmarshalling can
// produce (float64 from encoding/json, json.Number, ints from typed APIs)
// into a single int64.
//
// Precision caveat: the float64 case rounds for values above 2^53, so it must
// never be the source of record for snowflake message IDs — those are read
// from the typed _source (Doc.MessageID) instead; see computeCursorPagination.
func numericTo64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			f, ferr := n.Float64()
			if ferr != nil {
				return 0
			}
			return int64(f)
		}
		return i
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case uint64:
		return int64(n)
	}
	return 0
}

// numericToFloat is the float64 sibling of numericTo64 for OS _score values,
// which arrive as float64 from encoding/json but may also be json.Number or
// integer types depending on the client path.
func numericToFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0
		}
		return f
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case uint64:
		return float64(n)
	}
	return 0
}

// computeCursorPagination derives has_more / next_cursor from an OS hit list.
// The timestamp (and _score for relevance) come off the last raw hit's Sort
// array — the canonical sort tuple OS itself uses. The messageId tiebreaker
// is deliberately NOT taken from Sort: the default decoder unmarshals sort
// values as float64, which rounds snowflake IDs above 2^53 and silently
// corrupts the cursor at timestamp-tied boundaries (messages skipped or
// duplicated on the next page). It is read from the typed _source
// (Doc.MessageID, int64) instead, which keeps full precision.
//
// subSeq follows the same typed-_source policy: even though subSeq is small
// enough that float64 won't lose precision, reading it from Doc.SubSeq keeps
// the same source-of-record contract as messageId — the Sort tuple is the
// resume-protocol shape (search_after input), the _source is the
// authoritative value emitted onto the cursor.
//
// Invariant: when the last hit yields ts=0 or msgID=0 (e.g. mismatched sort
// mode, missing sort array, unparsable _source) we return (hasMore=false, "")
// rather than emitting a half-valid cursor — wire shape requires a non-empty,
// usable next_cursor whenever has_more is true (spec v4.2 §1.4).
func (h *Handler) computeCursorPagination(result *elastic.SearchResult, pageSize int, sort string) (bool, string) {
	if result == nil || result.Hits == nil || len(result.Hits.Hits) == 0 {
		return false, ""
	}
	if len(result.Hits.Hits) < pageSize {
		return false, ""
	}
	last := result.Hits.Hits[len(result.Hits.Hits)-1]
	ts, _, score := extractSortValues(last.Sort, sort == "relevance")
	msgID, subSeq := lastHitMessageIDAndSubSeq(last)
	if ts == 0 || msgID == 0 {
		return false, ""
	}
	return true, encodeCursor(h.cfg, ts, msgID, score, subSeq)
}

// lastHitMessageID reads the full-precision messageId from a hit's typed
// _source. Returns 0 when the source is missing or malformed, which the
// caller treats as "no cursor".
func lastHitMessageID(hit *elastic.SearchHit) int64 {
	id, _ := lastHitMessageIDAndSubSeq(hit)
	return id
}

// lastHitMessageIDAndSubSeq reads (messageId, subSeq) from a hit's typed
// _source in one Unmarshal — same precision policy as lastHitMessageID, but
// also returns the Part B subSeq tiebreaker for cursor encoding. Returns
// (0, 0) when the source is missing or malformed.
func lastHitMessageIDAndSubSeq(hit *elastic.SearchHit) (int64, int) {
	if hit == nil {
		return 0, 0
	}
	var doc Doc
	if err := json.Unmarshal(rawSource(hit.Source), &doc); err != nil {
		return 0, 0
	}
	return doc.MessageID, doc.SubSeq
}

// buildSearchAfterFromHit reconstructs an OS search_after tuple from a hit
// for in-loop round-refill (paginateWithFilter). The messageId tiebreaker
// is read from the typed _source rather than hit.Sort: JSON-decoded sort
// values are float64, which rounds snowflake ids above 2^53 and corrupts
// the resume boundary at timestamp ties. Same policy as
// computeCursorPagination, so the internal round-refill anchor and the
// external next_cursor share one full-precision id source.
//
// Sort tuple shapes (must match decodeCursorAsSearchAfter and the sort
// clauses in dsl.go::buildSearch):
//   - time_desc / time_asc: [timestamp, messageId, subSeq]
//   - relevance:            [timestamp, _score, messageId, subSeq]
//
// Timestamp comes off hit.Sort[0] as float64 — safe at second precision.
// _score for relevance comes off hit.Sort[1] as float64 — same as OS uses
// internally. subSeq comes off Doc.SubSeq (typed _source) so the cursor's
// emitted value matches the indexer's authoritative write rather than the
// float64-decoded Sort entry (Part B; see
// docs/messages-search/richtext-virtual-docs-cursor-tiebreaker-for-indexer.md).
//
// Returns ok=false when the typed _source can't be parsed or hit.Sort is
// malformed. Caller should stop the round loop on !ok rather than resume
// on a corrupt boundary.
func buildSearchAfterFromHit(hit *elastic.SearchHit, isRelevance bool) ([]any, bool) {
	if hit == nil {
		return nil, false
	}
	msgID, subSeq := lastHitMessageIDAndSubSeq(hit)
	if msgID == 0 {
		return nil, false
	}
	if isRelevance {
		if len(hit.Sort) < 3 {
			return nil, false
		}
		ts := numericTo64(hit.Sort[0])
		score := numericToFloat(hit.Sort[1])
		return []any{ts, score, msgID, subSeq}, true
	}
	if len(hit.Sort) < 2 {
		return nil, false
	}
	ts := numericTo64(hit.Sort[0])
	return []any{ts, msgID, subSeq}, true
}

// fileContentSourceExcludes returns the shared _source excludes context every
// messages_search endpoint attaches to its OS Search call. The excluded fields
// carry the Tika-extracted file body (payload.file.content, avg 30–100 KB per
// doc) and its extraction metadata (payload.file.contentMeta); both participate
// in the multi_match relevance score via the inverted index but never surface
// on the wire — Q1 C keeps FileHit without a contentSnippet field, and Q4 D
// keeps pickSnippet from promoting content into the snippet slot. Highlight
// fragments are drawn from the inverted index rather than _source, so this
// exclude does not affect highlight functionality.
func fileContentSourceExcludes() *elastic.FetchSourceContext {
	return elastic.NewFetchSourceContext(true).Exclude(
		"payload.file.content",
		"payload.file.contentMeta",
	)
}
