package bot_api

import (
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// validateBotGroupAccess verifies bot access to a group.
func (ba *BotAPI) validateBotGroupAccess(c *wkhttp.Context) (robotID, groupNo string, ok bool) {
	robotID = getRobotIDFromContext(c)

	// App Bot is DM-only — deny all group/thread operations
	if getBotKindFromContext(c) == BotKindApp {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAppBotUnsupported, nil, nil)
		return "", "", false
	}

	groupNo = c.Param("group_no")

	if !thread.IsValidGroupNo(groupNo) {
		respondBotAPIRequestInvalid(c, "group_no")
		return "", "", false
	}

	isMember, err := ba.groupService.ExistMember(groupNo, robotID)
	if err != nil {
		ba.Error("检查群成员失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return "", "", false
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrBotAPINotGroupMember, nil, nil)
		return "", "", false
	}

	return robotID, groupNo, true
}

// validateBotThreadAccess verifies bot access to a thread.
func (ba *BotAPI) validateBotThreadAccess(c *wkhttp.Context) (robotID, groupNo, shortID string, ok bool) {
	robotID, groupNo, ok = ba.validateBotGroupAccess(c)
	if !ok {
		return "", "", "", false
	}

	shortID = c.Param("short_id")
	if !thread.IsValidShortID(shortID) {
		respondBotAPIRequestInvalid(c, "short_id")
		return "", "", "", false
	}

	return robotID, groupNo, shortID, true
}

