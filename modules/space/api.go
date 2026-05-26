package space

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	rd "github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// Space 团队空间API
type Space struct {
	ctx *config.Context
	log.Log
	db  *DB
	mdb *managerDB // 用户侧需要复用管理端按 space_id 作用域的邀请码写操作时使用
}

// New 创建Space实例
func New(ctx *config.Context) *Space {
	return &Space{
		ctx: ctx,
		Log: log.NewTLog("Space"),
		db:  NewDB(ctx),
		mdb: newManagerDB(ctx.DB()),
	}
}

// checkSpaceActive 检查空间是否处于活跃状态，返回 true 表示已处理错误（空间不活跃）
func (s *Space) checkSpaceActive(c *wkhttp.Context, spaceId string) bool {
	active, err := s.db.isSpaceActive(spaceId)
	if err != nil {
		c.ResponseError(errors.New("查询空间状态失败"))
		return true
	}
	if !active {
		c.ResponseError(errors.New("空间不存在或已解散"))
		return true
	}
	return false
}

// Route 路由配置
func (s *Space) Route(r *wkhttp.WKHttp) {
	// 启动时加载已知 spaceId 到 ParseChannelID 缓存
	s.loadKnownSpaceIDs()

	auth := r.Group("/v1/space", s.ctx.AuthMiddleware(r))
	{
		auth.POST("/create", s.createSpace)
		auth.GET("/my", s.mySpaces)
		auth.POST("/join", s.joinSpace)

		auth.GET("/:space_id", s.getSpace)
		auth.PUT("/:space_id", s.updateSpace)
		auth.DELETE("/:space_id", s.disbandSpace)

		auth.GET("/:space_id/members", s.listMembers)
		auth.POST("/:space_id/members/add", s.addMembers)
		auth.POST("/:space_id/members/remove", s.removeMembers)
		auth.POST("/:space_id/leave", s.leaveSpace)
		auth.PUT("/:space_id/members/:uid/role", s.updateMemberRole)

		auth.POST("/:space_id/invite", s.createInvite)
		auth.PUT("/:space_id/invite/:code", s.updateInvite)
		auth.DELETE("/:space_id/invite/:code", s.deleteInvite)
		auth.GET("/:space_id/invites", s.listInvites)

		auth.GET("/:space_id/join-applies", s.joinApplies)
		auth.POST("/:space_id/join-applies/:id/approve", s.approveJoinApply)
		auth.POST("/:space_id/join-applies/:id/reject", s.rejectJoinApply)

		auth.POST("/:space_id/email-invites", s.createMemberEmailInvite)
		auth.GET("/:space_id/email-invites", s.listMemberEmailInvites)
		auth.DELETE("/:space_id/email-invites/:id", s.revokeMemberEmailInvite)
	}

	// 邀请码预览端点（公开无认证）严格 per-IP 限流：防枚举 + 暴破（issue #1000）。
	// 两个端点共享同一 limiter，使同一 IP 跨端点总配额受控。
	// 阈值与 user 模块 login 同档（10 req/min, burst 5），详见 PR #1090。
	// PoolSize=10：Lua 脚本短事务，与 user 模块 / main.go 保持一致。
	rlRedis := rd.NewClient(octoredis.MustBuildOptions(s.ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	}))
	invitePreviewLimit := appwkhttp.StrictIPRateLimitMiddleware(context.Background(), rlRedis, "space_invite", 10.0/60, 5)

	open := r.Group("/v1/space")
	{
		open.GET("/invite/:invite_code", invitePreviewLimit, s.getInviteInfo)
		open.GET("/invite/:invite_code/preview", invitePreviewLimit, s.getInvitePreview)
		open.GET("/email-invite", invitePreviewLimit, s.emailInvitePage)
		open.GET("/email-invite/:token", invitePreviewLimit, s.previewEmailInvite)
		open.GET("/join-approve", s.joinApprovePage)
		open.GET("/join-approve/detail", s.joinApproveDetail)
		open.POST("/join-approve/sure", s.joinApproveSure)
	}

	// 已登录的接受端点：复用 auth 中间件 + 同一 invitePreviewLimit（防穷举 token 撞库）。
	authAccept := r.Group("/v1/space", invitePreviewLimit, s.ctx.AuthMiddleware(r))
	{
		authAccept.POST("/email-invite/:token/accept", s.acceptEmailInvite)
	}
}

// envDisableUserCreateSpace 全局开关：运维通过环境变量 DM_SPACE_DISABLE_USER_CREATE=true
// 关闭用户侧创建空间入口（POST /v1/space/create）。管理端代建接口不受此开关约束。
const envDisableUserCreateSpace = "DM_SPACE_DISABLE_USER_CREATE"

// IsUserCreateDisabled 是否已通过环境变量关闭用户侧创建空间。
func IsUserCreateDisabled() bool {
	v := strings.TrimSpace(os.Getenv(envDisableUserCreateSpace))
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// createSpaceParams 创建空间的核心参数。Creator 为目标空间 owner，
// 管理端代建时设为被代建用户，业务端为登录用户。
type createSpaceParams struct {
	Creator        string
	Name           string
	Description    string
	Logo           string
	JoinMode       int
	MaxUsers       int
	PresetGroupIds *string
}

// createSpaceResult 创建空间的结果。InviteCode 为空代表邀请码创建失败（不致命）。
type createSpaceResult struct {
	SpaceID    string
	InviteCode string
}

// createSpace 创建空间（用户侧入口）
func (s *Space) createSpace(c *wkhttp.Context) {
	if IsUserCreateDisabled() {
		c.ResponseErrorWithStatus(errors.New("管理员已关闭空间创建功能"), http.StatusForbidden)
		return
	}
	loginUID := c.GetLoginUID()
	var req createSpaceReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if req.Name == "" {
		c.ResponseError(errors.New("空间名称不能为空"))
		return
	}
	if req.JoinMode < JoinModeDirect || req.JoinMode > JoinModeApproval {
		c.ResponseError(errors.New("无效的加入模式，仅支持 0(直接加入) 或 1(需要审批)"))
		return
	}

	result, err := s.createSpaceCore(createSpaceParams{
		Creator:     loginUID,
		Name:        req.Name,
		Description: req.Description,
		Logo:        req.Logo,
		JoinMode:    req.JoinMode,
	})
	if err != nil {
		s.Error("创建空间失败", zap.Error(err), zap.String("loginUID", loginUID))
		c.ResponseError(errors.New("创建空间失败"))
		return
	}

	c.Response(map[string]interface{}{
		"space_id":    result.SpaceID,
		"name":        req.Name,
		"description": req.Description,
		"logo":        req.Logo,
		"join_mode":   req.JoinMode,
	})
}

// createSpaceCore 创建空间事务核心（供用户侧与管理端代建复用）。
//
// 事务内写 space + owner 成员；事务外建邀请码、BotFather 入驻、刷新 ParseChannelID 缓存、
// 触发 SpaceMemberJoin 事件。与原先 createSpace 行为保持一致。
func (s *Space) createSpaceCore(p createSpaceParams) (*createSpaceResult, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	spaceId, err := s.createSpaceCoreTx(tx, p)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交事务失败: %w", err)
	}
	return s.createSpaceCorePostCommit(spaceId, p), nil
}

