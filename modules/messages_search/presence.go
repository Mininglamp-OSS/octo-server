package messages_search

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/olivere/elastic"
)

// deepProbePageSize is the old-ward search_after page size for the UNRESOLVED
// deep probe (§2.2). Fixed (not config) — it only trades round-trips against
// per-page filterVisible width and never affects the visible result, which is
// bounded by K2.
const deepProbePageSize = 50

// calibratedBucket is a terms bucket that survived presence calibration
// (INCLUDE): it carries the visible hits (most-recent-first) drawn either from
// the top-T sample or discovered by the deep probe. EXCLUDE buckets are dropped
// before this stage so they never reach the wire.
//
// latestVisibleTS is the timestamp (ms) of the most-recent VISIBLE hit
// (visible[0]) — the RC fix. The wire `latest_at` and the returned bucket order
// must both derive from this, NOT from the OS pre-filter max(timestamp) held in
// pb.latestTS: if the newest match in a bucket is hidden (admin/self-deleted,
// cleared history, visibles), the OS max would leak that hidden message's
// recency into latest_at and pull the bucket up the latest-first order.
type calibratedBucket struct {
	pb              parsedBucket
	visible         []*elastic.SearchHit
	latestVisibleTS int64
}

// presenceStats is the §7 埋点 for the presence pass: how many buckets went
// UNRESOLVED into the deep probe and how many docs the probe scanned in total.
type presenceStats struct {
	deepProbeBuckets int
	deepProbeDocs    int
}

// calibratePresence implements §2.2: for every candidate bucket it decides
// INCLUDE / EXCLUDE / UNRESOLVED from the over-fetched top-T sample, then deep-
// probes the UNRESOLVED ones to first-visible / K2. It closes the A-review
// Blocker — the raw top_hits are filtered through the five-gate filterVisible
// (revoked + visibles already dropped at the OS layer; admin/mutual is_deleted,
// self-delete, channel-offset watermark applied here) so no invisible message's
// snippet/sender/time can leak into preview.
//
// A single batched filterVisible covers the top-T of ALL buckets in one MySQL
// round-trip (the whole point of visibility.go's batch surface). Fail-closed:
// any DB error propagates so the handler responds INTERNAL_ERROR rather than
// releasing uncalibrated hits.
func (h *Handler) calibratePresence(
	ctx context.Context,
	client *elastic.Client,
	base elastic.Query,
	buckets []parsedBucket,
	loginUID string,
	timings *searchPhaseTimings,
) ([]calibratedBucket, presenceStats, error) {
	var stats presenceStats
	if len(buckets) == 0 {
		return nil, stats, nil
	}

	// Project every top-T hit once, collecting the refs for the single batched
	// filterVisible while remembering each hit's ref so the per-bucket verdict
	// can test membership without re-projecting.
	project := projectDocRef("", loginUID)
	bucketRefs := make([][]msgRef, len(buckets))
	var allRefs []msgRef
	for bi, pb := range buckets {
		refs := make([]msgRef, len(pb.hits))
		for i, hit := range pb.hits {
			if r, ok := project(hit); ok {
				refs[i] = r
				allRefs = append(allRefs, r)
			}
		}
		bucketRefs[bi] = refs
	}

	keep := map[string]struct{}{}
	if len(allRefs) > 0 {
		start := time.Now()
		var err error
		keep, err = h.filterVisible(ctx, loginUID, "", allRefs)
		timings.filterVisible += time.Since(start)
		if err != nil {
			return nil, stats, err
		}
	}

	out := make([]calibratedBucket, 0, len(buckets))
	for bi, pb := range buckets {
		var vis []*elastic.SearchHit
		for i, hit := range pb.hits {
			r := bucketRefs[bi][i]
			if r.MessageID == "" {
				continue
			}
			if _, okv := keep[r.MessageID]; okv {
				vis = append(vis, hit)
			}
		}
		switch classifyPresence(len(pb.hits), pb.docCount, len(vis)) {
		case presenceInclude:
			// ≥1 visible hit in the sample.
			out = append(out, calibratedBucket{pb: pb, visible: vis, latestVisibleTS: latestVisibleTS(vis)})
		case presenceExclude:
			// 0 visible and the top-T already covered the whole bucket → the
			// group has no visible hit at all → drop it.
			continue
		case presenceUnresolved:
			// The most recent T are all hidden but older docs remain.
			found, dvis, scanned, err := h.deepProbeBucket(ctx, client, base, pb, loginUID, timings)
			stats.deepProbeBuckets++
			stats.deepProbeDocs += scanned
			if err != nil {
				return nil, stats, err
			}
			if found {
				out = append(out, calibratedBucket{pb: pb, visible: dvis, latestVisibleTS: latestVisibleTS(dvis)})
			}
		}
	}
	return out, stats, nil
}

// presenceVerdict is the three-state §2.2 outcome of the top-T sample check.
type presenceVerdict int

const (
	presenceInclude presenceVerdict = iota
	presenceExclude
	presenceUnresolved
)

