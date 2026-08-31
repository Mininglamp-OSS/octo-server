package bot_api

import (
	"errors"
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

	// issue #352（PR #345 mandatory follow-up）：所有 bot 子区端点共享本门禁，
	// 必须用 ExistMemberActive（is_deleted=0 AND status=Normal，排除被拉黑成员）。
	// permissive ExistMember 会让被拉黑的 bot 继续通过 bot API 读写子区，
	// 与 #343/#345 落地的「子区门禁 = 活跃父群成员」语义矛盾。
	// GROUP 级端点（groups.go）保持 permissive ExistMember，by design 不动。
	isMember, err := ba.groupService.ExistMemberActive(groupNo, robotID)
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
		// OnBehalfOf — 复用 sendMessage 的 OBO 语义（YUJ-1166 / #81）。
		// 人类经 bot 代建 thread 时携带触发者 UID：服务端校验 bot 当前被该 grantor
		// 授权（active=1 AND global_enabled=1）且 grantor 对父群仍可读，通过后把
		// CreatorUID 落成 grantor 而非 bot 自己，让 CreateThread 结尾的
		// EnsureThreadFollowForCreator 无条件为「实际发起者」补关注（#739）。
		// 缺省（bot 主动建，无人类触发者）时 CreatorUID 保持 robotID —— 关注者就是
		// bot 自己，让 bot 能 loop 收到子区事件。
		OnBehalfOf string `json:"on_behalf_of,omitempty"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "name")
		return
	}

	// creatorUID 默认是 bot 自己；带 on_behalf_of 时收敛到 grantor（经 OBO 门校验）。
	creatorUID := robotID
	if obo := strings.TrimSpace(req.OnBehalfOf); obo != "" {
		// 建 thread 是父群内的写操作，OBO 门按父群频道校验：bot 被 grantor 授权 +
		// grantor 对父群仍可读（与 send.go 的第三方发送同一 checkOBO 谓词，TOCTOU 一致）。
		// fail-closed：授权不足直接拒，绝不静默回退到 robotID —— 否则人类触发场景会
		// 静默漏关注，正是本 issue 要根治的错位。
		if err := ba.checkOBO(robotID, obo, groupNo, common.ChannelTypeGroup.Uint8()); err != nil {
			if errors.Is(err, ErrOBONotAuthorized) {
				ba.Warn("OBO denied on thread create: no active grant or grantor lost group access",
					zap.String("bot", robotID),
					zap.String("on_behalf_of", obo),
					zap.String("groupNo", groupNo))
				httperr.ResponseErrorL(c, errcode.ErrBotAPIOBONotAuthorized, nil, nil)
				return
			}
			ba.Error("OBO check failed on thread create", zap.Error(err),
				zap.String("bot", robotID), zap.String("on_behalf_of", obo))
			httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
			return
		}
		creatorUID = obo
	}

	creatorName := creatorUID
	userResp, _ := ba.userService.GetUser(creatorUID)
	if userResp != nil && userResp.Name != "" {
		creatorName = userResp.Name
	}

	resp, err := ba.threadService.CreateThread(&thread.CreateThreadReq{
		GroupNo:         groupNo,
		Name:            req.Name,
		CreatorUID:      creatorUID,
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

	// 解散守卫：与用户端 threadMdUpdate 对齐，父群已解散时禁止 bot 写子区 GROUP.md。
	if disbanded, err := ba.isGroupDisbanded(groupNo); err != nil {
		ba.Error("check group disband status failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	} else if disbanded {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIGroupDisbanded, nil, nil)
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
