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
	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
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
	// settings / welcomeStore back the per-Space onboarding welcome CRUD
	// (task space-welcome-per-space-admin-crud). settings supplies the platform
	// global fallback; welcomeStore is the per-Space config source of truth.
	settings     *commonmod.SystemSettings
	welcomeStore *commonmod.SpaceWelcomeConfigStore
}

// New 创建Space实例
func New(ctx *config.Context) *Space {
	return &Space{
		ctx:          ctx,
		Log:          log.NewTLog("Space"),
		db:           NewDB(ctx),
		mdb:          newManagerDB(ctx.DB()),
		settings:     commonmod.EnsureSystemSettings(ctx),
		welcomeStore: commonmod.NewSpaceWelcomeConfigStore(ctx.DB()),
	}
}

// checkSpaceActive 检查空间是否处于活跃状态，返回 true 表示已处理错误（空间不活跃）
func (s *Space) checkSpaceActive(c *wkhttp.Context, spaceId string) bool {
	active, err := s.db.isSpaceActive(spaceId)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return true
	}
	if !active {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
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

	// 加入申请提交：一次成功的重申会给该 Space 的每个管理员各发一条私信，是一条
	// 用户可触发的扇出路径。第一道约束是"内容真的变了才通知"（见 joinSpace），
	// 这里补上仓库对认证端点的默认限流（挂在 AuthMiddleware 之后才能读到 uid）。
	joinLimited := r.Group("/v1/space", s.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, s.ctx))
	{
		joinLimited.POST("/join", s.joinSpace)
	}

	search := r.Group("/v1/space", s.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, s.ctx))
	{
		search.GET("/:space_id/members/search", s.searchMembers)

		// Per-Space onboarding welcome config CRUD (space admin, Role>=1). Shares
		// the authenticated + UID-rate-limited group; per-Space authorization is
		// enforced in each handler via requireSpaceAdmin.
		search.GET("/:space_id/welcome", s.getWelcome)
		search.PUT("/:space_id/welcome", s.putWelcome)
		search.DELETE("/:space_id/welcome", s.deleteWelcome)
	}

	// 邀请码预览端点（公开无认证）严格 per-IP 限流：防枚举 + 暴破（issue #1000）。
	// 两个端点共享同一 limiter，使同一 IP 跨端点总配额受控。
	// 阈值与 user 模块 login 同档（10 req/min, burst 5），详见 PR #1090。
	// PoolSize=10：Lua 脚本短事务，与 user 模块 / main.go 保持一致。
	rlRedis := octoredis.NewInstrumentedClient(s.ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	})
	invitePreviewLimit := r.StrictIPRateLimitMiddleware(context.Background(), rlRedis, "space_invite", 10.0/60, 5)

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

// envDisableUserCreateSpace 全局开关的历史 env 入口：运维通过环境变量
// DM_SPACE_DISABLE_USER_CREATE=true 关闭用户侧创建空间入口
// （POST /v1/space/create）。管理端代建接口不受此开关约束。
//
// 单一真源已迁移到 system_setting 表的 (space, disable_user_create) 行；
// 本 env 仍作 fallback 供没有 DB override 的老部署使用,但 admin 在管理台写入
// DB 后,DB 值优先。详见 modules/common/system_settings.go:SpaceDisableUserCreate。
const envDisableUserCreateSpace = "DM_SPACE_DISABLE_USER_CREATE"

// IsUserCreateDisabled 是否已通过环境变量关闭用户侧创建空间。env-only 的低层
// 解析器,语义与 modules/common/system_settings.go:parseSpaceDisableUserCreateEnv
// 保持一致 —— 后者是 system_setting 查询的 env fallback。修改此函数的解析
// 规则时必须同步那一处,否则同一开关会在两个出口产生漂移。
//
// 调用方应优先走 (*Space).isUserCreateDisabled 以获得 DB → env 完整链路；本
// 函数保留是为了向前兼容已有 callers / 测试,以及作为最简 env-only 探测点。
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