// classifyPresence maps (top-T sample size, doc_count, visible-in-sample count)
// to the §2.2 verdict. ≥1 visible → INCLUDE. 0 visible with the sample already
// covering the whole bucket (sampleSize >= doc_count ⟺ T >= doc_count, exact on
// the single-shard index) → EXCLUDE. 0 visible with older docs beyond the
// sample → UNRESOLVED (deep probe). Boundary: sampleSize == doc_count with 0
// visible is EXCLUDE, never a spurious UNRESOLVED (test ⑤). See §14.11 for the
// post-reshard caveat where doc_count turns approximate and EXCLUDE must yield
// to unconditional deep probing.
func classifyPresence(sampleSize int, docCount int64, visibleCount int) presenceVerdict {
	if visibleCount > 0 {
		return presenceInclude
	}
	if int64(sampleSize) >= docCount {
		return presenceExclude
	}
	return presenceUnresolved
}

// latestVisibleTS returns the timestamp (ms) of the most-recent VISIBLE hit in
// a calibrated bucket. visible is most-recent-first (top_hits / deep-probe both
// sort time_desc and the filter preserves that order), so visible[0] is the
// newest hit the caller may actually see. Empty → 0 (defensive; INCLUDE buckets
// always carry ≥1 visible hit).
func latestVisibleTS(visible []*elastic.SearchHit) int64 {
	if len(visible) == 0 {
		return 0
	}
	return hitTimestampMS(visible[0])
}

// hitTimestampMS reads the OS doc `timestamp` (ms) off a hit's _source. Used to
// recompute latest_at from the calibrated visible hit rather than the OS
// pre-filter max(timestamp), which would leak a hidden message's recency.
func hitTimestampMS(hit *elastic.SearchHit) int64 {
	if hit == nil {
		return 0
	}
	var d Doc
	if err := json.Unmarshal(rawSource(hit.Source), &d); err != nil {
		return 0
	}
	return d.Timestamp
}

// deepProbeBucket is the UNRESOLVED old-ward probe (§2.2). It resumes strictly
// after the oldest top-T hit (search_after — no overlap, no skip) and pages
// backwards through the single channel running filterVisible per page:
//
//   - first visible hit found → INCLUDE (returns that page's visible hits)
//   - channel exhausted (page shorter than requested) → EXCLUDE
//   - cumulative scan (top-T + probe) reaches K2 → EXCLUDE (bounded latency)
//
// The K2 budget counts the top-T we already examined, so the group is presence-
// exact for M_group ≤ K2 and a group whose only visible hit sits past K2 is a
// known, metered residual (test ⑥). The probe reuses the L1 `base` query
// (analysis already done once) narrowed to this channel; DM buckets pass their
// OS fake channelId as-is (that is what the index stores). This wires the OS
// round-trip closure and delegates the loop to deepProbeVisible so the paging /
// budget / three-outcome logic is unit-testable without a live cluster.
func (h *Handler) deepProbeBucket(
	ctx context.Context,
	client *elastic.Client,
	base elastic.Query,
	pb parsedBucket,
	loginUID string,
	timings *searchPhaseTimings,
) (found bool, visible []*elastic.SearchHit, scanned int, err error) {
	budget := h.cfg.Groups.K2 - len(pb.hits)
	if budget <= 0 || len(pb.hits) == 0 {
		return false, nil, 0, nil
	}
	initialSA, ok := buildSearchAfterFromHit(pb.hits[len(pb.hits)-1], false)
	if !ok {
		// Can't build a safe resume boundary → conservative EXCLUDE (never a
		// leak; at worst hides a group whose visible hit is older than T).
		return false, nil, 0, nil
	}

	sorters := searchSorters("time_desc")
	osSearch := func(searchAfter []any, size int) ([]*elastic.SearchHit, error) {
		query := elastic.NewBoolQuery().Filter(base, elastic.NewTermQuery("channelId", pb.key))
		applyVisiblesWhitelist(query, loginUID)
		svc := client.Search().
			Index(h.cfg.OSReadAlias).
			Query(query).
			Size(size).
			TrackTotalHits(false).
			FetchSourceContext(fileContentSourceExcludes()).
			SortBy(sorters...).
			SearchAfter(searchAfter...)
		res, qerr := svc.Do(ctx)
		if qerr != nil {
			return nil, qerr
		}
		if res == nil || res.Hits == nil {
			return nil, nil
		}
		return res.Hits.Hits, nil
	}
	return h.deepProbeVisible(ctx, loginUID, initialSA, budget, osSearch, timings)
}

