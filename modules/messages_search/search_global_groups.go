package messages_search

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/google/uuid"
	"github.com/olivere/elastic"
	"go.uber.org/zap"
)

// SearchGlobalGroupsReq is the request body for
// POST /v1/messages/_search_global_groups — the L1 group-aggregation
// (聚合优先) overview (aggregation-first design §2). Unlike
// _search_global_messages it has NO sort / page_size / cursor: L1 returns one
// aggregated overview per request and the bucket order is fixed to latest_at
// desc. `sequence` is echoed back verbatim for the frontend's stale-response
// guard (§4) — the backend does not validate its monotonicity.
type SearchGlobalGroupsReq struct {
	Keyword  string              `json:"keyword,omitempty"`
	Sequence int64               `json:"sequence,omitempty"`
	Filters  GlobalSearchFilters `json:"filters,omitempty"`
}

// GroupsResult is the `data` object of the L1 response. The outer envelope is
// the shared {data, pagination} shell (pagination.has_more = 命中群数 >
// maxGroups; next_cursor is always ""). total_groups is a cardinality HLL
// estimate so total_groups_approx is always true.
type GroupsResult struct {
	Sequence          int64         `json:"sequence"`
	QueryID           string        `json:"query_id"`
	TotalGroups       int64         `json:"total_groups"`
	TotalGroupsApprox bool          `json:"total_groups_approx"`
	Groups            []GroupBucket `json:"groups"`
}

// GroupBucket is one aggregated channel/thread/DM bucket. DM buckets carry the
// reversed peer uid in ChannelID (channel_type=1) with no parent_group_no /
// thread fields; group buckets set parent_group_no to their own channel_id;
// thread buckets (channel_type=5) carry parent_group_no + thread_id +
// thread_name. match_count is the OS doc_count (pre-visibility) so
// match_count_approx is always true.
type GroupBucket struct {
	ChannelID        string       `json:"channel_id"`
	ChannelType      uint8        `json:"channel_type"`
	ParentGroupNo    string       `json:"parent_group_no,omitempty"`
	GroupName        string       `json:"group_name"`
	ThreadID         string       `json:"thread_id,omitempty"`
	ThreadName       string       `json:"thread_name,omitempty"`
	MatchCount       int64        `json:"match_count"`
	MatchCountApprox bool         `json:"match_count_approx"`
	LatestAt         string       `json:"latest_at"`
	Preview          []MessageHit `json:"preview"`
}

func init() {
	registerRoute(func(h *Handler, g *wkhttp.RouterGroup) {
		g.POST("/_search_global_groups", h.searchGlobalGroups)
	})
}

