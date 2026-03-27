package space

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// Space 团队空间API
type Space struct {
	ctx *config.Context
	log.Log
	db *DB
}

// New 创建Space实例
func New(ctx *config.Context) *Space {
	return &Space{
		ctx: ctx,
		Log: log.NewTLog("Space"),
		db:  NewDB(ctx),
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
	}

	open := r.Group("/v1/space")
	{
		open.GET("/invite/:invite_code", s.getInviteInfo)
		open.GET("/invite/:invite_code/preview", s.getInvitePreview)
	}
}

// createSpace 创建空间
func (s *Space) createSpace(c *wkhttp.Context) {
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

	spaceId := util.GenerUUID()
	inviteCode := util.GenerUUID()[:8]

	tx, err := s.ctx.DB().Begin()
	if err != nil {
		c.ResponseError(errors.New("开启事务失败"))
		return
	}
	defer tx.RollbackUnlessCommitted()

	err = s.db.insertSpace(&SpaceModel{
		SpaceId:     spaceId,
		Name:        req.Name,
		Description: req.Description,
		Logo:        req.Logo,
		Creator:     loginUID,
		Status:      1,
	}, tx)
	if err != nil {
		c.ResponseError(errors.New("创建空间失败"))
		return
	}

	err = s.db.insertMember(&MemberModel{
		SpaceId: spaceId,
		UID:     loginUID,
		Role:    2, // owner
		Status:  1,
	}, tx)
	if err != nil {
		c.ResponseError(errors.New("添加空间成员失败"))
		return
	}

	if err = tx.Commit(); err != nil {
		c.ResponseError(errors.New("提交事务失败"))
		return
	}

	// 创建默认邀请链接
	resp := map[string]interface{}{
		"space_id":    spaceId,
		"name":        req.Name,
		"description": req.Description,
		"logo":        req.Logo,
	}
	inviteErr := s.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    loginUID,
		Status:     1,
	})
	if inviteErr == nil {
		resp["invite_code"] = inviteCode
	}
	c.Response(resp)

	// BotFather 自动加入新 Space
	_ = s.db.insertMemberIgnore(&MemberModel{
		SpaceId: spaceId,
		UID:     "botfather",
		Role:    0,
		Status:  1,
	})

	// 刷新 ParseChannelID 缓存
	go s.loadKnownSpaceIDs()

	// 触发 SpaceMemberJoin 事件（创建者）
	go s.fireSpaceMemberJoinEvent(loginUID, spaceId)
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

	// 查询邀请码
	inviteCode := ""
	invitations, invErr := s.db.queryInvitationsBySpaceID(spaceId)
	if invErr == nil && len(invitations) > 0 {
		inviteCode = invitations[0].InviteCode
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
		InviteCode:  inviteCode,
		CreatedAt:   detail.CreatedAt.String(),
		UpdatedAt:   detail.UpdatedAt.String(),
	})
}