// isUserCreateDisabled 走 system_setting DB → env 完整 fallback 链, 是
// createSpace handler 的判断入口。SystemSettings snapshot 由 ticker 自动刷新,
// admin 写 DB → Reload 后 60s 内多实例收敛, 本实例立即生效。
func (s *Space) isUserCreateDisabled() bool {
	return commonmod.EnsureSystemSettings(s.ctx).SpaceDisableUserCreate()
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
	if s.isUserCreateDisabled() {
		// Preserve the prior real 403: createSpace originally returned
		// ResponseErrorWithStatus(403). These clients branch on HTTP 403, so use
		// WithStatus rather than the D14 fixed-400 path (same call as #268's
		// mention_pref owner guards).
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSpaceCreationDisabled, nil, nil)
		return
	}
	loginUID := c.GetLoginUID()
	var req createSpaceReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	if req.Name == "" {
		respondSpaceRequestInvalid(c, "name")
		return
	}
	if req.JoinMode < JoinModeDirect || req.JoinMode > JoinModeApproval {
		respondSpaceRequestInvalid(c, "join_mode")
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
		respondSpaceRequestInvalid(c, "space_id")
		return
	}

	detail, err := s.db.querySpaceDetail(spaceId, loginUID)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if detail == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
		return
	}

	if detail.Role < 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotMember, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	var req updateSpaceReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	if req.Name == nil && req.Description == nil && req.Logo == nil && req.JoinMode == nil && req.PresetGroupIds == nil {
		respondSpaceRequestInvalid(c, "")
		return
	}

	// 字段级校验：长度单位为字符（rune），与 MySQL VARCHAR(N) 的字符语义一致。
	// 复用管理端常量（同包，含义完全相同），避免双写漂移。
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			respondSpaceRequestInvalid(c, "name")
			return
		}
		if utf8.RuneCountInString(trimmed) > managerSpaceNameMaxChars {
			respondSpaceFieldTooLong(c, "name", managerSpaceNameMaxChars)
			return
		}
		req.Name = &trimmed
	}
	if req.Description != nil {
		trimmed := strings.TrimSpace(*req.Description)
		if utf8.RuneCountInString(trimmed) > managerSpaceDescriptionMaxChars {
			respondSpaceFieldTooLong(c, "description", managerSpaceDescriptionMaxChars)
			return
		}
		req.Description = &trimmed
	}
	if req.Logo != nil && utf8.RuneCountInString(*req.Logo) > managerSpaceLogoMaxChars {
		respondSpaceFieldTooLong(c, "logo", managerSpaceLogoMaxChars)
		return
	}
	if req.JoinMode != nil && (*req.JoinMode < JoinModeDirect || *req.JoinMode > JoinModeApproval) {
		respondSpaceRequestInvalid(c, "join_mode")
		return
	}
	if req.PresetGroupIds != nil {
		if err := validatePresetGroupIds(*req.PresetGroupIds); err != nil {
			respondSpaceRequestInvalid(c, "preset_group_ids")
			return
		}
	}

	// allowBanned=false：用户端绝不能更新封禁空间。事务侧二次校验关闭了
	// "checkSpaceActive 通过 → 管理员并发 ban → 用户事务仍 UPDATE 落地" 的 TOCTOU race
	// （Jerry-Xin 在 PR #164 review 中指出的 Critical 问题）。
	before, err := s.mdb.updateSpaceProfile(spaceId, req.Name, req.Description, req.Logo, req.JoinMode, nil, req.PresetGroupIds, false)
	if err != nil {
		// 复用管理端 sentinel，并发解散 / 封禁与 active 检查之间的 race 由事务侧裁决。
		if errors.Is(err, ErrSpaceNotFound) {
			httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
			return
		}
		if errors.Is(err, ErrSpaceDisbandedForUpdate) {
			httperr.ResponseErrorL(c, errcode.ErrSpaceImmutable, nil, nil)
			return
		}
		if errors.Is(err, ErrSpaceBannedForUpdate) {
			httperr.ResponseErrorL(c, errcode.ErrSpaceImmutable, nil, nil)
			return
		}
		s.Error("用户修改空间信息失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("operator", loginUID))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role != 2 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	err = s.db.disbandSpace(spaceId)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotMember, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}

	resps := make([]memberResp, 0, len(members))
	for _, m := range members {
		resps = append(resps, memberResp{
			UID:       m.UID,
			Name:      m.DisplayName(),
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	var req addMemberReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	if len(req.UIDs) == 0 {
		respondSpaceRequestInvalid(c, "members")
		return
	}

	// 检查空间人数上限
	spaceInfo, err := s.db.querySpaceByID(spaceId)
	if err != nil || spaceInfo == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
		return
	}
	if spaceInfo.MaxUsers > 0 {
		memberCount, countErr := s.db.countActiveMembers(spaceId)
		if countErr != nil {
			httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
			return
		}
		if memberCount+len(req.UIDs) > spaceInfo.MaxUsers {
			httperr.ResponseErrorL(c, errcode.ErrSpaceFull, nil, nil)
			return
		}
	}

	newMembers := make([]string, 0, len(req.UIDs))
	for _, uid := range req.UIDs {
		existing, err := s.db.queryMemberIncludeRemoved(spaceId, uid)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
			return
		}
		if existing != nil {
			if existing.Status == 0 {
				if err = s.db.reactivateMember(spaceId, uid, 0); err != nil {
					httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
			httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	var req removeMemberReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}

	for _, uid := range req.UIDs {
		// 角色校验在锁内完成（removeMemberLocked 事务内重读 role）：
		// owner 与同级及更高角色静默跳过，与既有语义一致；锁内重读
		// 防止 pre-check 后目标被并发转让升为 owner 仍被移除（PR #339 review）。
		if err = s.db.removeMemberLocked(spaceId, uid, member.Role); err != nil {
			if errors.Is(err, ErrCannotRemoveOwner) || errors.Is(err, ErrRemoveHierarchy) {
				continue
			}
			s.Error("移除空间成员失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("uid", uid))
			httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotMember, nil, nil)
		return
	}
	if member.Role == 2 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceOwnerConstraint, nil, nil)
		return
	}

	// 锁内重读角色：pre-check 与移除之间可能被并发转让升为 owner（PR #339 review）
	err = s.db.removeMemberLocked(spaceId, loginUID, 2)
	if err != nil {
		if errors.Is(err, ErrCannotRemoveOwner) {
			httperr.ResponseErrorL(c, errcode.ErrSpaceOwnerConstraint, nil, nil)
			return
		}
		s.Error("退出空间失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("uid", loginUID))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role != 2 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	var req updateMemberRoleReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	if req.Role < 0 || req.Role > 2 {
		respondSpaceRequestInvalid(c, "role")
		return
	}

	// 验证目标成员存在且活跃
	target, err := s.db.queryMember(spaceId, targetUID)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if target == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceMemberNotFound, nil, nil)
		return
	}

	// 防无主空间：owner 不能被直接降级（本接口仅 owner 可调，即禁止自降级）。
	// 转让所有权必须通过把其他成员设为 role=2 触发下方的原子对调，
	// 与管理端 updateMemberRole 的约束一致（见 api_manager.go）。
	if target.Role == 2 && req.Role != 2 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceOwnerConstraint, nil, nil)
		return
	}
	// 幂等：目标已是该角色时直接成功。该分支同时挡住「转让给自己」——
	// 否则下方事务会把唯一 owner 先升后降（同一行），产生无主空间。
	if target.Role == req.Role {
		c.ResponseOK()
		return
	}

	// 转让 owner：复用带 SELECT ... FOR UPDATE 行锁的原语（与管理端一致）。
	// 不在 handler 层内联事务——目标行不加锁的话，pre-check 通过后目标被并发
	// 移除，提升 UPDATE 影响 0 行而降级仍执行，会产生无主空间（PR #339 review）。
	if req.Role == 2 {
		if err = s.db.transferOwnerAdmin(spaceId, targetUID); err != nil {
			if errors.Is(err, ErrTransferTargetMissing) {
				httperr.ResponseErrorL(c, errcode.ErrSpaceMemberNotFound, nil, nil)
				return
			}
			s.Error("转让空间所有权失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("targetUID", targetUID))
			httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
			return
		}
	} else {
		// 残余竞态（pre-check 后目标被并发转让升为 owner）由 updateMemberRole
		// 的 role<>2 SQL 守卫兜底（PR #339 review F1）
		err = s.db.updateMemberRole(spaceId, targetUID, req.Role)
		if err != nil {
			s.Error("修改成员角色失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("targetUID", targetUID), zap.Int("role", req.Role))
			httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
		respondSpaceRequestInvalid(c, "")
		return
	}
	if req.InviteCode == "" {
		respondSpaceRequestInvalid(c, "invite_code")
		return
	}

	invitation, err := s.db.queryInvitationByCode(req.InviteCode)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if invitation == nil {
		// queryInvitationByCode 已在 SQL 层过滤 status!=1 与已过期，命中即有效。
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeInvalid, nil, nil)
		return
	}

	// 检查空间是否存在
	space, err := s.db.querySpaceByID(invitation.SpaceId)
	if err != nil || space == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
		return
	}

	// 需要审批模式
	if space.JoinMode == JoinModeApproval {
		// 只读校验邀请码次数（不消耗，审批通过时也跳过）
		if invitation.MaxUses > 0 && invitation.UsedCount >= invitation.MaxUses {
			httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeExhausted, nil, nil)
			return
		}

		// 检查是否已是成员
		existing, err := s.db.queryMemberIncludeRemoved(invitation.SpaceId, loginUID)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
			return
		}
		if existing != nil && existing.Status == 1 {
			httperr.ResponseErrorL(c, errcode.ErrSpaceAlreadyMember, nil, nil)
			return
		}

		// 检查是否已有待处理申请
		pendingApply, err := s.db.queryPendingApplyBySpaceAndUID(invitation.SpaceId, loginUID)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
			return
		}
		if pendingApply != nil {
			// 同一个码重复提交：没有任何变化，保持原有的"已提交，等待审批"语义。
			// 这一分支是通知放大的第一道约束：只有申请内容真的变了才会重新惊动管理员。
			// 注意它只比对*当前存的那个码*，所以在两个有效码之间来回切换仍然次次算"变了"
			// ——真正兜底的是路由上的 SharedUIDRateLimiter（PR #684 review P2-4）。
			if pendingApply.InviteCode == req.InviteCode {
				c.Response(map[string]interface{}{
					"status":   "PENDING",
					"space_id": invitation.SpaceId,
					"msg":      "申请已提交，请等待审批",
				})
				return
			}

			// 换了一个有效邀请码重申：让待审批记录改用本次凭证并刷新申请时间。
			// 此前这里直接返回 PENDING，导致申请永远绑定旧邀请码——旧码一旦用尽/被禁用/
			// 过期，审批必然失败并回滚为待审批，而 uk_space_uid 又使申请人无法另建一条
			// 记录，只能等管理员先拒绝（issue #683）。
			affected, err := s.db.refreshPendingApplyInvite(pendingApply.Id, req.InviteCode)
			if err != nil {
				httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
				return
			}
			if affected > 0 {
				go s.notifyAdminsNewJoinApply(loginUID, invitation.SpaceId, space.Name, pendingApply.Id)

				c.Response(map[string]interface{}{
					"status":   "NEED_APPROVAL",
					"space_id": invitation.SpaceId,
					"msg":      "该空间需要管理员审批，申请已提交",
				})
				return
			}

			// 0 行：状态在本次读取之后被并发改动（管理员正在审批）。重新判定，
			// 绝不能盲目覆盖——否则会把已通过的申请打回待审批。
			latest, err := s.db.queryJoinApplyByID(pendingApply.Id)
			if err != nil {
				httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
				return
			}
			if latest != nil && latest.Status == 1 {
				// 已被通过：申请人此刻已是成员，与本函数前面的成员校验结论一致。
				httperr.ResponseErrorL(c, errcode.ErrSpaceAlreadyMember, nil, nil)
				return
			}
			// 已被拒绝或记录消失：按一次全新申请处理，走下面的 upsert。
		}

		// 创建或重置申请记录（被拒后重新申请时覆盖更新）
		applyID, err := s.db.upsertJoinApply(&spaceJoinApplyModel{
			SpaceId:    invitation.SpaceId,
			UID:        loginUID,
			InviteCode: req.InviteCode,
		})
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
			return
		}

		// upsertJoinApply 拒绝改写已通过的申请，因此回读确认这一行现在是什么。
		//
		// approveJoinApplyAtomic 保证 status=1 只与活跃成员行一起提交，所以看到
		// status=1 只有两种可能：申请人此刻确实是成员；或者审批通过之后成员资格又
		// 结束了（退出/被移除），留下一条陈旧记录。后者必须就地重置，否则守卫会把
		// 退出过的用户永久挡在门外（PR #684 review round 2/3）。
		// 正因为事务消灭了"已通过但还不是成员"这个中间态，这里不需要任何时间阈值。
		saved, err := s.db.queryJoinApplyByID(applyID)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
			return
		}
		if saved != nil && saved.Status == 1 {
			active, err := s.db.queryMember(invitation.SpaceId, loginUID)
			if err != nil {
				httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
				return
			}
			if active != nil {
				httperr.ResponseErrorL(c, errcode.ErrSpaceAlreadyMember, nil, nil)
				return
			}
			if _, err := s.db.resetApprovedApplyForRejoin(applyID, req.InviteCode); err != nil {
				httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
				return
			}
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if !allowed {
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeExhausted, nil, nil)
		return
	}

	// 执行加入逻辑。任何加入失败（含 ErrAlreadyMember：无新增成员）都应归还已消耗的名额。
	joinErr := s.executeJoinSpace(loginUID, invitation.SpaceId, space)
	if joinErr != nil {
		s.refundInvite(req.InviteCode)
		if errors.Is(joinErr, ErrSpaceFull) {
			httperr.ResponseErrorL(c, errcode.ErrSpaceFull, nil, nil)
			return
		}
		if errors.Is(joinErr, ErrAlreadyMember) {
			httperr.ResponseErrorL(c, errcode.ErrSpaceAlreadyMember, nil, nil)
			return
		}
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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

	s.afterJoinSpace(uid, spaceId, space)
	return nil
}