// searchGlobalGroups is POST /v1/messages/_search_global_groups. It runs a
// single OS terms(channelId) aggregation over the caller's allowlist scope
// (reusing the _search_global_messages DSL) plus an OS-layer visibles
// whitelist, then projects each bucket into the L1 overview shape. presence
// calibration + per-frequency preview allocation are backend B — this handler
// returns the raw top_hits preview.
func (h *Handler) searchGlobalGroups(c *wkhttp.Context) {
	var req SearchGlobalGroupsReq
	if err := c.BindJSON(&req); err != nil {
		respondValidation(c, "body", "invalid JSON")
		return
	}
	req.Keyword = strings.TrimSpace(req.Keyword)
	loginUID := c.GetLoginUID()

	if !validateKeywordOptional(c, req.Keyword) {
		return
	}
	// §2.3 trigger gate — STRICTER than _search_global_messages'
	// validateSearchNotEmpty: sent_at_* / content_types / channel_types alone
	// do NOT trigger. Only keyword ∨ sender_ids ∨ member_uids/member_uid ∨
	// channel_ids may open an aggregation, keeping the terms scope bounded.
	if !groupsTriggerSatisfied(req.Keyword, req.Filters) {
		respondValidation(c, "keyword", "at least one of keyword, sender_ids, member_uids, or channel_ids is required")
		return
	}
	if _, ok := validateGlobalBase(c, h.cfg, "", "", req.Filters, 0, false); !ok {
		return
	}

	osChannelIDs, spaceID, _, allowTimings, ok := h.resolveGlobalScope(c, loginUID, req.Filters.ChannelIDs, req.Filters.MemberUIDs, req.Filters.MemberUID)
	if !ok {
		return
	}
	recordAllowlistTimings(c, allowTimings)
	if len(osChannelIDs) == 0 {
		recordAudit(c, "search_global_groups", 0, "", req.Keyword, 0)
		c.Response(groupsEnvelope(emptyGroupsResult(req.Sequence), false))
		return
	}

	client, err := ESClient(h.cfg)
	if err != nil {
		h.Error("ESClient init failed", zap.Error(err))
		respondUpstream(c)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.Timeout)
	defer cancel()

	msgReq := SearchGlobalMessagesReq{Keyword: req.Keyword, Filters: req.Filters}
	base, analyzeErr := buildGlobalMessagesDSL(ctx, newOSIKSmartAnalyzer(client), h.cfg.StopwordStripEnabled, msgReq, osChannelIDs, spaceID)
	if analyzeErr != nil {
		h.Warn("messages_search: _analyze fallback (degraded keyword clause)", zap.Error(analyzeErr))
	}
	query := elastic.NewBoolQuery().Filter(base)
	applyVisiblesWhitelist(query, loginUID)

	res, qerr := h.runGroupsAggregation(ctx, client, query, req.Keyword != "")
	if qerr != nil {
		if responder := classifyOSError(qerr); responder != nil {
			h.Warn("OS search_global_groups failed", zap.Error(qerr))
			responder(c)
			return
		}
		h.Error("messages_search: groups aggregation failed", zap.Error(qerr))
		respondInternal(c)
		return
	}

	result, hasMore, berr := h.buildGroupsResult(ctx, c, res, req.Sequence, loginUID, client, base)
	if berr != nil {
		// Deep-probe OS round-trips can fail like any other search; a
		// classifiable OS error maps to UPSTREAM_UNAVAILABLE, everything else
		// (notably the fail-closed filterVisible DB gate) is INTERNAL_ERROR so
		// we never fall through to leaking uncalibrated preview.
		if responder := classifyOSError(berr); responder != nil {
			h.Warn("OS presence deep-probe failed", zap.Error(berr))
			responder(c)
			return
		}
		h.Error("messages_search: presence calibration failed", zap.Error(berr))
		respondInternal(c)
		return
	}
	recordAudit(c, "search_global_groups", 0, "", req.Keyword, len(result.Groups))
	c.Response(groupsEnvelope(result, hasMore))
}

// groupsTriggerSatisfied implements the §2.3 trigger gate. It deliberately
// does NOT count sent_at_* / content_types / channel_types — those alone must
// return VALIDATION_ERROR, which is why hasEffectiveFilters (which treats
// sent_at as effective) is not reused here.
func groupsTriggerSatisfied(keyword string, f GlobalSearchFilters) bool {
	if strings.TrimSpace(keyword) != "" {
		return true
	}
	for _, s := range f.SenderIDs {
		if strings.TrimSpace(s) != "" {
			return true
		}
	}
	for _, u := range f.MemberUIDs {
		if strings.TrimSpace(u) != "" {
			return true
		}
	}
	if strings.TrimSpace(f.MemberUID) != "" {
		return true
	}
	for _, ref := range f.ChannelIDs {
		if strings.TrimSpace(ref.ChannelID) != "" {
			return true
		}
	}
	return false
}

// applyVisiblesWhitelist adds the OS-layer visibles gate: a doc is visible when
// its `visibles` array is empty/absent (no per-message allowlist) OR it lists
// the caller. An empty array indexes no values, so exists(visibles) is false
// for both absent and empty docs — mustNot(exists) covers the "no gate" case.
// Together with the revoked mustNot already emitted by buildGlobalMessagesDSL
// this pushes the two OS-resident visibility signals into the query layer so
// backend B's presence calibration only has to chase the MySQL-only signals.
func applyVisiblesWhitelist(b *elastic.BoolQuery, loginUID string) {
	noGate := elastic.NewBoolQuery().MustNot(elastic.NewExistsQuery("visibles"))
	listed := elastic.NewTermQuery("visibles", loginUID)
	b.Filter(elastic.NewBoolQuery().Should(noGate, listed).MinimumShouldMatch("1"))
}