// createSpaceCoreTx 在传入事务内写入 space 行 + owner 成员行；不提交、不触发事务外副作用。
// 调用方负责提交事务并随后调用 createSpaceCorePostCommit 完成默认邀请码、BotFather、缓存与事件。
// 用于 email-invite accept 路径在同一事务内完成 token 消费与空间创建（issue #1138）。
func (s *Space) createSpaceCoreTx(tx *dbr.Tx, p createSpaceParams) (string, error) {
	spaceId := util.GenerUUID()
	model := &SpaceModel{
		SpaceId:        spaceId,
		Name:           p.Name,
		Description:    p.Description,
		Logo:           p.Logo,
		Creator:        p.Creator,
		JoinMode:       p.JoinMode,
		MaxUsers:       p.MaxUsers,
		PresetGroupIds: p.PresetGroupIds,
		Status:         SpaceStatusNormal,
	}
	if err := s.db.insertSpace(model, tx); err != nil {
		return "", fmt.Errorf("创建空间失败: %w", err)
	}
	if err := s.db.insertMember(&MemberModel{
		SpaceId: spaceId,
		UID:     p.Creator,
		Role:    2, // owner
		Status:  1,
	}, tx); err != nil {
		return "", fmt.Errorf("添加空间成员失败: %w", err)
	}
	return spaceId, nil
}

// createSpaceCorePostCommit 完成事务外副作用：创建默认邀请码、BotFather 入驻、刷新缓存、触发事件。
// 失败不会向上传播（默认邀请码失败仅记 warn）。
func (s *Space) createSpaceCorePostCommit(spaceId string, p createSpaceParams) *createSpaceResult {
	result := &createSpaceResult{SpaceID: spaceId}
	inviteModel := &InvitationModel{
		SpaceId: spaceId,
		Creator: p.Creator,
		Status:  1,
	}
	applyAutoInviteDefaults(inviteModel, time.Now())
	if code, inviteErr := s.insertInvitationWithRetry(inviteModel); inviteErr == nil {
		result.InviteCode = code
	} else {
		s.Warn("创建默认邀请码失败", zap.Error(inviteErr), zap.String("spaceId", spaceId))
	}

	_ = s.db.insertMemberIgnore(&MemberModel{
		SpaceId: spaceId,
		UID:     "botfather",
		Role:    0,
		Status:  1,
	})

	// Provision notify bot for new Space (async, non-blocking)
	if event.NotifyBotProvisioner != nil {
		event.NotifyBotProvisioner(spaceId, p.Name)
	}

	// 为创建者补齐默认分类（GH octo-server#1228）。失败不阻塞空间创建，list 端有兜底。
	ensureDefaultCategoryProvisioned(s.ctx, p.Creator, spaceId, s)

	go s.loadKnownSpaceIDs()
	go s.fireSpaceMemberJoinEvent(p.Creator, spaceId)

	return result
}

// getSpace 获取空间详情
func (s *Space) getSpace(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}

	detail, err := s.db.querySpaceDetail(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询空间失败"))
		return
	}
	if detail == nil {
		c.ResponseError(errors.New("空间不存在"))
		return
	}

	if detail.Role < 0 {
		c.ResponseError(errors.New("你不是该空间成员"))
		return
	}

	c.Response(spaceResp{
		SpaceId:     detail.SpaceId,
		Name:        detail.Name,
		Description: detail.Description,
		Logo:        detail.Logo,
		Creator:     detail.Creator,
		Status:      detail.Status,
		Role:        detail.Role,
		MaxUsers:    detail.MaxUsers,
		MemberCount: detail.MemberCount,
		JoinMode:    detail.JoinMode,
		CreatedAt:   detail.CreatedAt.String(),
		UpdatedAt:   detail.UpdatedAt.String(),
	})
}

// userSpacePresetGroupIdsMaxBytes preset_group_ids 列为 TEXT (≤65535 字节)。
// 校验取整数 sanity cap，超过即拒，避免把 driver-level 错误透传给用户。
const userSpacePresetGroupIdsMaxBytes = 65535

// updateSpace 用户侧修改空间基础信息（owner / admin 自服务）。
//
// 全字段部分更新：nil 字段不变更；至少需要提供一个字段。
// 复用管理端 managerDB.updateSpaceProfile 的事务 + SELECT ... FOR UPDATE 实现
// 保证存在性 / 状态校验与 UPDATE 同事务串行化，关闭 handler guard 与并发解散之间的
// TOCTOU 窗口；max_users 不在用户侧暴露（仍由管理端控制）。
func (s *Space) updateSpace(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	if s.checkSpaceActive(c, spaceId) {
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限修改空间信息"))
		return
	}

	var req updateSpaceReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if req.Name == nil && req.Description == nil && req.Logo == nil && req.JoinMode == nil && req.PresetGroupIds == nil {
		c.ResponseError(errors.New("至少需要提供 name / description / logo / join_mode / preset_group_ids 之一"))
		return
	}

	// 字段级校验：长度单位为字符（rune），与 MySQL VARCHAR(N) 的字符语义一致。
	// 复用管理端常量（同包，含义完全相同），避免双写漂移。
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			c.ResponseError(errors.New("空间名称不能为空"))
			return
		}
		if utf8.RuneCountInString(trimmed) > managerSpaceNameMaxChars {
			c.ResponseError(fmt.Errorf("空间名称不能超过 %d 个字符", managerSpaceNameMaxChars))
			return
		}
		req.Name = &trimmed
	}
	if req.Description != nil {
		trimmed := strings.TrimSpace(*req.Description)
		if utf8.RuneCountInString(trimmed) > managerSpaceDescriptionMaxChars {
			c.ResponseError(fmt.Errorf("空间描述不能超过 %d 个字符", managerSpaceDescriptionMaxChars))
			return
		}
		req.Description = &trimmed
	}
	if req.Logo != nil && utf8.RuneCountInString(*req.Logo) > managerSpaceLogoMaxChars {
		c.ResponseError(fmt.Errorf("Logo 不能超过 %d 个字符", managerSpaceLogoMaxChars))
		return
	}
	if req.JoinMode != nil && (*req.JoinMode < JoinModeDirect || *req.JoinMode > JoinModeApproval) {
		c.ResponseError(errors.New("无效的加入模式，仅支持 0(直接加入) 或 1(需要审批)"))
		return
	}
	if req.PresetGroupIds != nil {
		if err := validatePresetGroupIds(*req.PresetGroupIds); err != nil {
			c.ResponseError(err)
			return
		}
	}

	before, err := s.mdb.updateSpaceProfile(spaceId, req.Name, req.Description, req.Logo, req.JoinMode, nil, req.PresetGroupIds)
	if err != nil {
		// 复用管理端 sentinel，并发解散与 active 检查之间的 race 由事务侧裁决。
		if errors.Is(err, ErrSpaceNotFound) {
			c.ResponseError(errors.New("空间不存在"))
			return
		}
		if errors.Is(err, ErrSpaceDisbandedForUpdate) {
			c.ResponseError(errors.New("空间已解散，无法修改空间信息"))
			return
		}
		s.Error("用户修改空间信息失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("operator", loginUID))
		c.ResponseError(errors.New("更新空间信息失败"))
		return
	}

	// 审计日志：from 取自事务内锁定时刻的快照，避免并发更新导致 stale；
	// 字段命名与管理端 m.Info("管理员修改空间信息", ...) 对齐，便于运维统一查询。
	fields := []zap.Field{
		zap.String("spaceId", spaceId),
		zap.String("operator", loginUID),
		zap.Int("role", member.Role),
	}
	if req.Name != nil {
		fields = append(fields, zap.String("nameFrom", before.Name), zap.String("nameTo", *req.Name))
	}
	if req.Description != nil {
		fields = append(fields, zap.String("descFrom", before.Description), zap.String("descTo", *req.Description))
	}
	if req.Logo != nil {
		fields = append(fields, zap.String("logoFrom", before.Logo), zap.String("logoTo", *req.Logo))
	}
	if req.JoinMode != nil {
		fields = append(fields, zap.Int("joinModeFrom", before.JoinMode), zap.Int("joinModeTo", *req.JoinMode))
	}
	if req.PresetGroupIds != nil {
		fields = append(fields,
			zap.String("presetGroupIdsFrom", derefStringOr(before.PresetGroupIds, "")),
			zap.String("presetGroupIdsTo", *req.PresetGroupIds),
		)
	}
	s.Info("用户修改空间信息", fields...)
	c.ResponseOK()
}