// deepProbeVisible is the pure paging loop of the UNRESOLVED deep probe: it
// pulls old-ward pages via osSearch (deepProbePageSize, trimmed to the residual
// budget) and applies filterVisible to each. The three outcomes mirror §2.2:
// first visible hit → (true, that page's visible hits); a short page → channel
// exhausted → (false, nil); the K2-anchored budget consumed → (false, nil).
// Each page is filtered exactly once against recall-time state, so a permission
// change mid-probe (test ⑦) is self-consistent within the request — earlier
// pages are never re-scanned.
func (h *Handler) deepProbeVisible(
	ctx context.Context,
	loginUID string,
	initialSA []any,
	budget int,
	osSearch osQueryFn,
	timings *searchPhaseTimings,
) (found bool, visible []*elastic.SearchHit, scanned int, err error) {
	searchAfter := initialSA
	project := projectDocRef("", loginUID)
	for scanned < budget {
		size := deepProbePageSize
		if rem := budget - scanned; rem < size {
			size = rem
		}
		start := time.Now()
		hits, qerr := osSearch(searchAfter, size)
		if timings != nil {
			timings.osSearch += time.Since(start)
		}
		if qerr != nil {
			return false, nil, scanned, qerr
		}
		if len(hits) == 0 {
			return false, nil, scanned, nil // channel exhausted
		}
		scanned += len(hits)

		refs := make([]msgRef, len(hits))
		filterInput := make([]msgRef, 0, len(hits))
		for i, hit := range hits {
			if r, pok := project(hit); pok {
				refs[i] = r
				filterInput = append(filterInput, r)
			}
		}
		if len(filterInput) > 0 {
			fstart := time.Now()
			keep, ferr := h.filterVisible(ctx, loginUID, "", filterInput)
			if timings != nil {
				timings.filterVisible += time.Since(fstart)
			}
			if ferr != nil {
				return false, nil, scanned, ferr
			}
			var vis []*elastic.SearchHit
			for i, hit := range hits {
				if refs[i].MessageID == "" {
					continue
				}
				if _, okv := keep[refs[i].MessageID]; okv {
					vis = append(vis, hit)
				}
			}
			if len(vis) > 0 {
				return true, vis, scanned, nil // first visible → INCLUDE
			}
		}
		if len(hits) < size {
			return false, nil, scanned, nil // channel exhausted
		}
		nextSA, sok := buildSearchAfterFromHit(hits[len(hits)-1], false)
		if !sok {
			return false, nil, scanned, nil
		}
		searchAfter = nextSA
	}
	return false, nil, scanned, nil // K2 budget exhausted → EXCLUDE
}

// allocatePreviewN spreads the global previewBudget across the calibrated
// buckets weighted by match frequency (doc_count), so active groups get more
// preview rows and low-frequency groups fall to the floor of 1 (§4:
// 老/低频群 N=1、活跃多). Each N is clamped to [1, perGroupMax]. latest_at
// already governs bucket order (terms order + top_hits desc) and therefore
// WHICH rows preview shows; doc_count governs HOW MANY. Budget is a soft
// ceiling — the perGroupMax clamp may leave the sum below budget, never above
// (every bucket ≤ perGroupMax and the weighted extra sums to ≤ budget-n).
func allocatePreviewN(buckets []calibratedBucket, budget, perGroupMax int) []int {
	n := len(buckets)
	ns := make([]int, n)
	if n == 0 {
		return ns
	}
	if perGroupMax < 1 {
		perGroupMax = 1
	}
	// Floor: every included bucket shows at least one visible row.
	for i := range ns {
		ns[i] = 1
	}
	extra := budget - n
	if extra < 0 {
		extra = 0
	}

	weights := make([]float64, n)
	var totalW float64
	for i, b := range buckets {
		w := float64(b.pb.docCount)
		if w < 1 {
			w = 1
		}
		weights[i] = w
		totalW += w
	}

	if extra > 0 && totalW > 0 {
		// Largest-remainder apportionment of `extra` across the weights.
		type rem struct {
			idx  int
			frac float64
		}
		rems := make([]rem, n)
		assigned := 0
		for i := range ns {
			exact := float64(extra) * weights[i] / totalW
			add := int(exact)
			ns[i] += add
			assigned += add
			rems[i] = rem{idx: i, frac: exact - float64(add)}
		}
		leftover := extra - assigned
		sort.SliceStable(rems, func(a, b int) bool { return rems[a].frac > rems[b].frac })
		for k := 0; k < leftover && k < len(rems); k++ {
			ns[rems[k].idx]++
		}
	}

	for i := range ns {
		if ns[i] > perGroupMax {
			ns[i] = perGroupMax
		}
		if ns[i] < 1 {
			ns[i] = 1
		}
	}
	return ns
}

// gin context keys carrying the presence deep-probe 埋点 (§7) out to the audit
// middleware, alongside the YUJ-27 per-phase timings.
const (
	auditFieldPresenceProbeBucketsKey = "messages_search.audit.presence_probe_buckets"
	auditFieldPresenceProbeDocsKey    = "messages_search.audit.presence_probe_docs"
)

// recordPresenceStats ferries the presence deep-probe counters into the audit
// pipeline. Zero values are still recorded so "no probe this request" stays
// distinguishable from "field absent on a non-L1 request".
func recordPresenceStats(c *wkhttp.Context, s presenceStats) {
	if c == nil {
		return
	}
	c.Set(auditFieldPresenceProbeBucketsKey, s.deepProbeBuckets)
	c.Set(auditFieldPresenceProbeDocsKey, s.deepProbeDocs)
}
