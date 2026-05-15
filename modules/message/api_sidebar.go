// Package message — POST /v1/sidebar/sync
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
//     follow  – category_sort ASC → pinned DESC → follow_sort ASC →
//               intra-category sort ASC → target_id ASC (Issue #41 — sidebar
//               drag wins over category-management UI; pin overrides everything
//               within a category; see sortFollowItems for the full rationale).
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
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Structs
// ---------------------------------------------------------------------------

// Sidebar handles POST /v1/sidebar/sync.
type Sidebar struct {
	ctx             *config.Context
	groupCategoryDB *groupCategoryDB
	convExtDB       *convext.DB
	threadDB        *thread.DB
	followVersionDB *convext.FollowVersionDB
	// PR #21 review (Jerry-Xin Critical)：Space 过滤需要群表查询。
	groupService group.IService
	// groupDB 仅用于 thread ext 行的 Space 过滤（PR #21 Round-6 P0-2）查
	// external-group mapping。
	groupDB *group.DB
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
		groupService:    group.NewService(ctx),
		groupDB:         group.NewDB(ctx),
		Log:             log.NewTLog("Sidebar"),
	}
}

// sidebarSyncReq is the JSON body for POST /v1/sidebar/sync.
type sidebarSyncReq struct {
	Tab         string `json:"tab"`     // "follow" | "recent"
	Version     int64  `json:"version"` // IM core version cursor
	LastMsgSeqs string `json:"last_msg_seqs"`
	MsgCount    int64  `json:"msg_count"`
	DeviceUUID  string `json:"device_uuid"`
}

// SidebarItem is one entry in the sidebar response.
type SidebarItem struct {
	TargetType  int    `json:"target_type"` // 1 DM / 2 group / 5 thread
	TargetID    string `json:"target_id"`
	ChannelType uint8  `json:"channel_type"`
	ChannelID   string `json:"channel_id"`
	Timestamp   int64  `json:"timestamp"`
	Unread      int    `json:"unread"`
	IsPinned    bool   `json:"is_pinned"`
	IsFollowed  bool   `json:"is_followed"`
	CategoryID  *string `json:"category_id,omitempty"`
	// CategorySort 暴露给客户端的"类别之间排序权重"，来源是 group_category.sort
	// （PR #21 review by lml2468 blocker #3）。改类别顺序会 bump follow_version
	// 并改变这里返回的值，与 /category/sort 接口及 swagger 一致。
	CategorySort int `json:"category_sort,omitempty"`
	// intraCategorySort 是 group_setting.category_sort（类别内排序），仅在
	// 服务端 sortFollowItems 作为二级 key 使用，不暴露给客户端 —— 客户端
	// 拿到的 items 已经按服务端口径排序完毕。
	intraCategorySort int
	FollowSort        int    `json:"follow_sort,omitempty"`
	ParentChannelID   string `json:"parent_channel_id,omitempty"` // thread only
}

// sidebarSyncResp is the JSON response for POST /v1/sidebar/sync.
//
// Version 是 IM 会话游标（recent tab 的 cursor）。
// FollowVersion 是 user_follow_version 的当前值（follow tab 的 CAS / 增量检测）。
// PR review (Round 3) Blocking #1/#2 — IM 游标无法感知 follow 状态变化，所以需要
// 独立的 follow_version。客户端使用方式：
//   - 拉取 follow tab 后保存 follow_version，下次比较是否需要全量重建。
//   - 调 /v1/follow/sort 时把 follow_version 原样回传做 CAS。
type sidebarSyncResp struct {
	Items         []*SidebarItem `json:"items"`
	Version       int64          `json:"version"`
	FollowVersion int64          `json:"follow_version"`
}

// ---------------------------------------------------------------------------
// Route registration helper (called from Conversation.Route or standalone)
// ---------------------------------------------------------------------------

