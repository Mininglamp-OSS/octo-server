package user

import (
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	pkgerrors "github.com/pkg/errors"
	"go.uber.org/zap"
)

// 每个用户在每个 Space 下最多可置顶的频道数。
const pinnedMaxPerSpace = 7

// GroupMemberCheckFunc 检查用户是否为群成员的函数类型
type GroupMemberCheckFunc func(groupNo string, uid string) (bool, error)

var (
	groupMemberCheckerMu sync.RWMutex
	groupMemberChecker   GroupMemberCheckFunc
)

// RegisterGroupMemberChecker 注册群成员检查函数（供 group 模块调用）
func RegisterGroupMemberChecker(fn GroupMemberCheckFunc) {
	setGroupMemberChecker(fn)
}

func setGroupMemberChecker(fn GroupMemberCheckFunc) {
	groupMemberCheckerMu.Lock()
	groupMemberChecker = fn
	groupMemberCheckerMu.Unlock()
}

func getGroupMemberChecker() GroupMemberCheckFunc {
	groupMemberCheckerMu.RLock()
	defer groupMemberCheckerMu.RUnlock()
	return groupMemberChecker
}

// Pinned 置顶频道 API
type Pinned struct {
	db       *PinnedDB
	friendDB *friendDB
	log.Log
}

// NewPinned 创建 Pinned API
func NewPinned(db *PinnedDB, friendDB *friendDB) *Pinned {
	return &Pinned{
		db:       db,
		friendDB: friendDB,
		Log:      log.NewTLog("Pinned"),
	}
}

// 合法的频道类型白名单
var validChannelTypes = map[uint8]bool{
	common.ChannelTypePerson.Uint8():         true, // 1 私聊
	common.ChannelTypeGroup.Uint8():          true, // 2 群
	common.ChannelTypeCommunityTopic.Uint8(): true, // 5 子区
}

// addPinnedReq 添加置顶请求
type addPinnedReq struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
}

// Add 添加置顶频道
// POST /v1/user/pinned
func (p *Pinned) Add(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := spacepkg.GetSpaceID(c)
	if spaceID == "" {
		c.ResponseError(pkgerrors.New("space_id 不能为空"))
		return
	}

	var req addPinnedReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(pkgerrors.New("参数错误"))
		return
	}

	if req.ChannelID == "" {
		c.ResponseError(pkgerrors.New("channel_id 不能为空"))
		return
	}

	if !validChannelTypes[req.ChannelType] {
		c.ResponseError(pkgerrors.New("无效的频道类型"))
		return
	}

	// 校验用户是否有权访问该频道
	if err := p.validateChannelAccess(loginUID, req.ChannelID, req.ChannelType); err != nil {
		c.ResponseError(err)
		return
	}

	err := p.db.Add(loginUID, spaceID, req.ChannelID, req.ChannelType, pinnedMaxPerSpace)
	if err != nil {
		if errors.Is(err, ErrPinnedAlreadyExists) {
			c.ResponseError(err)
			return
		}
		if errors.Is(err, ErrPinnedLimitExceeded) {
			c.ResponseError(pkgerrors.Errorf("最多只能置顶 %d 个频道", pinnedMaxPerSpace))
			return
		}
		p.Error("添加置顶失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("添加置顶失败"))
		return
	}

	c.ResponseOK()
}

// Remove 移除置顶频道
// DELETE /v1/user/pinned?channel_id=xxx&channel_type=2
func (p *Pinned) Remove(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := spacepkg.GetSpaceID(c)
	if spaceID == "" {
		c.ResponseError(pkgerrors.New("space_id 不能为空"))
		return
	}

	channelID := c.Query("channel_id")
	if channelID == "" {
		c.ResponseError(pkgerrors.New("channel_id 不能为空"))
		return
	}

	channelTypeStr := c.Query("channel_type")
	channelType, err := strconv.ParseUint(channelTypeStr, 10, 8)
	if err != nil {
		c.ResponseError(pkgerrors.New("channel_type 参数无效"))
		return
	}

	if !validChannelTypes[uint8(channelType)] {
		c.ResponseError(pkgerrors.New("无效的频道类型"))
		return
	}

	if err := p.db.Remove(loginUID, spaceID, channelID, uint8(channelType)); err != nil {
		p.Error("移除置顶失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("移除置顶失败"))
		return
	}

	c.ResponseOK()
}

// pinnedResp 置顶频道响应
type pinnedResp struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	SortOrder   int    `json:"sort_order"`
}

