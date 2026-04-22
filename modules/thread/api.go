package thread

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"go.uber.org/zap"
)

// Thread API 处理器
type Thread struct {
	ctx          *config.Context
	db           *DB
	service      IService
	groupService group.IService
	log.Log
}

// New 创建 Thread API 处理器
func New(ctx *config.Context) *Thread {
	t := &Thread{
		ctx:          ctx,
		db:           NewDB(ctx),
		service:      NewService(ctx),
		groupService: group.NewService(ctx),
		Log:          log.NewTLog("Thread"),
	}

	// 注册消息监听器：归档子区收到消息后自动解档
	ctx.AddMessagesListener(t.onMessages)

	return t
}

// onMessages 消息监听器
func (t *Thread) onMessages(messages []*config.MessageResp) {
	for _, msg := range messages {
		// 只处理子区频道类型
		if msg.ChannelType != common.ChannelTypeCommunityTopic.Uint8() {
			continue
		}

		groupNo, shortID, err := ParseChannelID(msg.ChannelID)
		if err != nil {
			continue
		}

		thread, err := t.db.QueryByGroupNoAndShortID(groupNo, shortID)
		if err != nil || thread == nil {
			continue
		}

		// 归档状态收到消息，自动解档
		if thread.Status == ThreadStatusArchived {
			version, err := t.ctx.GenSeq(ThreadSeqKey)
			if err != nil {
				t.Error("生成版本号失败", zap.Error(err))
				continue
			}
			if err := t.db.UpdateStatus(shortID, ThreadStatusActive, version); err != nil {
				t.Error("自动解档失败", zap.Error(err), zap.String("shortID", shortID))
			} else {
				t.Info("归档子区收到消息，自动解档", zap.String("shortID", shortID))
			}
		}

		// 更新消息统计
		content := parsePayloadContent(msg.Payload)
		if runeLen := len([]rune(content)); runeLen > 500 {
			content = string([]rune(content)[:500])
		}
		if err := t.db.UpdateMessageStats(shortID, content, msg.FromUID); err != nil {
			t.Error("更新消息统计失败", zap.Error(err), zap.String("shortID", shortID))
		}

		// 发送者不是子区成员，自动加入
		if msg.FromUID != "" {
			if err := t.service.JoinThread(groupNo, shortID, msg.FromUID); err != nil {
				t.Error("自动加入子区失败", zap.Error(err), zap.String("uid", msg.FromUID))
			}
		}
	}
}

// Route 注册路由
func (t *Thread) Route(r *wkhttp.WKHttp) {
	threads := r.Group("/v1/groups/:group_no/threads", t.ctx.AuthMiddleware(r))
	{
		threads.POST("", t.createThread)
		threads.GET("", t.listThreads)
		threads.GET("/:short_id", t.getThread)
		threads.PUT("/:short_id", t.updateThread)
		threads.PUT("/:short_id/setting", t.updateSetting)
		threads.GET("/:short_id/members", t.listMembers)
		threads.POST("/:short_id/join", t.joinThread)
		threads.POST("/:short_id/leave", t.leaveThread)
		threads.POST("/:short_id/archive", t.archiveThread)
		threads.POST("/:short_id/unarchive", t.unarchiveThread)
		threads.DELETE("/:short_id", t.deleteThread)
		threads.GET("/:short_id/md", t.threadMdGet)
		threads.PUT("/:short_id/md", t.threadMdUpdate)
		threads.DELETE("/:short_id/md", t.threadMdDelete)
	}

	// 简化路由（不需要 group_no，通过 short_id 查询）
	threadSimple := r.Group("/v1/threads", t.ctx.AuthMiddleware(r))
	{
		threadSimple.POST("/:short_id/join", t.joinThreadSimple)
		threadSimple.POST("/:short_id/leave", t.leaveThreadSimple)
		threadSimple.GET("/:short_id", t.getThreadSimple)
	}
}

