package thread

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
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

		// 收到消息：DB 层在行锁内决定是否解档。
		// GenSeq 通过回调传入，只在锁内且确认当前是 archived 时才调用，确保版本号
		// 严格晚于 cron 的版本号（GenSeq 全局单调），避免 thread.version 倒退。
		// active 子区收消息走纯统计路径，不消耗 GenSeq。
		content := parsePayloadContent(msg.Payload)
		if runeLen := len([]rune(content)); runeLen > 500 {
			content = string([]rune(content)[:500])
		}
		if err := t.db.RecordMessageAndReactivate(shortID, content, msg.FromUID, func() (int64, error) {
			return t.ctx.GenSeq(ThreadSeqKey)
		}); err != nil {
			t.Error("更新消息统计/解档失败", zap.Error(err), zap.String("shortID", shortID))
		}

		// 发送者不是子区成员，自动加入
		if msg.FromUID != "" {
			if err := t.service.JoinThread(groupNo, shortID, msg.FromUID); err != nil {
				t.Error("自动加入子区失败", zap.Error(err), zap.String("uid", msg.FromUID))
			}
		}
	}
}

func respondThreadError(c *wkhttp.Context, code codes.Code, details i18n.Details) {
	httperr.ResponseErrorL(c, code, nil, details)
}

func respondThreadInvalidGroupNo(c *wkhttp.Context) {
	respondThreadError(c, errcode.ErrThreadGroupNoInvalid, i18n.Details{"field": "group_no"})
}

func respondThreadInvalidShortID(c *wkhttp.Context) {
	respondThreadError(c, errcode.ErrThreadShortIDInvalid, i18n.Details{"field": "short_id"})
}

func respondThreadInvalidName(c *wkhttp.Context) {
	respondThreadError(c, errcode.ErrThreadNameInvalid, i18n.Details{
		"field":      "name",
		"max_length": 100,
	})
}

func respondThreadInvalidRequest(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	respondThreadError(c, errcode.ErrThreadRequestInvalid, details)
}

func respondThreadSourceMessageInvalid(c *wkhttp.Context, details i18n.Details) {
	respondThreadError(c, errcode.ErrThreadSourceMessageInvalid, details)
}

func respondThreadServiceError(c *wkhttp.Context, err error) {
	respondThreadError(c, classifyThreadError(err), nil)
}