// afterJoinSpace 成员行落库之后的副作用：预设群组、默认分类、事件、缓存失效。
//
// 与成员写入分开，是因为审批路径把成员写入放进了事务（approveJoinApplyAtomic），
// 而这些副作用会调用外部服务、起 goroutine，绝不能持在事务里；它们必须在提交
// 之后执行，否则事务回滚后仍会留下已经外发的副作用。
func (s *Space) afterJoinSpace(uid, spaceId string, space *SpaceModel) {
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
		respondSpaceRequestInvalid(c, "invite_code")
		return
	}

	invitation, err := s.db.queryInvitationByCode(inviteCode)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if invitation == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeInvalid, nil, nil)
		return
	}

	space, err := s.db.querySpaceByID(invitation.SpaceId)
	if err != nil || space == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
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
		respondSpaceRequestInvalid(c, "invite_code")
		return
	}

	invitation, err := s.db.queryInvitationByCode(inviteCode)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if invitation == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeInvalid, nil, nil)
		return
	}

	space, err := s.db.querySpaceByID(invitation.SpaceId)
	if err != nil || space == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	var req updateInviteReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	if req.MaxUses == nil && req.ExpiresAt == nil && req.Status == nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	if req.MaxUses != nil && *req.MaxUses < 0 {
		respondSpaceRequestInvalid(c, "max_uses")
		return
	}
	if req.Status != nil && *req.Status != 0 && *req.Status != 1 {
		respondSpaceRequestInvalid(c, "status")
		return
	}

	// 解析过期时间：与管理端 parseInviteExpiresAt 共用 time.Local 约定，避免双路径写库时区漂移。
	expiresAt, err := parseInviteExpiresAt(req.ExpiresAt)
	if err != nil {
		respondSpaceRequestInvalid(c, "expires_at")
		return
	}

	// 复用管理端的 updateInvitationAdmin：
	//   - WHERE 无 status=1 限制，可对已禁用邀请码重启用（status=1）
	//   - 按 space_id + invite_code 双匹配，天然防跨空间越权
	affected, err := s.mdb.updateInvitationAdmin(spaceId, code, req.MaxUses, expiresAt, req.Status)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	if affected == 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeNotFound, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	count, err := s.db.countInvitesBySpace(spaceId, filter, now)
	if err != nil {
		s.Error("查询邀请码总数失败", zap.Error(err), zap.String("spaceId", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	affected, err := s.mdb.disableInvitation(spaceId, code)
	if err != nil {
		s.Error("禁用邀请码失败", zap.Error(err), zap.String("code", code))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	if affected == 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeNotFound, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	count, err := s.db.queryPendingApplyCountBySpace(spaceId)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
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
		respondSpaceRequestInvalid(c, "apply_id")
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	apply, err := s.db.queryJoinApplyByID(applyID)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if apply == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyNotFound, nil, nil)
		return
	}
	if apply.SpaceId != spaceId {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyNotFound, nil, nil)
		return
	}
	if apply.Status != 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyProcessed, nil, nil)
		return
	}

	space, err := s.db.querySpaceByID(spaceId)
	if err != nil || space == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
		return
	}

	// 状态翻转 + 名额消耗 + 写成员行，单事务完成（见 approveJoinApplyAtomic）
	outcome, failCode, err := s.runAtomicApproval(apply, space, loginUID)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	s.respondApprovalOutcome(c, outcome, failCode, apply, space)
}

