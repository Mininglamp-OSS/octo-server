package botfather

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// syncMessages 同步频道历史消息
func (bf *BotFather) syncMessages(c *wkhttp.Context) {
	var req BotSyncMessagesReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		c.ResponseError(errors.New("channel_id不能为空"))
		return
	}
	if req.ChannelType == 0 {
		c.ResponseError(errors.New("channel_type不能为空"))
		return
	}
	if req.Limit <= 0 {
		req.Limit = 50
	}
	if req.Limit > 200 {
		req.Limit = 200
	}

	robotID := getRobotIDFromContext(c)

	// 群聊场景：验证 bot 是否在群内
	if req.ChannelType == common.ChannelTypeGroup.Uint8() {
		var count int
		_, err := bf.db.session.SelectBySql(
			"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
			req.ChannelID, robotID,
		).Load(&count)
		if err != nil {
			bf.Error("查询群成员失败", zap.Error(err))
			c.ResponseError(errors.New("查询群成员失败"))
			return
		}
		if count == 0 {
			c.ResponseError(errors.New("bot is not a member of this group"))
			return
		}
	}

	channelID := bf.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)
	syncReq := config.SyncChannelMessageReq{
		LoginUID:        robotID,
		ChannelID:       channelID,
		ChannelType:     req.ChannelType,
		StartMessageSeq: req.StartMessageSeq,
		EndMessageSeq:   req.EndMessageSeq,
		Limit:           req.Limit,
		PullMode:        config.PullMode(req.PullMode),
	}
	resp, err := bf.ctx.IMSyncChannelMessage(syncReq)
	if err != nil {
		bf.Error("同步消息失败", zap.Error(err))
		c.ResponseError(errors.New("同步消息失败"))
		return
	}

	c.Response(resp)
}

// getGroups 获取机器人所在的群组列表
func (bf *BotFather) getGroups(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		c.ResponseError(errors.New("robot_id not found"))
		return
	}

	type GroupInfo struct {
		GroupNo string `json:"group_no"`
		Name    string `json:"name"`
		SpaceID string `json:"space_id,omitempty"`
	}

	spaceID := c.Query("space_id")
	var groups []GroupInfo
	var err error
	if spaceID != "" {
		_, err = bf.ctx.DB().SelectBySql(
			"SELECT gm.group_no, g.name, g.space_id FROM group_member gm INNER JOIN `group` g ON gm.group_no = g.group_no WHERE gm.uid = ? AND gm.is_deleted = 0 AND g.space_id = ?",
			robotID, spaceID,
		).Load(&groups)
	} else {
		_, err = bf.ctx.DB().SelectBySql(
			"SELECT gm.group_no, g.name, g.space_id FROM group_member gm INNER JOIN `group` g ON gm.group_no = g.group_no WHERE gm.uid = ? AND gm.is_deleted = 0",
			robotID,
		).Load(&groups)
	}
	if err != nil {
		bf.Error("查询机器人群组失败", zap.Error(err))
		c.ResponseError(errors.New("查询群组失败"))
		return
	}

	c.JSON(http.StatusOK, groups)
}

// getGroupInfo 获取群信息
func (bf *BotFather) getGroupInfo(c *wkhttp.Context) {
	robotID := c.GetString("robot_id")
	groupNo := c.Param("group_no")

	// Verify bot is a member of this group
	var count int
	_, err := bf.db.session.SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, robotID).Load(&count)
	if err != nil || count == 0 {
		c.ResponseError(errors.New("bot is not a member of this group"))
		return
	}

	var group struct {
		GroupNo   string `db:"group_no"`
		Name      string `db:"name"`
		Notice    string `db:"notice"`
		Creator   string `db:"creator"`
		Status    int    `db:"status"`
		CreatedAt string `db:"created_at"`
	}
	_, err = bf.db.session.Select("group_no, name, IFNULL(notice,'') notice, IFNULL(creator,'') creator, status, created_at").
		From("`group`").Where("group_no=?", groupNo).Load(&group)
	if err != nil {
		c.ResponseError(errors.New("group not found"))
		return
	}

	c.Response(map[string]interface{}{
		"group_no":   group.GroupNo,
		"name":       group.Name,
		"notice":     group.Notice,
		"creator":    group.Creator,
		"status":     group.Status,
		"created_at": group.CreatedAt,
	})
}

// getGroupMembers 获取群成员列表
func (bf *BotFather) getGroupMembers(c *wkhttp.Context) {
	robotID := c.GetString("robot_id")
	groupNo := c.Param("group_no")

	// Verify bot is a member
	var count int
	_, err := bf.db.session.SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, robotID).Load(&count)
	if err != nil || count == 0 {
		c.ResponseError(errors.New("bot is not a member of this group"))
		return
	}

	type member struct {
		UID       string `db:"uid" json:"uid"`
		Name      string `db:"name" json:"name"`
		Role      int    `db:"role" json:"role"`
		Robot     int    `db:"robot" json:"robot"`
		CreatedAt string `db:"created_at" json:"created_at"`
	}

	var members []member
	_, err = bf.db.session.SelectBySql(`
		SELECT gm.uid, IFNULL(u.name,'') name, gm.role, IFNULL(u.robot,0) robot, gm.created_at 
		FROM group_member gm 
		LEFT JOIN user u ON gm.uid = u.uid 
		WHERE gm.group_no = ? AND gm.is_deleted = 0
		ORDER BY gm.role DESC, gm.created_at ASC
	`, groupNo).Load(&members)
	if err != nil {
		c.ResponseError(err)
		return
	}

	c.Response(members)
}

