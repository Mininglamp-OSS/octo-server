package botfather

import (
	"net/http"
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

// validateBotGroupAccess 验证 bot 对群的访问权限
// 返回 robotID, groupNo, ok；如果 ok=false，已向客户端返回错误响应
func (bf *BotFather) validateBotGroupAccess(c *wkhttp.Context) (robotID, groupNo string, ok bool) {
	robotID = getRobotIDFromContext(c)
	groupNo = c.Param("group_no")

	if !thread.IsValidGroupNo(groupNo) {
		respondBotfatherRequestInvalid(c, "group_no")
		return "", "", false
	}

	isMember, err := bf.groupService.ExistMember(groupNo, robotID)
	if err != nil {
		bf.Error("检查群成员失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return "", "", false
	}
	if !isMember {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherBotNotGroupMember, nil, nil)
		return "", "", false
	}

	return robotID, groupNo, true
}

// validateBotThreadAccess 验证 bot 对子区的访问权限
// 返回 robotID, groupNo, shortID, ok；如果 ok=false，已向客户端返回错误响应
func (bf *BotFather) validateBotThreadAccess(c *wkhttp.Context) (robotID, groupNo, shortID string, ok bool) {
	robotID, groupNo, ok = bf.validateBotGroupAccess(c)
	if !ok {
		return "", "", "", false
	}

	shortID = c.Param("short_id")
	if !thread.IsValidShortID(shortID) {
		respondBotfatherRequestInvalid(c, "short_id")
		return "", "", "", false
	}

	return robotID, groupNo, shortID, true
}

// botCreateThread 创建子区
// POST /v1/bot/groups/:group_no/threads
func (bf *BotFather) botCreateThread(c *wkhttp.Context) {
	robotID, groupNo, ok := bf.validateBotGroupAccess(c)
	if !ok {
		return
	}

	var req struct {
		Name            string `json:"name" binding:"required,max=100"`
		SourceMessageID *int64 `json:"source_message_id"`
	}
	if err := c.BindJSON(&req); err != nil {
		bf.Error("参数错误", zap.Error(err))
		respondBotfatherRequestInvalid(c, "name")
		return
	}

	// 获取 bot 的显示名称
	creatorName := robotID
	userResp, _ := bf.userService.GetUserWithUsername(robotID)
	if userResp != nil && userResp.Name != "" {
		creatorName = userResp.Name
	}

	resp, err := bf.threadService.CreateThread(&thread.CreateThreadReq{
		GroupNo:         groupNo,
		Name:            req.Name,
		CreatorUID:      robotID,
		CreatorName:     creatorName,
		SourceMessageID: req.SourceMessageID,
	})
	if err != nil {
		bf.Error("创建子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("robotID", robotID))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}
	c.Response(resp)
}

// botListThreads 列出群内所有子区
// GET /v1/bot/groups/:group_no/threads
func (bf *BotFather) botListThreads(c *wkhttp.Context) {
	_, groupNo, ok := bf.validateBotGroupAccess(c)
	if !ok {
		return
	}

	// 向后兼容：未显式传 page_index/page_size 时，返回裸数组（旧调用方格式）。
	hasPageParam := c.Query("page_index") != "" || c.Query("page_size") != ""
	var pageIndex, pageSize int64
	if hasPageParam {
		pageIndex, pageSize = c.GetPage()
	} else {
		pageIndex, pageSize = 1, thread.MaxThreadPageSize
	}

	threads, total, err := bf.threadService.GetThreads(groupNo, nil, pageIndex, pageSize)
	if err != nil {
		bf.Error("获取子区列表失败", zap.Error(err), zap.String("groupNo", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
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

// botGetThread 获取子区详情
// GET /v1/bot/groups/:group_no/threads/:short_id
func (bf *BotFather) botGetThread(c *wkhttp.Context) {
	_, groupNo, shortID, ok := bf.validateBotThreadAccess(c)
	if !ok {
		return
	}

	resp, err := bf.threadService.GetThread(groupNo, shortID, "")
	if err != nil {
		bf.Error("获取子区详情失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	c.Response(resp)
}

// botDeleteThread 删除子区
// DELETE /v1/bot/groups/:group_no/threads/:short_id
func (bf *BotFather) botDeleteThread(c *wkhttp.Context) {
	robotID, groupNo, shortID, ok := bf.validateBotThreadAccess(c)
	if !ok {
		return
	}

	// DeleteThread 内部会检查是否为创建者或群管理员
	err := bf.threadService.DeleteThread(groupNo, shortID, robotID)
	if err != nil {
		bf.Error("删除子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// botListThreadMembers 获取子区成员列表
// GET /v1/bot/groups/:group_no/threads/:short_id/members
func (bf *BotFather) botListThreadMembers(c *wkhttp.Context) {
	_, groupNo, shortID, ok := bf.validateBotThreadAccess(c)
	if !ok {
		return
	}

	members, err := bf.threadService.GetMembers(groupNo, shortID)
	if err != nil {
		bf.Error("获取成员列表失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	c.Response(members)
}

// botJoinThread 加入子区
// POST /v1/bot/groups/:group_no/threads/:short_id/join
func (bf *BotFather) botJoinThread(c *wkhttp.Context) {
	robotID, groupNo, shortID, ok := bf.validateBotThreadAccess(c)
	if !ok {
		return
	}

	err := bf.threadService.JoinThread(groupNo, shortID, robotID)
	if err != nil {
		bf.Error("加入子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// botLeaveThread 离开子区
// POST /v1/bot/groups/:group_no/threads/:short_id/leave
func (bf *BotFather) botLeaveThread(c *wkhttp.Context) {
	robotID, groupNo, shortID, ok := bf.validateBotThreadAccess(c)
	if !ok {
		return
	}

	err := bf.threadService.LeaveThread(groupNo, shortID, robotID)
	if err != nil {
		bf.Error("离开子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// ==================== Bot Thread GROUP.md ====================

// botGetThreadMd Bot 获取子区 GROUP.md
// GET /v1/bot/groups/:group_no/threads/:short_id/md
func (bf *BotFather) botGetThreadMd(c *wkhttp.Context) {
	_, groupNo, shortID, ok := bf.validateBotThreadAccess(c)
	if !ok {
		return
	}

	result, err := bf.threadService.GetThreadMd(groupNo, shortID)
	if err != nil {
		bf.Error("query thread GROUP.md failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
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

// botUpdateThreadMd Bot 更新子区 GROUP.md
// PUT /v1/bot/groups/:group_no/threads/:short_id/md
func (bf *BotFather) botUpdateThreadMd(c *wkhttp.Context) {
	robotID, groupNo, shortID, ok := bf.validateBotThreadAccess(c)
	if !ok {
		return
	}

	// 权限检查在 API Handler 层完成：验证 bot_admin
	isBotAdmin, err := bf.groupService.IsBotAdmin(groupNo, robotID)
	if err != nil {
		bf.Error("check bot admin failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	if !isBotAdmin {
		respondBotfatherBotNotGroupAdmin(c)
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondBotfatherRequestInvalid(c, "")
		return
	}

	// 校验空内容
	if strings.TrimSpace(req.Content) == "" {
		respondBotfatherRequestInvalid(c, "content")
		return
	}

	maxSize := group.GetGroupMdMaxSize()
	if len(req.Content) > maxSize {
		respondBotfatherContentTooLarge(c, "content", maxSize)
		return
	}

	// Service 层：纯数据操作透传（通过 threadService，而非直接调用 DB）
	newVersion, err := bf.threadService.UpdateThreadMd(groupNo, shortID, req.Content, robotID)
	if err != nil {
		bf.Error("update thread GROUP.md failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}

	// 异步发送通知到子区频道
	go func() {
		defer func() {
			if r := recover(); r != nil {
				bf.Error("sendThreadMdNotification panic", zap.Any("recover", r))
			}
		}()
		bf.sendThreadMdNotification(groupNo, shortID, robotID, newVersion, "thread_md_updated", "Thread GROUP.md updated")
	}()

	c.JSON(http.StatusOK, gin.H{
		"version": newVersion,
	})
}

// sendThreadMdNotification Bot 发送子区 GROUP.md 变更通知
func (bf *BotFather) sendThreadMdNotification(groupNo, shortID, updatedBy string, version int64, eventType, contentText string) {
	botUIDs, err := bf.groupService.GetBotMemberUIDs(groupNo)
	if err != nil {
		bf.Error("query bot member UIDs failed", zap.Error(err))
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
	err = bf.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 0,
		},
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		FromUID:     updatedBy,
		Payload:     []byte(util.ToJson(payload)),
	})
	if err != nil {
		bf.Error("send thread GROUP.md notification failed", zap.Error(err))
	}
}
