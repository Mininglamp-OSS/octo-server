// Package message — POST /v2/sidebar/sync
//
// # Data-flow overview
//
//  1. Validate request (tab ∈ {follow,recent}, device_uuid required).
//  2. Call ctx.IMSyncUserConversation to get the raw conversation list from the
//     IM core (timestamp, unread, last_msg_seq).
//  3. Load ancillary data in parallel-ish batches:
//     a. group_setting   → category_id, category_sort  (groupCategoryDB)
//     b. user_conversation_ext → unfollowed groups, followed DMs, thread ext rows
//     c. user_pinned_channel  → pinned set             (raw DB query via ctx.DB())
//  4. Apply tab-specific filtering:
//     follow  – groups with category + not unfollowed; followed DMs; threads
//     with ext row whose parent group is in the follow set.
//     recent  – all DMs; groups/threads with timestamp > now-72h.
//  5. Append standalone thread ext entries not already in the IM result.
//  6. Sort:
//     follow  – category_sort ASC, pinned DESC, follow_sort ASC.
//     recent  – pinned DESC, timestamp DESC.
//  7. Return SidebarSyncResp{Items, Version}.
//
// Module dependencies: imports modules/conversation_ext (for ext rows) and
// modules/thread (for QueryByShortIDs to enrich thread items with last_message_at).
// Does NOT import modules/group or modules/user — group_setting / pinned data
// are read via ctx.DB() raw queries to avoid pulling in those packages.
//
// Note: a follow-up review item (Important #4 — read pinned via a user-module
// helper) may eventually replace the raw user_pinned_channel query.
package message

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Structs
// ---------------------------------------------------------------------------

// Sidebar handles POST /v2/sidebar/sync.
type Sidebar struct {
	ctx             *config.Context
	groupCategoryDB *groupCategoryDB
	convExtDB       *convext.DB
	threadDB        *thread.DB
	followVersionDB *convext.FollowVersionDB
	log.Log
}

// NewSidebar creates a Sidebar handler.
func NewSidebar(ctx *config.Context) *Sidebar {
	return &Sidebar{
		ctx:             ctx,
		groupCategoryDB: newGroupCategoryDB(ctx),
		convExtDB:       convext.NewDB(ctx),
		threadDB:        thread.NewDB(ctx),
		followVersionDB: convext.NewFollowVersionDB(ctx),
		Log:             log.NewTLog("Sidebar"),
	}
}

// sidebarSyncReq is the JSON body for POST /v2/sidebar/sync.
type sidebarSyncReq struct {
	Tab         string `json:"tab"`     // "follow" | "recent"
	Version     int64  `json:"version"` // IM core version cursor
	LastMsgSeqs string `json:"last_msg_seqs"`
	MsgCount    int64  `json:"msg_count"`
	DeviceUUID  string `json:"device_uuid"`
}

// SidebarItem is one entry in the sidebar response.
type SidebarItem struct {
	TargetType      int     `json:"target_type"` // 1 DM / 2 group / 5 thread
	TargetID        string  `json:"target_id"`
	ChannelType     uint8   `json:"channel_type"`
	ChannelID       string  `json:"channel_id"`
	Timestamp       int64   `json:"timestamp"`
	Unread          int     `json:"unread"`
	IsPinned        bool    `json:"is_pinned"`
	IsFollowed      bool    `json:"is_followed"`
	CategoryID      *string `json:"category_id,omitempty"`
	CategorySort    int     `json:"category_sort,omitempty"`
	FollowSort      int     `json:"follow_sort,omitempty"`
	ParentChannelID string  `json:"parent_channel_id,omitempty"` // thread only
}

// sidebarSyncResp is the JSON response for POST /v2/sidebar/sync.
//
// Version 是 IM 会话游标（recent tab 的 cursor）。
// FollowVersion 是 user_follow_version 的当前值（follow tab 的 CAS / 增量检测）。
// PR review (Round 3) Blocking #1/#2 — IM 游标无法感知 follow 状态变化，所以需要
// 独立的 follow_version。客户端使用方式：
//   - 拉取 follow tab 后保存 follow_version，下次比较是否需要全量重建。
//   - 调 /v2/follow/sort 时把 follow_version 原样回传做 CAS。
type sidebarSyncResp struct {
	Items         []*SidebarItem `json:"items"`
	Version       int64          `json:"version"`
	FollowVersion int64          `json:"follow_version"`
}

