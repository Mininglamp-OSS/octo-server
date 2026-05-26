package space

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// 空间基础字段长度上限，单位为**字符（rune）**，与 sql/20260307000002_space_legacy01.sql
// 中的 VARCHAR(N) 语义一致 —— MySQL VARCHAR(N) 限制的是字符数而非字节数（utf8mb4
// 下一个汉字仍计为 1）。因此校验必须用 utf8.RuneCountInString 而非 len()，
// 否则 34 个汉字（102 字节）会被误拒，但 schema 完全能容纳。
const (
	managerSpaceNameMaxChars        = 100
	managerSpaceDescriptionMaxChars = 500
	managerSpaceLogoMaxChars        = 200
)

// 管理端分页上限，防止恶意/误操作的大页请求把全表拉出来。
const managerMaxPageSize = 200

// 批量成员操作一次最多处理的 uid 数量，避免长事务拖垮 DB。
const managerMaxBatchUIDs = 200

// clampPage 规范化页码和每页大小，并执行上限保护。
// 入参类型 int64 以直接适配 c.GetPage() 的返回值。
func clampPage(pageIndex, pageSize int64) (int, int) {
	if pageIndex <= 0 {
		pageIndex = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > managerMaxPageSize {
		pageSize = managerMaxPageSize
	}
	return int(pageIndex), int(pageSize)
}

// Manager Space 后台管理 API
type Manager struct {
	ctx *config.Context
	log.Log
	db        *DB
	managerDB *managerDB
	space     *Space
}

// NewManager 创建 Space 管理实例。space 参数用于复用业务侧的 Space 实例
// （共享 executeJoinSpace / notifyApplicantJoinResult / loadKnownSpaceIDs），
// 避免创建冗余实例；space 为 nil 时会兜底自建（主要给老调用点留后路）。
func NewManager(ctx *config.Context, space *Space) *Manager {
	if space == nil {
		space = New(ctx)
	}
	return &Manager{
		ctx:       ctx,
		Log:       log.NewTLog("spaceManager"),
		db:        NewDB(ctx),
		managerDB: newManagerDB(ctx.DB()),
		space:     space,
	}
}

// Route 路由配置。所有路径统一使用复数 `/spaces/`，子资源按 REST 嵌套。
// 注：`/spaces/disabled` 作为静态路径必须先于 `/spaces/:space_id` 注册，
// Gin 内部会让静态路由优先匹配，但显式有序更稳妥。
func (m *Manager) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/manager", m.ctx.AuthMiddleware(r))
	{
		// 空间集合
		auth.GET("/spaces", m.list)                 // 活跃空间列表
		auth.POST("/spaces", m.create)              // 管理端代建空间
		auth.GET("/spaces/disabled", m.disableList) // 已解散 / 已封禁空间列表

		// 空间单体
		auth.GET("/spaces/:space_id", m.detail)                      // 空间详情
		auth.PUT("/spaces/:space_id", m.updateSpaceProfile)          // 修改基础信息（名称/加入方式/成员上限等）
		auth.DELETE("/spaces/:space_id", m.forceDisband)             // 强制解散
		auth.PUT("/spaces/:space_id/status/:status", m.liftBan)      // 封禁(2) / 解禁(1)

		// 成员
		auth.GET("/spaces/:space_id/members", m.members)                    // 成员列表
		auth.POST("/spaces/:space_id/members", m.addMembers)                // 强制添加
		auth.DELETE("/spaces/:space_id/members", m.removeMembers)           // 强制移除
		auth.PUT("/spaces/:space_id/members/:uid/role", m.updateMemberRole) // 修改成员角色

		// 邀请码
		auth.GET("/spaces/:space_id/invites", m.listInvites)            // 列表
		auth.POST("/spaces/:space_id/invites", m.createInvite)          // 创建
		auth.PUT("/spaces/:space_id/invites/:code", m.updateInvite)     // 修改 max_uses / expires_at / status
		auth.DELETE("/spaces/:space_id/invites/:code", m.disableInvite) // 软禁用（等价 PUT status=0）

		// Space-owner 邮件邀请（lazy-create，接受时创建空间并设为 owner）
		auth.POST("/spaces/invites", m.createSpaceOwnerEmailInvite)
		auth.GET("/spaces/invites", m.listSpaceOwnerEmailInvites)
		auth.DELETE("/spaces/invites/:id", m.revokeSpaceOwnerEmailInvite)

		// 加入申请
		auth.GET("/spaces/:space_id/join-applies", m.listJoinApplies)               // 列表
		auth.POST("/spaces/:space_id/join-applies/:id/approve", m.approveJoinApply) // 通过
		auth.POST("/spaces/:space_id/join-applies/:id/reject", m.rejectJoinApply)   // 拒绝
	}
}