// getGroupMd returns GROUP.md content for a bot
func (bf *BotFather) getGroupMd(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		c.ResponseError(errors.New("robot_id not found"))
		return
	}
	groupNo := c.Param("group_no")

	// Verify bot is a group member
	isMember, err := bf.groupService.ExistMember(groupNo, robotID)
	if err != nil {
		bf.Error("check group membership failed", zap.Error(err))
		c.ResponseError(errors.New("check group membership failed"))
		return
	}
	if !isMember {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "bot is not a member of this group", "status": 403})
		return
	}

	result, err := bf.groupService.GetGroupMd(groupNo)
	if err != nil {
		bf.Error("query GROUP.md failed", zap.Error(err))
		c.ResponseError(errors.New("query GROUP.md failed"))
		return
	}
	if result == nil {
		c.JSON(http.StatusOK, gin.H{
			"content":    "",
			"version":    0,
			"updated_at": nil,
			"updated_by": "",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"content":    result.Content,
		"version":    result.Version,
		"updated_at": result.UpdatedAt,
		"updated_by": result.UpdatedBy,
	})
}

// updateGroupMd updates GROUP.md content by a bot
func (bf *BotFather) updateGroupMd(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		c.ResponseError(errors.New("robot_id not found"))
		return
	}
	groupNo := c.Param("group_no")

	// Verify bot is a group member
	isMember, err := bf.groupService.ExistMember(groupNo, robotID)
	if err != nil {
		bf.Error("check group membership failed", zap.Error(err))
		c.ResponseError(errors.New("check group membership failed"))
		return
	}
	if !isMember {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "bot is not a member of this group", "status": 403})
		return
	}

	// Verify bot_admin
	isBotAdmin, err := bf.groupService.IsBotAdmin(groupNo, robotID)
	if err != nil {
		bf.Error("check bot admin failed", zap.Error(err))
		c.ResponseError(errors.New("check bot admin failed"))
		return
	}
	if !isBotAdmin {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "bot is not a bot_admin in this group", "status": 403})
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}

	maxSize := group.GetGroupMdMaxSize()
	if len(req.Content) > maxSize {
		c.ResponseError(fmt.Errorf("GROUP.md content exceeds max size %d bytes", maxSize))
		return
	}

	newVersion, err := bf.groupService.UpdateGroupMd(groupNo, req.Content, robotID)
	if err != nil {
		bf.Error("update GROUP.md failed", zap.Error(err))
		c.ResponseError(errors.New("update GROUP.md failed"))
		return
	}

	// Async send notification
	go func() {
		defer func() {
			if r := recover(); r != nil {
				bf.Error("sendGroupMdNotification panic", zap.Any("recover", r))
			}
		}()
		bf.sendGroupMdNotification(groupNo, robotID, newVersion)
	}()

	c.JSON(http.StatusOK, gin.H{
		"version": newVersion,
	})
}

// sendGroupMdNotification sends GROUP.md event notification from bot
func (bf *BotFather) sendGroupMdNotification(groupNo string, updatedBy string, version int64) {
	botUIDs, err := bf.groupService.GetBotMemberUIDs(groupNo)
	if err != nil {
		bf.Error("query bot member UIDs failed", zap.Error(err))
		return
	}

	payload := map[string]interface{}{
		"type":    common.Text,
		"content": "GROUP.md updated",
		"event": map[string]interface{}{
			"type":       "group_md_updated",
			"version":    version,
			"updated_by": updatedBy,
		},
	}
	if len(botUIDs) > 0 {
		payload["mention"] = map[string]interface{}{
			"uids": botUIDs,
		}
	}

	err = bf.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		FromUID:     updatedBy,
		Payload:     []byte(util.ToJson(payload)),
	})
	if err != nil {
		bf.Error("send GROUP.md notification failed", zap.Error(err))
	}
}

// spaceUIDPattern matches space-prefixed UIDs: s{digits}_{baseUID}
var spaceUIDPattern = regexp.MustCompile(`^s\d+_(.+)$`)

// stripSpacePrefix extracts the base UID from a space-prefixed UID.
// "s14_abc123" → "abc123", "abc123" → "abc123" (unchanged)
func stripSpacePrefix(uid string) string {
	if m := spaceUIDPattern.FindStringSubmatch(uid); len(m) == 2 {
		return m[1]
	}
	return uid
}

// getUserInfo 查询用户基本信息 (GET /v1/bot/user/info?uid=xxx)
// Bot 通过 token 认证后，查询指定 UID 的用户 name 和 avatar。
// 用于 OpenClaw adapter DM 场景的 sender 名字解析。
func (bf *BotFather) getUserInfo(c *wkhttp.Context) {
	uid := strings.TrimSpace(c.Query("uid"))
	if uid == "" {
		c.ResponseError(errors.New("uid参数不能为空"))
		return
	}

	// Strip space prefix if present (WuKongIM adds s{spaceId}_ in WS layer,
	// but user table stores bare UIDs)
	bareUID := stripSpacePrefix(uid)

	userResp, err := bf.userService.GetUser(bareUID)
	if err != nil || userResp == nil {
		c.JSON(http.StatusNotFound, gin.H{"msg": "用户不存在"})
		return
	}

	cfg := bf.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}

	c.JSON(http.StatusOK, gin.H{
		"uid":    userResp.UID,
		"name":   userResp.Name,
		"avatar": fmt.Sprintf("%s/users/%s/avatar", apiURL, userResp.UID),
	})
}