// refundInvite 归还一次已消耗的邀请码名额（空码跳过；错误仅记录日志，不影响主流程）。
//
// 仅服务于**直接加入**路径：那条路径在准入前就消耗了名额，加入失败必须退还。
// 审批路径不再使用它——名额消耗与成员写入同处一个事务，失败即整笔回滚，
// 没有需要补偿的已消耗名额（PR #684 round 3）。
func (s *Space) refundInvite(code string) {
	if code == "" {
		return
	}
	if _, err := s.db.decrementInviteUsedCountAtomic(code); err != nil {
		s.Error("回滚邀请码使用次数失败", zap.Error(err), zap.String("inviteCode", code))
	}
}

// runAtomicApproval 是三条审批入口共用的审批执行步骤：Space 内审批
// (approveJoinApply)、管理后台审批 (Manager.approveJoinApply)、以及管理员通知里的
// H5 auth_code 审批 (joinApproveSure)。三处必须走同一实现——只改其中两处会留下一条
// 行为不同的审批面。
//
// 状态翻转、名额消耗、成员写入都在 approveJoinApplyAtomic 的单个事务里完成，
// 失败即整笔回滚，因此这里不再有任何补偿写（旧实现要手工把状态写回 0 并退还名额，
// 那些补偿写自身失败时会留下无人能救的孤儿行）。
//
// 邀请码失效时负责按实际原因写日志，并通知申请人可以换码重申（issue #683 R7）。
// 返回的 failCode 仅在失败终态下有意义。
func (s *Space) runAtomicApproval(apply *spaceJoinApplyModel, space *SpaceModel, reviewerUID string) (approveOutcome, codes.Code, error) {
	outcome, inviteCode, err := s.db.approveJoinApplyAtomic(apply.Id, reviewerUID, apply.SpaceId, space.MaxUsers)
	if err != nil {
		s.Error("原子审批失败", zap.Error(err), zap.Int64("applyID", apply.Id))
		return outcome, errcode.ErrSpaceStoreFailed, err
	}
	if outcome == approveInviteUnusable {
		failCode := s.classifyInviteConsumeFailure(inviteCode, apply.Id)
		// 申请人此前完全收不到任何消息，因此不知道自己需要换一个有效邀请码重申。
		go s.notifyApplicantInviteUnusable(apply.UID, apply.SpaceId, space.Name)
		return outcome, failCode, nil
	}
	return outcome, codes.Code{}, nil
}

