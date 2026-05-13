// Package message — POST /v2/sidebar/sync
//
// # Data-flow overview
//
// 1. Validate request (tab ∈ {follow,recent}, device_uuid required).
// 2. Call ctx.IMSyncUserConversation to get the raw conversation list from the
//    IM core (timestamp, unread, last_msg_seq).
// 3. Load ancillary data in parallel-ish batches:
//      a. group_setting   → category_id, category_sort  (groupCategoryDB)
//      b. user_conversation_ext → unfollowed groups, followed DMs, thread ext rows
//      c. user_pinned_channel  → pinned set             (raw DB query via ctx.DB())
// 4. Apply tab-specific filtering:
//      follow  – groups with category + not unfollowed; followed DMs; threads
//                with ext row whose parent group is in the follow set.
//      recent  – all DMs; groups/threads with timestamp > now-72h.
// 5. Append standalone thread ext entries not already in the IM result.
// 6. Sort:
//      follow  – category_sort ASC, pinned DESC, follow_sort ASC.
//      recent  – pinned DESC, timestamp DESC.
// 7. Return SidebarSyncResp{Items, Version}.
//
// Design constraint: this file does NOT import modules/group, modules/user,
// or modules/thread to avoid circular dependencies.  It accesses the DB
// directly via ctx.DB() or through already-imported DB helpers in the
// message package.
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
	ctx            *config.Context
	groupCategoryDB *groupCategoryDB
	convExtDB      *convext.DB
	threadDB       *thread.DB
	log.Log
}

// NewSidebar creates a Sidebar handler.
func NewSidebar(ctx *config.Context) *Sidebar {
	return &Sidebar{
		ctx:            ctx,
		groupCategoryDB: newGroupCategoryDB(ctx),
		convExtDB:      convext.NewDB(ctx),
		threadDB:       thread.NewDB(ctx),
		Log:            log.NewTLog("Sidebar"),
	}
}

// sidebarSyncReq is the JSON body for POST /v2/sidebar/sync.
type sidebarSyncReq struct {
	Tab         string `json:"tab"`          // "follow" | "recent"
	Version     int64  `json:"version"`      // IM core version cursor
	LastMsgSeqs string `json:"last_msg_seqs"`
	MsgCount    int64  `json:"msg_count"`
	DeviceUUID  string `json:"device_uuid"`
}

// SidebarItem is one entry in the sidebar response.
type SidebarItem struct {
	TargetType      int     `json:"target_type"`                // 1 DM / 2 group / 5 thread
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
type sidebarSyncResp struct {
	Items   []*SidebarItem `json:"items"`
	Version int64          `json:"version"`
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

// validateSidebarRequest validates the request fields.
func validateSidebarRequest(req *sidebarSyncReq) error {
	if req.Tab != "follow" && req.Tab != "recent" {
		return errors.New("tab must be 'follow' or 'recent'")
	}
	if strings.TrimSpace(req.DeviceUUID) == "" {
		return errors.New("device_uuid is required")
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
		// Append standalone thread ext entries not present in IM result
		lastMsgAtMap := sb.loadThreadLastMsgAt(threadExtRows)
		items = mergeThreadEntries(items, threadExtRows, lastMsgAtMap)
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

	c.JSON(http.StatusOK, &sidebarSyncResp{
		Items:   items,
		Version: respVersion,
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
func (sb *Sidebar) loadThreadLastMsgAt(extRows []*convext.Model) map[string]*time.Time {
	result := make(map[string]*time.Time, len(extRows))
	if len(extRows) == 0 {
		return result
	}
	shortIDs := make([]string, 0, len(extRows))
	for _, m := range extRows {
		_, sid, err := parseThreadChannelIDSidebar(m.TargetID)
		if err == nil {
			shortIDs = append(shortIDs, sid)
		}
	}
	if len(shortIDs) == 0 {
		return result
	}
	threadMap, err := sb.threadDB.QueryByShortIDs(shortIDs)
	if err != nil {
		sb.Warn("sidebar sync: thread last_message_at query failed", zap.Error(err))
		return result
	}
	for _, m := range extRows {
		_, sid, err := parseThreadChannelIDSidebar(m.TargetID)
		if err != nil {
			continue
		}
		if tm, ok := threadMap[sid]; ok {
			result[sid] = tm.LastMessageAt
		}
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
				TargetType:  int(common.ChannelTypeGroup),
				TargetID:    conv.ChannelID,
				ChannelType: conv.ChannelType,
				ChannelID:   conv.ChannelID,
				Timestamp:   conv.Timestamp,
				Unread:      conv.Unread,
				IsFollowed:  true,
				CategoryID:  cs.CategoryID,
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
func mergeThreadEntries(
	existing []*SidebarItem,
	threadExtRows []*convext.Model,
	lastMsgAtMap map[string]*time.Time, // shortID → *time.Time
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
		// Compute timestamp from lastMsgAt map
		var ts int64
		groupNo, shortID, err := parseThreadChannelIDSidebar(ext.TargetID)
		if err == nil {
			if t, ok := lastMsgAtMap[shortID]; ok && t != nil {
				ts = t.Unix()
			}
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