// derefStringOr 安全解引用 *string，nil 时返回 fallback。
// 审计日志专用：preset_group_ids 是 *string，from 值可能为 nil。
func derefStringOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// validatePresetGroupIds 校验 preset_group_ids 的字符串载荷。
//
// 合法输入：
//   - 空字符串 ""           → 表示清空预设群组列表
//   - JSON 字符串数组 "[...]" → 元素必须严格为 JSON 字符串（含空数组 "[]"）
//
// 拒绝（之前用 json.Unmarshal 到 []string 会静默放过的坑）：
//   - 顶层 "null"            → Go 解码到 []string 得 nil slice 不报错
//   - 数组含 null：[null]     → Go 解码到 []string 把 null 写成 ""，不报错
//   - 数组含非字符串：[1,{}]  → 同上，会被解码失败拦下，但单测显式覆盖
//
// 实现：先按 []json.RawMessage 切片解，再逐元素校验首字节是 '"'，
// 这样能区分 JSON 字符串和 null/number/object/array。
func validatePresetGroupIds(raw string) error {
	if len(raw) > userSpacePresetGroupIdsMaxBytes {
		return fmt.Errorf("preset_group_ids 不能超过 %d 字节", userSpacePresetGroupIdsMaxBytes)
	}
	if raw == "" {
		return nil
	}
	// 用 *[]json.RawMessage 区分 "[...]" 与 "null"：top-level null 解到 *T 会得 nil 指针。
	var arr *[]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return errors.New("preset_group_ids 必须为 JSON 字符串数组")
	}
	if arr == nil {
		return errors.New("preset_group_ids 不能为 null，请用空字符串表示清空")
	}
	for i, elem := range *arr {
		// json.RawMessage 不包含前后空白（json.Decoder 已剥离），直接判首字节即可。
		// 字符串必以 '"' 开头；其余（null/数字/对象/数组）一律拒。
		if len(elem) == 0 || elem[0] != '"' {
			return fmt.Errorf("preset_group_ids[%d] 必须为字符串", i)
		}
		// 复活解到 string 以确保转义合法（如 "\uXXXX" 完整、引号闭合）。
		var s string
		if err := json.Unmarshal(elem, &s); err != nil {
			return fmt.Errorf("preset_group_ids[%d] 解析失败: %w", i, err)
		}
	}
	return nil
}

// disbandSpace 解散空间
func (s *Space) disbandSpace(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role != 2 {
		c.ResponseError(errors.New("只有拥有者才能解散空间"))
		return
	}

	err = s.db.disbandSpace(spaceId)
	if err != nil {
		c.ResponseError(errors.New("解散空间失败"))
		return
	}
	c.ResponseOK()
}

// mySpaces 我的空间列表
func (s *Space) mySpaces(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	spaces, err := s.db.queryMySpaces(loginUID)
	if err != nil {
		s.Error("查询空间列表失败", zap.Error(err), zap.String("loginUID", loginUID))
		c.ResponseError(errors.New("查询空间列表失败"))
		return
	}

	resps := make([]spaceResp, 0, len(spaces))
	for _, sp := range spaces {
		resps = append(resps, spaceResp{
			SpaceId:     sp.SpaceId,
			Name:        sp.Name,
			Description: sp.Description,
			Logo:        sp.Logo,
			Creator:     sp.Creator,
			Status:      sp.Status,
			Role:        sp.Role,
			MaxUsers:    sp.MaxUsers,
			MemberCount: sp.MemberCount,
			JoinMode:    sp.JoinMode,
			CreatedAt:   sp.CreatedAt.String(),
			UpdatedAt:   sp.UpdatedAt.String(),
		})
	}
	c.Response(resps)
}

// listMembers 获取空间成员列表
func (s *Space) listMembers(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil {
		c.ResponseError(errors.New("你不是该空间成员"))
		return
	}

	pageStr := c.Query("page")
	limitStr := c.Query("limit")
	page, _ := strconv.ParseUint(pageStr, 10, 64)
	limit, _ := strconv.ParseUint(limitStr, 10, 64)
	if page <= 0 {
		page = 1
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 10000 {
		limit = 10000
	}

	members, err := s.db.queryMembers(spaceId, loginUID, page, limit)
	if err != nil {
		c.ResponseError(errors.New("查询成员列表失败"))
		return
	}

	resps := make([]memberResp, 0, len(members))
	for _, m := range members {
		resps = append(resps, memberResp{
			UID:       m.UID,
			Name:      m.Name,
			Role:      m.Role,
			Robot:     m.Robot,
			CreatedAt: m.CreatedAt.String(),
		})
	}
	c.Response(resps)
}

// addMembers 添加成员
func (s *Space) addMembers(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	if s.checkSpaceActive(c, spaceId) {
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限添加成员"))
		return
	}

	var req addMemberReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if len(req.UIDs) == 0 {
		c.ResponseError(errors.New("成员列表不能为空"))
		return
	}

	// 检查空间人数上限
	spaceInfo, err := s.db.querySpaceByID(spaceId)
	if err != nil || spaceInfo == nil {
		c.ResponseError(errors.New("空间不存在或已解散"))
		return
	}
	if spaceInfo.MaxUsers > 0 {
		memberCount, countErr := s.db.countActiveMembers(spaceId)
		if countErr != nil {
			c.ResponseError(errors.New("查询空间成员数失败"))
			return
		}
		if memberCount+len(req.UIDs) > spaceInfo.MaxUsers {
			c.ResponseError(errors.New("空间成员数已达上限"))
			return
		}
	}

	newMembers := make([]string, 0, len(req.UIDs))
	for _, uid := range req.UIDs {
		existing, err := s.db.queryMemberIncludeRemoved(spaceId, uid)
		if err != nil {
			c.ResponseError(errors.New("查询成员信息失败"))
			return
		}
		if existing != nil {
			if existing.Status == 0 {
				if err = s.db.reactivateMember(spaceId, uid, 0); err != nil {
					c.ResponseError(errors.New("重新激活成员失败"))
					return
				}
				newMembers = append(newMembers, uid)
			}
			continue
		}
		if err = s.db.insertMemberNoTx(&MemberModel{
			SpaceId: spaceId,
			UID:     uid,
			Role:    0,
			Status:  1,
		}); err != nil {
			c.ResponseError(errors.New("添加成员失败"))
			return
		}
		newMembers = append(newMembers, uid)
	}
	c.ResponseOK()

	// 为每个新成员补齐默认分类（GH octo-server#1228）；异步以免阻塞响应。
	for _, uid := range newMembers {
		uid := uid
		go ensureDefaultCategoryProvisioned(s.ctx, uid, spaceId, s)
	}

	// 触发 SpaceMemberJoin 事件（每个新成员）
	for _, uid := range newMembers {
		go s.fireSpaceMemberJoinEvent(uid, spaceId)
	}

	// 失效通知成员缓存
	if event.SpaceMemberCacheInvalidator != nil {
		event.SpaceMemberCacheInvalidator(spaceId)
	}
}

// removeMembers 移除成员
func (s *Space) removeMembers(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	if s.checkSpaceActive(c, spaceId) {
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限移除成员"))
		return
	}

	var req removeMemberReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}

	for _, uid := range req.UIDs {
		target, err := s.db.queryMember(spaceId, uid)
		if err != nil {
			c.ResponseError(errors.New("查询目标成员失败"))
			return
		}
		if target == nil {
			continue
		}
		if target.Role == 2 {
			continue // 不能移除owner
		}
		if member.Role <= target.Role {
			continue // 不能移除同级或更高角色
		}
		if err = s.db.removeMember(spaceId, uid); err != nil {
			c.ResponseError(errors.New("移除成员失败"))
			return
		}
	}
	// 失效通知成员缓存
	if event.SpaceMemberCacheInvalidator != nil {
		event.SpaceMemberCacheInvalidator(spaceId)
	}
	c.ResponseOK()
}

