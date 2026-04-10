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
		c.ResponseError(errors.New("invalid request body"))
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
			bf.Error("failed to query group members", zap.Error(err))
			c.ResponseError(errors.New("failed to query group members"))
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
		OwnerUID  string `db:"owner_uid" json:"owner_uid,omitempty"`
		OwnerName string `db:"owner_name" json:"owner_name,omitempty"`
	}

	var members []member
	_, err = bf.db.session.SelectBySql(`
		SELECT gm.uid, IFNULL(u.name,'') name, gm.role, IFNULL(u.robot,0) robot, gm.created_at, IFNULL(r.creator_uid,'') AS owner_uid, IFNULL(u2.name,'') AS owner_name
		FROM group_member gm 
		LEFT JOIN user u ON gm.uid = u.uid 
		LEFT JOIN robot r ON gm.uid = r.robot_id AND r.status=1
		LEFT JOIN user u2 ON r.creator_uid = u2.uid
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

// resolveBotDisplayName 查询 Bot 的显示名，查不到时 fallback 到 robotID
func (bf *BotFather) resolveBotDisplayName(robotID string) string {
	botUser, err := bf.userDB.QueryByUID(robotID)
	if err == nil && botUser != nil && botUser.Name != "" {
		return botUser.Name
	}
	return robotID
}

// ========== Space Members API ==========

// botSpaceMembers 查询 Bot 所在 Space 的成员列表，支持按名称搜索
// GET /v1/bot/space/members?keyword=xxx&space_id=xxx&limit=50
func (bf *BotFather) botSpaceMembers(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		c.ResponseError(errors.New("robot_id not found"))
		return
	}

	keyword := strings.TrimSpace(c.Query("keyword"))
	spaceID := strings.TrimSpace(c.Query("space_id"))
	limitStr := c.Query("limit")
	limit := 50
	if l, err := fmt.Sscanf(limitStr, "%d", &limit); err != nil || l == 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	type MemberInfo struct {
		UID   string `json:"uid"`
		Name  string `json:"name"`
		Robot int    `json:"robot"`
	}

	var members []MemberInfo
	var err error

	if spaceID == "" {
		// 查找 bot 所在的所有 Space
		var spaceIDs []string
		_, err = bf.ctx.DB().SelectBySql(
			"SELECT space_id FROM space_member WHERE uid=? AND status=1", robotID,
		).Load(&spaceIDs)
		if err != nil || len(spaceIDs) == 0 {
			c.JSON(http.StatusOK, []MemberInfo{})
			return
		}
		spaceID = spaceIDs[0]
	} else {
		// 校验 bot 是否属于该 Space
		var count int
		bf.ctx.DB().SelectBySql(
			"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1", spaceID, robotID,
		).LoadOne(&count)
		if count == 0 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "bot is not a member of this space"})
			return
		}
	}

	if keyword != "" {
		_, err = bf.ctx.DB().SelectBySql(
			"SELECT sm.uid, IFNULL(u.name,'') as name, IFNULL(u.robot,0) as robot FROM space_member sm LEFT JOIN user u ON sm.uid=u.uid WHERE sm.space_id=? AND sm.status=1 AND u.name LIKE ? LIMIT ?",
			spaceID, "%"+keyword+"%", limit,
		).Load(&members)
	} else {
		_, err = bf.ctx.DB().SelectBySql(
			"SELECT sm.uid, IFNULL(u.name,'') as name, IFNULL(u.robot,0) as robot FROM space_member sm LEFT JOIN user u ON sm.uid=u.uid WHERE sm.space_id=? AND sm.status=1 LIMIT ?",
			spaceID, limit,
		).Load(&members)
	}
	if err != nil {
		bf.Error("query space members failed", zap.Error(err))
		c.ResponseError(errors.New("failed to query space members"))
		return
	}

	c.JSON(http.StatusOK, members)
}

// ========== Bot Group Management APIs ==========

// botGroupCreate 创建群 (POST /v1/bot/groups/create)
func (bf *BotFather) botGroupCreate(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		c.ResponseError(errors.New("robot_id not found"))
		return
	}

	var req struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
		Creator string   `json:"creator"`
		SpaceID string   `json:"space_id"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if len(req.Members) == 0 {
		c.ResponseError(errors.New("members is required"))
		return
	}
	// creator 可选，不传则默认 members[0] 为群主
	if req.Creator == "" {
		req.Creator = req.Members[0]
	}

	// 如果没传 space_id，自动使用 Bot 所在的第一个 Space
	if req.SpaceID == "" {
		var spaceIDs []string
		bf.ctx.DB().SelectBySql(
			"SELECT space_id FROM space_member WHERE uid=? AND status=1 LIMIT 1", robotID,
		).Load(&spaceIDs)
		if len(spaceIDs) > 0 {
			req.SpaceID = spaceIDs[0]
		}
	}

	// 调用 Service 创建群
	createResp, err := bf.groupService.CreateGroup(&group.CreateGroupServiceReq{
		Creator: req.Creator,
		Members: req.Members,
		Name:    req.Name,
		SpaceID: req.SpaceID,
		BotUID:  robotID,
	})
	if err != nil {
		bf.Error("create group failed", zap.Error(err))
		c.ResponseError(err)
		return
	}

	resp := map[string]interface{}{
		"group_no": createResp.GroupNo,
		"name":     createResp.Name,
	}
	if len(createResp.SkippedMembers) > 0 {
		resp["skipped_members"] = createResp.SkippedMembers
	}
	c.Response(resp)
}

