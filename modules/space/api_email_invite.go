package space

import (
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/db"
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
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}

	if err := s.requireSpaceAdmin(spaceId, loginUID); err != nil {
		c.ResponseError(err)
		return
	}
	if s.checkSpaceActive(c, spaceId) {
		return
	}

	var req spaceMemberEmailInviteReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}
	if err := validateMemberInviteReq(&req); err != nil {
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
		s.Error("生成邀请 token 失败", zap.Error(err))
		c.ResponseError(errors.New("生成邀请失败"))
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
		c.ResponseError(errors.New("创建邀请失败"))
		return
	}
	model.Id = id

	resp := toEmailInviteResp(model)
	resp.Token = rawToken
	c.Response(resp)
}

// listMemberEmailInvites 列出空间的 member 类型邀请（全部，不按 creator 过滤——
// 同空间的 admin/owner 均应能看到彼此发出的邀请，便于协同管理）。
func (s *Space) listMemberEmailInvites(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	if err := s.requireSpaceAdmin(spaceId, loginUID); err != nil {
		c.ResponseError(err)
		return
	}

	statusFilter := parseStatusQuery(c.Query("status"))
	pageIndex, pageSize := clampPage(c.GetPage())
	offset := (pageIndex - 1) * pageSize

	list, count, err := s.db.listEmailInvitesBySpace(spaceId, statusFilter, pageSize, offset)
	if err != nil {
		s.Error("查询 member 邀请列表失败", zap.Error(err), zap.String("spaceId", spaceId))
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

// revokeMemberEmailInvite 撤销 member 邀请（仅 pending，仅该 space 的 admin/owner）。
func (s *Space) revokeMemberEmailInvite(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")
	if spaceId == "" {
		c.ResponseError(errors.New("空间ID不能为空"))
		return
	}
	if err := s.requireSpaceAdmin(spaceId, loginUID); err != nil {
		c.ResponseError(err)
		return
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.ResponseError(errors.New("邀请ID无效"))
		return
	}
	inv, err := s.db.queryEmailInviteByID(id)
	if err != nil {
		s.Error("查询邀请失败", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("查询邀请失败"))
		return
	}
	if inv == nil || inv.InviteType != EmailInviteTypeMember || inv.SpaceId != spaceId {
		c.ResponseError(errors.New("邀请不存在"))
		return
	}
	affected, err := s.db.revokeEmailInvite(id)
	if err != nil {
		s.Error("撤销邀请失败", zap.Error(err), zap.Int64("id", id))
		c.ResponseError(errors.New("撤销邀请失败"))
		return
	}
	if affected == 0 {
		c.ResponseError(errors.New("仅 pending 状态邀请可撤销"))
		return
	}
	c.ResponseOK()
}

// requireSpaceAdmin 校验 loginUID 是 spaceId 下的 admin/owner。
// 未达要求时返回带说明的 error，调用方直接 ResponseError。
func (s *Space) requireSpaceAdmin(spaceId, loginUID string) error {
	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		return errors.New("查询成员信息失败")
	}
	if member == nil || member.Role < 1 {
		return errors.New("无权限操作")
	}
	return nil
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