// managerSpaceResp 管理后台空间响应
type managerSpaceResp struct {
	SpaceId     string `json:"space_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Logo        string `json:"logo"`
	Creator     string `json:"creator"`
	CreatorName string `json:"creator_name"`
	Status      int    `json:"status"`
	JoinMode    int    `json:"join_mode"`
	MaxUsers    int    `json:"max_users"`
	MemberCount int    `json:"member_count"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// managerMemberResp 管理后台成员响应
type managerMemberResp struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Role      int    `json:"role"`
	Status    int    `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toSpaceResp(m *managerSpaceModel) *managerSpaceResp {
	return &managerSpaceResp{
		SpaceId:     m.SpaceId,
		Name:        m.Name,
		Description: m.Description,
		Logo:        m.Logo,
		Creator:     m.Creator,
		CreatorName: m.CreatorName,
		Status:      m.Status,
		JoinMode:    m.JoinMode,
		MaxUsers:    m.MaxUsers,
		MemberCount: m.MemberCount,
		CreatedAt:   m.CreatedAt.String(),
		UpdatedAt:   m.UpdatedAt.String(),
	}
}

// requireAdmin 统一的 admin/superAdmin 角色检查。未通过时已写入响应，调用方应立即返回。
func (m *Manager) requireAdmin(c *wkhttp.Context) bool {
	if err := c.CheckLoginRole(); err != nil {
		c.ResponseError(err)
		return false
	}
	return true
}

// listByStatuses 分页列表通用实现。statuses 为空时不过滤状态。
func (m *Manager) listByStatuses(c *wkhttp.Context, statuses []int) {
	if !m.requireAdmin(c) {
		return
	}
	keyword := c.Query("keyword")
	pageIndex, pageSize := clampPage(c.GetPage())

	list, err := m.managerDB.querySpaces(keyword, statuses, uint64(pageSize), uint64(pageIndex))
	if err != nil {
		m.Error("查询空间列表失败", zap.Error(err))
		c.ResponseError(errors.New("查询空间列表失败"))
		return
	}
	count, err := m.managerDB.countSpaces(keyword, statuses)
	if err != nil {
		m.Error("查询空间总数失败", zap.Error(err))
		c.ResponseError(errors.New("查询空间总数失败"))
		return
	}

	resp := make([]*managerSpaceResp, 0, len(list))
	for _, sp := range list {
		resp = append(resp, toSpaceResp(sp))
	}
	c.Response(map[string]interface{}{
		"count": count,
		"list":  resp,
	})
}

// list 活跃空间列表
func (m *Manager) list(c *wkhttp.Context) {
	m.listByStatuses(c, []int{SpaceStatusNormal})
}

// managerCreateSpaceReq 管理端代建空间请求体
type managerCreateSpaceReq struct {
	CreatorUID     string  `json:"creator_uid"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	Logo           string  `json:"logo"`
	JoinMode       int     `json:"join_mode"`
	MaxUsers       int     `json:"max_users"`
	PresetGroupIds *string `json:"preset_group_ids"`
}

// create 管理端代建空间：creator 记为目标用户，正常触发 IM 事件/预设群组，
// 绕过 DM_SPACE_DISABLE_USER_CREATE 全局开关。
func (m *Manager) create(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	var req managerCreateSpaceReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if req.CreatorUID == "" {
		c.ResponseError(errors.New("creator_uid 不能为空"))
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
	if req.MaxUsers < 0 {
		c.ResponseError(errors.New("max_users 不能为负"))
		return
	}
	exists, err := m.managerDB.isUserExists(req.CreatorUID)
	if err != nil {
		m.Error("校验用户失败", zap.Error(err), zap.String("creator_uid", req.CreatorUID))
		c.ResponseError(errors.New("校验用户失败"))
		return
	}
	if !exists {
		c.ResponseError(errors.New("目标用户不存在"))
		return
	}
	result, err := m.space.createSpaceCore(createSpaceParams{
		Creator:        req.CreatorUID,
		Name:           req.Name,
		Description:    req.Description,
		Logo:           req.Logo,
		JoinMode:       req.JoinMode,
		MaxUsers:       req.MaxUsers,
		PresetGroupIds: req.PresetGroupIds,
	})
	if err != nil {
		m.Error("管理端代建空间失败", zap.Error(err), zap.String("creator_uid", req.CreatorUID), zap.String("operator", c.GetLoginUID()))
		c.ResponseError(errors.New("创建空间失败"))
		return
	}
	m.Info("管理员代建空间",
		zap.String("spaceId", result.SpaceID),
		zap.String("creator_uid", req.CreatorUID),
		zap.String("operator", c.GetLoginUID()),
	)
	resp := map[string]interface{}{
		"space_id":    result.SpaceID,
		"creator_uid": req.CreatorUID,
		"name":        req.Name,
	}
	if result.InviteCode != "" {
		resp["invite_code"] = result.InviteCode
	}
	c.Response(resp)
}

// disableList 已解散 + 已封禁空间列表
func (m *Manager) disableList(c *wkhttp.Context) {
	m.listByStatuses(c, []int{SpaceStatusDisbanded, SpaceStatusBanned})
}

// detail 空间详情（包含已解散）
func (m *Manager) detail(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	sp, err := m.managerDB.querySpaceIncludeDisbanded(spaceId)
	if err != nil {
		m.Error("查询空间详情失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间详情失败"))
		return
	}
	if sp == nil {
		c.ResponseError(errors.New("空间不存在"))
		return
	}
	c.Response(toSpaceResp(sp))
}