// List 获取置顶频道列表
// GET /v1/user/pinned
func (p *Pinned) List(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := spacepkg.GetSpaceID(c)
	if spaceID == "" {
		c.ResponseError(pkgerrors.New("space_id 不能为空"))
		return
	}

	list, err := p.db.List(loginUID, spaceID)
	if err != nil {
		p.Error("获取置顶列表失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("获取置顶列表失败"))
		return
	}

	resp := make([]pinnedResp, 0, len(list))
	for _, item := range list {
		resp = append(resp, pinnedResp{
			ChannelID:   item.ChannelID,
			ChannelType: item.ChannelType,
			SortOrder:   item.SortOrder,
		})
	}

	c.Response(resp)
}

// updatePinnedSortReq 更新排序请求
type updatePinnedSortReq struct {
	Items []PinnedSortItem `json:"items"`
}

// UpdateSort 更新置顶排序
// PUT /v1/user/pinned/sort
func (p *Pinned) UpdateSort(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := spacepkg.GetSpaceID(c)
	if spaceID == "" {
		c.ResponseError(pkgerrors.New("space_id 不能为空"))
		return
	}

	var req updatePinnedSortReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(pkgerrors.New("参数错误"))
		return
	}

	if len(req.Items) == 0 {
		c.ResponseError(pkgerrors.New("items 不能为空"))
		return
	}

	// 限制 items 数量
	if len(req.Items) > pinnedMaxPerSpace {
		c.ResponseError(pkgerrors.Errorf("items 数量不能超过 %d", pinnedMaxPerSpace))
		return
	}

	if err := p.db.UpdateSort(loginUID, spaceID, req.Items); err != nil {
		p.Error("更新排序失败", zap.Error(err))
		c.ResponseError(pkgerrors.New("更新排序失败"))
		return
	}

	c.ResponseOK()
}

// validateChannelAccess 校验用户是否有权访问该频道
func (p *Pinned) validateChannelAccess(uid, channelID string, channelType uint8) error {
	switch channelType {
	case common.ChannelTypePerson.Uint8(): // 私聊
		isFriend, err := p.friendDB.IsFriend(uid, channelID)
		if err != nil {
			p.Error("校验好友关系失败", zap.Error(err))
			return pkgerrors.New("校验频道权限失败")
		}
		if !isFriend {
			return pkgerrors.New("你和该用户不是好友")
		}
	case common.ChannelTypeGroup.Uint8(): // 群
		checker := getGroupMemberChecker()
		if checker == nil {
			p.Error("群成员检查函数未注册")
			return pkgerrors.New("系统配置错误")
		}
		isMember, err := checker(channelID, uid)
		if err != nil {
			p.Error("校验群成员失败", zap.Error(err))
			return pkgerrors.New("校验频道权限失败")
		}
		if !isMember {
			return pkgerrors.New("你不是该群的成员")
		}
	case common.ChannelTypeCommunityTopic.Uint8(): // 子区
		checker := getGroupMemberChecker()
		if checker == nil {
			p.Error("群成员检查函数未注册")
			return pkgerrors.New("系统配置错误")
		}
		// 子区 channel_id 格式：{group_no}____{short_id}
		parts := strings.SplitN(channelID, "____", 2)
		if len(parts) != 2 {
			return pkgerrors.New("无效的子区频道ID")
		}
		parentGroupNo := parts[0]
		isMember, err := checker(parentGroupNo, uid)
		if err != nil {
			p.Error("校验子区父群成员失败", zap.Error(err))
			return pkgerrors.New("校验频道权限失败")
		}
		if !isMember {
			return pkgerrors.New("你不是该子区所属群的成员")
		}
	}
	return nil
}
