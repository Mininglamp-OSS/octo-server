package bot_api

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// threadChannelIDSeparator marks a thread channel id.
const threadChannelIDSeparator = "____"

// getGroups handles GET /v1/bot/groups.
func (ba *BotAPI) getGroups(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
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
		_, err = ba.ctx.DB().SelectBySql(
			"SELECT gm.group_no, g.name, g.space_id FROM group_member gm INNER JOIN `group` g ON gm.group_no = g.group_no WHERE gm.uid = ? AND gm.is_deleted = 0 AND g.space_id = ?",
			robotID, spaceID,
		).Load(&groups)
	} else {
		_, err = ba.ctx.DB().SelectBySql(
			"SELECT gm.group_no, g.name, g.space_id FROM group_member gm INNER JOIN `group` g ON gm.group_no = g.group_no WHERE gm.uid = ? AND gm.is_deleted = 0",
			robotID,
		).Load(&groups)
	}
	if err != nil {
		ba.Error("查询机器人群组失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}

	c.JSON(http.StatusOK, groups)
}

// getGroupInfo handles GET /v1/bot/groups/:group_no.
func (ba *BotAPI) getGroupInfo(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	groupNo := c.Param("group_no")

	var count int
	err := ba.db.session.SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, robotID).LoadOne(&count)
	if err != nil {
		ba.Error("query group membership failed", zap.Error(err), zap.String("groupNo", groupNo), zap.String("robotID", robotID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if count == 0 {
		httperr.ResponseErrorL(c, errcode.ErrBotAPINotGroupMember, nil, nil)
		return
	}

	var grp struct {
		GroupNo   string `db:"group_no"`
		Name      string `db:"name"`
		Notice    string `db:"notice"`
		Creator   string `db:"creator"`
		Status    int    `db:"status"`
		CreatedAt string `db:"created_at"`
	}
	_, err = ba.db.session.Select("group_no, name, IFNULL(notice,'') notice, IFNULL(creator,'') creator, status, created_at").
		From("`group`").Where("group_no=?", groupNo).Load(&grp)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIGroupNotFound, nil, nil)
			return
		}
		ba.Error("query group info failed", zap.Error(err), zap.String("groupNo", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}

	c.Response(map[string]interface{}{
		"group_no":   grp.GroupNo,
		"name":       grp.Name,
		"notice":     grp.Notice,
		"creator":    grp.Creator,
		"status":     grp.Status,
		"created_at": grp.CreatedAt,
	})
}

// getGroupMembers handles GET /v1/bot/groups/:group_no/members.
func (ba *BotAPI) getGroupMembers(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	groupNo := c.Param("group_no")

	var count int
	err := ba.db.session.SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, robotID).LoadOne(&count)
	if err != nil {
		ba.Error("query group membership failed", zap.Error(err), zap.String("groupNo", groupNo), zap.String("robotID", robotID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if count == 0 {
		httperr.ResponseErrorL(c, errcode.ErrBotAPINotGroupMember, nil, nil)
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
	_, err = ba.db.session.SelectBySql(`
		SELECT gm.uid, IFNULL(u.name,'') name, gm.role, IFNULL(u.robot,0) robot, gm.created_at, IFNULL(r.creator_uid,'') AS owner_uid, IFNULL(u2.name,'') AS owner_name
		FROM group_member gm 
		LEFT JOIN user u ON gm.uid = u.uid 
		LEFT JOIN robot r ON gm.uid = r.robot_id AND r.status=1
		LEFT JOIN user u2 ON r.creator_uid = u2.uid
		WHERE gm.group_no = ? AND gm.is_deleted = 0
		ORDER BY gm.role DESC, gm.created_at ASC
	`, groupNo).Load(&members)
	if err != nil {
		ba.Error("query group members failed", zap.Error(err), zap.String("groupNo", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}

	c.Response(members)
}

// getGroupMd handles GET /v1/bot/groups/:group_no/md.
func (ba *BotAPI) getGroupMd(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}
	groupNo := c.Param("group_no")
	if strings.Contains(groupNo, threadChannelIDSeparator) {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIThreadChannelNotAccepted, nil, nil)
		return
	}

	isMember, err := ba.groupService.ExistMember(groupNo, robotID)
	if err != nil {
		ba.Error("check group membership failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotGroupMember, nil, nil)
		return
	}

	result, err := ba.groupService.GetGroupMd(groupNo)
	if err != nil {
		ba.Error("query GROUP.md failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
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

// updateGroupMd handles PUT /v1/bot/groups/:group_no/md.
func (ba *BotAPI) updateGroupMd(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}
	groupNo := c.Param("group_no")
	if strings.Contains(groupNo, threadChannelIDSeparator) {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIThreadChannelNotAccepted, nil, nil)
		return
	}

	isMember, err := ba.groupService.ExistMember(groupNo, robotID)
	if err != nil {
		ba.Error("check group membership failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotGroupMember, nil, nil)
		return
	}

	isBotAdmin, err := ba.groupService.IsBotAdmin(groupNo, robotID)
	if err != nil {
		ba.Error("check bot admin failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if !isBotAdmin {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotGroupAdmin, nil, nil)
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}

	maxSize := group.GetGroupMdMaxSize()
	if len(req.Content) > maxSize {
		respondBotAPIContentTooLarge(c, "content", maxSize)
		return
	}

	newVersion, err := ba.groupService.UpdateGroupMd(groupNo, req.Content, robotID)
	if err != nil {
		ba.Error("update GROUP.md failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				ba.Error("goroutine panic",
					zap.Any("recover", r),
					zap.String("stack", string(debug.Stack())),
				)
			}
		}()
		ba.sendGroupMdNotification(groupNo, robotID, newVersion)
	}()

	c.JSON(http.StatusOK, gin.H{
		"version": newVersion,
	})
}

// botSpaceMembers handles GET /v1/bot/space/members.
func (ba *BotAPI) botSpaceMembers(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
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
		var spaceIDs []string
		_, err = ba.ctx.DB().SelectBySql(
			"SELECT space_id FROM space_member WHERE uid=? AND status=1", robotID,
		).Load(&spaceIDs)
		if err != nil || len(spaceIDs) == 0 {
			c.JSON(http.StatusOK, []MemberInfo{})
			return
		}
		spaceID = spaceIDs[0]
	} else {
		var count int
		if spErr := ba.ctx.DB().SelectBySql(
			"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1", spaceID, robotID,
		).LoadOne(&count); spErr != nil {
			ba.Error("query space membership failed", zap.Error(spErr))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if count == 0 {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotSpaceMember, nil, nil)
			return
		}
	}

	if keyword != "" {
		// Escape LIKE wildcards in user input
		escaped := strings.ReplaceAll(keyword, "\\", "\\\\")
		escaped = strings.ReplaceAll(escaped, "%", "\\%")
		escaped = strings.ReplaceAll(escaped, "_", "\\_")
		_, err = ba.ctx.DB().SelectBySql(
			"SELECT sm.uid, IFNULL(u.name,'') as name, IFNULL(u.robot,0) as robot FROM space_member sm LEFT JOIN user u ON sm.uid=u.uid WHERE sm.space_id=? AND sm.status=1 AND u.name LIKE ? LIMIT ?",
			spaceID, "%"+escaped+"%", limit,
		).Load(&members)
	} else {
		_, err = ba.ctx.DB().SelectBySql(
			"SELECT sm.uid, IFNULL(u.name,'') as name, IFNULL(u.robot,0) as robot FROM space_member sm LEFT JOIN user u ON sm.uid=u.uid WHERE sm.space_id=? AND sm.status=1 LIMIT ?",
			spaceID, limit,
		).Load(&members)
	}
	if err != nil {
		ba.Error("query space members failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}

	c.JSON(http.StatusOK, members)
}

// botGroupCreate handles POST /v1/bot/createGroup.
func (ba *BotAPI) botGroupCreate(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}

	// App Bot is DM-only — deny group operations
	if getBotKindFromContext(c) == BotKindApp {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotUnsupported, nil, nil)
		return
	}

	var req struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
		Creator string   `json:"creator"`
		SpaceID string   `json:"space_id"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if len(req.Members) == 0 {
		respondBotAPIRequestInvalid(c, "members")
		return
	}
	if len(req.Members) > 200 {
		respondBotAPILimitExceeded(c, "members", 200)
		return
	}

	if req.SpaceID == "" {
		var spaceIDs []string
		_, spErr := ba.ctx.DB().SelectBySql(
			"SELECT space_id FROM space_member WHERE uid=? AND status=1 LIMIT 1", robotID,
		).Load(&spaceIDs)
		if spErr != nil {
			ba.Error("query bot space failed", zap.Error(spErr))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if len(spaceIDs) > 0 {
			req.SpaceID = spaceIDs[0]
		}
	}

	memberUsers, err := ba.userDB.QueryByUIDs(req.Members)
	if err != nil {
		ba.Error("query member info failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	robotSet := make(map[string]bool)
	for _, u := range memberUsers {
		if u.Robot == 1 {
			robotSet[u.UID] = true
		}
	}
	var humanMembers []string
	for _, uid := range req.Members {
		if !robotSet[uid] {
			humanMembers = append(humanMembers, uid)
		}
	}
	if len(humanMembers) == 0 {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIMemberNotHuman, nil, nil)
		return
	}

	if req.Creator == "" {
		req.Creator = humanMembers[0]
	} else {
		creatorUser, err := ba.userDB.QueryByUID(req.Creator)
		if err != nil {
			ba.Error("query creator info failed", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
			return
		}
		if creatorUser == nil {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIUserNotFound, nil, nil)
			return
		}
		if creatorUser.Robot == 1 {
			httperr.ResponseErrorL(c, errcode.ErrBotAPIMemberNotHuman, nil, i18n.Details{"field": "creator"})
			return
		}
	}

	createResp, err := ba.groupService.CreateGroup(&group.CreateGroupServiceReq{
		Creator: req.Creator,
		Members: humanMembers,
		Name:    req.Name,
		SpaceID: req.SpaceID,
		BotUID:  robotID,
	})
	if err != nil {
		ba.Error("create group failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
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

// botGroupUpdate handles PUT /v1/bot/groups/:group_no/info.
func (ba *BotAPI) botGroupUpdate(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	groupNo := c.Param("group_no")

	// App Bot is DM-only — deny group operations
	if getBotKindFromContext(c) == BotKindApp {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotUnsupported, nil, nil)
		return
	}

	botName := ba.resolveBotDisplayName(robotID)

	isMember, err := ba.groupService.ExistMember(groupNo, robotID)
	if err != nil {
		ba.Error("check group membership failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotGroupMember, nil, nil)
		return
	}

	isBotAdmin, err := ba.groupService.IsBotAdmin(groupNo, robotID)
	if err != nil {
		ba.Error("check bot admin failed", zap.Error(err), zap.String("groupNo", groupNo), zap.String("robotID", robotID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if !isBotAdmin {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotGroupAdmin, nil, nil)
		return
	}

	var req struct {
		Name   *string `json:"name"`
		Notice *string `json:"notice"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}
	if req.Name == nil && req.Notice == nil {
		respondBotAPIRequestInvalid(c, "")
		return
	}

	err = ba.groupService.UpdateGroupInfo(&group.UpdateGroupInfoServiceReq{
		GroupNo:      groupNo,
		OperatorUID:  robotID,
		OperatorName: botName,
		Name:         req.Name,
		Notice:       req.Notice,
	})
	if err != nil {
		ba.Error("update group failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}

	c.Response(map[string]interface{}{"ok": true})
}

// botGroupMemberAdd handles POST /v1/bot/groups/:group_no/members/add.
func (ba *BotAPI) botGroupMemberAdd(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	groupNo := c.Param("group_no")

	// App Bot is DM-only — deny group operations
	if getBotKindFromContext(c) == BotKindApp {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotUnsupported, nil, nil)
		return
	}

	botName := ba.resolveBotDisplayName(robotID)

	isMember, err := ba.groupService.ExistMember(groupNo, robotID)
	if err != nil {
		ba.Error("check group membership failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotGroupMember, nil, nil)
		return
	}

	var req struct {
		Members []string `json:"members"`
	}
	if err := c.BindJSON(&req); err != nil || len(req.Members) == 0 {
		respondBotAPIRequestInvalid(c, "members")
		return
	}
	if len(req.Members) > 200 {
		respondBotAPILimitExceeded(c, "members", 200)
		return
	}

	memberUsers, err := ba.userDB.QueryByUIDs(req.Members)
	if err != nil {
		ba.Error("query member info failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	var humanMembers []string
	var skippedBots []string
	for _, u := range memberUsers {
		if u.Robot == 1 {
			skippedBots = append(skippedBots, u.UID)
			continue
		}
		humanMembers = append(humanMembers, u.UID)
	}
	if len(humanMembers) == 0 {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIMemberNotHuman, nil, nil)
		return
	}

	addResp, err := ba.groupService.AddGroupMembers(&group.AddGroupMembersServiceReq{
		GroupNo:      groupNo,
		Members:      humanMembers,
		OperatorUID:  robotID,
		OperatorName: botName,
	})
	if err != nil {
		ba.Error("add group members failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}

	resp := map[string]interface{}{"ok": true, "added": addResp.Added}
	if len(skippedBots) > 0 {
		resp["skipped_bots"] = skippedBots
	}
	c.Response(resp)
}

// botGroupMemberRemove handles POST /v1/bot/groups/:group_no/members/remove.
func (ba *BotAPI) botGroupMemberRemove(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	groupNo := c.Param("group_no")

	// App Bot is DM-only — deny group operations
	if getBotKindFromContext(c) == BotKindApp {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotUnsupported, nil, nil)
		return
	}

	botName := ba.resolveBotDisplayName(robotID)

	isMember, err := ba.groupService.ExistMember(groupNo, robotID)
	if err != nil {
		ba.Error("check group membership failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotGroupMember, nil, nil)
		return
	}

	isBotAdmin, err := ba.groupService.IsBotAdmin(groupNo, robotID)
	if err != nil {
		ba.Error("check bot admin failed", zap.Error(err), zap.String("groupNo", groupNo), zap.String("robotID", robotID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if !isBotAdmin {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPINotGroupAdmin, nil, nil)
		return
	}

	var req struct {
		Members []string `json:"members"`
	}
	if err := c.BindJSON(&req); err != nil || len(req.Members) == 0 {
		respondBotAPIRequestInvalid(c, "members")
		return
	}

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

	removeResp, err := ba.groupService.RemoveGroupMembers(&group.RemoveGroupMembersServiceReq{
		GroupNo:      groupNo,
		Members:      filteredMembers,
		OperatorUID:  robotID,
		OperatorName: botName,
	})
	if err != nil {
		ba.Error("remove group members failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}

	c.Response(map[string]interface{}{"ok": true, "removed": removeResp.Removed})
}

// sendGroupMdNotification sends GROUP.md event notification.
func (ba *BotAPI) sendGroupMdNotification(groupNo string, updatedBy string, version int64) {
	botUIDs, err := ba.groupService.GetBotMemberUIDs(groupNo)
	if err != nil {
		ba.Error("query bot member UIDs failed", zap.Error(err))
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

	ba.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		FromUID:     updatedBy,
		Payload:     []byte(util.ToJson(payload)),
	})
}