// botCreateThread handles POST /v1/bot/groups/:group_no/threads.
func (ba *BotAPI) botCreateThread(c *wkhttp.Context) {
	robotID, groupNo, ok := ba.validateBotGroupAccess(c)
	if !ok {
		return
	}

	var req struct {
		Name            string `json:"name" binding:"required,max=100"`
		SourceMessageID *int64 `json:"source_message_id"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "name")
		return
	}

	creatorName := robotID
	userResp, _ := ba.userService.GetUser(robotID)
	if userResp != nil && userResp.Name != "" {
		creatorName = userResp.Name
	}

	resp, err := ba.threadService.CreateThread(&thread.CreateThreadReq{
		GroupNo:         groupNo,
		Name:            req.Name,
		CreatorUID:      robotID,
		CreatorName:     creatorName,
		SourceMessageID: req.SourceMessageID,
	})
	if err != nil {
		ba.Error("创建子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("robotID", robotID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}
	c.Response(resp)
}

// botListThreads handles GET /v1/bot/groups/:group_no/threads.
func (ba *BotAPI) botListThreads(c *wkhttp.Context) {
	_, groupNo, ok := ba.validateBotGroupAccess(c)
	if !ok {
		return
	}

	hasPageParam := c.Query("page_index") != "" || c.Query("page_size") != ""
	var pageIndex, pageSize int64
	if hasPageParam {
		pageIndex, pageSize = c.GetPage()
	} else {
		pageIndex, pageSize = 1, thread.MaxThreadPageSize
	}

	threads, total, err := ba.threadService.GetThreads(groupNo, nil, pageIndex, pageSize)
	if err != nil {
		ba.Error("获取子区列表失败", zap.Error(err), zap.String("groupNo", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if !hasPageParam {
		c.Response(threads)
		return
	}
	c.Response(map[string]interface{}{
		"count": total,
		"list":  threads,
	})
}

// botGetThread handles GET /v1/bot/groups/:group_no/threads/:short_id.
func (ba *BotAPI) botGetThread(c *wkhttp.Context) {
	_, groupNo, shortID, ok := ba.validateBotThreadAccess(c)
	if !ok {
		return
	}

	resp, err := ba.threadService.GetThread(groupNo, shortID, "")
	if err != nil {
		ba.Error("获取子区详情失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	c.Response(resp)
}

// botDeleteThread handles DELETE /v1/bot/groups/:group_no/threads/:short_id.
func (ba *BotAPI) botDeleteThread(c *wkhttp.Context) {
	robotID, groupNo, shortID, ok := ba.validateBotThreadAccess(c)
	if !ok {
		return
	}

	err := ba.threadService.DeleteThread(groupNo, shortID, robotID)
	if err != nil {
		ba.Error("删除子区失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// botListThreadMembers handles GET /v1/bot/groups/:group_no/threads/:short_id/members.
func (ba *BotAPI) botListThreadMembers(c *wkhttp.Context) {
	_, groupNo, shortID, ok := ba.validateBotThreadAccess(c)
	if !ok {
		return
	}

	members, err := ba.threadService.GetMembers(groupNo, shortID)
	if err != nil {
		ba.Error("获取成员列表失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	c.Response(members)
}

// botJoinThread handles POST /v1/bot/groups/:group_no/threads/:short_id/join.
func (ba *BotAPI) botJoinThread(c *wkhttp.Context) {
	robotID, groupNo, shortID, ok := ba.validateBotThreadAccess(c)
	if !ok {
		return
	}

	err := ba.threadService.JoinThread(groupNo, shortID, robotID)
	if err != nil {
		ba.Error("加入子区失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// botLeaveThread handles POST /v1/bot/groups/:group_no/threads/:short_id/leave.
func (ba *BotAPI) botLeaveThread(c *wkhttp.Context) {
	robotID, groupNo, shortID, ok := ba.validateBotThreadAccess(c)
	if !ok {
		return
	}

	err := ba.threadService.LeaveThread(groupNo, shortID, robotID)
	if err != nil {
		ba.Error("离开子区失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// botGetThreadMd handles GET /v1/bot/groups/:group_no/threads/:short_id/md.
func (ba *BotAPI) botGetThreadMd(c *wkhttp.Context) {
	_, groupNo, shortID, ok := ba.validateBotThreadAccess(c)
	if !ok {
		return
	}

	result, err := ba.threadService.GetThreadMd(groupNo, shortID)
	if err != nil {
		ba.Error("query thread GROUP.md failed", zap.Error(err))
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

// botUpdateThreadMd handles PUT /v1/bot/groups/:group_no/threads/:short_id/md.
func (ba *BotAPI) botUpdateThreadMd(c *wkhttp.Context) {
	robotID, groupNo, shortID, ok := ba.validateBotThreadAccess(c)
	if !ok {
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

	if strings.TrimSpace(req.Content) == "" {
		respondBotAPIRequestInvalid(c, "content")
		return
	}

	maxSize := group.GetGroupMdMaxSize()
	if len(req.Content) > maxSize {
		respondBotAPIContentTooLarge(c, "content", maxSize)
		return
	}

	newVersion, err := ba.threadService.UpdateThreadMd(groupNo, shortID, req.Content, robotID)
	if err != nil {
		ba.Error("update thread GROUP.md failed", zap.Error(err))
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
		ba.sendThreadMdNotification(groupNo, shortID, robotID, newVersion, "thread_md_updated", "Thread GROUP.md updated")
	}()

	c.JSON(http.StatusOK, gin.H{
		"version": newVersion,
	})
}

// sendThreadMdNotification sends thread GROUP.md change notification.
func (ba *BotAPI) sendThreadMdNotification(groupNo, shortID, updatedBy string, version int64, eventType, contentText string) {
	botUIDs, err := ba.groupService.GetBotMemberUIDs(groupNo)
	if err != nil {
		ba.Error("query bot member UIDs failed", zap.Error(err))
		return
	}

	payload := map[string]interface{}{
		"type":    common.Text,
		"content": contentText,
		"event": map[string]interface{}{
			"type":       eventType,
			"version":    version,
			"updated_by": updatedBy,
			"group_no":   groupNo,
			"short_id":   shortID,
		},
	}
	if len(botUIDs) > 0 {
		payload["mention"] = map[string]interface{}{
			"uids": botUIDs,
		}
	}

	channelID := thread.BuildChannelID(groupNo, shortID)
	ba.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 0,
		},
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		FromUID:     updatedBy,
		Payload:     []byte(util.ToJson(payload)),
	})
}
