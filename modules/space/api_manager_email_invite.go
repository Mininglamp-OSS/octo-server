package space

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/db"
	"go.uber.org/zap"
)

// managerCreateOwnerEmailInviteReq 管理端创建 space-owner 邮件邀请请求体。
type managerCreateOwnerEmailInviteReq struct {
	Email              string  `json:"email"`
	PlannedName        string  `json:"planned_name"`
	PlannedDescription string  `json:"planned_description"`
	PlannedLogo        string  `json:"planned_logo"`
	PlannedMaxUsers    int     `json:"planned_max_users"`
	PlannedJoinMode    int     `json:"planned_join_mode"`
	ExpiresAt          *string `json:"expires_at"`
}

// managerEmailInviteResp 列表与创建成功的公共响应结构。创建时 Token 字段带明文 token；
// 列表响应中 Token 永远为空（入库只保存 hash，无法还原）。
type managerEmailInviteResp struct {
	ID                 int64  `json:"id"`
	InviteType         int    `json:"invite_type"`
	Email              string `json:"email"`
	SpaceId            string `json:"space_id,omitempty"`
	Role               int    `json:"role,omitempty"`
	PlannedName        string `json:"planned_name,omitempty"`
	PlannedDescription string `json:"planned_description,omitempty"`
	PlannedLogo        string `json:"planned_logo,omitempty"`
	PlannedMaxUsers    int    `json:"planned_max_users,omitempty"`
	PlannedJoinMode    int    `json:"planned_join_mode,omitempty"`
	Status             int    `json:"status"`
	ExpiresAt          string `json:"expires_at,omitempty"`
	CreatedBy          string `json:"created_by"`
	ConsumedBy         string `json:"consumed_by,omitempty"`
	ConsumedAt         string `json:"consumed_at,omitempty"`
	CreatedAt          string `json:"created_at"`
	Token              string `json:"token,omitempty"`
}

// createSpaceOwnerEmailInvite 管理端创建一条 owner 类型邮件邀请。
func (m *Manager) createSpaceOwnerEmailInvite(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	var req managerCreateOwnerEmailInviteReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if err := validateOwnerInviteReq(&req); err != nil {
		c.ResponseError(err)
		return
	}
	expiresAt, err := parseInviteExpiresAt(req.ExpiresAt)
	if err != nil {
		c.ResponseError(err)
		return
	}

	rawToken, tokenHash, err := generateEmailInviteToken()
	if err != nil {
		m.Error("生成邀请 token 失败", zap.Error(err))
		c.ResponseError(errors.New("生成邀请失败"))
		return
	}

	model := &spaceEmailInviteModel{
		TokenHash:          tokenHash,
		InviteType:         EmailInviteTypeOwner,
		Email:              strings.ToLower(strings.TrimSpace(req.Email)),
		PlannedName:        strings.TrimSpace(req.PlannedName),
		PlannedDescription: req.PlannedDescription,
		PlannedLogo:        req.PlannedLogo,
		PlannedMaxUsers:    req.PlannedMaxUsers,
		PlannedJoinMode:    req.PlannedJoinMode,
		Status:             EmailInviteStatusPending,
		CreatedBy:          c.GetLoginUID(),
	}
	if expiresAt != nil {
		t := db.Time(*expiresAt)
		model.ExpiresAt = &t
	}
	id, err := m.db.insertEmailInvite(model)
	if err != nil {
		m.Error("写入 owner 邮件邀请失败", zap.Error(err))
		c.ResponseError(errors.New("创建邀请失败"))
		return
	}
	model.Id = id

	resp := toEmailInviteResp(model)
	resp.Token = rawToken
	c.Response(resp)
}