// leaveSpace 退出空间
func (s *Space) leaveSpace(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	if s.checkSpaceActive(c, spaceId) {
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil {
		c.ResponseError(errors.New("你不是该空间成员"))
		return
	}
	if member.Role == 2 {
		c.ResponseError(errors.New("拥有者不能退出空间，请先转让拥有权"))
		return
	}

	err = s.db.removeMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("退出空间失败"))
		return
	}
	// 失效通知成员缓存
	if event.SpaceMemberCacheInvalidator != nil {
		event.SpaceMemberCacheInvalidator(spaceId)
	}
	c.ResponseOK()
}

// updateMemberRole 修改成员角色
func (s *Space) updateMemberRole(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	targetUID := c.Param("uid")

	if s.checkSpaceActive(c, spaceId) {
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role != 2 {
		c.ResponseError(errors.New("只有拥有者才能修改成员角色"))
		return
	}

	var req updateMemberRoleReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if req.Role < 0 || req.Role > 2 {
		c.ResponseError(errors.New("无效的角色值"))
		return
	}

	// 验证目标成员存在且活跃
	target, err := s.db.queryMember(spaceId, targetUID)
	if err != nil {
		c.ResponseError(errors.New("查询目标成员失败"))
		return
	}
	if target == nil {
		c.ResponseError(errors.New("目标成员不存在或已被移除"))
		return
	}

	// 如果要转让owner，使用事务保证原子性
	if req.Role == 2 {
		tx, txErr := s.ctx.DB().Begin()
		if txErr != nil {
			c.ResponseError(errors.New("开启事务失败"))
			return
		}
		defer tx.RollbackUnlessCommitted()

		// 先提升目标为owner
		if err = s.db.updateMemberRoleTx(tx, spaceId, targetUID, 2); err != nil {
			c.ResponseError(errors.New("提升目标角色失败"))
			return
		}
		// 再把当前owner降为admin
		if err = s.db.updateMemberRoleTx(tx, spaceId, loginUID, 1); err != nil {
			c.ResponseError(errors.New("降级当前拥有者失败"))
			return
		}
		if err = tx.Commit(); err != nil {
			c.ResponseError(errors.New("提交事务失败"))
			return
		}
	} else {
		err = s.db.updateMemberRole(spaceId, targetUID, req.Role)
		if err != nil {
			c.ResponseError(errors.New("修改角色失败"))
			return
		}
	}
	c.ResponseOK()
}

// createInvite 创建邀请链接
func (s *Space) createInvite(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	if s.checkSpaceActive(c, spaceId) {
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限创建邀请链接"))
		return
	}

	inviteModel := &InvitationModel{
		SpaceId: spaceId,
		Creator: loginUID,
		Status:  1,
	}
	applyAutoInviteDefaults(inviteModel, time.Now())
	inviteCode, err := s.insertInvitationWithRetry(inviteModel)
	if err != nil {
		s.Error("创建邀请链接失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("创建邀请链接失败"))
		return
	}

	c.Response(map[string]string{
		"invite_code": inviteCode,
	})
}

// joinSpace 通过邀请码加入空间
func (s *Space) joinSpace(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	var req joinSpaceReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if req.InviteCode == "" {
		c.ResponseError(errors.New("邀请码不能为空"))
		return
	}

	invitation, err := s.db.queryInvitationByCode(req.InviteCode)
	if err != nil {
		c.ResponseError(errors.New("查询邀请信息失败"))
		return
	}
	if invitation == nil {
		// queryInvitationByCode 已在 SQL 层过滤 status!=1 与已过期，命中即有效。
		c.ResponseError(errors.New("邀请码无效或已过期"))
		return
	}

	// 检查空间是否存在
	space, err := s.db.querySpaceByID(invitation.SpaceId)
	if err != nil || space == nil {
		c.ResponseError(errors.New("空间不存在或已解散"))
		return
	}

	// 需要审批模式
	if space.JoinMode == JoinModeApproval {
		// 只读校验邀请码次数（不消耗，审批通过时也跳过）
		if invitation.MaxUses > 0 && invitation.UsedCount >= invitation.MaxUses {
			c.ResponseError(errors.New("邀请码已达到使用次数上限"))
			return
		}

		// 检查是否已是成员
		existing, err := s.db.queryMemberIncludeRemoved(invitation.SpaceId, loginUID)
		if err != nil {
			c.ResponseError(errors.New("查询成员信息失败"))
			return
		}
		if existing != nil && existing.Status == 1 {
			c.ResponseError(errors.New("你已经是该空间成员"))
			return
		}

		// 检查是否已有待处理申请
		pendingApply, err := s.db.queryPendingApplyBySpaceAndUID(invitation.SpaceId, loginUID)
		if err != nil {
			c.ResponseError(errors.New("查询申请信息失败"))
			return
		}
		if pendingApply != nil {
			c.Response(map[string]interface{}{
				"status":   "PENDING",
				"space_id": invitation.SpaceId,
				"msg":      "申请已提交，请等待审批",
			})
			return
		}

		// 创建或重置申请记录（被拒后重新申请时覆盖更新）
		applyID, err := s.db.upsertJoinApply(&spaceJoinApplyModel{
			SpaceId:    invitation.SpaceId,
			UID:        loginUID,
			InviteCode: req.InviteCode,
		})
		if err != nil {
			c.ResponseError(errors.New("提交申请失败"))
			return
		}

		// 异步通知管理员
		go s.notifyAdminsNewJoinApply(loginUID, invitation.SpaceId, space.Name, applyID)

		c.Response(map[string]interface{}{
			"status":   "NEED_APPROVAL",
			"space_id": invitation.SpaceId,
			"msg":      "该空间需要管理员审批，申请已提交",
		})
		return
	}

	// 直接加入模式：原子检查并递增使用次数
	allowed, err := s.db.incrementInviteUsedCountAtomic(req.InviteCode)
	if err != nil {
		c.ResponseError(errors.New("检查邀请码使用次数失败"))
		return
	}
	if !allowed {
		c.ResponseError(errors.New("邀请码已达到使用次数上限"))
		return
	}

	// 执行加入逻辑。任何加入失败（含 ErrAlreadyMember：无新增成员）都应归还已消耗的名额。
	joinErr := s.executeJoinSpace(loginUID, invitation.SpaceId, space)
	if joinErr != nil {
		s.refundInvite(req.InviteCode)
		if errors.Is(joinErr, ErrSpaceFull) {
			c.ResponseWithStatus(http.StatusBadRequest, map[string]interface{}{
				"status": "SPACE_FULL",
				"msg":    "空间已满，无法加入",
			})
			return
		}
		if errors.Is(joinErr, ErrAlreadyMember) {
			c.ResponseError(errors.New("你已经是该空间成员"))
			return
		}
		c.ResponseError(errors.New("加入空间失败"))
		return
	}

	c.Response(map[string]string{
		"space_id": invitation.SpaceId,
	})
}

// executeJoinSpace 执行加入空间的核心逻辑（供直接加入和审批通过共用）
func (s *Space) executeJoinSpace(uid, spaceId string, space *SpaceModel) error {
	existing, err := s.db.queryMemberIncludeRemoved(spaceId, uid)
	if err != nil {
		return fmt.Errorf("query member failed: %w", err)
	}
	if existing != nil {
		if existing.Status == 1 {
			return ErrAlreadyMember
		}
		err = s.db.atomicReactivateMemberIfNotFull(spaceId, uid, space.MaxUsers)
	} else {
		err = s.db.atomicAddMemberIfNotFull(spaceId, uid, space.MaxUsers)
	}
	if err != nil {
		return err
	}

	// 异步加入预设群组
	if space.PresetGroupIds != nil && *space.PresetGroupIds != "" {
		go s.joinPresetGroups(uid, spaceId, *space.PresetGroupIds)
	}

	// 为新成员补齐默认分类（GH octo-server#1228）。失败不阻塞加入流程。
	ensureDefaultCategoryProvisioned(s.ctx, uid, spaceId, s)

	// 触发 SpaceMemberJoin 事件
	go s.fireSpaceMemberJoinEvent(uid, spaceId)

	// 失效通知成员缓存
	if event.SpaceMemberCacheInvalidator != nil {
		event.SpaceMemberCacheInvalidator(spaceId)
	}

	return nil
}

// joinPresetGroups 将用户加入预设群组（通过直接DB操作避免循环依赖）
func (s *Space) joinPresetGroups(uid string, spaceID string, presetGroupIdsJSON string) {
	defer func() {
		if r := recover(); r != nil {
			s.Error("joinPresetGroups panic", zap.Any("recover", r), zap.String("uid", uid), zap.String("spaceID", spaceID))
		}
	}()

	var groupNos []string
	if err := json.Unmarshal([]byte(presetGroupIdsJSON), &groupNos); err != nil {
		s.Warn("解析预设群组ID失败", zap.String("preset_group_ids", presetGroupIdsJSON), zap.Error(err))
		return
	}

	session := s.ctx.DB()
	for _, groupNo := range groupNos {
		if groupNo == "" {
			continue
		}
		// 检查群是否存在、未解散，且属于当前 Space
		var groupStatus int
		count, err := session.SelectBySql("SELECT status FROM `group` WHERE group_no=? AND space_id=?", groupNo, spaceID).Load(&groupStatus)
		if err != nil || count == 0 || groupStatus != 1 {
			s.Warn("预设群组不存在、已解散或不属于当前 Space，跳过", zap.String("group_no", groupNo), zap.String("space_id", spaceID))
			continue
		}
		// 检查用户是否已在群中
		var memberCount int
		_, err = session.SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, uid).Load(&memberCount)
		if err != nil {
			s.Warn("检查群成员失败，跳过", zap.String("group_no", groupNo), zap.Error(err))
			continue
		}
		if memberCount > 0 {
			s.Warn("用户已在群中，跳过", zap.String("group_no", groupNo), zap.String("uid", uid))
			continue
		}
		// 添加成员
		_, err = session.InsertBySql("INSERT INTO group_member (group_no, uid) VALUES (?, ?)", groupNo, uid).Exec()
		if err != nil {
			s.Warn("自动加入预设群组失败", zap.String("group_no", groupNo), zap.String("uid", uid), zap.Error(err))
			continue
		}
		s.Info("用户已自动加入预设群组", zap.String("group_no", groupNo), zap.String("uid", uid))
	}
}

// getInviteInfo 获取邀请信息（公开接口）
func (s *Space) getInviteInfo(c *wkhttp.Context) {
	inviteCode := c.Param("invite_code")
	if inviteCode == "" {
		c.ResponseError(errors.New("邀请码不能为空"))
		return
	}

	invitation, err := s.db.queryInvitationByCode(inviteCode)
	if err != nil {
		c.ResponseError(errors.New("查询邀请信息失败"))
		return
	}
	if invitation == nil {
		c.ResponseError(errors.New("邀请码无效"))
		return
	}

	space, err := s.db.querySpaceByID(invitation.SpaceId)
	if err != nil || space == nil {
		c.ResponseError(errors.New("空间不存在或已解散"))
		return
	}

	expiresAtStr := ""
	if invitation.ExpiresAt != nil {
		expiresAtStr = time.Time(*invitation.ExpiresAt).Format(inviteTimeLayout)
	}

	// 查询 Space 成员数
	memberCount := 0
	var cnt struct {
		Count int `db:"count"`
	}
	_, err = s.ctx.DB().SelectBySql("SELECT COUNT(*) as count FROM space_member WHERE space_id=? AND status=1", invitation.SpaceId).Load(&cnt)
	if err != nil {
		s.Error("查询空间成员数失败", zap.Error(err), zap.String("spaceId", invitation.SpaceId))
	} else {
		memberCount = cnt.Count
	}

	c.Response(inviteResp{
		InviteCode:  invitation.InviteCode,
		SpaceId:     invitation.SpaceId,
		SpaceName:   space.Name,
		Creator:     invitation.Creator,
		MaxUses:     invitation.MaxUses,
		UsedCount:   invitation.UsedCount,
		ExpiresAt:   expiresAtStr,
		MemberCount: memberCount,
		JoinMode:    space.JoinMode,
	})
}

// GetCoMemberUIDs 获取与指定用户同在至少一个空间的所有用户UID（公开方法，供其他模块调用）
func (s *Space) GetCoMemberUIDs(uid string) ([]string, error) {
	return s.db.queryCoMemberUIDs(uid)
}

// getInvitePreview 获取邀请预览（公开接口）
// 注意：公开接口不返回敏感信息（Bot 列表、精确成员数量）
func (s *Space) getInvitePreview(c *wkhttp.Context) {
	inviteCode := c.Param("invite_code")
	if inviteCode == "" {
		c.ResponseError(errors.New("邀请码不能为空"))
		return
	}

	invitation, err := s.db.queryInvitationByCode(inviteCode)
	if err != nil {
		c.ResponseError(errors.New("查询邀请信息失败"))
		return
	}
	if invitation == nil {
		c.ResponseError(errors.New("邀请码无效"))
		return
	}

	space, err := s.db.querySpaceByID(invitation.SpaceId)
	if err != nil || space == nil {
		c.ResponseError(errors.New("空间不存在或已解散"))
		return
	}

	expiresAtStr := ""
	if invitation.ExpiresAt != nil {
		expiresAtStr = time.Time(*invitation.ExpiresAt).Format(inviteTimeLayout)
	}

	// 公开接口只返回基本空间信息，不暴露 Bot 列表和精确成员数量
	c.Response(invitePreviewResp{
		InviteCode:  invitation.InviteCode,
		SpaceId:     invitation.SpaceId,
		SpaceName:   space.Name,
		Description: space.Description,
		Logo:        space.Logo,
		Creator:     "", // 不暴露创建者 UID
		MaxUses:     invitation.MaxUses,
		UsedCount:   invitation.UsedCount,
		ExpiresAt:   expiresAtStr,
		MemberCount: 0,              // 不暴露精确成员数量
		JoinMode:    space.JoinMode, // 告知客户端是否需要审批
		Bots:        nil,            // 不暴露 Bot 列表
	})
}

// updateInvite 更新邀请码设置。支持 max_uses / expires_at / status。
// status 字段允许空间 owner/admin 自助禁用或重启用邀请码，语义与管理端 PUT 一致。
func (s *Space) updateInvite(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	code := c.Param("code")

	if s.checkSpaceActive(c, spaceId) {
		return
	}

	// 检查权限（需要管理员或拥有者）
	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限修改邀请码设置"))
		return
	}

	var req updateInviteReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if req.MaxUses == nil && req.ExpiresAt == nil && req.Status == nil {
		c.ResponseError(errors.New("至少需要提供 max_uses / expires_at / status 之一"))
		return
	}
	if req.MaxUses != nil && *req.MaxUses < 0 {
		c.ResponseError(errors.New("max_uses 不能为负"))
		return
	}
	if req.Status != nil && *req.Status != 0 && *req.Status != 1 {
		c.ResponseError(errors.New("status 仅支持 0(禁用) 或 1(启用)"))
		return
	}

	// 解析过期时间：与管理端 parseInviteExpiresAt 共用 time.Local 约定，避免双路径写库时区漂移。
	expiresAt, err := parseInviteExpiresAt(req.ExpiresAt)
	if err != nil {
		c.ResponseError(err)
		return
	}

	// 复用管理端的 updateInvitationAdmin：
	//   - WHERE 无 status=1 限制，可对已禁用邀请码重启用（status=1）
	//   - 按 space_id + invite_code 双匹配，天然防跨空间越权
	affected, err := s.mdb.updateInvitationAdmin(spaceId, code, req.MaxUses, expiresAt, req.Status)
	if err != nil {
		c.ResponseError(errors.New("更新邀请码失败"))
		return
	}
	if affected == 0 {
		c.ResponseError(errors.New("邀请码不存在"))
		return
	}

	c.ResponseOK()
}