// RegisterSidebarRoutes mounts /v1/sidebar/sync onto the router.
func RegisterSidebarRoutes(r *wkhttp.WKHttp, ctx *config.Context) {
	sb := NewSidebar(ctx)
	uidLimit := appwkhttp.SharedUIDRateLimiter(ctx)
	grp := r.Group("/v1/sidebar", ctx.AuthMiddleware(r), uidLimit, spacepkg.SpaceMiddleware(ctx))
	{
		grp.POST("/sync", sb.Sync)
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

// Sync handles POST /v1/sidebar/sync.
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

	// 0. follow_version 必须先读再读数据（PR #21 review P1-1 by yujiawei）。
	//    如果先读数据再读 version，并发的 Follow*/Unfollow*/UpdateSort 在两次
	//    读取之间 bump 会让响应回 "V+1 + 数据V"——客户端缓存命中 V+1 不再触发
	//    刷新，新关注的 item 被静默吞掉。
	//    把读 version 放到第一步：任何并发 bump 都会让 follow_version < real_version，
	//    下一次 sync 看到更大的 version，客户端自然刷新，错过可恢复。
	//
	//    返 0 时客户端只会理解为 "没有新状态"，对业务无害（仍是保守正确）。
	var followVersion int64
	if v, err := sb.followVersionDB.Get(loginUID, spaceID); err != nil {
		sb.Warn("sidebar sync: follow_version query failed (non-fatal)", zap.Error(err))
	} else {
		followVersion = v
	}

	// 1. Fetch IM conversations (version=0, no device offset logic for v2).
	//
	//    rawConversations 必须独立保留 —— 它是 cursor 推进的唯一来源
	//    （见 step 6 + maxConversationVersion 注释）。Space 过滤后的列表只用于
	//    构建 items；如果用过滤后列表推 cursor，最高 version 的会话恰好属于另一个
	//    Space 时会被剔除，respVersion 退回 req.Version，客户端反复拉同一批 raw
	//    conversations 死循环（PR #21 Round-4 review B1 by Jerry-Xin / lml2468 / yujiawei）。
	rawConversations, err := sb.ctx.IMSyncUserConversation(loginUID, req.Version, req.MsgCount, req.LastMsgSeqs, nil)
	if err != nil {
		sb.Error("sidebar sync: IM fetch failed", zap.Error(err))
		c.ResponseError(errors.New("同步会话失败"))
		return
	}

	// 1b. Space 过滤（PR #21 review by Jerry-Xin Critical）：
	//     IM 核心返回的是用户在全局视野下的会话，必须用 X-Space-ID 收紧到
	//     当前请求 Space，否则 sidebar 会把别的 Space 的活跃 DM/Group/Thread
	//     暴露到当前 Space 的 follow/recent tab。规则与 v1 完全一致：
	//     - 群：按 group 表的 space_id；
	//     - 子区：跟父群的 space_id；
	//     - DM：默认 Space 兜底 + Bot 成员关系 + payload.space_id 匹配；
	//     - 系统 Bot：补齐占位，所有 Space 可见。
	//
	//    conversations 是过滤 + 系统 Bot 补齐后的"可见列表"，专门给后续 build/sort
	//    使用；cursor 不取自这里（见 step 6）。
	conversations := rawConversations
	if spaceID != "" {
		conversations = FilterRawConversationsBySpace(rawConversations, spaceID, loginUID, sb.ctx, sb.groupService)
		conversations = EnsureSystemBotsPresentRaw(conversations)
	}

	// 2. Load ancillary data
	//
	// PR #21 Round-4 review B2 (Jerry-Xin / yujiawei Important #1)：
	// follow tab 的语义是 "服务器对客户端宣告完整 follow 列表"，伴随 follow_version
	// 作为 fresh 标记。任何"影响 follow tab 内容"的查询失败如果被吞成 warn + 部分
	// 列表，客户端会把 partial 当作 fresh 缓存进而长期看不到正确数据 —— 这是
	// privacy/正确性回归。所以 follow tab 路径下，下列 4 个查询的失败一律 fail-closed
	// 返回 500，让客户端短时退避后重试；recent tab 不依赖这些数据，仍 fail-open。
	//
	// 已 fail-closed 的列表：categorySetting / unfollowedGroups / followedDMs /
	// threadExtRows / loadThreadLastMsgAt。
	isFollowTab := req.Tab == "follow"
	failClosedForFollow := func(stage string, err error) bool {
		if err == nil {
			return false
		}
		if isFollowTab {
			sb.Error("sidebar sync: "+stage+" failed (follow tab fail-closed)", zap.Error(err))
			c.ResponseError(errors.New("同步会话失败"))
			return true
		}
		sb.Warn("sidebar sync: "+stage+" failed (recent tab non-fatal)", zap.Error(err))
		return false
	}

	// 2a. Thread ext rows (for follow tab thread entries) —— 必须先于 2b（category
	//     settings 查询），因为 DB-only thread 的父群可能没出现在 IM 返回里，需要把
	//     父群 groupNo 合入 categorySetting 查询，否则 mergeThreadEntries 会因 parent
	//     category 缺失而 skip 该 thread（PR #21 Round-4 review I6 / lml2468 #3）。
	threadExtRows := []*convext.Model{}
	if isFollowTab {
		rows, err := sb.convExtDB.ListThreadExts(loginUID, spaceID)
		if failClosedForFollow("thread ext query", err) {
			return
		}
		threadExtRows = rows
	}
	// 2a.5. Space-filter thread ext rows by parent group's space_id
	//       (PR #21 Round-6 P0-2 by Jerry-Xin / yujiawei).
	//       FollowThread 的旧版鉴权可能让 ext 行的 (uid, space_id) 与父群真实 space
	//       不一致；categorySetting 是按 (uid, group_no) 查的、不带 space 谓词，
	//       所以仅靠 categorySetting 不能挡住跨 Space thread。这一步显式按父群
	//       space_id 过滤，与 v2 sidebar group 过滤保持同口径。
	//       fail-closed：群表查询失败 → follow tab 整体退避。
	if isFollowTab && spaceID != "" && len(threadExtRows) > 0 {
		var err error
		threadExtRows, err = sb.filterThreadExtsBySpace(threadExtRows, spaceID, loginUID)
		if failClosedForFollow("thread ext space filter", err) {
			return
		}
	}
	threadExtMap := make(map[string]*convext.Model, len(threadExtRows))
	for _, m := range threadExtRows {
		threadExtMap[m.TargetID] = m
	}

	// 2b. Group category settings
	//     groupNos = IM 返回的 group 频道 ∪ thread ext 行的父群（去重）。
	groupNos := extractGroupNos(conversations)
	if isFollowTab && len(threadExtRows) > 0 {
		groupNos = appendThreadParentGroupNos(groupNos, threadExtRows)
	}
	categorySetting := map[string]*GroupCategorySetting{}
	if len(groupNos) > 0 {
		settings, err := sb.groupCategoryDB.QueryCategorySettingsByGroupNos(groupNos, loginUID)
		if failClosedForFollow("category query", err) {
			return
		}
		for _, s := range settings {
			categorySetting[s.GroupNo] = s
		}
	}

	// 2c. Unfollowed groups
	unfollowedGroups := map[string]struct{}{}
	unfollowed, err := sb.convExtDB.ListUnfollowedGroups(loginUID, spaceID)
	if failClosedForFollow("unfollowed groups query", err) {
		return
	}
	for _, m := range unfollowed {
		unfollowedGroups[m.TargetID] = struct{}{}
	}

	// 2d. Followed DMs
	followedDMs := map[string]*convext.Model{}
	dms, err := sb.convExtDB.ListFollowedDM(loginUID, spaceID)
	if failClosedForFollow("followed DM query", err) {
		return
	}
	for _, m := range dms {
		followedDMs[m.TargetID] = m
	}

	// 2d2. Group ext rows (Issue #41 fix #1)：读取群的 user_conversation_ext，
	//      把 follow_sort 喂给 buildFollowItems 群分支。recent tab 不依赖 follow_sort，
	//      仍走 fail-open。
	groupExts := map[string]*convext.Model{}
	if isFollowTab {
		rows, err := sb.convExtDB.ListGroupExts(loginUID, spaceID)
		if failClosedForFollow("group ext query", err) {
			return
		}
		for _, m := range rows {
			groupExts[m.TargetID] = m
		}
	}

	// 2d3. DM category sorts (Issue #41 fix #2)：DM 引用的 dm_category_id 对应
	//      group_category.sort 必须显式 JOIN 出来，才能让带 category 的 DM 与同
	//      category 的群进入同一排序桶。
	dmCategorySorts := map[string]int{}
	if isFollowTab && len(followedDMs) > 0 {
		ids := make([]string, 0, len(followedDMs))
		seen := make(map[string]struct{}, len(followedDMs))
		for _, m := range followedDMs {
			if m.DMCategoryID == nil {
				continue
			}
			if _, dup := seen[*m.DMCategoryID]; dup {
				continue
			}
			seen[*m.DMCategoryID] = struct{}{}
			ids = append(ids, *m.DMCategoryID)
		}
		if len(ids) > 0 {
			sorts, err := sb.groupCategoryDB.QueryCategorySortsByIDs(ids, loginUID)
			if failClosedForFollow("dm category sort query", err) {
				return
			}
			dmCategorySorts = sorts
		}
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
		items = buildFollowItems(conversations, categorySetting, unfollowedGroups, followedDMs, threadExtMap, groupExts, dmCategorySorts)
		// Append standalone thread ext entries not present in IM result.
		// Pass categorySetting + unfollowedGroups so parent-follow filter applies
		// to DB-only thread entries as well (PR review Round-3 Blocking #4).
		lastMsgAtMap, err := sb.loadThreadLastMsgAt(threadExtRows)
		if failClosedForFollow("thread last_message_at query", err) {
			return
		}
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

	// 6. Compute response version from raw conversations.
	//    必须用 rawConversations（过滤前）—— Space filter 把 max-version 会话
	//    剔除时 respVersion 仍要前进，否则客户端死循环（PR #21 Round-4 review B1）。
	//    follow_version 已在 step 0 读取（PR #21 review P1-1）。
	respVersion := maxConversationVersion(rawConversations, req.Version)

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
//
// PR #21 Round-4 review B2：返回 error 让调用方决定 fail-closed 还是 fail-open。
// 在 follow tab 路径下吞掉错误并返回空 map 会让 mergeThreadEntries 把所有 DB-only
// thread 当 "不活跃" skip，客户端 follow tab 残缺但仍带 follow_version，看上去 fresh —
// 客户端长期缓存错误结果。所以接口契约改为：错误必须显式上报。
func (sb *Sidebar) loadThreadLastMsgAt(extRows []*convext.Model) (map[string]*time.Time, error) {
	result := make(map[string]*time.Time, len(extRows))
	if len(extRows) == 0 {
		return result, nil
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
		return result, nil
	}
	threadMap, err := sb.threadDB.QueryActiveByGroupShortIDs(refs)
	if err != nil {
		return nil, fmt.Errorf("sidebar load thread last_message_at: %w", err)
	}
	// QueryActiveByGroupShortIDs 已经按 "{groupNo}____{shortID}" 做键，直接转写。
	for key, lite := range threadMap {
		result[key] = lite.LastMessageAt
	}
	return result, nil
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

// filterThreadExtsBySpace 按父群的 space_id 过滤 thread ext 行（v2 sidebar 专用）。
//
// PR #21 Round-6 P0-2：FollowThread 的旧鉴权允许在 X-Space-ID=B 下写 Space A 的群
// 对应的 thread ext 行；categorySetting 又是按 (uid, group_no) 取的，不带 space 谓词，
// 单靠 categorySetting + unfollowedGroups 挡不住跨 Space thread。这里在 sidebar 渲染
// 前显式按父群 space_id 过滤，规则与 FilterRawConversationsBySpace 的 group 分支一致：
//   - 内部群：parent.space_id == spaceID 才保留
//   - 外部群：当前 user 作为外部成员加入该群且 sourceSpaceID == spaceID 才保留
//   - 旧群 (parent.space_id == "")：保留（沿用历史"所有 Space 可见"语义）
//
// fail-closed：群表查询失败时返回 error，调用方 follow tab 整体退避，避免半结果泄露。
func (sb *Sidebar) filterThreadExtsBySpace(rows []*convext.Model, spaceID, loginUID string) ([]*convext.Model, error) {
	parentNos := uniqueThreadParentGroupNos(rows)
	if len(parentNos) == 0 {
		return rows, nil
	}
	groupInfos, err := sb.groupService.GetGroups(parentNos)
	if err != nil {
		return nil, fmt.Errorf("filter thread ext by space: get groups: %w", err)
	}
	parentSpaceMap := make(map[string]string, len(groupInfos))
	for _, g := range groupInfos {
		parentSpaceMap[g.GroupNo] = g.SpaceID
	}
	externalMap, err := sb.groupDB.QueryExternalGroupNosForUser(loginUID)
	if err != nil {
		return nil, fmt.Errorf("filter thread ext by space: external group map: %w", err)
	}

	kept := make([]*convext.Model, 0, len(rows))
	for _, ext := range rows {
		parentNo, _, perr := parseThreadChannelIDSidebar(ext.TargetID)
		if perr != nil {
			continue // malformed; drop silently
		}
		parentSpaceID, known := parentSpaceMap[parentNo]
		if !known {
			// Parent group disappeared from group table (disbanded mid-flight).
			// Drop fail-closed; cleanup hooks will eventually remove the ext row.
			continue
		}
		if parentSpaceID == "" {
			// Legacy group without space_id: keep (mirrors v1 visibility).
			kept = append(kept, ext)
			continue
		}
		if parentSpaceID == spaceID {
			kept = append(kept, ext)
			continue
		}
		if sourceSpace, ok := externalMap[parentNo]; ok && sourceSpace == spaceID {
			kept = append(kept, ext)
		}
	}
	return kept, nil
}

// uniqueThreadParentGroupNos collects distinct parent groupNos from a slice of
// thread ext rows. Malformed thread target IDs are skipped.
func uniqueThreadParentGroupNos(rows []*convext.Model) []string {
	seen := make(map[string]struct{}, len(rows))
	parents := make([]string, 0, len(rows))
	for _, m := range rows {
		parent, _, err := parseThreadChannelIDSidebar(m.TargetID)
		if err != nil {
			continue
		}
		if _, dup := seen[parent]; dup {
			continue
		}
		seen[parent] = struct{}{}
		parents = append(parents, parent)
	}
	return parents
}

// appendThreadParentGroupNos 把 thread ext 行的父群 groupNo 去重后追加到
// groupNos 切片中。
//
// PR #21 Round-4 review I6 (lml2468 #3)：DB-only thread（父群最近没新消息、
// 没出现在 IM 返回里）的父群 category 必须一起加载，否则 mergeThreadEntries 在
// parent-follow predicate 处把它 skip，用户 follow 的子区会从 follow tab 消失。
// 这里只追加 groupNo，调用方负责走 QueryCategorySettingsByGroupNos 拉真实数据。
func appendThreadParentGroupNos(groupNos []string, threadExtRows []*convext.Model) []string {
	seen := make(map[string]struct{}, len(groupNos)+len(threadExtRows))
	for _, g := range groupNos {
		seen[g] = struct{}{}
	}
	for _, m := range threadExtRows {
		parentNo, _, err := parseThreadChannelIDSidebar(m.TargetID)
		if err != nil {
			continue
		}
		if _, ok := seen[parentNo]; ok {
			continue
		}
		seen[parentNo] = struct{}{}
		groupNos = append(groupNos, parentNo)
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
//
// Issue #41：
//   - groupExts 提供群的 user_conversation_ext 行（key = TargetID = groupNo），
//     用于把 follow_sort 写到群 SidebarItem 上。旧实现完全忽略该字段，导致 sidebar
//     拖拽群条目后下次 sync 返回顺序不变。无 ext 行的群按 0 处理。
//   - dmCategorySorts 把 DM 关联的 group_category.sort 提供给 DM 排序键，让带
//     category 的 DM 与同 category 群落到同一桶。
func buildFollowItems(
	convs []*config.SyncUserConversationResp,
	categorySetting map[string]*GroupCategorySetting,
	unfollowedGroups map[string]struct{},
	followedDMs map[string]*convext.Model,
	threadExtMap map[string]*convext.Model,
	groupExts map[string]*convext.Model,
	dmCategorySorts map[string]int,
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
			// Issue #41 fix #1：读取群的 follow_sort；ext 行可能不存在（用户从未拖拽），
			// map 缺失视为 0。
			var groupFollowSort int
			if ext, has := groupExts[conv.ChannelID]; has && ext != nil {
				groupFollowSort = ext.FollowSort
			}
			items = append(items, &SidebarItem{
				TargetType:        int(common.ChannelTypeGroup),
				TargetID:          conv.ChannelID,
				ChannelType:       conv.ChannelType,
				ChannelID:         conv.ChannelID,
				Timestamp:         conv.Timestamp,
				Unread:            conv.Unread,
				IsFollowed:        true,
				CategoryID:        cs.CategoryID,
				CategorySort:      cs.CategoryGroupSort,
				intraCategorySort: cs.CategorySort,
				FollowSort:        groupFollowSort,
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
			// PR #21 Round-6：DMCategoryID 现在已经是 VARCHAR(32) UUID（与 group_category
			// 共用 namespace），直接透传给客户端即可。
			// Issue #41 fix #2：DM 关联了 category 时必须从 group_category.sort 读取
			// CategorySort，旧实现仅 copy CategoryID 而 CategorySort=0，导致带 category
			// 的 DM 永远排在"无 category"桶里。
			if ext.DMCategoryID != nil {
				item.CategoryID = ext.DMCategoryID
				if cs, has := dmCategorySorts[*ext.DMCategoryID]; has {
					item.CategorySort = cs
				}
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
			// PR #21 Round-6 P1-2 (yujiawei) + 原型确认：thread 必须继承父群的
			// CategorySort + intraCategorySort，让 thread 和父群在同一分类块内排序，
			// 而不是默认全部落到 CategorySort=0 桶（与 DM 混在 follow tab 顶部）。
			items = append(items, &SidebarItem{
				TargetType:        int(common.ChannelTypeCommunityTopic),
				TargetID:          conv.ChannelID,
				ChannelType:       conv.ChannelType,
				ChannelID:         conv.ChannelID,
				Timestamp:         conv.Timestamp,
				Unread:            conv.Unread,
				IsFollowed:        true,
				FollowSort:        extRow.FollowSort,
				ParentChannelID:   groupNo,
				CategoryID:        cs.CategoryID,
				CategorySort:      cs.CategoryGroupSort,
				intraCategorySort: cs.CategorySort,
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
		// 与 buildFollowItems thread 分支一致：继承父群 CategorySort + intraCategorySort。
		result = append(result, &SidebarItem{
			TargetType:        int(common.ChannelTypeCommunityTopic),
			TargetID:          ext.TargetID,
			ChannelType:       common.ChannelTypeCommunityTopic.Uint8(),
			ChannelID:         ext.TargetID,
			Timestamp:         ts,
			IsFollowed:        true,
			FollowSort:        ext.FollowSort,
			ParentChannelID:   groupNo,
			CategoryID:        cs.CategoryID,
			CategorySort:      cs.CategoryGroupSort,
			intraCategorySort: cs.CategorySort,
		})
	}
	return result
}

// ---------------------------------------------------------------------------
// Sorting
// ---------------------------------------------------------------------------

// sortFollowItems sorts items for the follow tab:
//
//	T1: CategorySort       ASC  (group_category.sort —— 类别之间的顺序)
//	T2: IsPinned           DESC (pin overrides everything within a category)
//	T3: FollowSort         ASC  (user_conversation_ext.follow_sort, sidebar drag wins)
//	T4: intraCategorySort  ASC  (group_setting.category_sort —— category-mgmt UI 回退)
//	T5: TargetID           ASC  (deterministic tiebreaker)
//
// Issue #41：旧实现把 intraCategorySort 排在 FollowSort 之前，导致 sidebar 拖拽
// 排序被 category-management UI 设置过的同类内顺序覆盖；并且群分支根本没读
// follow_sort，使群恒以 0 排在 DM 前。
//
// 新顺序的语义：用户在 sidebar 没拖过任何条目时 FollowSort=0，所有条目按
// intraCategorySort（即 category-management UI 的顺序）展示；一旦拖动 sidebar，
// 被拖条目的 FollowSort 非 0 即胜过 intraCategorySort。两套 UI 都仍然生效，
// 且 sidebar 作为更直接的 UI 作为最终来源。
func sortFollowItems(items []*SidebarItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.CategorySort != b.CategorySort {
			return a.CategorySort < b.CategorySort
		}
		if a.IsPinned != b.IsPinned {
			return a.IsPinned // pinned first
		}
		if a.FollowSort != b.FollowSort {
			return a.FollowSort < b.FollowSort
		}
		if a.intraCategorySort != b.intraCategorySort {
			return a.intraCategorySort < b.intraCategorySort
		}
		return a.TargetID < b.TargetID
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