// respondApprovalOutcome 把审批终态映射成 HTTP 响应，并在成功时执行提交后的副作用。
// 返回 true 表示已经写出响应，调用方直接 return。
func (s *Space) respondApprovalOutcome(c *wkhttp.Context, outcome approveOutcome, failCode codes.Code,
	apply *spaceJoinApplyModel, space *SpaceModel) {
	switch outcome {
	case approveAlreadyHandled:
		// 已被其他管理员处理，保持幂等
		c.ResponseOK()
	case approveAlreadyMember:
		// 申请人本就是成员：审批已落库，未新增成员，也未消耗名额
		c.ResponseOK()
	case approveInviteUnusable:
		httperr.ResponseErrorL(c, failCode, nil, nil)
	case approveSpaceFull:
		httperr.ResponseErrorL(c, errcode.ErrSpaceFull, nil, nil)
	case approveOK:
		s.afterJoinSpace(apply.UID, apply.SpaceId, space)
		go s.notifyApplicantJoinResult(apply.UID, apply.SpaceId, space.Name, true)
		c.ResponseOK()
	default:
		// 新增终态若忘了在此处理，绝不能悄悄走成功分支放人进空间。
		s.Error("未知的审批终态", zap.Int("outcome", int(outcome)), zap.Int64("applyID", apply.Id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
	}
}

// classifyInviteConsumeFailure 区分邀请码消耗失败的原因，返回对外错误码。
//
// incrementInviteUsedCountAtomic 的 WHERE 同时包含 status=1、未过期、未达 max_uses，
// 任一不满足都只返回 false。此前三种情况一律回报"已达到使用次数上限"，与实际原因
// 不符时会把排查引向错误方向（issue #683）。
//
// 这里不构成枚举面：调用方是已通过 role>=1 / requireAdmin 校验的管理员，且邀请码就
// 记录在他正在审批的申请上——他本来就能看到这个码。精确原因（禁用 / 过期 / 用尽）
// 仍只写日志，对外只分"用尽"与"失效"两类，沿用已注册的错误码。
func (s *Space) classifyInviteConsumeFailure(code string, applyID int64) codes.Code {
	invitation, err := s.db.queryInvitationByCodeUnfiltered(code)
	if err != nil {
		s.Error("查询邀请码失效原因失败", zap.Error(err), zap.Int64("applyID", applyID))
		return errcode.ErrSpaceInviteCodeExhausted
	}
	if invitation == nil {
		s.Warn("审批失败：邀请码已不存在", zap.Int64("applyID", applyID))
		return errcode.ErrSpaceInviteCodeInvalid
	}
	if invitation.Status != 1 {
		s.Warn("审批失败：邀请码已被禁用", zap.Int64("applyID", applyID), zap.Int("status", invitation.Status))
		return errcode.ErrSpaceInviteCodeInvalid
	}
	if invitation.ExpiresAt != nil && !time.Time(*invitation.ExpiresAt).After(time.Now()) {
		s.Warn("审批失败：邀请码已过期", zap.Int64("applyID", applyID))
		return errcode.ErrSpaceInviteCodeInvalid
	}
	s.Warn("审批失败：邀请码使用次数已达上限",
		zap.Int64("applyID", applyID),
		zap.Int("usedCount", invitation.UsedCount),
		zap.Int("maxUses", invitation.MaxUses))
	return errcode.ErrSpaceInviteCodeExhausted
}

// rejectJoinApply 管理员拒绝加入申请
func (s *Space) rejectJoinApply(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	applyIDStr := c.Param("id")

	applyID, err := strconv.ParseInt(applyIDStr, 10, 64)
	if err != nil || applyID <= 0 {
		respondSpaceRequestInvalid(c, "apply_id")
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	apply, err := s.db.queryJoinApplyByID(applyID)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if apply == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyNotFound, nil, nil)
		return
	}
	if apply.SpaceId != spaceId {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyNotFound, nil, nil)
		return
	}
	if apply.Status != 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyProcessed, nil, nil)
		return
	}

	_, err = s.db.updateJoinApplyStatus(applyID, 2, loginUID)
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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