// forceDisband 强制解散空间（同时移除全部成员）
func (m *Manager) forceDisband(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	sp, err := m.managerDB.querySpaceIncludeDisbanded(spaceId)
	if err != nil {
		m.Error("查询空间失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间失败"))
		return
	}
	if sp == nil {
		c.ResponseError(errors.New("空间不存在"))
		return
	}
	if sp.Status == 0 {
		c.ResponseOK()
		return
	}
	if err = m.managerDB.forceDisbandSpace(spaceId); err != nil {
		m.Error("强制解散空间失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("强制解散空间失败"))
		return
	}
	m.Info("管理员强制解散空间", zap.String("spaceId", spaceId), zap.String("operator", c.GetLoginUID()))
	// 刷新 ParseChannelID 缓存，避免已解散的 spaceId 继续被前缀路由认为有效
	go m.space.loadKnownSpaceIDs()
	c.ResponseOK()
}

// members 管理后台查询成员（含已移除）
func (m *Manager) members(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	sp, err := m.managerDB.querySpaceIncludeDisbanded(spaceId)
	if err != nil {
		m.Error("查询空间失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间失败"))
		return
	}
	if sp == nil {
		c.ResponseError(errors.New("空间不存在"))
		return
	}

	keyword := c.Query("keyword")
	pageIndex, pageSize := clampPage(c.GetPage())

	list, err := m.managerDB.queryMembersAdmin(spaceId, keyword, uint64(pageSize), uint64(pageIndex))
	if err != nil {
		m.Error("查询空间成员失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间成员失败"))
		return
	}
	count, err := m.managerDB.countMembersAdmin(spaceId, keyword)
	if err != nil {
		m.Error("查询空间成员总数失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间成员总数失败"))
		return
	}

	resp := make([]*managerMemberResp, 0, len(list))
	for _, mem := range list {
		resp = append(resp, &managerMemberResp{
			UID:       mem.UID,
			Name:      mem.Name,
			Role:      mem.Role,
			Status:    mem.Status,
			CreatedAt: mem.CreatedAt.String(),
			UpdatedAt: mem.UpdatedAt.String(),
		})
	}
	c.Response(map[string]interface{}{
		"count": count,
		"list":  resp,
	})
}

// ==================== P1 handlers ====================

// liftBan 封禁 / 解禁空间：status=1 恢复正常，status=2 置为封禁。
// status=0（解散）请用 DELETE /space/:space_id。
func (m *Manager) liftBan(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	statusStr := c.Param("status")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	status, err := strconv.Atoi(statusStr)
	if err != nil || (status != SpaceStatusNormal && status != SpaceStatusBanned) {
		c.ResponseError(errors.New("无效的状态值，仅支持 1(正常) 或 2(封禁)"))
		return
	}
	sp, err := m.managerDB.querySpaceIncludeDisbanded(spaceId)
	if err != nil {
		m.Error("查询空间失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间失败"))
		return
	}
	if sp == nil {
		c.ResponseError(errors.New("空间不存在"))
		return
	}
	if sp.Status == SpaceStatusDisbanded {
		c.ResponseError(errors.New("空间已解散，无法更新状态"))
		return
	}
	if sp.Status == status {
		c.ResponseOK()
		return
	}
	if err = m.managerDB.updateSpaceStatus(spaceId, status); err != nil {
		m.Error("更新空间状态失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("更新空间状态失败"))
		return
	}
	m.Info("管理员修改空间状态", zap.String("spaceId", spaceId), zap.String("operator", c.GetLoginUID()), zap.Int("from", sp.Status), zap.Int("to", status))
	// 刷新 ParseChannelID 缓存：loadKnownSpaceIDs 只加载 status=1 的空间，
	// 封禁 1→2 需要把该 spaceId 从缓存中剔除，解禁 2→1 需要加回去，否则路由会走偏。
	go m.space.loadKnownSpaceIDs()
	c.ResponseOK()
}

// managerUpdateSpaceProfileReq 管理端修改空间基础信息请求体。
// 所有字段可选，但至少需要提供一个；未提供（nil）的字段保持不变。
type managerUpdateSpaceProfileReq struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Logo        *string `json:"logo"`
	JoinMode    *int    `json:"join_mode"`
	MaxUsers    *int    `json:"max_users"`
}

