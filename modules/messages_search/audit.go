package messages_search

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// auditMiddleware emits a structured audit log line per request after the
// handler chain returns. Tracks PRM-02 fields:
//   - kind          (search_messages | search_media | search_files | search_all)
//   - login_uid
//   - channel_type / channel_id
//   - keyword_hash  (first 16 hex chars of SHA-256 — keeps the keyword opaque
//     while still allowing post-hoc deduplication)
//   - took_ms
//
// We intentionally do NOT log the keyword in clear: search queries can carry
// PII (names, IDs, sensitive search terms) and the audit channel is shared
// with other ops use-cases that should not see them.
func (h *Handler) auditMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		start := time.Now()
		c.Next()
		took := time.Since(start)

		fields := []zap.Field{
			zap.String("path", c.FullPath()),
			zap.String("login_uid", c.GetLoginUID()),
			zap.Int64("took_ms", took.Milliseconds()),
			zap.Int("status", c.Writer.Status()),
		}
		if v, ok := c.Get(auditFieldKindKey); ok {
			if s, _ := v.(string); s != "" {
				fields = append(fields, zap.String("kind", s))
			}
		}
		if v, ok := c.Get(auditFieldChannelTypeKey); ok {
			if t, _ := v.(uint8); t != 0 {
				fields = append(fields, zap.Uint8("channel_type", t))
			}
		}
		if v, ok := c.Get(auditFieldChannelIDKey); ok {
			if s, _ := v.(string); s != "" {
				fields = append(fields, zap.String("channel_id", s))
			}
		}
		if v, ok := c.Get(auditFieldKeywordHashKey); ok {
			if s, _ := v.(string); s != "" {
				fields = append(fields, zap.String("keyword_hash", s))
			}
		}
		if v, ok := c.Get(auditFieldHitsKey); ok {
			if n, _ := v.(int); n >= 0 {
				fields = append(fields, zap.Int("hits", n))
			}
		}
		// Per-phase timings (YUJ-27): emit whichever phases the handler
		// recorded. Only global endpoints touch these; single-channel paths
		// leave them unset and the fields drop from the log line.
		if v, ok := c.Get(auditFieldAllowlistMsKey); ok {
			if ms, _ := v.(int64); ms >= 0 {
				fields = append(fields, zap.Int64("allowlist_ms", ms))
			}
		}
		if v, ok := c.Get(auditFieldMemberActiveMsKey); ok {
			if ms, _ := v.(int64); ms >= 0 {
				fields = append(fields, zap.Int64("member_active_ms", ms))
			}
		}
		if v, ok := c.Get(auditFieldBlacklistMsKey); ok {
			if ms, _ := v.(int64); ms >= 0 {
				fields = append(fields, zap.Int64("blacklist_ms", ms))
			}
		}
		if v, ok := c.Get(auditFieldThreadEnumMsKey); ok {
			if ms, _ := v.(int64); ms >= 0 {
				fields = append(fields, zap.Int64("thread_enum_ms", ms))
			}
		}
		if v, ok := c.Get(auditFieldDMPeersMsKey); ok {
			if ms, _ := v.(int64); ms >= 0 {
				fields = append(fields, zap.Int64("dm_peers_ms", ms))
			}
		}
		if v, ok := c.Get(auditFieldOSSearchMsKey); ok {
			if ms, _ := v.(int64); ms >= 0 {
				fields = append(fields, zap.Int64("os_search_ms", ms))
			}
		}
		if v, ok := c.Get(auditFieldFilterVisibleMsKey); ok {
			if ms, _ := v.(int64); ms >= 0 {
				fields = append(fields, zap.Int64("filter_visible_ms", ms))
			}
		}
		if v, ok := c.Get(auditFieldSenderJoinMsKey); ok {
			if ms, _ := v.(int64); ms >= 0 {
				fields = append(fields, zap.Int64("sender_join_ms", ms))
			}
		}
		if v, ok := c.Get(auditFieldOversamplePagesKey); ok {
			if n, _ := v.(int); n >= 0 {
				fields = append(fields, zap.Int("oversample_pages", n))
			}
		}
		h.Info("messages_search.audit", fields...)
	}
}

// hashKeyword renders a keyword as the audit-friendly opaque hash (first 16
// hex chars of SHA-256). Empty keywords produce an empty string.
func hashKeyword(keyword string) string {
	if keyword == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(keyword))
	return hex.EncodeToString(sum[:])[:16]
}

