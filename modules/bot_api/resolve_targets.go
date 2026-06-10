package bot_api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// resolve/targets limit semantics — intentionally distinct from botSpaceMembers
// (default 50 / max 200). This endpoint defaults to 20 and caps at 50.
const (
	resolveTargetsDefaultLimit = 20
	resolveTargetsMaxLimit     = 50
)

// resolveTargetKinds enumerates the accepted `kind` filter values.
const (
	resolveKindAll    = "all"
	resolveKindGroup  = "group"
	resolveKindThread = "thread"
)

// resolveTargetCandidate is one match returned by botResolveTargets. Groups and
// threads share this shape and are distinguished by `kind`; absent fields are
// omitted (a group has no short_id / parent_name). Channel type is 2 for a
// group and 5 for a thread (community topic).
type resolveTargetCandidate struct {
	Kind        string `json:"kind"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	Name        string `json:"name"`
	GroupNo     string `json:"group_no"`
	ShortID     string `json:"short_id,omitempty"`
	ParentName  string `json:"parent_name,omitempty"`
}

// botResolveTargets handles GET /v1/bot/resolve/targets.
//
// It searches the calling bot's visible groups and threads by name and returns
// a unified candidate array so the adapter can disambiguate a named target
// ("forward to 'XXX'") instead of guessing. Visibility is defined purely by
// membership (group_member.uid=robotID AND is_deleted=0) — there is no space_id
// filter, so a bot pulled into a cross-space external group can still resolve
// that group and its threads. App Bots have no group_member rows, so (like the
// read-only getGroups precedent) they naturally resolve to an empty set rather
// than being rejected.
func (ba *BotAPI) botResolveTargets(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}

	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		respondBotAPIRequestInvalid(c, "name")
		return
	}

	kind := strings.TrimSpace(c.Query("kind"))
	if kind == "" {
		kind = resolveKindAll
	}
	if kind != resolveKindAll && kind != resolveKindGroup && kind != resolveKindThread {
		respondBotAPIRequestInvalid(c, "kind")
		return
	}

	// Explicit parse + clamp. Unlike botSpaceMembers (fmt.Sscanf), we validate
	// with strconv.Atoi and degrade leniently: a missing / unparseable / <=0
	// value keeps the default rather than erroring.
	limit := resolveTargetsDefaultLimit
	if s := strings.TrimSpace(c.Query("limit")); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > resolveTargetsMaxLimit {
		limit = resolveTargetsMaxLimit
	}

	// Escape LIKE wildcards in user input (same escaping as botSpaceMembers).
	escaped := strings.ReplaceAll(name, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "%", "\\%")
	escaped = strings.ReplaceAll(escaped, "_", "\\_")
	pattern := "%" + escaped + "%"

	candidates := make([]resolveTargetCandidate, 0)
	truncated := false

	if kind == resolveKindAll || kind == resolveKindGroup {
		type groupRow struct {
			GroupNo string `db:"group_no"`
			Name    string `db:"name"`
		}
		var rows []groupRow
		// Only the bot's own groups (group_member.uid=robotID AND is_deleted=0);
		// naturally includes cross-space external groups, no space filter.
		// LIMIT limit+1 probes for truncation. ORDER BY (g.name = ?) DESC puts an
		// exact (un-wildcarded) name match first, then a stable group_no order.
		_, err := ba.ctx.DB().SelectBySql(
			"SELECT g.group_no, g.name FROM group_member gm INNER JOIN `group` g ON gm.group_no = g.group_no "+
				"WHERE gm.uid = ? AND gm.is_deleted = 0 AND g.name LIKE ? "+
				"ORDER BY (g.name = ?) DESC, g.group_no ASC LIMIT ?",
			robotID, pattern, name, limit+1,
		).Load(&rows)
		if err != nil {
			ba.Error("resolve targets: query groups failed", zap.Error(err), zap.String("robotID", robotID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if len(rows) > limit {
			truncated = true
			rows = rows[:limit]
		}
		for _, r := range rows {
			candidates = append(candidates, resolveTargetCandidate{
				Kind:        resolveKindGroup,
				ChannelID:   r.GroupNo,
				ChannelType: common.ChannelTypeGroup.Uint8(),
				Name:        r.Name,
				GroupNo:     r.GroupNo,
			})
		}
	}

	if kind == resolveKindAll || kind == resolveKindThread {
		type threadRow struct {
			GroupNo    string `db:"group_no"`
			ShortID    string `db:"short_id"`
			Name       string `db:"name"`
			ParentName string `db:"parent_name"`
		}
		var rows []threadRow
		// Only threads under groups the bot belongs to. status IN (1,2) keeps
		// active + archived and excludes deleted(3), matching the system
		// GetThreads / sanitizeListStatuses contract. LIMIT limit+1 probes for
		// truncation; exact name match first, then a stable ordering.
		//
		// COLLATE utf8mb4_general_ci is pinned here because thread.* is
		// utf8mb4_general_ci while `group`/group_member.* are utf8mb4_0900_ai_ci.
		// Two distinct reasons, do not conflate them:
		//   1. REQUIRED — the JOIN ON column-to-column equalities
		//      (gm.group_no = t.group_no, g.group_no = t.group_no) compare two
		//      columns of differing collation with no literal to coerce them, so
		//      MySQL 8.0 raises Error 1267 (Illegal mix of collations). COLLATE
		//      on these is the actual fix.
		//   2. DEFENSIVE — the ORDER BY exact-match check (t.name = ?) and the
		//      t.name LIKE ? filter compare a column against a parameter/literal,
		//      whose collation is coercible, so they normally do NOT raise 1267.
		//      COLLATE there only pins the sort/compare semantics to one explicit
		//      collation so ranking stays consistent.
		// general_ci is chosen as the common collation; only the thread query
		// needs this — the group query above touches no general_ci columns and is
		// left untouched.
		_, err := ba.ctx.DB().SelectBySql(
			"SELECT t.group_no, t.short_id, t.name, g.name AS parent_name FROM thread t "+
				"INNER JOIN group_member gm ON gm.group_no = t.group_no COLLATE utf8mb4_general_ci AND gm.uid = ? AND gm.is_deleted = 0 "+
				"INNER JOIN `group` g ON g.group_no = t.group_no COLLATE utf8mb4_general_ci "+
				"WHERE t.status IN (1, 2) AND t.name LIKE ? "+
				"ORDER BY (t.name = ? COLLATE utf8mb4_general_ci) DESC, t.status ASC, t.group_no ASC, t.short_id ASC LIMIT ?",
			robotID, pattern, name, limit+1,
		).Load(&rows)
		if err != nil {
			ba.Error("resolve targets: query threads failed", zap.Error(err), zap.String("robotID", robotID))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if len(rows) > limit {
			truncated = true
			rows = rows[:limit]
		}
		for _, r := range rows {
			candidates = append(candidates, resolveTargetCandidate{
				Kind:        resolveKindThread,
				ChannelID:   thread.BuildChannelID(r.GroupNo, r.ShortID),
				ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
				Name:        r.Name,
				GroupNo:     r.GroupNo,
				ShortID:     r.ShortID,
				ParentName:  r.ParentName,
			})
		}
	}

	c.JSON(http.StatusOK, map[string]interface{}{
		"candidates": candidates,
		"total":      len(candidates),
		"truncated":  truncated,
	})
}