// updateSpaceProfile 管理端修改空间基础信息：名称、描述、Logo、加入方式、成员上限。
//
// 已解散空间禁止修改；max_users 若 > 0 则与当前活跃成员数对比拒绝低于的请求。
// 该 max_users 校验是**尽力而为**：成员计数与最终 UPDATE 不在同一事务，
// 若并发 addMembers / 用户加入挤进窗口，可能出现 active > max_users 的瞬时不一致。
// 这与现有 addMembers 主动绕过 max_users 的设计取舍一致，且本接口仅由管理员调用、频率极低。
// max_users == 0 表示不限，规则上始终允许。
func (m *Manager) updateSpaceProfile(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}

	var req managerUpdateSpaceProfileReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if req.Name == nil && req.Description == nil && req.Logo == nil && req.JoinMode == nil && req.MaxUsers == nil {
		c.ResponseError(errors.New("至少需要提供 name / description / logo / join_mode / max_users 之一"))
		return
	}

	// 字段级校验：在查 DB 前先把请求侧问题拦下。
	// 长度单位为字符（rune），与 MySQL VARCHAR(N) 的字符语义一致。
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
	if req.MaxUsers != nil && *req.MaxUsers < 0 {
		c.ResponseError(errors.New("max_users 不能为负"))
		return
	}

	sp, err := m.managerDB.querySpaceIncludeDisbanded(spaceId)
	if err != nil {
		m.Error("查询空间失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间失败"))
		return
	}
	if sp == nil {
		c.ResponseError(errors.New("空间不存在"))
		return
	}
	if sp.Status == SpaceStatusDisbanded {
		c.ResponseError(errors.New("空间已解散，无法修改空间信息"))
		return
	}

	// max_users > 0 时不得低于当前活跃成员数；0 表示不限，跳过校验。
	// countActiveMembers 走的是 m.db（业务侧 session）而非 m.managerDB，与本文件其他 handler 一致；
	// 两个 session 共享同一 *sql.DB 连接池，差异仅在 builder 包装，无功能性影响。
	if req.MaxUsers != nil && *req.MaxUsers > 0 {
		active, err := m.db.countActiveMembers(spaceId)
		if err != nil {
			m.Error("查询空间成员数失败", zap.Error(err), zap.String("spaceId", spaceId))
			c.ResponseError(errors.New("查询空间成员数失败"))
			return
		}
		if *req.MaxUsers < active {
			c.ResponseError(fmt.Errorf("max_users (%d) 不能低于当前活跃成员数 (%d)", *req.MaxUsers, active))
			return
		}
	}

	// presetGroupIds 仅由用户侧 PUT /v1/space/:space_id 使用，管理端固定传 nil。
	before, err := m.managerDB.updateSpaceProfile(spaceId, req.Name, req.Description, req.Logo, req.JoinMode, req.MaxUsers, nil)
	if err != nil {
		// 事务内 SELECT FOR UPDATE 用 sentinel error 报告并发场景；HTTP 层映射为 4xx 提示。
		// 不能依赖 RowsAffected 区分：MySQL 默认返回变更行数而非匹配行数，
		// 字段值与现值完全相同的幂等请求会被误判为"行不存在"。
		if errors.Is(err, ErrSpaceNotFound) {
			c.ResponseError(errors.New("空间不存在"))
			return
		}
		if errors.Is(err, ErrSpaceDisbandedForUpdate) {
			c.ResponseError(errors.New("空间已解散，无法修改空间信息"))
			return
		}
		m.Error("更新空间信息失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("更新空间信息失败"))
		return
	}
	// 审计日志：from 值取自事务内锁定时的快照（before），避免并发更新窗口下 tx 外读到 stale 旧值。
	fields := []zap.Field{
		zap.String("spaceId", spaceId),
		zap.String("operator", c.GetLoginUID()),
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
	if req.MaxUsers != nil {
		fields = append(fields, zap.Int("maxUsersFrom", before.MaxUsers), zap.Int("maxUsersTo", *req.MaxUsers))
	}
	m.Info("管理员修改空间信息", fields...)
	c.ResponseOK()
}

// addMembers 管理员强制添加成员（绕过 max_users 限制）。
// 注意：此操作绕过了 executeJoinSpace 的业务副作用（SpaceMemberJoin 事件、预设群组），
// 属于 low-level 管理操作；常规入口请走 /v1/space/join。
func (m *Manager) addMembers(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	sp, err := m.managerDB.querySpaceIncludeDisbanded(spaceId)
	if err != nil {
		m.Error("查询空间失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间失败"))
		return
	}
	if sp == nil {
		c.ResponseError(errors.New("空间不存在"))
		return
	}
	if sp.Status == SpaceStatusDisbanded {
		c.ResponseError(errors.New("空间已解散，无法添加成员"))
		return
	}
	var req addMemberReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	uids := normalizeUIDs(req.UIDs)
	if len(uids) == 0 {
		c.ResponseError(errors.New("成员列表不能为空"))
		return
	}
	if len(uids) > managerMaxBatchUIDs {
		c.ResponseError(fmt.Errorf("单次最多处理 %d 个成员", managerMaxBatchUIDs))
		return
	}
	if err := m.managerDB.upsertMembers(spaceId, uids); err != nil {
		m.Error("添加成员失败", zap.Error(err), zap.String("spaceId", spaceId), zap.Strings("uids", uids))
		c.ResponseError(errors.New("添加成员失败"))
		return
	}
	m.Info("管理员添加空间成员", zap.String("spaceId", spaceId), zap.String("operator", c.GetLoginUID()), zap.Strings("uids", uids))
	// 为每个成员补齐默认分类（GH octo-server#1228）。管理端 upsert 不区分新/旧成员，
	// 一律调用一次 ensure；底层函数幂等（INSERT IGNORE + 唯一索引），已存在则 no-op。
	for _, uid := range uids {
		uid := uid
		go ensureDefaultCategoryProvisioned(m.space.ctx, uid, spaceId, m)
	}
	c.ResponseOK()
}