// ---------------------------------------------------------------------------
// Route registration helper (called from Conversation.Route or standalone)
// ---------------------------------------------------------------------------

// RegisterSidebarRoutes mounts /v2/sidebar/sync onto the router.
func RegisterSidebarRoutes(r *wkhttp.WKHttp, ctx *config.Context) {
	sb := NewSidebar(ctx)
	uidLimit := appwkhttp.SharedUIDRateLimiter(ctx)
	v2 := r.Group("/v2/sidebar", ctx.AuthMiddleware(r), uidLimit, spacepkg.SpaceMiddleware(ctx))
	{
		v2.POST("/sync", sb.Sync)
	}
}

// ---------------------------------------------------------------------------
// Request validation
// ---------------------------------------------------------------------------

// 上限常量：IM 透传字段的边界。
// 透传给下游服务前 fail-fast，避免恶意/异常输入放大到 IM 核心。
const (
	// maxMsgCount 是 IM_SyncUserConversation 的 msg_count 上限。
	// 客户端通常 <= 100，这里设 1000 做上限。
	maxMsgCount int64 = 1000
	// maxLastMsgSeqsLen 是 last_msg_seqs 字符串长度上限（约 5000 个会话）。
	maxLastMsgSeqsLen = 65536
	// maxDeviceUUIDLen 是客户端生成的 UUID 长度上限。
	maxDeviceUUIDLen = 128
)