// listSpaceOwnerEmailInvites 管理端列出所有 owner 邀请（仅自己创建的；按创建人过滤避免管理员互见）。
func (m *Manager) listSpaceOwnerEmailInvites(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	statusFilter := parseStatusQuery(c.Query("status"))
	pageIndex, pageSize := clampPage(c.GetPage())
	offset := (pageIndex - 1) * pageSize

	list, count, err := m.db.listEmailInvitesByCreator(
		c.GetLoginUID(), EmailInviteTypeOwner, statusFilter, pageSize, offset,
	)
	if err != nil {
		m.Error("查询 owner 邀请列表失败", zap.Error(err))
		c.ResponseError(errors.New("查询邀请列表失败"))
		return
	}
	resp := make([]*managerEmailInviteResp, 0, len(list))
	for _, it := range list {
		resp = append(resp, toEmailInviteResp(it))
	}
	c.Response(map[string]interface{}{
		"count": count,
		"list":  resp,
	})
}

// revokeSpaceOwnerEmailInvite 撤销一条 pending owner 邀请。仅允许创建者撤销自己的邀请。
func (m *Manager) revokeSpaceOwnerEmailInvite(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.ResponseError(errors.New("邀请ID无效"))
		return
	}
	inv, err := m.db.queryEmailInviteByID(id)
	if err != nil {
		m.Error("查询邀请失败", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("查询邀请失败"))
		return
	}
	if inv == nil || inv.InviteType != EmailInviteTypeOwner {
		c.ResponseError(errors.New("邀请不存在"))
		return
	}
	if inv.CreatedBy != c.GetLoginUID() {
		c.ResponseError(errors.New("无权操作该邀请"))
		return
	}
	affected, err := m.db.revokeEmailInvite(id)
	if err != nil {
		m.Error("撤销邀请失败", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("撤销邀请失败"))
		return
	}
	if affected == 0 {
		c.ResponseError(errors.New("仅 pending 状态邀请可撤销"))
		return
	}
	c.ResponseOK()
}

// ---- helpers ----

// validateOwnerInviteReq 输入校验：邮箱、空间名、计划参数合理性。
func validateOwnerInviteReq(req *managerCreateOwnerEmailInviteReq) error {
	if err := validateInviteEmail(strings.TrimSpace(req.Email)); err != nil {
		return err
	}
	if strings.TrimSpace(req.PlannedName) == "" {
		return errors.New("空间名不能为空")
	}
	if req.PlannedMaxUsers < 0 {
		return errors.New("成员上限必须非负")
	}
	if req.PlannedJoinMode != JoinModeDirect && req.PlannedJoinMode != JoinModeApproval {
		return errors.New("加入模式无效")
	}
	return nil
}

// parseStatusQuery 解析 status 查询参数；未指定或非法时返回 -1 表示不过滤。
func parseStatusQuery(raw string) int {
	if raw == "" {
		return -1
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	switch v {
	case EmailInviteStatusPending, EmailInviteStatusConsumed,
		EmailInviteStatusExpired, EmailInviteStatusRevoked:
		return v
	}
	return -1
}

func toEmailInviteResp(m *spaceEmailInviteModel) *managerEmailInviteResp {
	resp := &managerEmailInviteResp{
		ID:                 m.Id,
		InviteType:         m.InviteType,
		Email:              m.Email,
		SpaceId:            m.SpaceId,
		Role:               m.Role,
		PlannedName:        m.PlannedName,
		PlannedDescription: m.PlannedDescription,
		PlannedLogo:        m.PlannedLogo,
		PlannedMaxUsers:    m.PlannedMaxUsers,
		PlannedJoinMode:    m.PlannedJoinMode,
		Status:             m.Status,
		CreatedBy:          m.CreatedBy,
		ConsumedBy:         m.ConsumedBy,
		CreatedAt:          m.CreatedAt.String(),
	}
	if m.ExpiresAt != nil {
		resp.ExpiresAt = m.ExpiresAt.String()
	}
	if m.ConsumedAt != nil {
		resp.ConsumedAt = m.ConsumedAt.String()
	}
	return resp
}
