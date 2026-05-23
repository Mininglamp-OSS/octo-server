package conversation_ext

import (
	"errors"
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	pkgerrors "github.com/pkg/errors"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Interfaces — narrow surfaces tested via stubs in api_test.go
// ---------------------------------------------------------------------------

// followService is the subset of *Service that Follow handlers need.
type followService interface {
	FollowDM(uid, spaceID, peerUID string, categoryID *string) error
	UnfollowDM(uid, spaceID, peerUID string) error
	UnfollowChannel(uid, spaceID, groupNo string) error
	FollowChannel(uid, spaceID, groupNo string) error
	FollowThread(uid, spaceID, threadChannelID string) error
	UnfollowThread(uid, spaceID, threadChannelID string) error
	// AuthorizeAndMaterializeDefaultFollowedGroups is the UpdateSort pre-flight
	// step that gates client-supplied target_type=2 group IDs through
	// DefaultFollowedGroupGuard before any DB write (issue #151 code review #1).
	AuthorizeAndMaterializeDefaultFollowedGroups(uid, spaceID string, candidateGroupNos []string) error
}

// sortDB is the subset of *DB that the sort handler needs.
type sortDB interface {
	UpdateSort(uid, spaceID string, items []SortItem, expectedVersion int64) error
}

// validFollowTargetTypes is the white-list for target_type in sort items.
// 1 = DM, 2 = Group, 5 = Thread (CommunityTopic).
var validFollowTargetTypes = map[uint8]bool{
	targetTypeDM:     true,
	targetTypeGroup:  true,
	targetTypeThread: true,
}

// ---------------------------------------------------------------------------
// Follow — the HTTP handler struct
// ---------------------------------------------------------------------------

// Follow holds the 7 Follow/Unfollow API handlers.
type Follow struct {
	svc followService
	db  sortDB
	log.Log
}

// NewFollow creates a Follow API handler.
func NewFollow(svc followService, db sortDB) *Follow {
	return &Follow{
		svc: svc,
		db:  db,
		Log: log.NewTLog("Follow"),
	}
}

// ---------------------------------------------------------------------------
// Request / response types
// ---------------------------------------------------------------------------

type followDMReq struct {
	PeerUID string `json:"peer_uid"`
	// CategoryID 引用 group_category.category_id (VARCHAR(32) UUID)。
	// DM 与群共用 group_category 命名空间（PR #21 Round-6, 原型 image-v1.png）。
	// 不传或 null 表示未分类。
	CategoryID *string `json:"category_id"`
}

type unfollowChannelReq struct {
	GroupNo string `json:"group_no"`
}

type followThreadReq struct {
	ThreadChannelID string `json:"thread_channel_id"`
}

type sortItemReq struct {
	TargetType uint8  `json:"target_type"`
	TargetID   string `json:"target_id"`
	Sort       int    `json:"sort"`
}

type updateSortReq struct {
	Items []sortItemReq `json:"items"`
	// Version 是 CAS 锚。客户端把最近一次 sidebar 响应里的 follow_version
	// 原样回传，服务端与 user_follow_version 表（PR review Round-3
	// Blocking #1/#2）比对。
	Version int64 `json:"version"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// spaceGuard reads space_id from the context and returns an error response if
// it is empty. Returns ("", false) on error so the caller can return early.
func spaceGuard(c *wkhttp.Context) (spaceID string, ok bool) {
	spaceID = spacepkg.GetSpaceID(c)
	if spaceID == "" {
		c.ResponseError(pkgerrors.New("space_id 不能为空"))
		return "", false
	}
	return spaceID, true
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// FollowDM 关注 DM 并可选指定分组
// POST /v1/follow/dm
func (f *Follow) FollowDM(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID, ok := spaceGuard(c)
	if !ok {
		return
	}

	var req followDMReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(pkgerrors.New("参数错误"))
		return
	}
	if req.PeerUID == "" {
		c.ResponseError(pkgerrors.New("peer_uid 不能为空"))
		return
	}

	if err := f.svc.FollowDM(loginUID, spaceID, req.PeerUID, req.CategoryID); err != nil {
		// PR #21 Round-6：DMCategoryChecker 拒绝时把具体业务错暴露给客户端，
		// 不要吞成通用 "关注 DM 失败"，让客户端知道是 category 不存在 / 不属于自己。
		if errors.Is(err, ErrDMCategoryForbidden) {
			c.ResponseError(err)
			return
		}
		f.Error("关注 DM 失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("关注 DM 失败"))
		return
	}
	c.ResponseOK()
}

// UnfollowDM 取消关注 DM
// DELETE /v1/follow/dm?peer_uid=xxx
func (f *Follow) UnfollowDM(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID, ok := spaceGuard(c)
	if !ok {
		return
	}

	peerUID := c.Query("peer_uid")
	if peerUID == "" {
		c.ResponseError(pkgerrors.New("peer_uid 不能为空"))
		return
	}

	if err := f.svc.UnfollowDM(loginUID, spaceID, peerUID); err != nil {
		f.Error("取消关注 DM 失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("取消关注 DM 失败"))
		return
	}
	c.ResponseOK()
}

// UnfollowChannel 群"取消关注"（写黑名单）
// POST /v1/follow/channel/unfollow
func (f *Follow) UnfollowChannel(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID, ok := spaceGuard(c)
	if !ok {
		return
	}

	var req unfollowChannelReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(pkgerrors.New("参数错误"))
		return
	}
	if req.GroupNo == "" {
		c.ResponseError(pkgerrors.New("group_no 不能为空"))
		return
	}

	if err := f.svc.UnfollowChannel(loginUID, spaceID, req.GroupNo); err != nil {
		f.Error("取消关注群失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("取消关注群失败"))
		return
	}
	c.ResponseOK()
}

// FollowChannel 重新关注群（清黑名单）
// POST /v1/follow/channel/refollow
func (f *Follow) FollowChannel(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID, ok := spaceGuard(c)
	if !ok {
		return
	}

	var req unfollowChannelReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(pkgerrors.New("参数错误"))
		return
	}
	if req.GroupNo == "" {
		c.ResponseError(pkgerrors.New("group_no 不能为空"))
		return
	}

	if err := f.svc.FollowChannel(loginUID, spaceID, req.GroupNo); err != nil {
		// PR #123 round-1 review (Jerry-Xin / yujiawei P1)：鉴权失败返回 403，
		// 不向客户端泄露内部细节（仅写日志）。与 FollowThread 同样处理 ErrThreadForbidden。
		if errors.Is(err, ErrChannelForbidden) {
			f.Warn("关注群鉴权失败", zap.Error(err))
			c.ResponseErrorWithStatus(pkgerrors.New("无权关注该群"), http.StatusForbidden)
			return
		}
		f.Error("重新关注群失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("重新关注群失败"))
		return
	}
	c.ResponseOK()
}

// FollowThread 关注子区（隐式连带父群）
// POST /v1/follow/thread
func (f *Follow) FollowThread(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID, ok := spaceGuard(c)
	if !ok {
		return
	}

	var req followThreadReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(pkgerrors.New("参数错误"))
		return
	}
	if req.ThreadChannelID == "" {
		c.ResponseError(pkgerrors.New("thread_channel_id 不能为空"))
		return
	}

	if err := f.svc.FollowThread(loginUID, spaceID, req.ThreadChannelID); err != nil {
		// PR review (Round 3) Blocking #3：鉴权失败返回 403。
		// 不向客户端泄露内部细节，只写到日志（zap.Error）。
		if errors.Is(err, ErrThreadForbidden) {
			f.Warn("关注子区认证失败", zap.Error(err))
			c.ResponseErrorWithStatus(pkgerrors.New("无权关注该子区"), http.StatusForbidden)
			return
		}
		f.Error("关注子区失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("关注子区失败"))
		return
	}
	c.ResponseOK()
}

// UnfollowThread 取消关注子区
// DELETE /v1/follow/thread?thread_channel_id=xxx
func (f *Follow) UnfollowThread(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID, ok := spaceGuard(c)
	if !ok {
		return
	}

	threadChannelID := c.Query("thread_channel_id")
	if threadChannelID == "" {
		c.ResponseError(pkgerrors.New("thread_channel_id 不能为空"))
		return
	}

	if err := f.svc.UnfollowThread(loginUID, spaceID, threadChannelID); err != nil {
		f.Error("取消关注子区失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("取消关注子区失败"))
		return
	}
	c.ResponseOK()
}

// UpdateSort 关注 Tab 内手动排序 CAS
// PUT /v1/follow/sort
func (f *Follow) UpdateSort(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID, ok := spaceGuard(c)
	if !ok {
		return
	}

	var req updateSortReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(pkgerrors.New("参数错误"))
		return
	}

	if len(req.Items) == 0 {
		c.ResponseError(pkgerrors.New("items 不能为空"))
		return
	}
	// PR #21 Round-4 review I3 (yujiawei)：cap len、reject 空 target_id 与重复对，
	// 让客户端看到精确错误而非通用 "sort target not found"，也避免无效请求打到 DB。
	if len(req.Items) > maxUpdateSortItems {
		c.ResponseError(pkgerrors.Errorf("items 太多（max %d）", maxUpdateSortItems))
		return
	}

	// Validate each item's target_type against the white-list.
	items := make([]SortItem, 0, len(req.Items))
	seen := make(map[sortItemKey]struct{}, len(req.Items))
	for _, it := range req.Items {
		if !validFollowTargetTypes[it.TargetType] {
			c.ResponseError(pkgerrors.New("无效的 target_type，仅支持 1（DM）/ 2（群）/ 5（子区）"))
			return
		}
		if it.TargetID == "" {
			c.ResponseError(pkgerrors.New("items 中存在空的 target_id"))
			return
		}
		key := sortItemKey{TargetType: it.TargetType, TargetID: it.TargetID}
		if _, dup := seen[key]; dup {
			c.ResponseError(pkgerrors.Errorf("items 中存在重复项: (target_type=%d, target_id=%q)", it.TargetType, it.TargetID))
			return
		}
		seen[key] = struct{}{}
		items = append(items, SortItem{
			TargetType: it.TargetType,
			TargetID:   it.TargetID,
		})
	}

	// Issue #151 — pre-flight materialization for default-followed groups.
	// Collect target_type=2 candidates from the client payload, gate them
	// through DefaultFollowedGroupGuard (rejects any group the user has not
	// placed in a category — see service.go AuthorizeAndMaterializeDefault-
	// FollowedGroups), and INSERT IGNORE the survivors.  The downstream
	// db.UpdateSort then runs in strict mode: any client-injected fake group
	// not surviving the guard becomes a regular ErrSortTargetNotFound, which
	// the client must handle by re-fetching the follow list.
	var groupCandidates []string
	for _, it := range items {
		if it.TargetType == targetTypeGroup {
			groupCandidates = append(groupCandidates, it.TargetID)
		}
	}
	if len(groupCandidates) > 0 {
		if err := f.svc.AuthorizeAndMaterializeDefaultFollowedGroups(loginUID, spaceID, groupCandidates); err != nil {
			f.Error("default-followed group materialization failed", zap.Error(err))
			c.ResponseError(pkgerrors.New("更新排序失败"))
			return
		}
	}

	if err := f.db.UpdateSort(loginUID, spaceID, items, req.Version); err != nil {
		if errors.Is(err, ErrVersionConflict) {
			c.ResponseError(err)
			return
		}
		// PR #21 Round-4 review I5 (lml2468)：ErrSortTargetNotFound 是 swagger
		// 承诺的客户端可处理业务错误，必须区别于通用 DB 失败，让客户端走
		// "重拉关注列表后整体重试" 的恢复路径。
		if errors.Is(err, ErrSortTargetNotFound) {
			c.ResponseError(err)
			return
		}
		f.Error("更新排序失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("更新排序失败"))
		return
	}
	c.ResponseOK()
}

// maxUpdateSortItems caps the per-request items array length to keep tx lock
// scope bounded —— 与 maxMsgCount=1000、maxLastMsgSeqsLen=65536 在 sidebar 一侧
// 同一审美，500 个 follow 项已经覆盖产品上限场景。
const maxUpdateSortItems = 500

// sortItemKey 用作 dedup map 的复合键。
type sortItemKey struct {
	TargetType uint8
	TargetID   string
}

// (Follow.Route 已删除 —— PR #21 Round-6 P1 by yujiawei：原本是 no-op wrapper，
// 真正的 routing 在 1module.go followRouter 上实现，保留 no-op 反而误导后来的
// 维护者以为它会注册路由。)