// createThread 创建子区
// POST /v1/groups/:group_no/threads
func (t *Thread) createThread(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	loginUID := c.GetLoginUID()
	loginName := c.GetLoginName()

	// 验证 groupNo 格式
	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}

	var req struct {
		Name                 string          `json:"name" binding:"required,max=100"`
		SourceMessageID      *int64          `json:"source_message_id"`
		SourceMessagePayload json.RawMessage `json:"source_message_payload"`
	}
	if err := c.BindJSON(&req); err != nil {
		t.Error("参数错误", zap.Error(err))
		c.ResponseError(errors.New("invalid request: name is required"))
		return
	}

	// 校验 source_message_payload
	if len(req.SourceMessagePayload) > 0 {
		if req.SourceMessageID == nil {
			c.ResponseError(errors.New("source_message_payload requires source_message_id"))
			return
		}
		if len(req.SourceMessagePayload) > maxSourcePayloadBytes {
			c.ResponseError(errors.New("source_message_payload too large"))
			return
		}
		if !json.Valid(req.SourceMessagePayload) || string(req.SourceMessagePayload) == "null" {
			c.ResponseError(errors.New("invalid source_message_payload"))
			return
		}
	}

	resp, err := t.service.CreateThread(&CreateThreadReq{
		GroupNo:              groupNo,
		Name:                 req.Name,
		CreatorUID:           loginUID,
		CreatorName:          loginName,
		SourceMessageID:      req.SourceMessageID,
		SourceMessagePayload: req.SourceMessagePayload,
	})
	if err != nil {
		t.Error("创建子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("uid", loginUID))
		c.ResponseError(err)
		return
	}
	c.Response(resp)
}