func classifyThreadError(err error) codes.Code {
	if err == nil {
		return errcode.ErrThreadStoreFailed
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not a group member"):
		return errcode.ErrThreadNotGroupMember
	case strings.Contains(msg, "no permission"):
		return errcode.ErrThreadPermissionDenied
	case strings.Contains(msg, "creator cannot leave"):
		return errcode.ErrThreadCreatorCannotLeave
	case strings.Contains(msg, "thread status changed concurrently"):
		return errcode.ErrThreadStatusChanged
	case strings.Contains(msg, "thread is not active"):
		return errcode.ErrThreadNotActive
	// Check "not found" before "deleted": the DB layer returns the ambiguous
	// "thread not found or already deleted" when affected=0, which we map to
	// 404 NotFound (the more accurate default for clients hitting unknown IDs).
	// The explicit service-layer "thread has been deleted" still maps to 410.
	case strings.Contains(msg, "thread not found"):
		return errcode.ErrThreadNotFound
	case strings.Contains(msg, "thread has been deleted"):
		return errcode.ErrThreadDeleted
	case strings.Contains(msg, "name is required"):
		return errcode.ErrThreadNameInvalid
	case strings.Contains(msg, "invalid mute"), strings.Contains(msg, "mute must"):
		return errcode.ErrThreadSettingInvalid
	default:
		return errcode.ErrThreadStoreFailed
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
		respondThreadInvalidGroupNo(c)
		return
	}

	var req struct {
		Name                 string          `json:"name" binding:"required,max=100"`
		SourceMessageID      *int64          `json:"source_message_id"`
		SourceMessagePayload json.RawMessage `json:"source_message_payload"`
	}
	if err := c.BindJSON(&req); err != nil {
		t.Error("参数错误", zap.Error(err))
		respondThreadInvalidName(c)
		return
	}

	// 校验 source_message_payload
	if len(req.SourceMessagePayload) > 0 {
		if req.SourceMessageID == nil {
			respondThreadSourceMessageInvalid(c, i18n.Details{"field": "source_message_id"})
			return
		}
		if len(req.SourceMessagePayload) > maxSourcePayloadBytes {
			respondThreadSourceMessageInvalid(c, i18n.Details{
				"field":    "source_message_payload",
				"max_size": maxSourcePayloadBytes,
			})
			return
		}
		if !json.Valid(req.SourceMessagePayload) || string(req.SourceMessagePayload) == "null" {
			respondThreadSourceMessageInvalid(c, i18n.Details{"field": "source_message_payload"})
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
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	var req struct {
		Name string `json:"name" binding:"required,max=100"`
	}
	if err := c.BindJSON(&req); err != nil {
		t.Error("参数错误", zap.Error(err))
		respondThreadInvalidName(c)
		return
	}

	err := t.service.UpdateName(groupNo, shortID, loginUID, req.Name)
	if err != nil {
		t.Error("修改子区名称失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	var req map[string]interface{}
	if err := c.BindJSON(&req); err != nil {
		t.Error("参数错误", zap.Error(err))
		respondThreadInvalidRequest(c, "body")
		return
	}
	if len(req) == 0 {
		c.ResponseOK()
		return
	}

	if err := t.service.UpdateSetting(groupNo, shortID, loginUID, req); err != nil {
		t.Error("更新子区设置失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}

	// 验证是活跃父群成员（排除黑名单，被拉黑用户不应越权读子区内容）
	isMember, err := t.groupService.ExistMemberActive(groupNo, loginUID)
	if err != nil {
		t.Error("检查群成员失败", zap.Error(err))
		respondThreadServiceError(c, err)
		return
	}
	if !isMember {
		respondThreadError(c, errcode.ErrThreadNotGroupMember, nil)
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

	// ?status= 决定返回子区状态集合：不传或 active=只看活跃；archived=只看已归档；
	// all=活跃+已归档（不含 deleted）。前端"已归档"入口走 status=archived。
	statuses, err := parseListThreadStatuses(c.Query("status"))
	if err != nil {
		respondThreadError(c, errcode.ErrThreadStatusInvalid, i18n.Details{"field": "status"})
		return
	}

	threads, total, err := t.service.GetThreads(groupNo, statuses, pageIndex, pageSize)
	if err != nil {
		t.Error("获取子区列表失败", zap.Error(err), zap.String("groupNo", groupNo))
		respondThreadServiceError(c, err)
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

// parseListThreadStatuses 解析 listThreads 的 ?status= 入参。
// 不传/active → [active]，archived → [archived]，all → [active, archived]。
// 其它任何值返回 InvalidArgument 错误，避免静默退化为默认值放大客户端 bug。
func parseListThreadStatuses(raw string) ([]int, error) {
	switch raw {
	case "", ListStatusActive:
		return []int{ThreadStatusActive}, nil
	case ListStatusArchived:
		return []int{ThreadStatusArchived}, nil
	case ListStatusAll:
		return []int{ThreadStatusActive, ThreadStatusArchived}, nil
	default:
		// 不把 raw 原样回显，避免控制字符 / 超长输入污染 JSON 错误体 / 日志。
		return nil, errors.New("invalid status: must be active, archived, or all")
	}
}

// getThread 获取子区详情
// GET /v1/groups/:group_no/threads/:short_id
func (t *Thread) getThread(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	shortID := c.Param("short_id")
	loginUID := c.GetLoginUID()

	// 验证参数格式
	if !IsValidGroupNo(groupNo) {
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	// 验证是活跃父群成员（排除黑名单，被拉黑用户不应越权读子区内容）
	isMember, err := t.groupService.ExistMemberActive(groupNo, loginUID)
	if err != nil {
		t.Error("检查群成员失败", zap.Error(err))
		respondThreadServiceError(c, err)
		return
	}
	if !isMember {
		respondThreadError(c, errcode.ErrThreadNotGroupMember, nil)
		return
	}

	thread, err := t.service.GetThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("获取子区详情失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	err := t.service.ArchiveThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("归档子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	err := t.service.UnarchiveThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("取消归档失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	// 验证是活跃父群成员（排除黑名单，被拉黑用户不应越权读子区内容）
	isMember, err := t.groupService.ExistMemberActive(groupNo, loginUID)
	if err != nil {
		t.Error("检查群成员失败", zap.Error(err))
		respondThreadServiceError(c, err)
		return
	}
	if !isMember {
		respondThreadError(c, errcode.ErrThreadNotGroupMember, nil)
		return
	}

	members, err := t.service.GetMembers(groupNo, shortID)
	if err != nil {
		t.Error("获取成员列表失败", zap.Error(err), zap.String("groupNo", groupNo))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	err := t.service.JoinThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("加入子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	err := t.service.LeaveThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("离开子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	err := t.service.DeleteThread(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("删除子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	// 权限：必须是活跃父群成员（排除黑名单，threadMdGet 返回 GROUP.md 正文，防越权读）
	isMember, err := t.groupService.ExistMemberActive(groupNo, loginUID)
	if err != nil {
		t.Error("check group member failed", zap.Error(err))
		respondThreadError(c, errcode.ErrThreadStoreFailed, nil)
		return
	}
	if !isMember {
		respondThreadError(c, errcode.ErrThreadPermissionDenied, nil)
		return
	}

	result, err := t.service.GetThreadMd(groupNo, shortID)
	if err != nil {
		t.Error("query thread GROUP.md failed", zap.Error(err))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondThreadInvalidRequest(c, "body")
		return
	}

	// 校验空内容
	if strings.TrimSpace(req.Content) == "" {
		respondThreadError(c, errcode.ErrThreadGroupMDContentEmpty, i18n.Details{"field": "content"})
		return
	}

	maxSize := group.GetGroupMdMaxSize()
	if len(req.Content) > maxSize {
		respondThreadError(c, errcode.ErrThreadGroupMDContentTooLarge, i18n.Details{
			"field":    "content",
			"max_size": maxSize,
		})
		return
	}

	// 先检查子区是否存在，避免 canOperate 对不存在子区返回 "no permission"
	existThread, err := t.service.ExistThread(groupNo, shortID)
	if err != nil {
		t.Error("check thread existence failed", zap.Error(err))
		respondThreadError(c, errcode.ErrThreadStoreFailed, nil)
		return
	}
	if !existThread {
		respondThreadError(c, errcode.ErrThreadNotFound, nil)
		return
	}

	// 权限检查在 API Handler 层完成
	canEdit, err := t.service.CanEditThreadMd(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("check edit permission failed", zap.Error(err))
		respondThreadError(c, errcode.ErrThreadStoreFailed, nil)
		return
	}
	if !canEdit {
		respondThreadError(c, errcode.ErrThreadPermissionDenied, nil)
		return
	}

	// Service 层：纯数据操作透传
	newVersion, err := t.service.UpdateThreadMd(groupNo, shortID, req.Content, loginUID)
	if err != nil {
		t.Error("update thread GROUP.md failed", zap.Error(err))
		respondThreadError(c, errcode.ErrThreadStoreFailed, nil)
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
		respondThreadInvalidGroupNo(c)
		return
	}
	if !IsValidShortID(shortID) {
		respondThreadInvalidShortID(c)
		return
	}

	// 先检查子区是否存在，避免 canOperate 对不存在子区返回 "no permission"
	existThread, err := t.service.ExistThread(groupNo, shortID)
	if err != nil {
		t.Error("check thread existence failed", zap.Error(err))
		respondThreadError(c, errcode.ErrThreadStoreFailed, nil)
		return
	}
	if !existThread {
		respondThreadError(c, errcode.ErrThreadNotFound, nil)
		return
	}

	// 权限检查在 API Handler 层完成
	canEdit, err := t.service.CanEditThreadMd(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("check edit permission failed", zap.Error(err))
		respondThreadError(c, errcode.ErrThreadStoreFailed, nil)
		return
	}
	if !canEdit {
		respondThreadError(c, errcode.ErrThreadPermissionDenied, nil)
		return
	}

	// Service 层：纯数据操作透传
	newVersion, err := t.service.DeleteThreadMd(groupNo, shortID, loginUID)
	if err != nil {
		t.Error("delete thread GROUP.md failed", zap.Error(err))
		respondThreadError(c, errcode.ErrThreadStoreFailed, nil)
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
		respondThreadInvalidShortID(c)
		return
	}

	// 通过 short_id 查询 group_no
	thread, err := t.db.QueryByShortID(shortID)
	if err != nil {
		t.Error("查询子区失败", zap.Error(err))
		respondThreadError(c, errcode.ErrThreadNotFound, nil)
		return
	}
	if thread == nil {
		respondThreadError(c, errcode.ErrThreadNotFound, nil)
		return
	}

	err = t.service.JoinThread(thread.GroupNo, shortID, loginUID)
	if err != nil {
		t.Error("加入子区失败", zap.Error(err), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidShortID(c)
		return
	}

	thread, err := t.db.QueryByShortID(shortID)
	if err != nil || thread == nil {
		respondThreadError(c, errcode.ErrThreadNotFound, nil)
		return
	}

	err = t.service.LeaveThread(thread.GroupNo, shortID, loginUID)
	if err != nil {
		t.Error("离开子区失败", zap.Error(err), zap.String("shortID", shortID))
		respondThreadServiceError(c, err)
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
		respondThreadInvalidShortID(c)
		return
	}

	thread, err := t.db.QueryByShortID(shortID)
	if err != nil || thread == nil {
		respondThreadError(c, errcode.ErrThreadNotFound, nil)
		return
	}

	// 验证是活跃父群成员（排除黑名单）
	isMember, err := t.groupService.ExistMemberActive(thread.GroupNo, loginUID)
	if err != nil {
		respondThreadServiceError(c, err)
		return
	}
	if !isMember {
		respondThreadError(c, errcode.ErrThreadNotGroupMember, nil)
		return
	}

	resp, err := t.service.GetThread(thread.GroupNo, shortID, loginUID)
	if err != nil {
		respondThreadServiceError(c, err)
		return
	}
	c.Response(resp)
}