// listInvites 空间 owner/admin 查看本空间邀请码列表。
// 查询参数：
//   - status=1 / status=（默认）: 仅有效（status=1 且未过期）
//   - status=0: 仅禁用
//   - status=all: 全部（包含禁用与过期）
//   - page_index / page_size: 分页，上限 managerMaxPageSize
func (s *Space) listInvites(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	if s.checkSpaceActive(c, spaceId) {
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限查看邀请码列表"))
		return
	}

	filter := parseInviteListFilter(c.Query("status"))
	pageIndex, pageSize := clampPage(c.GetPage())
	// 一次快照 time.Now()，保证 list 与 count 看到一致的过期边界，防止
	// 临近过期时二者产生 off-by-one 差异。
	now := time.Now()

	list, err := s.db.queryInvitesBySpace(spaceId, filter, now, uint64(pageSize), uint64(pageIndex))
	if err != nil {
		s.Error("查询邀请码列表失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询邀请码列表失败"))
		return
	}
	count, err := s.db.countInvitesBySpace(spaceId, filter, now)
	if err != nil {
		s.Error("查询邀请码总数失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询邀请码总数失败"))
		return
	}

	resp := make([]*spaceInviteListResp, 0, len(list))
	for _, inv := range list {
		expiresAt := ""
		if inv.ExpiresAt != nil {
			expiresAt = time.Time(*inv.ExpiresAt).Format(inviteTimeLayout)
		}
		resp = append(resp, &spaceInviteListResp{
			InviteCode: inv.InviteCode,
			SpaceId:    inv.SpaceId,
			Creator:    inv.Creator,
			MaxUses:    inv.MaxUses,
			UsedCount:  inv.UsedCount,
			ExpiresAt:  expiresAt,
			Status:     inv.Status,
			CreatedAt:  time.Time(inv.CreatedAt).Format(inviteTimeLayout),
		})
	}
	c.Response(map[string]interface{}{
		"count": count,
		"list":  resp,
	})
}

