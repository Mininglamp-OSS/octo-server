package space

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/db"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
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

// managerEmailInviteResp 列表与创建成功的公共响应结构。raw token 入库只保存 hash，
// Phase 6 起改为通过邮件投递；API 不再返回明文 token，避免日志/前端意外泄露。
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
}

// createSpaceOwnerEmailInvite 管理端创建一条 owner 类型邮件邀请。
func (m *Manager) createSpaceOwnerEmailInvite(c *wkhttp.Context) {
	if !m.requireAdmin(c) {
		return
	}
	var req managerCreateOwnerEmailInviteReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	if err := validateOwnerInviteReq(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	expiresAt, err := parseInviteExpiresAt(req.ExpiresAt)
	if err != nil {
		respondSpaceRequestInvalid(c, "expires_at")
		return
	}

	rawToken, tokenHash, err := generateEmailInviteToken()
	if err != nil {
		m.Error("生成邀请 token 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	model.Id = id

	// 异步发邮件：邮件失败不应让创建接口失败，否则前端拿不到 invite ID 也无从重发。
	// 浅拷贝再传给 goroutine —— 见 api_email_invite.go 同名注释。
	invCopy := *model
	go m.space.dispatchInviteEmail(&invCopy, rawToken)

	c.Response(toEmailInviteResp(model))
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
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
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
		respondSpaceRequestInvalid(c, "invite_id")
		return
	}
	inv, err := m.db.queryEmailInviteByID(id)
	if err != nil {
		m.Error("查询邀请失败", zap.Error(err), zap.Int64("id", id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if inv == nil || inv.InviteType != EmailInviteTypeOwner {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteNotFound, nil, nil)
		return
	}
	if inv.CreatedBy != c.GetLoginUID() {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}
	affected, err := m.db.revokeEmailInvite(id)
	if err != nil {
		m.Error("撤销邀请失败", zap.Error(err), zap.Int64("id", id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	if affected == 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteProcessed, nil, nil)
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
