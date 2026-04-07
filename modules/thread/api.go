package thread

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
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
	return &Thread{
		ctx:          ctx,
		db:           NewDB(ctx),
		service:      NewService(ctx),
		groupService: group.NewService(ctx),
		Log:          log.NewTLog("Thread"),
	}
}

// Route 注册路由
func (t *Thread) Route(r *wkhttp.WKHttp) {
	threads := r.Group("/v1/groups/:group_no/threads", t.ctx.AuthMiddleware(r))
	{
		threads.POST("", t.createThread)
		threads.GET("", t.listThreads)
		threads.GET("/:short_id", t.getThread)
		threads.GET("/:short_id/members", t.listMembers)
		threads.POST("/:short_id/join", t.joinThread)
		threads.POST("/:short_id/leave", t.leaveThread)
		threads.POST("/:short_id/archive", t.archiveThread)
		threads.POST("/:short_id/unarchive", t.unarchiveThread)
		threads.DELETE("/:short_id", t.deleteThread)
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
		Name            string `json:"name" binding:"required,max=100"`
		SourceMessageID *int64 `json:"source_message_id"`
	}
	if err := c.BindJSON(&req); err != nil {
		t.Error("参数错误", zap.Error(err))
		c.ResponseError(errors.New("invalid request: name is required"))
		return
	}

	resp, err := t.service.CreateThread(&CreateThreadReq{
		GroupNo:         groupNo,
		Name:            req.Name,
		CreatorUID:      loginUID,
		CreatorName:     loginName,
		SourceMessageID: req.SourceMessageID,
	})
	if err != nil {
		t.Error("创建子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("uid", loginUID))
		c.ResponseError(err)
		return
	}
	c.Response(resp)
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

	threads, err := t.service.GetThreads(groupNo)
	if err != nil {
		t.Error("获取子区列表失败", zap.Error(err), zap.String("groupNo", groupNo))
		c.ResponseError(err)
		return
	}
	c.Response(threads)
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