// botGroupUpdate 编辑群信息 (PUT /v1/bot/groups/:group_no)
func (bf *BotFather) botGroupUpdate(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	groupNo := c.Param("group_no")
	botName := bf.resolveBotDisplayName(robotID)

	// 权限检查：Bot 必须是群成员
	isMember, err := bf.groupService.ExistMember(groupNo, robotID)
	if err != nil || !isMember {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "bot is not a member of this group"})
		return
	}

	// 权限检查：Bot 必须是 bot_admin
	isBotAdmin, err := bf.groupService.IsBotAdmin(groupNo, robotID)
	if err != nil || !isBotAdmin {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "bot is not a bot_admin in this group"})
		return
	}

	var req struct {
		Name   *string `json:"name"`
		Notice *string `json:"notice"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.Name == nil && req.Notice == nil {
		c.ResponseError(errors.New("at least one of name or notice is required"))
		return
	}

	// 调用 Service 更新群信息
	err = bf.groupService.UpdateGroupInfo(&group.UpdateGroupInfoServiceReq{
		GroupNo:      groupNo,
		OperatorUID:  robotID,
		OperatorName: botName,
		Name:         req.Name,
		Notice:       req.Notice,
	})
	if err != nil {
		bf.Error("update group failed", zap.Error(err))
		c.ResponseError(err)
		return
	}

	c.Response(map[string]interface{}{"ok": true})
}

// botGroupMemberAdd 添加群成员 (POST /v1/bot/groups/:group_no/members/add)
func (bf *BotFather) botGroupMemberAdd(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	groupNo := c.Param("group_no")
	botName := bf.resolveBotDisplayName(robotID)

	// 权限检查：Bot 必须是群成员
	isMember, err := bf.groupService.ExistMember(groupNo, robotID)
	if err != nil || !isMember {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "bot is not a member of this group"})
		return
	}

	var req struct {
		Members []string `json:"members"`
	}
	if err := c.BindJSON(&req); err != nil || len(req.Members) == 0 {
		c.ResponseError(errors.New("members is required"))
		return
	}

	// 调用 Service 添加群成员
	addResp, err := bf.groupService.AddGroupMembers(&group.AddGroupMembersServiceReq{
		GroupNo:      groupNo,
		Members:      req.Members,
		OperatorUID:  robotID,
		OperatorName: botName,
	})
	if err != nil {
		bf.Error("add group members failed", zap.Error(err))
		c.ResponseError(err)
		return
	}

	c.Response(map[string]interface{}{"ok": true, "added": addResp.Added})
}

// botGroupMemberRemove 移除群成员 (POST /v1/bot/groups/:group_no/members/remove)
func (bf *BotFather) botGroupMemberRemove(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	groupNo := c.Param("group_no")
	botName := bf.resolveBotDisplayName(robotID)

	// 权限检查：Bot 必须是群成员
	isMember, err := bf.groupService.ExistMember(groupNo, robotID)
	if err != nil || !isMember {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "bot is not a member of this group"})
		return
	}

	// 权限检查：Bot 必须是 bot_admin
	isBotAdmin, err := bf.groupService.IsBotAdmin(groupNo, robotID)
	if err != nil || !isBotAdmin {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "bot is not a bot_admin in this group"})
		return
	}

	var req struct {
		Members []string `json:"members"`
	}
	if err := c.BindJSON(&req); err != nil || len(req.Members) == 0 {
		c.ResponseError(errors.New("members is required"))
		return
	}

	// Bot 不能移除自己
	filteredMembers := make([]string, 0, len(req.Members))
	for _, uid := range req.Members {
		if uid != robotID {
			filteredMembers = append(filteredMembers, uid)
		}
	}
	if len(filteredMembers) == 0 {
		c.Response(map[string]interface{}{"ok": true, "removed": 0})
		return
	}

	// 调用 Service 移除群成员
	removeResp, err := bf.groupService.RemoveGroupMembers(&group.RemoveGroupMembersServiceReq{
		GroupNo:      groupNo,
		Members:      filteredMembers,
		OperatorUID:  robotID,
		OperatorName: botName,
	})
	if err != nil {
		bf.Error("remove group members failed", zap.Error(err))
		c.ResponseError(err)
		return
	}

	c.Response(map[string]interface{}{"ok": true, "removed": removeResp.Removed})
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