// removeMembers 管理员强制移除成员。
// 禁止移除 owner——实际检查在 managerDB.removeMembersForce 的事务内用
// SELECT ... FOR UPDATE 原子完成，避免 handler 层 check 与 update 之间的 TOCTOU。
func (m *Manager) removeMembers(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	sp, err := m.managerDB.querySpaceIncludeDisbanded(spaceId)
	if err != nil {
		m.Error("查询空间失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间失败"))
		return
	}
	if sp == nil {
		c.ResponseError(errors.New("空间不存在"))
		return
	}
	var req removeMemberReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	uids := normalizeUIDs(req.UIDs)
	if len(uids) == 0 {
		c.ResponseError(errors.New("成员列表不能为空"))
		return
	}
	if len(uids) > managerMaxBatchUIDs {
		c.ResponseError(fmt.Errorf("单次最多处理 %d 个成员", managerMaxBatchUIDs))
		return
	}
	if err := m.managerDB.removeMembersForce(spaceId, uids); err != nil {
		if errors.Is(err, ErrCannotRemoveOwner) {
			c.ResponseError(errors.New("无法移除拥有者，请先通过修改角色转让所有权"))
			return
		}
		m.Error("移除成员失败", zap.Error(err), zap.String("spaceId", spaceId), zap.Strings("uids", uids))
		c.ResponseError(errors.New("移除成员失败"))
		return
	}
	m.Info("管理员移除空间成员", zap.String("spaceId", spaceId), zap.String("operator", c.GetLoginUID()), zap.Strings("uids", uids))
	c.ResponseOK()
}