// updateThread 修改子区信息
// PUT /v1/groups/:group_no/threads/:short_id
func (t *Thread) updateThread(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	var req struct {
		Name string `json:"name" binding:"required,max=100"`
	}
	if err := c.BindJSON(&req); err != nil {
		t.Error("参数错误", zap.Error(err))
		c.ResponseError(errors.New("invalid request: name is required and must not exceed 100 characters"))
		return
	}

	err := t.service.UpdateName(groupNo, shortID, loginUID, req.Name)
	if err != nil {
		t.Error("修改子区名称失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// updateSetting 更新当前用户对子区的个人设置(目前支持 mute)
// PUT /v1/groups/:group_no/threads/:short_id/setting
func (t *Thread) updateSetting(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	var req map[string]interface{}
	if err := c.BindJSON(&req); err != nil {
		t.Error("参数错误", zap.Error(err))
		c.ResponseError(errors.New("invalid request"))
		return
	}
	if len(req) == 0 {
		c.ResponseOK()
		return
	}

	if err := t.service.UpdateSetting(groupNo, shortID, loginUID, req); err != nil {
		t.Error("更新子区设置失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// listThreads 列出子区
// GET /v1/groups/:group_no/threads
func (t *Thread) listThreads(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	loginUID := c.GetLoginUID()

	// 验证 groupNo 格式
	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}

	// 验证是群成员
	isMember, err := t.groupService.ExistMember(groupNo, loginUID)
	if err != nil {
		t.Error("检查群成员失败", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if !isMember {
		c.ResponseError(errors.New("not a group member"))
		return
	}

	// 向后兼容：未显式传 page_index/page_size 时，返回裸数组（旧客户端格式，
	// 避免老 App 因响应结构变更解析失败）；一旦传了任一分页参数，就返回 {count, list} 信封。
	hasPageParam := c.Query("page_index") != "" || c.Query("page_size") != ""
	var pageIndex, pageSize int64
	if hasPageParam {
		pageIndex, pageSize = c.GetPage()
	} else {
		pageIndex, pageSize = 1, MaxThreadPageSize
	}

	threads, total, err := t.service.GetThreads(groupNo, pageIndex, pageSize)
	if err != nil {
		t.Error("获取子区列表失败", zap.Error(err), zap.String("groupNo", groupNo))
		c.ResponseError(err)
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

// getThread 获取子区详情
// GET /v1/groups/:group_no/threads/:short_id
func (t *Thread) getThread(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	// 验证参数格式
	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	// 验证是群成员
	isMember, err := t.groupService.ExistMember(groupNo, loginUID)
	if err != nil {
		t.Error("检查群成员失败", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if !isMember {
		c.ResponseError(errors.New("not a group member"))
		return
	}

	thread, err := t.service.GetThread(groupNo, shortID)
	if err != nil {
		t.Error("获取子区详情失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.Response(thread)
}

// archiveThread 归档子区
// POST /v1/groups/:group_no/threads/:short_id/archive
func (t *Thread) archiveThread(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	// 验证参数格式
	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	err := t.service.ArchiveThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("归档子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// unarchiveThread 取消归档
// POST /v1/groups/:group_no/threads/:short_id/unarchive
func (t *Thread) unarchiveThread(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	// 验证参数格式
	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	err := t.service.UnarchiveThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("取消归档失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// listMembers 获取子区成员列表
// GET /v1/groups/:group_no/threads/:short_id/members
func (t *Thread) listMembers(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	// 验证参数格式
	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	// 验证是群成员
	isMember, err := t.groupService.ExistMember(groupNo, loginUID)
	if err != nil {
		t.Error("检查群成员失败", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if !isMember {
		c.ResponseError(errors.New("not a group member"))
		return
	}

	members, err := t.service.GetMembers(groupNo, shortID)
	if err != nil {
		t.Error("获取成员列表失败", zap.Error(err), zap.String("groupNo", groupNo))
		c.ResponseError(err)
		return
	}
	c.Response(members)
}

// joinThread 加入子区
// POST /v1/groups/:group_no/threads/:short_id/join
func (t *Thread) joinThread(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	// 验证参数格式
	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	err := t.service.JoinThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("加入子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// leaveThread 离开子区
// POST /v1/groups/:group_no/threads/:short_id/leave
func (t *Thread) leaveThread(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	// 验证参数格式
	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	err := t.service.LeaveThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("离开子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// deleteThread 删除子区
// DELETE /v1/groups/:group_no/threads/:short_id
func (t *Thread) deleteThread(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	// 验证参数格式
	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	err := t.service.DeleteThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("删除子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// ==================== 子区 GROUP.md ====================

// threadMdResp 子区 GROUP.md 响应
type threadMdResp struct {
	Content   string     `json:"content"`
	Version   int64      `json:"version"`
	UpdatedAt *time.Time `json:"updated_at"`
	UpdatedBy string     `json:"updated_by"`
}

// threadMdGet 获取子区 GROUP.md
func (t *Thread) threadMdGet(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	// 权限：必须是父群成员
	isMember, err := t.groupService.ExistMember(groupNo, loginUID)
	if err != nil {
		t.Error("check group member failed", zap.Error(err))
		c.ResponseError(errors.New("check group member failed"))
		return
	}
	if !isMember {
		c.ResponseError(errors.New("no permission"))
		return
	}

	result, err := t.service.GetThreadMd(groupNo, shortID)
	if err != nil {
		t.Error("query thread GROUP.md failed", zap.Error(err))
		c.ResponseError(errors.New("query thread GROUP.md failed"))
		return
	}
	if result == nil {
		c.Response(threadMdResp{
			Content:   "",
			Version:   0,
			UpdatedAt: nil,
			UpdatedBy: "",
		})
		return
	}
	c.Response(threadMdResp{
		Content:   result.Content,
		Version:   result.Version,
		UpdatedAt: result.UpdatedAt,
		UpdatedBy: result.UpdatedBy,
	})
}

// threadMdUpdate 更新子区 GROUP.md
func (t *Thread) threadMdUpdate(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}

	// 校验空内容
	if strings.TrimSpace(req.Content) == "" {
		c.ResponseError(errors.New("content must not be empty"))
		return
	}

	maxSize := group.GetGroupMdMaxSize()
	if len(req.Content) > maxSize {
		c.ResponseError(fmt.Errorf("GROUP.md content exceeds max size %d bytes", maxSize))
		return
	}

	// 先检查子区是否存在，避免 canOperate 对不存在子区返回 "no permission"
	existThread, err := t.service.ExistThread(groupNo, shortID)
	if err != nil {
		t.Error("check thread existence failed", zap.Error(err))
		c.ResponseError(errors.New("check thread existence failed"))
		return
	}
	if !existThread {
		c.ResponseError(errors.New("thread not found"))
		return
	}

	// 权限检查在 API Handler 层完成
	canEdit, err := t.service.CanEditThreadMd(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("check edit permission failed", zap.Error(err))
		c.ResponseError(errors.New("check edit permission failed"))
		return
	}
	if !canEdit {
		c.ResponseError(errors.New("no permission to edit thread GROUP.md"))
		return
	}

	// Service 层：纯数据操作透传
	newVersion, err := t.service.UpdateThreadMd(groupNo, shortID, req.Content, loginUID)
	if err != nil {
		t.Error("update thread GROUP.md failed", zap.Error(err))
		c.ResponseError(errors.New("update thread GROUP.md failed"))
		return
	}

	// 异步发送通知
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Error("sendThreadMdNotification panic", zap.Any("recover", r))
			}
		}()
		t.sendThreadMdNotification(groupNo, shortID, loginUID, newVersion, "thread_md_updated", "Thread GROUP.md updated")
	}()

	c.Response(map[string]interface{}{
		"version": newVersion,
	})
}

// threadMdDelete 删除子区 GROUP.md
func (t *Thread) threadMdDelete(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	if !IsValidGroupNo(groupNo) {
		c.ResponseError(errors.New("invalid group_no format"))
		return
	}
	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	// 先检查子区是否存在，避免 canOperate 对不存在子区返回 "no permission"
	existThread, err := t.service.ExistThread(groupNo, shortID)
	if err != nil {
		t.Error("check thread existence failed", zap.Error(err))
		c.ResponseError(errors.New("check thread existence failed"))
		return
	}
	if !existThread {
		c.ResponseError(errors.New("thread not found"))
		return
	}

	// 权限检查在 API Handler 层完成
	canEdit, err := t.service.CanEditThreadMd(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("check edit permission failed", zap.Error(err))
		c.ResponseError(errors.New("check edit permission failed"))
		return
	}
	if !canEdit {
		c.ResponseError(errors.New("no permission to delete thread GROUP.md"))
		return
	}

	// Service 层：纯数据操作透传
	newVersion, err := t.service.DeleteThreadMd(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("delete thread GROUP.md failed", zap.Error(err))
		c.ResponseError(errors.New("delete thread GROUP.md failed"))
		return
	}

	// 异步发送通知
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Error("sendThreadMdNotification panic", zap.Any("recover", r))
			}
		}()
		t.sendThreadMdNotification(groupNo, shortID, loginUID, newVersion, "thread_md_deleted", "Thread GROUP.md deleted")
	}()

	c.ResponseOK()
}

// sendThreadMdNotification 发送子区 GROUP.md 变更通知
func (t *Thread) sendThreadMdNotification(groupNo, shortID, updatedBy string, version int64, eventType, contentText string) {
	// 查询父群内所有 Bot 成员
	botUIDs, err := t.groupService.GetBotMemberUIDs(groupNo)
	if err != nil {
		t.Error("query bot member UIDs failed", zap.Error(err))
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

	channelID := BuildChannelID(groupNo, shortID)
	err = t.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 0,
		},
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		FromUID:     updatedBy,
		Payload:     []byte(util.ToJson(payload)),
	})
	if err != nil {
		t.Error("send thread GROUP.md notification failed", zap.Error(err))
	}
}

// ========== 简化路由（通过 short_id 查询 group_no）==========

// joinThreadSimple 加入子区（简化路由）
// POST /v1/threads/:short_id/join
func (t *Thread) joinThreadSimple(c *wkhttp.Context) {
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	// 通过 short_id 查询 group_no
	thread, err := t.db.QueryByShortID(shortID)
	if err != nil {
		t.Error("查询子区失败", zap.Error(err))
		c.ResponseError(errors.New("thread not found"))
		return
	}
	if thread == nil {
		c.ResponseError(errors.New("thread not found"))
		return
	}

	err = t.service.JoinThread(thread.GroupNo, shortID, loginUID)
	if err != nil {
		t.Error("加入子区失败", zap.Error(err), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// leaveThreadSimple 离开子区（简化路由）
// POST /v1/threads/:short_id/leave
func (t *Thread) leaveThreadSimple(c *wkhttp.Context) {
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	thread, err := t.db.QueryByShortID(shortID)
	if err != nil || thread == nil {
		c.ResponseError(errors.New("thread not found"))
		return
	}

	err = t.service.LeaveThread(thread.GroupNo, shortID, loginUID)
	if err != nil {
		t.Error("离开子区失败", zap.Error(err), zap.String("shortID", shortID))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// getThreadSimple 获取子区详情（简化路由）
// GET /v1/threads/:short_id
func (t *Thread) getThreadSimple(c *wkhttp.Context) {
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	if !IsValidShortID(shortID) {
		c.ResponseError(errors.New("invalid short_id format"))
		return
	}

	thread, err := t.db.QueryByShortID(shortID)
	if err != nil || thread == nil {
		c.ResponseError(errors.New("thread not found"))
		return
	}

	// 验证是父群成员
	isMember, err := t.groupService.ExistMember(thread.GroupNo, loginUID)
	if err != nil {
		c.ResponseError(err)
		return
	}
	if !isMember {
		c.ResponseError(errors.New("not a group member"))
		return
	}

	resp, err := t.service.GetThread(thread.GroupNo, shortID)
	if err != nil {
		c.ResponseError(err)
		return
	}
	c.Response(resp)
}