// updateSpace 更新空间信息
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

	err = s.db.updateSpace(spaceId, req.Name, req.Description, req.Logo, req.PresetGroupIds)
	if err != nil {
		c.ResponseError(errors.New("更新空间失败"))
		return
	}
	c.ResponseOK()
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
		inviteCode := ""
		invitations, invErr := s.db.queryInvitationsBySpaceID(sp.SpaceId)
		if invErr == nil && len(invitations) > 0 {
			inviteCode = invitations[0].InviteCode
		}
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
			InviteCode:  inviteCode,
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

	// 触发 SpaceMemberJoin 事件（每个新成员）
	for _, uid := range newMembers {
		go s.fireSpaceMemberJoinEvent(uid, spaceId)
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

	inviteCode := util.GenerUUID()[:8]
	err = s.db.insertInvitation(&InvitationModel{
		SpaceId:    spaceId,
		InviteCode: inviteCode,
		Creator:    loginUID,
		Status:     1,
	})
	if err != nil {
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
		c.ResponseError(errors.New("邀请码无效或已过期"))
		return
	}

	// 检查是否过期
	if invitation.ExpiresAt != nil {
		expiresAt := time.Time(*invitation.ExpiresAt)
		if !expiresAt.IsZero() && expiresAt.Before(time.Now()) {
			c.ResponseError(errors.New("邀请码已过期"))
			return
		}
	}

	// 原子检查并递增使用次数（防止 TOCTOU 竞态条件）
	allowed, err := s.db.incrementInviteUsedCountAtomic(req.InviteCode)
	if err != nil {
		c.ResponseError(errors.New("检查邀请码使用次数失败"))
		return
	}
	if !allowed {
		c.ResponseError(errors.New("邀请码已达到使用次数上限"))
		return
	}

	// 检查空间是否存在
	space, err := s.db.querySpaceByID(invitation.SpaceId)
	if err != nil || space == nil {
		c.ResponseError(errors.New("空间不存在或已解散"))
		return
	}

	// 检查是否已经是成员
	existing, err := s.db.queryMemberIncludeRemoved(invitation.SpaceId, loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询成员信息失败"))
		return
	}
	if existing != nil {
		if existing.Status == 1 {
			c.ResponseError(errors.New("你已经是该空间成员"))
			return
		}
		// 原子检查容量并重新激活成员
		err = s.db.atomicReactivateMemberIfNotFull(invitation.SpaceId, loginUID, space.MaxUsers)
	} else {
		// 原子检查容量并添加新成员
		err = s.db.atomicAddMemberIfNotFull(invitation.SpaceId, loginUID, space.MaxUsers)
	}
	if err != nil {
		if errors.Is(err, ErrSpaceFull) {
			c.ResponseWithStatus(http.StatusBadRequest, map[string]interface{}{
				"status": "SPACE_FULL",
				"msg":    "空间已满，无法加入",
			})
			return
		}
		c.ResponseError(errors.New("加入空间失败"))
		return
	}

	c.Response(map[string]string{
		"space_id": invitation.SpaceId,
	})

	// 异步加入预设群组（不影响 joinSpace 结果）
	if space.PresetGroupIds != nil && *space.PresetGroupIds != "" {
		go s.joinPresetGroups(loginUID, space.SpaceId, *space.PresetGroupIds)
	}

	// 触发 SpaceMemberJoin 事件
	go s.fireSpaceMemberJoinEvent(loginUID, invitation.SpaceId)
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
		expiresAtStr = time.Time(*invitation.ExpiresAt).Format("2006-01-02 15:04:05")
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
		expiresAtStr = time.Time(*invitation.ExpiresAt).Format("2006-01-02 15:04:05")
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
		MemberCount: 0,   // 不暴露精确成员数量
		Bots:        nil, // 不暴露 Bot 列表
	})
}

// updateInvite 更新邀请码设置
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

	// 检查邀请码是否存在且属于该 Space
	invitation, err := s.db.queryInvitationBySpaceAndCode(spaceId, code)
	if err != nil {
		c.ResponseError(errors.New("查询邀请码失败"))
		return
	}
	if invitation == nil {
		c.ResponseError(errors.New("邀请码不存在"))
		return
	}

	var req updateInviteReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求参数错误"))
		return
	}

	// 解析过期时间
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse("2006-01-02 15:04:05", *req.ExpiresAt)
		if err != nil {
			c.ResponseError(errors.New("过期时间格式错误，请使用 2006-01-02 15:04:05 格式"))
			return
		}
		expiresAt = &t
	}

	err = s.db.updateInvitation(code, req.MaxUses, expiresAt)
	if err != nil {
		c.ResponseError(errors.New("更新邀请码失败"))
		return
	}

	c.ResponseOK()
}

// fireSpaceMemberJoinEvent 触发 SpaceMemberJoin 事件
func (s *Space) fireSpaceMemberJoinEvent(uid string, spaceId string) {
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