// normalizeUIDs 去重 + 过滤空字符串，保持输入顺序。
func normalizeUIDs(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// updateMemberRole 修改成员角色；role=2 时自动把当前 owner 降级为 admin。
func (m *Manager) updateMemberRole(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	targetUID := c.Param("uid")
	if spaceId == "" || targetUID == "" {
		c.ResponseError(errors.New("空间ID或成员UID不能为空"))
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
	target, err := m.db.queryMember(spaceId, targetUID)
	if err != nil {
		m.Error("查询目标成员失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("uid", targetUID))
		c.ResponseError(errors.New("查询目标成员失败"))
		return
	}
	if target == nil {
		c.ResponseError(errors.New("目标成员不存在或已被移除"))
		return
	}
	// 禁止把 owner 直接降级（否则空间无主）；必须通过设置其他成员为 role=2 触发 transferOwnerAdmin 来转移。
	if target.Role == 2 && req.Role != 2 {
		c.ResponseError(errors.New("无法直接降级拥有者，请将其他成员设为拥有者以转让所有权"))
		return
	}
	if req.Role == 2 {
		if err = m.managerDB.transferOwnerAdmin(spaceId, targetUID); err != nil {
			if errors.Is(err, ErrTransferTargetMissing) {
				c.ResponseError(errors.New("目标成员已被移除，无法转让所有权"))
				return
			}
			m.Error("转让拥有权失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("uid", targetUID))
			c.ResponseError(errors.New("转让拥有权失败"))
			return
		}
	} else {
		if err = m.db.updateMemberRole(spaceId, targetUID, req.Role); err != nil {
			m.Error("修改角色失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("uid", targetUID))
			c.ResponseError(errors.New("修改角色失败"))
			return
		}
	}
	m.Info("管理员修改成员角色", zap.String("spaceId", spaceId), zap.String("uid", targetUID), zap.Int("role", req.Role), zap.String("operator", c.GetLoginUID()))
	c.ResponseOK()
}

// managerInviteResp 管理后台邀请响应
type managerInviteResp struct {
	InviteCode string `json:"invite_code"`
	SpaceId    string `json:"space_id"`
	Creator    string `json:"creator"`
	MaxUses    int    `json:"max_uses"`
	UsedCount  int    `json:"used_count"`
	ExpiresAt  string `json:"expires_at"`
	Status     int    `json:"status"`
	CreatedAt  string `json:"created_at"`
}

// listInvites 查询空间全部邀请码（含已禁用）
func (m *Manager) listInvites(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	pageIndex, pageSize := clampPage(c.GetPage())
	list, err := m.managerDB.queryInvitesAdmin(spaceId, uint64(pageSize), uint64(pageIndex))
	if err != nil {
		m.Error("查询邀请码失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询邀请码失败"))
		return
	}
	count, err := m.managerDB.countInvitesAdmin(spaceId)
	if err != nil {
		m.Error("查询邀请码总数失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询邀请码总数失败"))
		return
	}
	resp := make([]*managerInviteResp, 0, len(list))
	for _, inv := range list {
		expiresAt := ""
		if inv.ExpiresAt != nil {
			expiresAt = inv.ExpiresAt.String()
		}
		resp = append(resp, &managerInviteResp{
			InviteCode: inv.InviteCode,
			SpaceId:    inv.SpaceId,
			Creator:    inv.Creator,
			MaxUses:    inv.MaxUses,
			UsedCount:  inv.UsedCount,
			ExpiresAt:  expiresAt,
			Status:     inv.Status,
			CreatedAt:  inv.CreatedAt.String(),
		})
	}
	c.Response(map[string]interface{}{
		"count": count,
		"list":  resp,
	})
}

// managerCreateInviteReq 管理端创建邀请码请求体
type managerCreateInviteReq struct {
	MaxUses   *int    `json:"max_uses"`
	ExpiresAt *string `json:"expires_at"`
}

// managerUpdateInviteReq 管理端修改邀请码请求体
type managerUpdateInviteReq struct {
	MaxUses   *int    `json:"max_uses"`
	ExpiresAt *string `json:"expires_at"`
	Status    *int    `json:"status"` // 0=禁用 1=启用
}

// createInvite 管理端为空间创建邀请码，未指定字段走默认值（DM_SPACE_INVITE_DEFAULT_*）。
func (m *Manager) createInvite(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	sp, err := m.managerDB.querySpaceIncludeDisbanded(spaceId)
	if err != nil {
		m.Error("查询空间失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间失败"))
		return
	}
	if sp == nil {
		c.ResponseError(errors.New("空间不存在"))
		return
	}
	// 正向校验：仅正常状态空间可创建邀请码（封禁空间即使创建了，加入时也会被 querySpaceByID 拒）。
	if sp.Status != SpaceStatusNormal {
		c.ResponseError(errors.New("仅正常状态的空间可创建邀请码"))
		return
	}

	var req managerCreateInviteReq
	if c.Request.ContentLength > 0 {
		if err := c.BindJSON(&req); err != nil {
			c.ResponseError(errors.New("请求参数错误"))
			return
		}
	}
	expiresAt, err := parseInviteExpiresAt(req.ExpiresAt)
	if err != nil {
		c.ResponseError(err)
		return
	}

	operator := c.GetLoginUID()
	model := &InvitationModel{
		SpaceId: spaceId,
		Creator: operator,
		Status:  1,
	}
	// 按字段应用默认值：req 中未传的字段走环境变量默认，传了的（即使 0）透传用户意图。
	// 这样 `{"max_uses": 0}` 能表达"不限使用"而不被默认值覆盖。
	defMaxUses, defExpiresAt := inviteDefaults(time.Now())
	if req.MaxUses != nil {
		if *req.MaxUses < 0 {
			c.ResponseError(errors.New("max_uses 不能为负"))
			return
		}
		model.MaxUses = *req.MaxUses
	} else {
		model.MaxUses = defMaxUses
	}
	if expiresAt != nil {
		t := db.Time(*expiresAt)
		model.ExpiresAt = &t
	} else if defExpiresAt != nil {
		t := db.Time(*defExpiresAt)
		model.ExpiresAt = &t
	}

	code, err := m.space.insertInvitationWithRetry(model)
	if err != nil {
		m.Error("管理端创建邀请码失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("operator", operator))
		c.ResponseError(errors.New("创建邀请码失败"))
		return
	}

	m.Info("管理员创建邀请码",
		zap.String("spaceId", spaceId),
		zap.String("code", code),
		zap.String("operator", operator),
	)

	// 显式 Format 保证响应格式与 parseInviteExpiresAt 接受的输入格式一致，
	// 避免客户端拿到响应后回传被 parse 拒绝。
	expiresStr := ""
	if model.ExpiresAt != nil {
		expiresStr = time.Time(*model.ExpiresAt).Format(inviteTimeLayout)
	}
	c.Response(map[string]interface{}{
		"invite_code": code,
		"space_id":    spaceId,
		"creator":     operator,
		"max_uses":    model.MaxUses,
		"expires_at":  expiresStr,
		"status":      model.Status,
	})
}

// updateInvite 管理端修改邀请码 max_uses / expires_at / status，未传字段保持不变。
// status=0 等价于软禁用（与 DELETE 路径一致）。
func (m *Manager) updateInvite(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	code := c.Param("code")
	if spaceId == "" || code == "" {
		c.ResponseError(errors.New("空间ID或邀请码不能为空"))
		return
	}

	var req managerUpdateInviteReq
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
	expiresAt, err := parseInviteExpiresAt(req.ExpiresAt)
	if err != nil {
		c.ResponseError(err)
		return
	}

	affected, err := m.managerDB.updateInvitationAdmin(spaceId, code, req.MaxUses, expiresAt, req.Status)
	if err != nil {
		m.Error("修改邀请码失败", zap.Error(err), zap.String("spaceId", spaceId), zap.String("code", code))
		c.ResponseError(errors.New("修改邀请码失败"))
		return
	}
	if affected == 0 {
		c.ResponseError(errors.New("邀请码不存在"))
		return
	}

	m.Info("管理员修改邀请码",
		zap.String("spaceId", spaceId),
		zap.String("code", code),
		zap.String("operator", c.GetLoginUID()),
	)
	c.ResponseOK()
}

// inviteTimeLayout 邀请码 API 的 expires_at 时间格式。
// 用户侧 updateInvite 与管理端 create/updateInvite 共用此常量，避免双写路径漂移。
const inviteTimeLayout = "2006-01-02 15:04:05"

// parseInviteExpiresAt 解析 expires_at 字符串，空字符串视为未传。
// 时区采用服务器 time.Local——管理端与用户侧统一共用本函数。
// 部署环境应显式设置 TZ，确保客户端发送的"服务器本地时间"解释一致。
func parseInviteExpiresAt(raw *string) (*time.Time, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	t, err := time.ParseInLocation(inviteTimeLayout, *raw, time.Local)
	if err != nil {
		return nil, errors.New("过期时间格式错误，请使用 2006-01-02 15:04:05 格式")
	}
	return &t, nil
}

// disableInvite 禁用邀请码
func (m *Manager) disableInvite(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	code := c.Param("code")
	if spaceId == "" || code == "" {
		c.ResponseError(errors.New("空间ID或邀请码不能为空"))
		return
	}
	affected, err := m.managerDB.disableInvitation(spaceId, code)
	if err != nil {
		m.Error("禁用邀请码失败", zap.Error(err), zap.String("code", code))
		c.ResponseError(errors.New("禁用邀请码失败"))
		return
	}
	if affected == 0 {
		c.ResponseError(errors.New("邀请码不存在"))
		return
	}
	m.Info("管理员禁用邀请码", zap.String("spaceId", spaceId), zap.String("code", code), zap.String("operator", c.GetLoginUID()))
	c.ResponseOK()
}

// managerJoinApplyResp 管理后台申请响应
type managerJoinApplyResp struct {
	ID            int64  `json:"id"`
	SpaceId       string `json:"space_id"`
	UID           string `json:"uid"`
	ApplicantName string `json:"applicant_name"`
	InviteCode    string `json:"invite_code"`
	Remark        string `json:"remark"`
	Status        int    `json:"status"`
	ReviewerUID   string `json:"reviewer_uid"`
	CreatedAt     string `json:"created_at"`
}

// listJoinApplies 查询申请列表。query 支持 status 过滤（不传则返回全部）
func (m *Manager) listJoinApplies(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	status := -1
	if s := c.Query("status"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v >= 0 && v <= 2 {
			status = v
		}
	}
	pageIndex, pageSize := clampPage(c.GetPage())

	list, err := m.managerDB.queryJoinAppliesAdmin(spaceId, status, uint64(pageSize), uint64(pageIndex))
	if err != nil {
		m.Error("查询申请列表失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询申请列表失败"))
		return
	}
	count, err := m.managerDB.countJoinAppliesAdmin(spaceId, status)
	if err != nil {
		m.Error("查询申请总数失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询申请总数失败"))
		return
	}
	resp := make([]*managerJoinApplyResp, 0, len(list))
	for _, a := range list {
		name := a.ApplicantName
		if name == "" {
			name = a.UID
		}
		resp = append(resp, &managerJoinApplyResp{
			ID:            a.Id,
			SpaceId:       a.SpaceId,
			UID:           a.UID,
			ApplicantName: name,
			InviteCode:    a.InviteCode,
			Remark:        a.Remark,
			Status:        a.Status,
			ReviewerUID:   a.ReviewerUID,
			CreatedAt:     a.CreatedAt.String(),
		})
	}
	c.Response(map[string]interface{}{
		"count": count,
		"list":  resp,
	})
}

// approveJoinApply 管理员审批通过：复用 Space.executeJoinSpace 的加入逻辑。
func (m *Manager) approveJoinApply(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	applyIDStr := c.Param("id")
	reviewerUID := c.GetLoginUID()

	applyID, err := strconv.ParseInt(applyIDStr, 10, 64)
	if err != nil || applyID <= 0 {
		c.ResponseError(errors.New("申请ID无效"))
		return
	}
	apply, err := m.db.queryJoinApplyByID(applyID)
	if err != nil {
		m.Error("查询申请记录失败", zap.Error(err), zap.Int64("applyID", applyID))
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
	sp, err := m.db.querySpaceByID(spaceId)
	if err != nil {
		m.Error("查询空间失败", zap.Error(err), zap.String("spaceId", spaceId))
		c.ResponseError(errors.New("查询空间失败"))
		return
	}
	if sp == nil {
		c.ResponseError(errors.New("空间不存在或已解散"))
		return
	}
	affected, err := m.db.updateJoinApplyStatus(applyID, 1, reviewerUID)
	if err != nil {
		m.Error("更新申请状态失败", zap.Error(err), zap.Int64("applyID", applyID))
		c.ResponseError(errors.New("更新申请状态失败"))
		return
	}
	if affected == 0 {
		c.ResponseOK() // 已被其他人处理
		return
	}

	// 审批通过时消耗邀请码名额（方案 B：在准入时消耗）
	inviteConsumed, consumeErr := m.space.consumeInviteOnApprove(apply.InviteCode)
	if consumeErr != nil {
		m.Error("检查邀请码使用次数失败", zap.Error(consumeErr), zap.Int64("applyID", applyID))
		if _, rbErr := m.db.updateJoinApplyStatusRaw(applyID, 0, ""); rbErr != nil {
			m.Error("回滚申请状态失败", zap.Error(rbErr), zap.Int64("applyID", applyID))
		}
		c.ResponseError(errors.New("检查邀请码使用次数失败"))
		return
	}
	if apply.InviteCode != "" && !inviteConsumed {
		if _, rbErr := m.db.updateJoinApplyStatusRaw(applyID, 0, ""); rbErr != nil {
			m.Error("回滚申请状态失败", zap.Error(rbErr), zap.Int64("applyID", applyID))
		}
		c.ResponseError(errors.New("该邀请码已用尽或已失效，无法通过此申请"))
		return
	}

	if joinErr := m.space.executeJoinSpace(apply.UID, spaceId, sp); joinErr != nil {
		// ErrAlreadyMember：用户已在空间里（并发路径），apply 视作审批成功，但未新增成员，归还名额
		if errors.Is(joinErr, ErrAlreadyMember) {
			if inviteConsumed {
				m.space.refundInvite(apply.InviteCode)
			}
			c.ResponseOK()
			return
		}
		m.space.rollbackApplyAndInvite(applyID, apply.InviteCode, inviteConsumed)
		if errors.Is(joinErr, ErrSpaceFull) {
			c.ResponseError(errors.New("空间已满，无法通过申请"))
			return
		}
		m.Error("加入空间失败", zap.Error(joinErr), zap.Int64("applyID", applyID))
		c.ResponseError(errors.New("加入空间失败"))
		return
	}
	go m.space.notifyApplicantJoinResult(apply.UID, spaceId, sp.Name, true)
	m.Info("管理员通过加入申请", zap.String("spaceId", spaceId), zap.Int64("applyID", applyID), zap.String("applicant", apply.UID), zap.String("operator", reviewerUID))
	c.ResponseOK()
}

// rejectJoinApply 管理员审批拒绝
func (m *Manager) rejectJoinApply(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	spaceId := c.Param("space_id")
	applyIDStr := c.Param("id")
	reviewerUID := c.GetLoginUID()

	applyID, err := strconv.ParseInt(applyIDStr, 10, 64)
	if err != nil || applyID <= 0 {
		c.ResponseError(errors.New("申请ID无效"))
		return
	}
	apply, err := m.db.queryJoinApplyByID(applyID)
	if err != nil {
		m.Error("查询申请记录失败", zap.Error(err), zap.Int64("applyID", applyID))
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
	if _, err = m.db.updateJoinApplyStatus(applyID, 2, reviewerUID); err != nil {
		m.Error("更新申请状态失败", zap.Error(err), zap.Int64("applyID", applyID))
		c.ResponseError(errors.New("更新申请状态失败"))
		return
	}
	sp, spErr := m.db.querySpaceByID(spaceId)
	if spErr != nil {
		m.Warn("查询空间失败", zap.Error(spErr), zap.String("spaceId", spaceId))
	}
	spaceName := spaceId
	if sp != nil {
		spaceName = sp.Name
	}
	go m.space.notifyApplicantJoinResult(apply.UID, spaceId, spaceName, false)
	m.Info("管理员拒绝加入申请", zap.String("spaceId", spaceId), zap.Int64("applyID", applyID), zap.String("applicant", apply.UID), zap.String("operator", reviewerUID))
	c.ResponseOK()
}