// validateSidebarRequest validates the request fields.
func validateSidebarRequest(req *sidebarSyncReq) error {
	if req.Tab != "follow" && req.Tab != "recent" {
		return errors.New("tab must be 'follow' or 'recent'")
	}
	deviceUUID := strings.TrimSpace(req.DeviceUUID)
	if deviceUUID == "" {
		return errors.New("device_uuid is required")
	}
	if len(deviceUUID) > maxDeviceUUIDLen {
		return fmt.Errorf("device_uuid too long (max %d)", maxDeviceUUIDLen)
	}
	if req.Version < 0 {
		return errors.New("version must be >= 0")
	}
	if req.MsgCount < 0 {
		return errors.New("msg_count must be >= 0")
	}
	if req.MsgCount > maxMsgCount {
		return fmt.Errorf("msg_count too large (max %d)", maxMsgCount)
	}
	if len(req.LastMsgSeqs) > maxLastMsgSeqsLen {
		return fmt.Errorf("last_msg_seqs too long (max %d bytes)", maxLastMsgSeqsLen)
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// Sync handles POST /v2/sidebar/sync.
func (sb *Sidebar) Sync(c *wkhttp.Context) {
	var req sidebarSyncReq
	if err := c.BindJSON(&req); err != nil {
		sb.Error("sidebar sync: bad JSON", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if err := validateSidebarRequest(&req); err != nil {
		c.ResponseError(err)
		return
	}

	loginUID := c.GetLoginUID()
	spaceID := spacepkg.GetSpaceID(c)

	// 1. Fetch IM conversations (version=0, no device offset logic for v2)
	conversations, err := sb.ctx.IMSyncUserConversation(loginUID, req.Version, req.MsgCount, req.LastMsgSeqs, nil)
	if err != nil {
		sb.Error("sidebar sync: IM fetch failed", zap.Error(err))
		c.ResponseError(errors.New("同步会话失败"))
		return
	}

	// 2. Load ancillary data
	// 2a. Group category settings
	groupNos := extractGroupNos(conversations)
	categorySetting := map[string]*GroupCategorySetting{}
	if len(groupNos) > 0 {
		settings, err := sb.groupCategoryDB.QueryCategorySettingsByGroupNos(groupNos, loginUID)
		if err != nil {
			sb.Warn("sidebar sync: category query failed (non-fatal)", zap.Error(err))
		} else {
			for _, s := range settings {
				categorySetting[s.GroupNo] = s
			}
		}
	}

	// 2b. Unfollowed groups
	unfollowedGroups := map[string]struct{}{}
	if unfollowed, err := sb.convExtDB.ListUnfollowedGroups(loginUID, spaceID); err != nil {
		sb.Warn("sidebar sync: unfollowed groups query failed (non-fatal)", zap.Error(err))
	} else {
		for _, m := range unfollowed {
			unfollowedGroups[m.TargetID] = struct{}{}
		}
	}

	// 2c. Followed DMs
	followedDMs := map[string]*convext.Model{}
	if dms, err := sb.convExtDB.ListFollowedDM(loginUID, spaceID); err != nil {
		sb.Warn("sidebar sync: followed DM query failed (non-fatal)", zap.Error(err))
	} else {
		for _, m := range dms {
			followedDMs[m.TargetID] = m
		}
	}

	// 2d. Thread ext rows (for follow tab thread entries)
	threadExtRows := []*convext.Model{}
	if req.Tab == "follow" {
		rows, err := sb.convExtDB.ListThreadExts(loginUID, spaceID)
		if err != nil {
			sb.Warn("sidebar sync: thread ext query failed (non-fatal)", zap.Error(err))
		} else {
			threadExtRows = rows
		}
	}
	threadExtMap := make(map[string]*convext.Model, len(threadExtRows))
	for _, m := range threadExtRows {
		threadExtMap[m.TargetID] = m
	}

	// 2e. Pinned channels
	pinnedSet, err := sb.loadPinnedSet(loginUID, spaceID)
	if err != nil {
		sb.Warn("sidebar sync: pinned query failed (non-fatal)", zap.Error(err))
		pinnedSet = map[string]struct{}{}
	}

	// 3. Build tab-specific items
	var items []*SidebarItem
	switch req.Tab {
	case "follow":
		items = buildFollowItems(conversations, categorySetting, unfollowedGroups, followedDMs, threadExtMap)
		// Append standalone thread ext entries not present in IM result.
		// Pass categorySetting + unfollowedGroups so parent-follow filter applies
		// to DB-only thread entries as well (PR review Round-3 Blocking #4).
		lastMsgAtMap := sb.loadThreadLastMsgAt(threadExtRows)
		items = mergeThreadEntries(items, threadExtRows, lastMsgAtMap, categorySetting, unfollowedGroups)
	case "recent":
		items = buildRecentItems(conversations, pinnedSet)
	}

	// 4. Enrich pinned flag (follow tab items also need it)
	for _, item := range items {
		k := channelKey(item.TargetID, uint8(item.TargetType))
		if _, ok := pinnedSet[k]; ok {
			item.IsPinned = true
		}
	}

	// 5. Sort
	switch req.Tab {
	case "follow":
		sortFollowItems(items)
	case "recent":
		sortRecentItems(items)
	}

	// 6. Compute response version from raw conversations
	respVersion := maxConversationVersion(conversations, req.Version)

	// 7. Load follow_version (PR review Round-3 Blocking #1/#2).
	//    非致命：返 0 时客户端只会理解为"没有新状态"，对业务无害。
	var followVersion int64
	if v, err := sb.followVersionDB.Get(loginUID, spaceID); err != nil {
		sb.Warn("sidebar sync: follow_version query failed (non-fatal)", zap.Error(err))
	} else {
		followVersion = v
	}

	c.JSON(http.StatusOK, &sidebarSyncResp{
		Items:         items,
		Version:       respVersion,
		FollowVersion: followVersion,
	})
}

// ---------------------------------------------------------------------------
// loadPinnedSet loads the user's pinned channels as a set keyed by channelKey.
// ---------------------------------------------------------------------------

func (sb *Sidebar) loadPinnedSet(uid, spaceID string) (map[string]struct{}, error) {
	type row struct {
		ChannelID   string `db:"channel_id"`
		ChannelType uint8  `db:"channel_type"`
	}
	var rows []row
	_, err := sb.ctx.DB().SelectBySql(
		"SELECT channel_id, channel_type FROM user_pinned_channel WHERE uid=? AND space_id=?",
		uid, spaceID,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("loadPinnedSet: %w", err)
	}
	set := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		set[channelKey(r.ChannelID, r.ChannelType)] = struct{}{}
	}
	return set, nil
}

// loadThreadLastMsgAt queries the thread table for last_message_at of the
// given thread ext rows.
//
// PR review (Round 3) Blocking #3 / Important #4：用 (group_no, short_id) 复合键
// 匹配，只取 status != deleted 的行。SELECT 也收窄到最小列，不必要地读 thread
// 其他列（thread_md 等大文本）。
//
// 返回 map 的键就是 ext.TargetID（"{groupNo}____{shortID}" 格式）。
// map 中不存在的键意味着"该 thread 已删除或不存在或跨群错配"，
// 调用方据此 skip 该 ext 行，避免把幽灵 thread emit 给客户端。
//
// PR review follow-up：之前的实现以 shortID 单键作 map，跨群同名 shortID 会
// 互相覆盖，且 mergeThreadEntries 无法区分"thread 不存在"与"last_message_at NULL"。
// 改为复合键后两种语义都清楚：键存在 = thread 活跃；值为 nil = last_message_at NULL。
func (sb *Sidebar) loadThreadLastMsgAt(extRows []*convext.Model) map[string]*time.Time {
	result := make(map[string]*time.Time, len(extRows))
	if len(extRows) == 0 {
		return result
	}
	refs := make([]thread.ShortRef, 0, len(extRows))
	for _, m := range extRows {
		gno, sid, err := parseThreadChannelIDSidebar(m.TargetID)
		if err != nil {
			continue
		}
		refs = append(refs, thread.ShortRef{GroupNo: gno, ShortID: sid})
	}
	if len(refs) == 0 {
		return result
	}
	threadMap, err := sb.threadDB.QueryActiveByGroupShortIDs(refs)
	if err != nil {
		sb.Warn("sidebar sync: thread last_message_at query failed", zap.Error(err))
		return result
	}
	// QueryActiveByGroupShortIDs 已经按 "{groupNo}____{shortID}" 做键，直接转写。
	for key, lite := range threadMap {
		result[key] = lite.LastMessageAt
	}
	return result
}

// ---------------------------------------------------------------------------
// Filter helpers
// ---------------------------------------------------------------------------

// threeDaysAgo returns the unix timestamp 72h ago.
func threeDaysAgo() int64 { return time.Now().Add(-72 * time.Hour).Unix() }

// channelKey returns a string key for (channelID, channelType).
func channelKey(channelID string, channelType uint8) string {
	return fmt.Sprintf("%s-%d", channelID, channelType)
}

// extractGroupNos collects group channel IDs from IM conversations.
func extractGroupNos(convs []*config.SyncUserConversationResp) []string {
	groupNos := make([]string, 0, len(convs))
	for _, c := range convs {
		if c.ChannelType == common.ChannelTypeGroup.Uint8() {
			groupNos = append(groupNos, c.ChannelID)
		}
	}
	return groupNos
}

// parseThreadChannelIDSidebar splits "{groupNo}____{shortID}" → (groupNo, shortID).
// Uses the 4-underscore separator convention matching the thread package.
const threadSeparator = "____"

func parseThreadChannelIDSidebar(channelID string) (groupNo, shortID string, err error) {
	idx := strings.Index(channelID, threadSeparator)
	if idx <= 0 || idx+len(threadSeparator) >= len(channelID) {
		return "", "", fmt.Errorf("invalid thread channel id: %q", channelID)
	}
	return channelID[:idx], channelID[idx+len(threadSeparator):], nil
}

// buildFollowItems constructs the SidebarItem list for the follow tab from
// the IM conversation list.
// Rules:
//   - Group: must have a category_id entry AND not be in unfollowedGroups.
//   - DM:    must have a followedDMs entry with followed_dm=1.
//   - Thread: parent group must be in the follow set AND the thread must have
//     an ext row in threadExtMap.
func buildFollowItems(
	convs []*config.SyncUserConversationResp,
	categorySetting map[string]*GroupCategorySetting,
	unfollowedGroups map[string]struct{},
	followedDMs map[string]*convext.Model,
	threadExtMap map[string]*convext.Model,
) []*SidebarItem {
	items := make([]*SidebarItem, 0, len(convs))
	for _, conv := range convs {
		switch conv.ChannelType {
		case common.ChannelTypeGroup.Uint8():
			cs, ok := categorySetting[conv.ChannelID]
			if !ok || cs.CategoryID == nil {
				continue // no category → not in follow set
			}
			if _, unfollowed := unfollowedGroups[conv.ChannelID]; unfollowed {
				continue
			}
			items = append(items, &SidebarItem{
				TargetType:   int(common.ChannelTypeGroup),
				TargetID:     conv.ChannelID,
				ChannelType:  conv.ChannelType,
				ChannelID:    conv.ChannelID,
				Timestamp:    conv.Timestamp,
				Unread:       conv.Unread,
				IsFollowed:   true,
				CategoryID:   cs.CategoryID,
				CategorySort: cs.CategorySort,
			})

		case common.ChannelTypePerson.Uint8():
			ext, ok := followedDMs[conv.ChannelID]
			if !ok {
				continue
			}
			item := &SidebarItem{
				TargetType:  int(common.ChannelTypePerson),
				TargetID:    conv.ChannelID,
				ChannelType: conv.ChannelType,
				ChannelID:   conv.ChannelID,
				Timestamp:   conv.Timestamp,
				Unread:      conv.Unread,
				IsFollowed:  true,
				FollowSort:  ext.FollowSort,
			}
			if ext.DMCategoryID != nil {
				catIDStr := fmt.Sprintf("%d", *ext.DMCategoryID)
				item.CategoryID = &catIDStr
			}
			items = append(items, item)

		case common.ChannelTypeCommunityTopic.Uint8():
			// Thread must have an ext row
			extRow, hasExt := threadExtMap[conv.ChannelID]
			if !hasExt {
				continue
			}
			// Parent group must be in follow set
			groupNo, _, err := parseThreadChannelIDSidebar(conv.ChannelID)
			if err != nil {
				continue
			}
			cs, ok := categorySetting[groupNo]
			if !ok || cs.CategoryID == nil {
				continue
			}
			if _, unfollowed := unfollowedGroups[groupNo]; unfollowed {
				continue
			}
			items = append(items, &SidebarItem{
				TargetType:      int(common.ChannelTypeCommunityTopic),
				TargetID:        conv.ChannelID,
				ChannelType:     conv.ChannelType,
				ChannelID:       conv.ChannelID,
				Timestamp:       conv.Timestamp,
				Unread:          conv.Unread,
				IsFollowed:      true,
				FollowSort:      extRow.FollowSort,
				ParentChannelID: groupNo,
			})
		}
	}
	return items
}

// buildRecentItems constructs the SidebarItem list for the recent tab.
// Rules:
//   - DM: always included (no time window).
//   - Group / Thread: only included if timestamp > threeDaysAgo().
//   - The returned slice is not yet sorted.
//
// Intentional non-rule (PR review Important #6): unfollowed groups
// (group_unfollowed=1 in user_conversation_ext) are NOT filtered out here.
// Per PM decision on issue #337 — "取消关注就是移除关注列表，放到最近 tab" —
// an unfollowed group still belongs in the recent tab as long as it has
// activity within the 72 h window.  The unfollow blacklist only affects
// the follow tab.
func buildRecentItems(
	convs []*config.SyncUserConversationResp,
	pinnedSet map[string]struct{},
) []*SidebarItem {
	cutoff := threeDaysAgo()
	items := make([]*SidebarItem, 0, len(convs))
	for _, conv := range convs {
		isDM := conv.ChannelType == common.ChannelTypePerson.Uint8()
		if !isDM && conv.Timestamp <= cutoff {
			continue
		}
		pinned := false
		if pinnedSet != nil {
			_, pinned = pinnedSet[channelKey(conv.ChannelID, conv.ChannelType)]
		}
		parentID := ""
		if conv.ChannelType == common.ChannelTypeCommunityTopic.Uint8() {
			groupNo, _, err := parseThreadChannelIDSidebar(conv.ChannelID)
			if err == nil {
				parentID = groupNo
			}
		}
		items = append(items, &SidebarItem{
			TargetType:      int(conv.ChannelType),
			TargetID:        conv.ChannelID,
			ChannelType:     conv.ChannelType,
			ChannelID:       conv.ChannelID,
			Timestamp:       conv.Timestamp,
			Unread:          conv.Unread,
			IsPinned:        pinned,
			ParentChannelID: parentID,
		})
	}
	return items
}

// mergeThreadEntries appends thread ext entries (from user_conversation_ext)
// that are NOT already present in the existing items slice.
// This covers threads that have an ext row but the IM core didn't return them
// as independent conversation entries.
//
// PR review (Round 3) Blocking #4 — DB-only thread entries must apply the same
// parent-follow predicate as IM-returned threads (see buildFollowItems thread
// branch): the parent group must have a non-nil category AND must not be in
// the unfollowed set. Without this filter, threads whose parent group was
// unfollowed (or whose category was removed) would still surface in the follow
// tab, exposing stale state.
//
// PR review follow-up：ext 行存在但目标 thread 已被删除（cleanup 延迟 / 失败）的
// 情况，loadThreadLastMsgAt 不会把它放进 lastMsgAtMap。本函数据此 skip，
// 避免把 timestamp=0 的"幽灵 thread"emit 给客户端。
//
// Malformed thread channel IDs (no separator, empty parts) are skipped silently;
// they should never be persisted in the first place but defensive handling
// avoids appending entries with an empty ParentChannelID.
func mergeThreadEntries(
	existing []*SidebarItem,
	threadExtRows []*convext.Model,
	// lastMsgAtMap 的键是 ext.TargetID（"{groupNo}____{shortID}" 格式）。
	// 键存在表示 thread 仍活跃（status != deleted 且 group_no 匹配）。
	// 值为 nil 表示 thread 活跃但 last_message_at 还是 NULL（新建后未发消息）。
	lastMsgAtMap map[string]*time.Time,
	categorySetting map[string]*GroupCategorySetting,
	unfollowedGroups map[string]struct{},
) []*SidebarItem {
	if len(threadExtRows) == 0 {
		return existing
	}
	// Build a set of already-present thread target IDs.
	presentIDs := make(map[string]struct{}, len(existing))
	for _, it := range existing {
		if it.TargetType == int(common.ChannelTypeCommunityTopic) {
			presentIDs[it.TargetID] = struct{}{}
		}
	}

	result := existing
	for _, ext := range threadExtRows {
		if _, present := presentIDs[ext.TargetID]; present {
			continue
		}
		groupNo, _, err := parseThreadChannelIDSidebar(ext.TargetID)
		if err != nil {
			// Malformed ID — never expose to client.
			continue
		}
		// PR review follow-up：thread 必须仍活跃（在 lastMsgAtMap 中）。
		// 不存在意味着 thread 已删除 / 不存在 / 跨群错配。
		lastMsgAt, alive := lastMsgAtMap[ext.TargetID]
		if !alive {
			continue
		}
		// Apply parent-follow predicate (mirrors buildFollowItems thread branch).
		cs, ok := categorySetting[groupNo]
		if !ok || cs.CategoryID == nil {
			continue // parent group not in follow set
		}
		if _, unfollowed := unfollowedGroups[groupNo]; unfollowed {
			continue // parent group explicitly unfollowed
		}
		var ts int64
		if lastMsgAt != nil {
			ts = lastMsgAt.Unix()
		}
		result = append(result, &SidebarItem{
			TargetType:      int(common.ChannelTypeCommunityTopic),
			TargetID:        ext.TargetID,
			ChannelType:     common.ChannelTypeCommunityTopic.Uint8(),
			ChannelID:       ext.TargetID,
			Timestamp:       ts,
			IsFollowed:      true,
			FollowSort:      ext.FollowSort,
			ParentChannelID: groupNo,
		})
	}
	return result
}

// ---------------------------------------------------------------------------
// Sorting
// ---------------------------------------------------------------------------

// sortFollowItems sorts items for the follow tab:
// primary: category_sort ASC; secondary: pinned DESC; tertiary: follow_sort ASC.
func sortFollowItems(items []*SidebarItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.CategorySort != b.CategorySort {
			return a.CategorySort < b.CategorySort
		}
		if a.IsPinned != b.IsPinned {
			return a.IsPinned // pinned first
		}
		return a.FollowSort < b.FollowSort
	})
}

// sortRecentItems sorts items for the recent tab:
// primary: pinned DESC; secondary: timestamp DESC.
func sortRecentItems(items []*SidebarItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.IsPinned != b.IsPinned {
			return a.IsPinned
		}
		return a.Timestamp > b.Timestamp
	})
}