// runGroupsAggregation issues the single size:0 terms(channelId) aggregation.
// Each bucket carries max(timestamp) (latest_at), a top_hits over-fetch of
// T=perGroupMax+presenceProbe (preview + backend-B calibration headroom); a
// sibling cardinality(channelId) yields the approximate total_groups.
func (h *Handler) runGroupsAggregation(ctx context.Context, client *elastic.Client, query elastic.Query, withHighlight bool) (*elastic.SearchResult, error) {
	gc := h.cfg.Groups
	preview := elastic.NewTopHitsAggregation().
		Size(gc.TopHitsSize()).
		SortBy(searchSorters("time_desc")...).
		FetchSourceContext(fileContentSourceExcludes())
	if withHighlight {
		preview = preview.Highlight(buildSearchAllHighlight())
	}
	byChannel := elastic.NewTermsAggregation().
		Field("channelId").
		Size(gc.MaxGroups).
		OrderByAggregation("latest", false).
		SubAggregation("latest", elastic.NewMaxAggregation().Field("timestamp")).
		SubAggregation("preview", preview)
	return client.Search().
		Index(h.cfg.OSReadAlias).
		Query(query).
		Size(0).
		TrackTotalHits(false).
		Aggregation("by_channel", byChannel).
		Aggregation("group_count", elastic.NewCardinalityAggregation().Field("channelId")).
		Do(ctx)
}

// parsedBucket is the pre-projection view of one terms bucket.
type parsedBucket struct {
	key      string
	docCount int64
	latestTS int64
	hits     []*elastic.SearchHit // already capped to perGroupMax
}

// buildGroupsResult projects the aggregation response into the L1 data object.
// Backend B: between parse and assemble it runs presence calibration (§2.2) so
// every returned bucket "确有可见命中" and every preview row is a filterVisible
// -passed visible hit — the raw top_hits are NEVER projected to the wire (that
// was the A-review Blocker: uncalibrated preview leaked snippet/sender/time of
// admin-deleted / self-deleted / cleared-history messages). has_more still
// reflects 命中群数 > maxGroups (terms sum_other_doc_count on the single-shard
// index). Returns an error when calibration hits a fail-closed DB gate or a
// deep-probe OS round-trip fails, so the caller can respond upstream/internal
// instead of leaking.
func (h *Handler) buildGroupsResult(ctx context.Context, c *wkhttp.Context, res *elastic.SearchResult, seq int64, loginUID string, client *elastic.Client, base elastic.Query) (GroupsResult, bool, error) {
	result := emptyGroupsResult(seq)
	// Keep the FULL over-fetched top-T sample (T=perGroupMax+presenceProbe) for
	// calibration — do not truncate to perGroupMax here; preview trimming to the
	// per-frequency N happens after INCLUDE/EXCLUDE is decided.
	parsed, totalGroups, hasMore, ok := parseGroupsAggregation(res, h.cfg.Groups.TopHitsSize())
	result.TotalGroups = totalGroups
	if !ok {
		return result, false, nil
	}

	var timings searchPhaseTimings
	calibrated, stats, err := h.calibratePresence(ctx, client, base, parsed, loginUID, &timings)
	recordSearchTimings(c, timings)
	recordPresenceStats(c, stats)
	if err != nil {
		return result, false, err
	}

	// Per-frequency N allocation (§4): spread previewBudget across the surviving
	// buckets weighted by match frequency, then take the most-recent N visible
	// rows per bucket.
	ns := allocatePreviewN(calibrated, h.cfg.Groups.PreviewBudget, h.cfg.Groups.PerGroupMax)
	visibleHits := make([][]*elastic.SearchHit, len(calibrated))
	for i, cb := range calibrated {
		k := ns[i]
		if k > len(cb.visible) {
			k = len(cb.visible)
		}
		visibleHits[i] = cb.visible[:k]
	}

	var groupNos, shortIDs, peerUIDs []string
	for _, cb := range calibrated {
		ct, wireID, parentGroupNo, shortID := classifyGroupBucket(cb.pb.key, loginUID)
		switch ct {
		case channelTypePerson:
			peerUIDs = append(peerUIDs, wireID)
		case channelTypeGroup:
			groupNos = append(groupNos, cb.pb.key)
		case channelTypeThread:
			groupNos = append(groupNos, parentGroupNo)
			shortIDs = append(shortIDs, shortID)
		}
	}

	groupNames := h.resolveGroupNames(groupNos)
	threadNames := h.resolveThreadNames(shortIDs)
	peerNames := h.resolvePeerNames(peerUIDs)
	previews := h.projectPreview(ctx, visibleHits, loginUID)

	for i, cb := range calibrated {
		result.Groups = append(result.Groups, assembleGroupBucket(cb.pb, previews[i], loginUID, groupNames, threadNames, peerNames))
	}
	return result, hasMore, nil
}