// deleteInvite 空间 owner/admin 软禁用本空间邀请码（语义等价 PUT status=0）。
func (s *Space) deleteInvite(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	code := c.Param("code")

	if s.checkSpaceActive(c, spaceId) {
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限禁用邀请码"))
		return
	}

	affected, err := s.mdb.disableInvitation(spaceId, code)
	if err != nil {
		s.Error("禁用邀请码失败", zap.Error(err), zap.String("code", code))
		c.ResponseError(errors.New("禁用邀请码失败"))
		return
	}
	if affected == 0 {
		c.ResponseError(errors.New("邀请码不存在"))
		return
	}
	c.ResponseOK()
}

// parseInviteListFilter 把请求参数 status 映射到 inviteListFilter。
// 未传或传 "1" 视为 active（Discord 式默认），传 "0" 视为 disabled，传 "all" 视为全部。
func parseInviteListFilter(raw string) inviteListFilter {
	switch raw {
	case "", "1":
		return inviteListActive
	case "0":
		return inviteListDisabled
	case "all":
		return inviteListAll
	default:
		return inviteListActive
	}
}

// joinApplies 管理员查看待审批的加入申请列表
func (s *Space) joinApplies(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限查看申请列表"))
		return
	}

	pageStr := c.Query("page")
	pageSizeStr := c.Query("page_size")
	page := 1
	pageSize := 20
	if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
		page = p
	}
	if ps, err := strconv.Atoi(pageSizeStr); err == nil && ps > 0 && ps <= 100 {
		pageSize = ps
	}
	offset := (page - 1) * pageSize

	list, err := s.db.queryPendingAppliesBySpace(spaceId, pageSize, offset)
	if err != nil {
		c.ResponseError(errors.New("查询申请列表失败"))
		return
	}
	count, err := s.db.queryPendingApplyCountBySpace(spaceId)
	if err != nil {
		c.ResponseError(errors.New("查询申请数量失败"))
		return
	}

	respList := make([]*spaceJoinApplyResp, 0, len(list))
	for _, apply := range list {
		applicantName := apply.ApplicantName
		if applicantName == "" {
			applicantName = apply.UID
		}

		respList = append(respList, &spaceJoinApplyResp{
			ID:            apply.Id,
			SpaceId:       apply.SpaceId,
			UID:           apply.UID,
			ApplicantName: applicantName,
			Remark:        apply.Remark,
			Status:        apply.Status,
			ReviewerUID:   apply.ReviewerUID,
			CreatedAt:     apply.CreatedAt.String(),
		})
	}

	c.Response(&spaceJoinApplyListResp{
		List:  respList,
		Count: count,
	})
}

// approveJoinApply 管理员通过加入申请
func (s *Space) approveJoinApply(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	applyIDStr := c.Param("id")

	applyID, err := strconv.ParseInt(applyIDStr, 10, 64)
	if err != nil || applyID <= 0 {
		c.ResponseError(errors.New("申请ID无效"))
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限审批"))
		return
	}

	apply, err := s.db.queryJoinApplyByID(applyID)
	if err != nil {
		c.ResponseError(errors.New("查询申请记录失败"))
		return
	}
	if apply == nil {
		c.ResponseError(errors.New("申请记录不存在"))
		return
	}
	if apply.SpaceId != spaceId {
		c.ResponseError(errors.New("申请记录不属于当前空间"))
		return
	}
	if apply.Status != 0 {
		c.ResponseError(errors.New("该申请已被处理"))
		return
	}

	space, err := s.db.querySpaceByID(spaceId)
	if err != nil || space == nil {
		c.ResponseError(errors.New("空间不存在或已解散"))
		return
	}

	// 先更新申请状态（防止并发审批 + 确保加入后状态一致）
	affected, err := s.db.updateJoinApplyStatus(applyID, 1, loginUID)
	if err != nil {
		c.ResponseError(errors.New("更新申请状态失败"))
		return
	}
	if affected == 0 {
		// 已被其他管理员处理
		c.ResponseOK()
		return
	}

	// 审批通过时消耗邀请码名额（方案 B：在准入时消耗，与直接加入模式对称）
	inviteConsumed, consumeErr := s.consumeInviteOnApprove(apply.InviteCode)
	if consumeErr != nil {
		if _, rbErr := s.db.updateJoinApplyStatusRaw(applyID, 0, ""); rbErr != nil {
			s.Error("回滚申请状态失败", zap.Error(rbErr), zap.Int64("applyID", applyID))
		}
		c.ResponseError(errors.New("检查邀请码使用次数失败"))
		return
	}
	if apply.InviteCode != "" && !inviteConsumed {
		if _, rbErr := s.db.updateJoinApplyStatusRaw(applyID, 0, ""); rbErr != nil {
			s.Error("回滚申请状态失败", zap.Error(rbErr), zap.Int64("applyID", applyID))
		}
		c.ResponseError(errors.New("该邀请码已用尽或已失效，无法通过此申请"))
		return
	}

	// 执行加入逻辑（跳过邀请码校验，管理员审批即授权）
	joinErr := s.executeJoinSpace(apply.UID, spaceId, space)
	if joinErr != nil {
		if errors.Is(joinErr, ErrSpaceFull) {
			s.rollbackApplyAndInvite(applyID, apply.InviteCode, inviteConsumed)
			c.ResponseError(errors.New("空间已满，无法通过申请"))
			return
		}
		if errors.Is(joinErr, ErrAlreadyMember) {
			// 已是成员：apply 保持已审批状态，但未新增成员，归还本次消耗的名额
			if inviteConsumed {
				s.refundInvite(apply.InviteCode)
			}
			c.ResponseOK()
			return
		}
		s.rollbackApplyAndInvite(applyID, apply.InviteCode, inviteConsumed)
		c.ResponseError(errors.New("加入空间失败"))
		return
	}

	go s.notifyApplicantJoinResult(apply.UID, spaceId, space.Name, true)

	c.ResponseOK()
}