// notifyApplicantInviteUnusable 告知申请人：审批因其申请所绑定的邀请码已失效而未能完成，
// 需要用一个仍然有效的邀请码重新提交。
//
// 没有这条通知，申请人无从得知自己卡住了，joinSpace 里新增的"重申即换码"自救路径
// 也就不会被用到，申请会一直停在待审批（issue #683 R7）。
// 与同文件其它站内通知一致使用中文文案：IM 私信内容尚未接入 i18n（错误响应与邮件才有），
// 这里刻意保持一致而不是单独引入一套。
func (s *Space) notifyApplicantInviteUnusable(applicantUID, spaceId, spaceName string) {
	content := fmt.Sprintf(
		"你的 Space 加入申请暂时无法通过\n空间: %s\n原因: 申请使用的邀请码已失效（次数用尽、被禁用或已过期）\n请向管理员索取新的邀请链接后重新提交申请",
		spaceName)

	payload := map[string]interface{}{
		"content": content,
		"type":    common.Text,
	}
	// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM via NewPersonalMsgSendReq builder.
	if err := s.ctx.SendMessage(config.NewPersonalMsgSendReq(
		applicantUID,
		s.ctx.GetConfig().Account.SystemUID,
		payload,
		spaceId,
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
	)); err != nil {
		s.Warn("发送邀请码失效通知失败", zap.Error(err), zap.String("applicantUID", applicantUID), zap.String("spaceId", spaceId))
	}
}