// parseGroupsAggregation is the pure (I/O-free) reader of the OS aggregation
// response: it extracts total_groups (cardinality), has_more (terms
// sum_other_doc_count > 0 on the single-shard index) and each bucket's key /
// doc_count / latest_at / preview hits (capped to capHits). Backend B passes
// TopHitsSize() as capHits so the full over-fetched top-T sample survives for
// presence calibration; preview trimming to the per-frequency N happens later.
// Kept separate from name resolution so the has_more + bucket parsing can be
// unit-tested without standing up group/user/thread services. ok=false means
// the response carried no by_channel terms aggregation.
func parseGroupsAggregation(res *elastic.SearchResult, capHits int) (buckets []parsedBucket, totalGroups int64, hasMore, ok bool) {
	if res == nil || res.Aggregations == nil {
		return nil, 0, false, false
	}
	if card, cok := res.Aggregations.Cardinality("group_count"); cok && card.Value != nil {
		totalGroups = int64(*card.Value)
	}
	terms, tok := res.Aggregations.Terms("by_channel")
	if !tok || terms == nil {
		return nil, totalGroups, false, false
	}
	hasMore = terms.SumOfOtherDocCount > 0
	for _, b := range terms.Buckets {
		key := bucketKeyString(b)
		if key == "" {
			continue
		}
		latestTS := int64(0)
		if m, mok := b.Max("latest"); mok && m.Value != nil {
			latestTS = int64(*m.Value)
		}
		var hits []*elastic.SearchHit
		if th, hok := b.TopHits("preview"); hok && th != nil && th.Hits != nil {
			hits = th.Hits.Hits
			if capHits > 0 && len(hits) > capHits {
				hits = hits[:capHits]
			}
		}
		buckets = append(buckets, parsedBucket{key: key, docCount: b.DocCount, latestTS: latestTS, hits: hits})
	}
	return buckets, totalGroups, hasMore, true
}

// projectPreview turns each bucket's already-calibrated visible hits into
// []MessageHit and fills sender names/avatars in a single batched senderJoin
// across all buckets (the global endpoints deliberately skip per-group remark
// scoping — channelType=0 degrades senderJoin to a plain user lookup). Input is
// the presence-calibrated + N-trimmed visible hits per bucket, so nothing here
// reaches the wire without having passed filterVisible.
func (h *Handler) projectPreview(ctx context.Context, bucketHits [][]*elastic.SearchHit, loginUID string) [][]MessageHit {
	previews := make([][]MessageHit, len(bucketHits))
	var senderIDs []string
	for bi, hitList := range bucketHits {
		hits := make([]MessageHit, 0, len(hitList))
		for _, hit := range hitList {
			var doc Doc
			if err := json.Unmarshal(rawSource(hit.Source), &doc); err != nil {
				h.Warn("messages_search: bad groups preview _source skipped", zap.Error(err))
				continue
			}
			wireID, wireType := wireChannelFromDoc(doc, loginUID)
			mh := h.singleMessageHit(doc, wireID, wireType, map[string][]string(hit.Highlight))
			hits = append(hits, mh)
			senderIDs = append(senderIDs, doc.From)
			for _, im := range mh.InnerMessages {
				if im.SenderID != "" {
					senderIDs = append(senderIDs, im.SenderID)
				}
			}
		}
		previews[bi] = hits
	}
	join := h.senderJoin(ctx, uniqUIDs(senderIDs), 0, "")
	for bi := range previews {
		for i := range previews[bi] {
			mh := &previews[bi][i]
			mh.SenderName = join.Names[mh.SenderID]
			mh.SenderAvatarURL = join.Avatars[mh.SenderID]
			for j := range mh.InnerMessages {
				if uid := mh.InnerMessages[j].SenderID; uid != "" {
					mh.InnerMessages[j].SenderName = join.Names[uid]
				}
			}
		}
	}
	return previews
}

// classifyGroupBucket maps a terms(channelId) key to its wire shape. DM buckets
// carry a p2p fake channelId (`uidA@uidB`) reversed to the peer uid; thread
// buckets carry the composite `{group_no}____{short_id}`; everything else is a
// group whose parent_group_no is itself.
func classifyGroupBucket(key, loginUID string) (channelType uint8, wireID, parentGroupNo, threadShortID string) {
	if common.IsFakeChannel(key) {
		return channelTypePerson, common.GetToChannelIDWithFakeChannelID(key, loginUID), "", ""
	}
	if groupNo, shortID, err := thread.ParseChannelID(key); err == nil {
		return channelTypeThread, key, groupNo, shortID
	}
	return channelTypeGroup, key, key, ""
}