// refundInvite 归还一次已消耗的邀请码名额（空码跳过；错误仅记录日志，不影响主流程）。
func (s *Space) refundInvite(code string) {
	if code == "" {
		return
	}
	if _, err := s.db.decrementInviteUsedCountAtomic(code); err != nil {
		s.Error("回滚邀请码使用次数失败", zap.Error(err), zap.String("inviteCode", code))
	}
}

// consumeInviteOnApprove 审批通过时尝试消耗邀请码名额。
// apply.InviteCode 为空时（如旧数据）跳过消耗，返回 (false, nil)，由调用方按未消耗处理。
func (s *Space) consumeInviteOnApprove(code string) (bool, error) {
	if code == "" {
		return false, nil
	}
	return s.db.incrementInviteUsedCountAtomic(code)
}

// rollbackApplyAndInvite 审批加入失败时回滚申请状态，并在确实消耗过名额时归还。
func (s *Space) rollbackApplyAndInvite(applyID int64, code string, consumed bool) {
	if _, rbErr := s.db.updateJoinApplyStatusRaw(applyID, 0, ""); rbErr != nil {
		s.Error("回滚申请状态失败", zap.Error(rbErr), zap.Int64("applyID", applyID))
	}
	if consumed {
		s.refundInvite(code)
	}
}

// rejectJoinApply 管理员拒绝加入申请
func (s *Space) rejectJoinApply(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	applyIDStr := c.Param("id")

	applyID, err := strconv.ParseInt(applyIDStr, 10, 64)
	if err != nil || applyID <= 0 {
		c.ResponseError(errors.New("申请ID无效"))
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if member == nil || member.Role < 1 {
		c.ResponseError(errors.New("无权限审批"))
		return
	}

	apply, err := s.db.queryJoinApplyByID(applyID)
	if err != nil {
		c.ResponseError(errors.New("查询申请记录失败"))
		return
	}
	if apply == nil {
		c.ResponseError(errors.New("申请记录不存在"))
		return
	}
	if apply.SpaceId != spaceId {
		c.ResponseError(errors.New("申请记录不属于当前空间"))
		return
	}
	if apply.Status != 0 {
		c.ResponseError(errors.New("该申请已被处理"))
		return
	}

	_, err = s.db.updateJoinApplyStatus(applyID, 2, loginUID)
	if err != nil {
		c.ResponseError(errors.New("更新申请状态失败"))
		return
	}

	spaceName := ""
	space, spErr := s.db.querySpaceByID(spaceId)
	if spErr != nil {
		s.Warn("查询空间信息失败", zap.Error(spErr), zap.String("spaceId", spaceId))
	}
	if space != nil {
		spaceName = space.Name
	}

	go s.notifyApplicantJoinResult(apply.UID, spaceId, spaceName, false)

	c.ResponseOK()
}

// notifyAdminsNewJoinApply 通知管理员有新的加入申请（每人生成独立 auth_code）
func (s *Space) notifyAdminsNewJoinApply(applicantUID, spaceId, spaceName string, applyID int64) {
	admins, err := s.db.queryAdminsAndOwner(spaceId)
	if err != nil || len(admins) == 0 {
		s.Warn("查询管理员列表失败或无管理员", zap.Error(err), zap.String("spaceId", spaceId))
		return
	}

	applicantName := applicantUID
	var userInfo struct {
		Name  string
		Email string
	}
	cnt, _ := s.ctx.DB().SelectBySql("SELECT IFNULL(name,'') as name, IFNULL(email,'') as email FROM `user` WHERE uid=?", applicantUID).Load(&userInfo)
	if cnt > 0 && userInfo.Name != "" {
		applicantName = userInfo.Name
	}

	emailText := ""
	if userInfo.Email != "" {
		emailText = fmt.Sprintf("\n\n邮箱: %s", userInfo.Email)
	}

	for _, admin := range admins {
		// 为每个管理员生成独立 auth_code（7天有效）
		authCode := util.GenerUUID()
		authData := util.ToJson(map[string]interface{}{
			"apply_id":     applyID,
			"space_id":     spaceId,
			"reviewer_uid": admin.UID,
			"type":         "spaceJoinApprove",
		})
		if err := s.ctx.GetRedisConn().SetAndExpire(
			fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode),
			authData, time.Hour*24*7,
		); err != nil {
			s.Warn("缓存审批授权码失败", zap.Error(err), zap.String("adminUID", admin.UID))
			continue
		}

		approveURL := fmt.Sprintf("%s/v1/space/join-approve?auth_code=%s", s.ctx.GetConfig().External.BaseURL, authCode)
		content := fmt.Sprintf("有新的 Space 加入申请\n\n用户: %s (%s)\n\n空间: %s%s\n\n审批链接: %s",
			applicantName, applicantUID, spaceName, emailText, approveURL)
		notifyPayload := map[string]interface{}{
			"content": content,
			"type":    common.Text,
		}
		// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM via NewPersonalMsgSendReq builder.
		// spaceId 是当前申请的 Space，作为 senderSpaceID 即权威值。
		if err := s.ctx.SendMessage(config.NewPersonalMsgSendReq(
			admin.UID,
			s.ctx.GetConfig().Account.SystemUID,
			notifyPayload,
			spaceId,
			config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
		)); err != nil {
			s.Warn("发送加入申请通知失败", zap.Error(err), zap.String("adminUID", admin.UID), zap.String("spaceId", spaceId))
		}
	}
}

// notifyApplicantJoinResult 通知申请人审批结果
func (s *Space) notifyApplicantJoinResult(applicantUID, spaceId, spaceName string, approved bool) {
	var content string
	if approved {
		content = fmt.Sprintf("你的 Space 加入申请已通过！\n空间: %s\n现在可以进入空间了", spaceName)
	} else {
		content = fmt.Sprintf("你的 Space 加入申请被拒绝\n空间: %s", spaceName)
	}

	resultPayload := map[string]interface{}{
		"content": content,
		"type":    common.Text,
	}
	// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM via NewPersonalMsgSendReq builder.
	_ = s.ctx.SendMessage(config.NewPersonalMsgSendReq(
		applicantUID,
		s.ctx.GetConfig().Account.SystemUID,
		resultPayload,
		spaceId,
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
	))
}

// joinApprovePage 返回 H5 审批页面（注入 apiURL）
func (s *Space) joinApprovePage(c *wkhttp.Context) {
	htmlBytes, err := os.ReadFile("./assets/web/space_join_approve.html")
	if err != nil {
		c.ResponseError(errors.New("页面加载失败"))
		return
	}
	safeBaseURL := strconv.Quote(s.ctx.GetConfig().External.BaseURL)
	html := strings.Replace(string(htmlBytes), `"{{API_BASE_URL}}"`, safeBaseURL, 1)
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}