// joinApprovePage 返回 H5 审批页面（注入 apiURL）
func (s *Space) joinApprovePage(c *wkhttp.Context) {
	htmlBytes, err := os.ReadFile("./assets/web/space_join_approve.html")
	if err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
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
		respondSpaceRequestInvalid(c, "auth_code")
		return
	}

	authInfo, err := s.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode))
	if err != nil || authInfo == "" {
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeInvalid, nil, nil)
		return
	}

	var authMap map[string]interface{}
	if err := util.ReadJsonByByte([]byte(authInfo), &authMap); err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeInvalid, nil, nil)
		return
	}

	authType, _ := authMap["type"].(string)
	if authType != "spaceJoinApprove" {
		respondSpaceRequestInvalid(c, "auth_code")
		return
	}

	applyIDNum, _ := authMap["apply_id"].(json.Number)
	applyID, _ := applyIDNum.Int64()
	spaceId, _ := authMap["space_id"].(string)

	apply, err := s.db.queryJoinApplyByID(applyID)
	if err != nil || apply == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyNotFound, nil, nil)
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
		respondSpaceRequestInvalid(c, "auth_code")
		return
	}
	if action != "approve" && action != "reject" {
		respondSpaceRequestInvalid(c, "action")
		return
	}

	cacheKey := fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode)
	authInfo, err := s.ctx.GetRedisConn().GetString(cacheKey)
	if err != nil || authInfo == "" {
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeInvalid, nil, nil)
		return
	}
	// 保留 auth_code 让其自然过期，审批后仍可查看详情
	// DB 层 WHERE status=0 已原子防重

	var authMap map[string]interface{}
	if err := util.ReadJsonByByte([]byte(authInfo), &authMap); err != nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceInviteCodeInvalid, nil, nil)
		return
	}

	authType, _ := authMap["type"].(string)
	if authType != "spaceJoinApprove" {
		respondSpaceRequestInvalid(c, "auth_code")
		return
	}

	applyIDNum, _ := authMap["apply_id"].(json.Number)
	applyID, _ := applyIDNum.Int64()
	spaceId, _ := authMap["space_id"].(string)
	reviewerUID, _ := authMap["reviewer_uid"].(string)

	apply, err := s.db.queryJoinApplyByID(applyID)
	if err != nil || apply == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyNotFound, nil, nil)
		return
	}
	if apply.SpaceId != spaceId {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyNotFound, nil, nil)
		return
	}
	if apply.Status != 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceApplyProcessed, nil, nil)
		return
	}

	space, err := s.db.querySpaceByID(spaceId)
	if err != nil || space == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotFound, nil, nil)
		return
	}

	if action == "approve" {
		// 与另外两条审批入口共用同一个原子实现
		outcome, failCode, err := s.runAtomicApproval(apply, space, reviewerUID)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
			return
		}
		s.respondApprovalOutcome(c, outcome, failCode, apply, space)
		return
	} else {
		_, err = s.db.updateJoinApplyStatus(applyID, 2, reviewerUID)
		if err != nil {
			httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