// gin context keys used to ferry per-request audit fields out of the handler
// and into the trailing middleware.
const (
	auditFieldKindKey        = "messages_search.audit.kind"
	auditFieldChannelTypeKey = "messages_search.audit.channel_type"
	auditFieldChannelIDKey   = "messages_search.audit.channel_id"
	auditFieldKeywordHashKey = "messages_search.audit.keyword_hash"
	auditFieldHitsKey        = "messages_search.audit.hits"

	// Per-phase timing keys (YUJ-27). Set by the global path only — single-
	// channel handlers may adopt them incrementally. Values stored as int64
	// milliseconds; oversample_pages is an int round-count.
	auditFieldAllowlistMsKey     = "messages_search.audit.allowlist_ms"
	auditFieldMemberActiveMsKey  = "messages_search.audit.member_active_ms"
	auditFieldBlacklistMsKey     = "messages_search.audit.blacklist_ms"
	auditFieldThreadEnumMsKey    = "messages_search.audit.thread_enum_ms"
	auditFieldDMPeersMsKey       = "messages_search.audit.dm_peers_ms"
	auditFieldOSSearchMsKey      = "messages_search.audit.os_search_ms"
	auditFieldFilterVisibleMsKey = "messages_search.audit.filter_visible_ms"
	auditFieldSenderJoinMsKey    = "messages_search.audit.sender_join_ms"
	auditFieldOversamplePagesKey = "messages_search.audit.oversample_pages"
)

// recordAudit stores the per-request audit fields the middleware will pick up.
// Called by every handler exactly once per request.
func recordAudit(c *wkhttp.Context, kind string, channelType uint8, channelID, keyword string, hits int) {
	c.Set(auditFieldKindKey, kind)
	c.Set(auditFieldChannelTypeKey, channelType)
	c.Set(auditFieldChannelIDKey, channelID)
	c.Set(auditFieldKeywordHashKey, hashKeyword(keyword))
	c.Set(auditFieldHitsKey, hits)
}

// recordAllowlistTimings ferries per-phase MySQL costs measured inside
// buildAllowlist onto the request context so the audit middleware can emit
// them next to took_ms. Zero-valued phases (helper not exercised for this
// request — e.g. empty spaceID -> no thread enumeration) are still recorded
// so "absent field vs 0 ms" stays distinguishable in the log.
func recordAllowlistTimings(c *wkhttp.Context, t allowlistTimings) {
	if c == nil {
		return
	}
	c.Set(auditFieldAllowlistMsKey, t.totalMs())
	c.Set(auditFieldMemberActiveMsKey, t.memberActive.Milliseconds())
	c.Set(auditFieldBlacklistMsKey, t.blacklist.Milliseconds())
	c.Set(auditFieldThreadEnumMsKey, t.threadEnum.Milliseconds())
	c.Set(auditFieldDMPeersMsKey, t.dmPeers.Milliseconds())
}

// recordSearchTimings ferries the post-allowlist phase costs into the audit
// pipeline. Called after paginateWithFilterDepth returns.
func recordSearchTimings(c *wkhttp.Context, t searchPhaseTimings) {
	if c == nil {
		return
	}
	c.Set(auditFieldOSSearchMsKey, t.osSearch.Milliseconds())
	c.Set(auditFieldFilterVisibleMsKey, t.filterVisible.Milliseconds())
	c.Set(auditFieldSenderJoinMsKey, t.senderJoin.Milliseconds())
	c.Set(auditFieldOversamplePagesKey, t.oversamplePages)
}

// allowlistTimings accumulates the per-phase MySQL costs paid inside
// buildAllowlist. Zero-value fields represent phases that were not touched
// for a given request (e.g. spaceID="" skips space_member).
type allowlistTimings struct {
	memberActive time.Duration // group ExistMembersActive batch
	blacklist    time.Duration // DM ExistBlacklistsBoth batch
	threadEnum   time.Duration // thread QueryNonDeletedShortIDsByGroupNos
	dmPeers      time.Duration // GetFriends + fetchSpaceMemberUIDs + bot filter
}

func (t allowlistTimings) totalMs() int64 {
	return (t.memberActive + t.blacklist + t.threadEnum + t.dmPeers).Milliseconds()
}

// searchPhaseTimings tracks the post-allowlist phases of a global search:
// wall-clock time spent inside the paginate loop (OS round-trips +
// filterVisible), sender-join, and how many oversample rounds we ran.
type searchPhaseTimings struct {
	osSearch        time.Duration
	filterVisible   time.Duration
	senderJoin      time.Duration
	oversamplePages int
}