// assembleGroupBucket builds the wire bucket from the parsed bucket + resolved
// name maps. Pure (no I/O) so the DM reversal / thread parent_group_no / group
// shapes are unit-testable without OS or MySQL.
func assembleGroupBucket(pb parsedBucket, preview []MessageHit, loginUID string, groupNames, threadNames, peerNames map[string]string) GroupBucket {
	if preview == nil {
		preview = []MessageHit{}
	}
	ct, wireID, parentGroupNo, threadShortID := classifyGroupBucket(pb.key, loginUID)
	gb := GroupBucket{
		ChannelID:        wireID,
		ChannelType:      ct,
		MatchCount:       pb.docCount,
		MatchCountApprox: true,
		LatestAt:         msToRFC3339(pb.latestTS),
		Preview:          preview,
	}
	switch ct {
	case channelTypePerson:
		gb.GroupName = peerNames[wireID]
	case channelTypeGroup:
		gb.ParentGroupNo = pb.key
		gb.GroupName = groupNames[pb.key]
	case channelTypeThread:
		gb.ParentGroupNo = parentGroupNo
		gb.ThreadID = pb.key
		gb.ThreadName = threadNames[threadShortID]
		gb.GroupName = groupNames[parentGroupNo]
	}
	return gb
}

// resolveGroupNames batch-resolves group_no → group name. Soft-fail: on error
// the names are dropped (empty group_name) but the request still serves.
func (h *Handler) resolveGroupNames(groupNos []string) map[string]string {
	out := map[string]string{}
	groupNos = uniqUIDs(groupNos)
	if len(groupNos) == 0 {
		return out
	}
	infos, err := h.groupService.GetGroups(groupNos)
	if err != nil {
		h.Warn("messages_search: groups name lookup failed", zap.Error(err))
		return out
	}
	for _, g := range infos {
		if g != nil {
			out[g.GroupNo] = g.Name
		}
	}
	return out
}

// resolvePeerNames batch-resolves DM peer uid → display name via the user
// profile (§5b: DM header名字走对端用户档案 fallback). Soft-fail to placeholder.
func (h *Handler) resolvePeerNames(peerUIDs []string) map[string]string {
	out := map[string]string{}
	peerUIDs = uniqUIDs(peerUIDs)
	if len(peerUIDs) == 0 {
		return out
	}
	users, err := h.userService.GetUsers(peerUIDs)
	if err != nil {
		h.Warn("messages_search: DM peer name lookup failed", zap.Error(err))
		return out
	}
	for _, u := range users {
		if u != nil {
			out[u.UID] = u.Name
		}
	}
	return out
}

// resolveThreadNames batch-resolves thread short_id → thread name. Soft-fail:
// on error thread_name is left empty and the bucket still renders under its
// parent group.
func (h *Handler) resolveThreadNames(shortIDs []string) map[string]string {
	out := map[string]string{}
	shortIDs = uniqUIDs(shortIDs)
	if len(shortIDs) == 0 {
		return out
	}
	models, err := thread.NewDB(h.ctx).QueryByShortIDs(shortIDs)
	if err != nil {
		h.Warn("messages_search: thread name lookup failed", zap.Error(err))
		return out
	}
	for sid, m := range models {
		if m != nil {
			out[sid] = m.Name
		}
	}
	return out
}

// bucketKeyString reads the terms bucket key as a string (channelId is a
// keyword field). Falls back to key_as_string for defensiveness.
func bucketKeyString(b *elastic.AggregationBucketKeyItem) string {
	if b == nil {
		return ""
	}
	if s, ok := b.Key.(string); ok {
		return s
	}
	if b.KeyAsString != nil {
		return *b.KeyAsString
	}
	return ""
}

// emptyGroupsResult is the zero-hit / empty-scope data object (§5.5).
func emptyGroupsResult(seq int64) GroupsResult {
	return GroupsResult{
		Sequence:          seq,
		QueryID:           uuid.NewString(),
		TotalGroups:       0,
		TotalGroupsApprox: true,
		Groups:            []GroupBucket{},
	}
}

// groupsEnvelope wraps the L1 data object into the shared {data, pagination}
// shell. next_cursor is always "" (L1 has no per-row pagination).
func groupsEnvelope(result GroupsResult, hasMore bool) CursorList {
	return CursorList{
		Data:       result,
		Pagination: Pagination{HasMore: hasMore, NextCursor: ""},
	}
}
