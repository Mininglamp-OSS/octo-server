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

// spaceMemberEmailInviteReq space owner/admin 发起的 member 类型邮件邀请请求体。
type spaceMemberEmailInviteReq struct {
	Email     string  `json:"email"`
	Role      int     `json:"role"` // 0=普通成员 1=管理员
	ExpiresAt *string `json:"expires_at"`
}

// createMemberEmailInvite 空间 owner/admin 发送一条 member 类型邮件邀请。
func (s *Space) createMemberEmailInvite(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	if spaceId == "" {
		respondSpaceRequestInvalid(c, "space_id")
		return
	}

	if s.requireSpaceAdmin(c, spaceId, loginUID) {
		return
	}
	if s.checkSpaceActive(c, spaceId) {
		return
	}

	var req spaceMemberEmailInviteReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	if err := validateMemberInviteReq(&req); err != nil {
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
		s.Error("生成邀请 token 失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}

	model := &spaceEmailInviteModel{
		TokenHash:  tokenHash,
		InviteType: EmailInviteTypeMember,
		Email:      strings.ToLower(strings.TrimSpace(req.Email)),
		SpaceId:    spaceId,
		Role:       req.Role,
		Status:     EmailInviteStatusPending,
		CreatedBy:  loginUID,
	}
	if expiresAt != nil {
		t := db.Time(*expiresAt)
		model.ExpiresAt = &t
	}
	id, err := s.db.insertEmailInvite(model)
	if err != nil {
		s.Error("写入 member 邮件邀请失败", zap.Error(err), zap.String("spaceId", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	model.Id = id

	// 异步发邮件：邮件失败不应让创建接口失败，否则前端拿不到 invite ID 也无从重发。
	// 浅拷贝再传给 goroutine —— 当前 dispatch 与 toEmailInviteResp 都是纯读，
	// Go 内存模型不会 race，但解耦后任何一方未来加写操作也不会回归 -race。
	// TODO(#1138 follow-up): admin 多次 create 会触发多封邮件。现阶段沿用项目里
	// invite-code 创建端点的策略——仅 IP 级 rate limit，无 per-recipient 节流；
	// 若出现滥用，再叠加 Redis cooldown（与 SendVerifyCode 的 email_rate_limit: 同模式）。
	invCopy := *model
	go s.dispatchInviteEmail(&invCopy, rawToken)

	c.Response(toEmailInviteResp(model))
}

// listMemberEmailInvites 列出空间的 member 类型邀请（全部，不按 creator 过滤——
// 同空间的 admin/owner 均应能看到彼此发出的邀请，便于协同管理）。
func (s *Space) listMemberEmailInvites(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	if spaceId == "" {
		respondSpaceRequestInvalid(c, "space_id")
		return
	}
	if s.requireSpaceAdmin(c, spaceId, loginUID) {
		return
	}

	statusFilter := parseStatusQuery(c.Query("status"))
	pageIndex, pageSize := clampPage(c.GetPage())
	offset := (pageIndex - 1) * pageSize

	list, count, err := s.db.listEmailInvitesBySpace(spaceId, statusFilter, pageSize, offset)
	if err != nil {
		s.Error("查询 member 邀请列表失败", zap.Error(err), zap.String("spaceId", spaceId))
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

// revokeMemberEmailInvite 撤销 member 邀请（仅 pending，仅该 space 的 admin/owner）。
func (s *Space) revokeMemberEmailInvite(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	if spaceId == "" {
		respondSpaceRequestInvalid(c, "space_id")
		return
	}
	if s.requireSpaceAdmin(c, spaceId, loginUID) {
		return
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		respondSpaceRequestInvalid(c, "invite_id")
		return
	}
	inv, err := s.db.queryEmailInviteByID(id)
	if err != nil {
		s.Error("查询邀请失败", zap.Error(err), zap.Int64("id", id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if inv == nil || inv.InviteType != EmailInviteTypeMember || inv.SpaceId != spaceId {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteNotFound, nil, nil)
		return
	}
	affected, err := s.db.revokeEmailInvite(id)
	if err != nil {
		s.Error("撤销邀请失败", zap.Error(err), zap.Int64("id", id))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	if affected == 0 {
		httperr.ResponseErrorL(c, errcode.ErrSpaceEmailInviteProcessed, nil, nil)
		return
	}
	c.ResponseOK()
}

// requireSpaceAdmin 校验 loginUID 是 spaceId 下的 admin/owner。返回 true 表示已
// 写出错误响应、调用方应直接 return（与 checkSpaceActive 同模式，避免向调用方
// 透传服务层错误字符串）。
func (s *Space) requireSpaceAdmin(c *wkhttp.Context, spaceId, loginUID string) bool {
	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		s.Error("查询成员信息失败", zap.Error(err), zap.String("spaceId", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return true
	}
	if member == nil || member.Role < 1 {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return true
	}
	return false
}

// validateMemberInviteReq 校验 member 邀请请求参数。
func validateMemberInviteReq(req *spaceMemberEmailInviteReq) error {
	if err := validateInviteEmail(strings.TrimSpace(req.Email)); err != nil {
		return err
	}
	if req.Role != EmailInviteRoleMember && req.Role != EmailInviteRoleAdmin {
		return errors.New("角色无效")
	}
	return nil
}