// joinApproveDetail 获取审批详情（公开接口，通过 auth_code 鉴权）
func (s *Space) joinApproveDetail(c *wkhttp.Context) {
	authCode := c.Query("auth_code")
	if authCode == "" {
		c.ResponseError(errors.New("auth_code 不能为空"))
		return
	}

	authInfo, err := s.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode))
	if err != nil || authInfo == "" {
		c.ResponseError(errors.New("授权码无效或已过期"))
		return
	}

	var authMap map[string]interface{}
	if err := util.ReadJsonByByte([]byte(authInfo), &authMap); err != nil {
		c.ResponseError(errors.New("授权信息解析失败"))
		return
	}

	authType, _ := authMap["type"].(string)
	if authType != "spaceJoinApprove" {
		c.ResponseError(errors.New("授权码类型无效"))
		return
	}

	applyIDNum, _ := authMap["apply_id"].(json.Number)
	applyID, _ := applyIDNum.Int64()
	spaceId, _ := authMap["space_id"].(string)

	apply, err := s.db.queryJoinApplyByID(applyID)
	if err != nil || apply == nil {
		c.ResponseError(errors.New("申请记录不存在"))
		return
	}

	space, _ := s.db.querySpaceByID(spaceId)
	spaceName := spaceId
	if space != nil {
		spaceName = space.Name
	}

	applicantName := apply.UID
	var applicantEmail string
	var reviewerName string

	uids := []interface{}{apply.UID}
	if apply.ReviewerUID != "" && apply.ReviewerUID != apply.UID {
		uids = append(uids, apply.ReviewerUID)
	}
	var userRows []struct {
		UID   string
		Name  string
		Email string
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(uids)), ",")
	s.ctx.DB().SelectBySql(
		fmt.Sprintf("SELECT uid, IFNULL(name,'') as name, IFNULL(email,'') as email FROM `user` WHERE uid IN (%s)", placeholders),
		uids...,
	).Load(&userRows)
	for _, u := range userRows {
		if u.UID == apply.UID && u.Name != "" {
			applicantName = u.Name
			applicantEmail = u.Email
		}
		if u.UID == apply.ReviewerUID && u.Name != "" {
			reviewerName = u.Name
		}
	}

	c.Response(map[string]interface{}{
		"apply_id":        apply.Id,
		"space_id":        spaceId,
		"space_name":      spaceName,
		"uid":             apply.UID,
		"applicant_name":  applicantName,
		"applicant_email": applicantEmail,
		"status":          apply.Status,
		"reviewer_uid":    apply.ReviewerUID,
		"reviewer_name":   reviewerName,
	})
}

// joinApproveSure 执行审批（公开接口，通过 auth_code 鉴权，一次性）
func (s *Space) joinApproveSure(c *wkhttp.Context) {
	authCode := c.Query("auth_code")
	action := c.Query("action")
	if authCode == "" {
		c.ResponseError(errors.New("auth_code 不能为空"))
		return
	}
	if action != "approve" && action != "reject" {
		c.ResponseError(errors.New("action 必须是 approve 或 reject"))
		return
	}

	cacheKey := fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode)
	authInfo, err := s.ctx.GetRedisConn().GetString(cacheKey)
	if err != nil || authInfo == "" {
		c.ResponseError(errors.New("授权码无效或已过期"))
		return
	}
	// 保留 auth_code 让其自然过期，审批后仍可查看详情
	// DB 层 WHERE status=0 已原子防重

	var authMap map[string]interface{}
	if err := util.ReadJsonByByte([]byte(authInfo), &authMap); err != nil {
		c.ResponseError(errors.New("授权信息解析失败"))
		return
	}

	authType, _ := authMap["type"].(string)
	if authType != "spaceJoinApprove" {
		c.ResponseError(errors.New("授权码类型无效"))
		return
	}

	applyIDNum, _ := authMap["apply_id"].(json.Number)
	applyID, _ := applyIDNum.Int64()
	spaceId, _ := authMap["space_id"].(string)
	reviewerUID, _ := authMap["reviewer_uid"].(string)

	apply, err := s.db.queryJoinApplyByID(applyID)
	if err != nil || apply == nil {
		c.ResponseError(errors.New("申请记录不存在"))
		return
	}
	if apply.SpaceId != spaceId {
		c.ResponseError(errors.New("申请记录不属于当前空间"))
		return
	}
	if apply.Status != 0 {
		c.ResponseError(errors.New("该申请已被处理"))
		return
	}

	space, err := s.db.querySpaceByID(spaceId)
	if err != nil || space == nil {
		c.ResponseError(errors.New("空间不存在或已解散"))
		return
	}

	if action == "approve" {
		affected, err := s.db.updateJoinApplyStatus(applyID, 1, reviewerUID)
		if err != nil {
			c.ResponseError(errors.New("更新申请状态失败"))
			return
		}
		if affected == 0 {
			c.ResponseOK()
			return
		}

		inviteConsumed, consumeErr := s.consumeInviteOnApprove(apply.InviteCode)
		if consumeErr != nil {
			if _, rbErr := s.db.updateJoinApplyStatusRaw(applyID, 0, ""); rbErr != nil {
				s.Error("回滚申请状态失败", zap.Error(rbErr), zap.Int64("applyID", applyID))
			}
			c.ResponseError(errors.New("检查邀请码使用次数失败"))
			return
		}
		if apply.InviteCode != "" && !inviteConsumed {
			if _, rbErr := s.db.updateJoinApplyStatusRaw(applyID, 0, ""); rbErr != nil {
				s.Error("回滚申请状态失败", zap.Error(rbErr), zap.Int64("applyID", applyID))
			}
			c.ResponseError(errors.New("该邀请码已用尽或已失效，无法通过此申请"))
			return
		}

		joinErr := s.executeJoinSpace(apply.UID, spaceId, space)
		if joinErr != nil {
			if errors.Is(joinErr, ErrSpaceFull) {
				s.rollbackApplyAndInvite(applyID, apply.InviteCode, inviteConsumed)
				c.ResponseError(errors.New("空间已满，无法通过申请"))
				return
			}
			if errors.Is(joinErr, ErrAlreadyMember) {
				// apply 保持已审批状态，但未新增成员，归还名额
				if inviteConsumed {
					s.refundInvite(apply.InviteCode)
				}
			} else {
				s.rollbackApplyAndInvite(applyID, apply.InviteCode, inviteConsumed)
				c.ResponseError(errors.New("加入空间失败"))
				return
			}
		}

		go s.notifyApplicantJoinResult(apply.UID, spaceId, space.Name, true)
	} else {
		_, err = s.db.updateJoinApplyStatus(applyID, 2, reviewerUID)
		if err != nil {
			c.ResponseError(errors.New("更新申请状态失败"))
			return
		}
		go s.notifyApplicantJoinResult(apply.UID, spaceId, space.Name, false)
	}

	c.ResponseOK()
}

// fireSpaceMemberJoinEvent 触发 SpaceMemberJoin 事件
func (s *Space) fireSpaceMemberJoinEvent(uid string, spaceId string) {
	if s.ctx.Event == nil {
		return
	}
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		s.Error("开启SpaceMemberJoin事件事务失败", zap.Error(err))
		return
	}
	eventID, err := s.ctx.EventBegin(&wkevent.Data{
		Event: event.SpaceMemberJoin,
		Type:  wkevent.Message,
		Data: map[string]interface{}{
			"uid":      uid,
			"space_id": spaceId,
		},
	}, tx)
	if err != nil {
		tx.Rollback()
		s.Error("开启SpaceMemberJoin事件失败", zap.Error(err), zap.String("uid", uid), zap.String("spaceId", spaceId))
		return
	}
	if err = tx.Commit(); err != nil {
		s.Error("提交SpaceMemberJoin事件事务失败", zap.Error(err))
		return
	}
	s.ctx.EventCommit(eventID)
}

// loadKnownSpaceIDs 从 DB 加载所有 spaceId 到 ParseChannelID 缓存
func (s *Space) loadKnownSpaceIDs() {
	var ids []string
	_, err := s.db.session.SelectBySql("SELECT space_id FROM space WHERE status=1").Load(&ids)
	if err != nil {
		s.Error("加载已知 spaceId 失败", zap.Error(err))
		return
	}
	spacepkg.RegisterSpaceIDs(ids)
	s.Info("已注册 spaceId 到 ParseChannelID 缓存", zap.Int("count", len(ids)), zap.Strings("ids", ids))
}
