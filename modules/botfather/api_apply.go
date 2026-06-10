package botfather

import (
	"fmt"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

const (
	AccessModeRequireApproval = 0 // 需要审批
	AccessModeAutoApprove     = 1 // 自动通过
	AccessModeForbidden       = 2 // 禁止申请
)

// robotApply 申请使用AI
func (bf *BotFather) robotApply(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	if loginUID == "" {
		httperr.ResponseErrorL(c, errcode.ErrSharedAuthRequired, nil, nil)
		return
	}

	var req RobotApplyReq
	if err := c.BindJSON(&req); err != nil {
		bf.Error("数据格式有误", zap.Error(err))
		respondBotfatherRequestInvalid(c, "")
		return
	}

	if req.RobotUID == "" {
		respondBotfatherRequestInvalid(c, "robot_uid")
		return
	}

	// 查询目标AI
	robot, err := bf.db.queryRobotByRobotID(req.RobotUID)
	if err != nil {
		bf.Error("查询机器人失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	if robot == nil || robot.Status != 1 {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherRobotNotFound, nil, nil)
		return
	}

	// 检查是否是自己的AI
	if robot.CreatorUID == loginUID {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherCannotApplyOwnBot, nil, nil)
		return
	}

	// 检查auto_approve（数据库实际字段，两态：0=需要审批 1=自动通过）
	switch robot.AutoApprove {
	case AccessModeAutoApprove:
		// 自动通过：直接建立好友关系
		err = bf.createFriendRelation(loginUID, req.RobotUID)
		if err != nil {
			bf.Error("创建好友关系失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
			return
		}
		c.Response(map[string]interface{}{
			"status":  "approved",
			"message": bf.localizedMessage(c, MsgApplyAutoApproved),
		})
		return
	}

	// 需要审批：检查是否已经是好友
	isFriend, err := bf.userService.IsFriend(loginUID, req.RobotUID)
	if err != nil {
		bf.Error("检查好友关系失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	if isFriend {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherAlreadyFriends, nil, nil)
		return
	}

	// 检查是否有待处理的申请
	applyDB := newRobotApplyDB(bf.ctx)
	existingApply, err := applyDB.queryPendingByUIDAndRobot(loginUID, req.RobotUID)
	if err != nil {
		bf.Error("查询申请记录失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	if existingApply != nil {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherApplyExists, nil, nil)
		return
	}

	// 提取 space_id：body > query > header
	applySpaceID := req.SpaceID
	if applySpaceID == "" {
		applySpaceID = c.Query("space_id")
	}
	if applySpaceID == "" {
		applySpaceID = c.GetHeader("X-Space-ID")
	}

	// YUJ-231 / GH#1291：纵深防御——robot 申请 claim 的 space_id 来自 client
	// 可控输入。无校验则攻击者可伪造任意 Space，让 bot notify payload 写进
	// 受害者非成员 Space 视图。与 P1-2 friend apply 同构，参考 YUJ-201 pattern。
	// 非成员降级为空串，notify 链（notifyOwnerNewApply / notifyApplicantResult）
	// 用规整后的 applySpaceID 写入 payload。
	if applySpaceID != "" {
		inSpace, membershipErr := spacepkg.CheckMembership(bf.ctx.DB(), applySpaceID, loginUID)
		if membershipErr != nil {
			bf.Error("robot 申请 space_id 成员校验失败",
				zap.String("uid", loginUID),
				zap.String("spaceId", applySpaceID),
				zap.Error(membershipErr))
			applySpaceID = ""
		} else if !inSpace {
			bf.Warn("robot apply: not a member of claimed space, dropping claim",
				zap.String("uid", loginUID),
				zap.String("spaceId", applySpaceID))
			applySpaceID = ""
		}
	}

	// 创建申请记录
	apply := &robotApplyModel{
		UID:      loginUID,
		RobotUID: req.RobotUID,
		OwnerUID: robot.CreatorUID,
		Remark:   req.Remark,
		Status:   ApplyStatusPending,
		SpaceID:  applySpaceID,
	}
	err = applyDB.insert(apply)
	if err != nil {
		bf.Error("创建申请记录失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}
	bf.notifyOwnerNewApply(loginUID, req.RobotUID, robot.CreatorUID, req.Remark, applySpaceID)

	c.Response(map[string]interface{}{
		"status":  "pending",
		"message": bf.localizedMessage(c, MsgApplySubmitted),
	})
}

// robotApplySure Owner通过申请
func (bf *BotFather) robotApplySure(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	if loginUID == "" {
		httperr.ResponseErrorL(c, errcode.ErrSharedAuthRequired, nil, nil)
		return
	}

	var req RobotApplySureReq
	if err := c.BindJSON(&req); err != nil {
		bf.Error("数据格式有误", zap.Error(err))
		respondBotfatherRequestInvalid(c, "")
		return
	}

	if req.ApplyID <= 0 {
		respondBotfatherRequestInvalid(c, "apply_id")
		return
	}

	applyDB := newRobotApplyDB(bf.ctx)
	apply, err := applyDB.queryByID(req.ApplyID)
	if err != nil {
		bf.Error("查询申请记录失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	if apply == nil {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherApplyNotFound, nil, nil)
		return
	}

	// 验证Owner身份
	if apply.OwnerUID != loginUID {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherNotOwner, nil, nil)
		return
	}

	// 检查状态
	if apply.Status != ApplyStatusPending {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherApplyProcessed, nil, nil)
		return
	}

	// 更新状态
	err = applyDB.updateStatus(req.ApplyID, ApplyStatusApproved)
	if err != nil {
		bf.Error("更新申请状态失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}

	// 建立好友关系
	err = bf.createFriendRelation(apply.UID, apply.RobotUID)
	if err != nil {
		bf.Error("创建好友关系失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}

	// 通知申请人：优先从 DB 读取申请时的 Space ID
	sureSpaceID := apply.SpaceID
	if sureSpaceID == "" {
		sureSpaceID = space.GetCommonSpaceID(bf.ctx, apply.UID, apply.RobotUID)
	}
	bf.notifyApplicantResult(apply.UID, apply.RobotUID, true, sureSpaceID)

	c.ResponseOK()
}

// robotApplyRefuse Owner拒绝申请
func (bf *BotFather) robotApplyRefuse(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	if loginUID == "" {
		httperr.ResponseErrorL(c, errcode.ErrSharedAuthRequired, nil, nil)
		return
	}

	applyIDStr := c.Param("apply_id")
	applyID, err := strconv.ParseInt(applyIDStr, 10, 64)
	if err != nil || applyID <= 0 {
		respondBotfatherRequestInvalid(c, "apply_id")
		return
	}

	applyDB := newRobotApplyDB(bf.ctx)
	apply, err := applyDB.queryByID(applyID)
	if err != nil {
		bf.Error("查询申请记录失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	if apply == nil {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherApplyNotFound, nil, nil)
		return
	}

	// 验证Owner身份
	if apply.OwnerUID != loginUID {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherNotOwner, nil, nil)
		return
	}

	// 检查状态
	if apply.Status != ApplyStatusPending {
		httperr.ResponseErrorL(c, errcode.ErrBotfatherApplyProcessed, nil, nil)
		return
	}

	// 更新状态
	err = applyDB.updateStatus(applyID, ApplyStatusRejected)
	if err != nil {
		bf.Error("更新申请状态失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}

	// 通知申请人：优先从 DB 读取申请时的 Space ID
	refuseSpaceID := apply.SpaceID
	if refuseSpaceID == "" {
		refuseSpaceID = space.GetCommonSpaceID(bf.ctx, apply.UID, apply.RobotUID)
	}
	bf.notifyApplicantResult(apply.UID, apply.RobotUID, false, refuseSpaceID)

	c.ResponseOK()
}

// robotApplies Owner查看待审批列表
func (bf *BotFather) robotApplies(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	if loginUID == "" {
		httperr.ResponseErrorL(c, errcode.ErrSharedAuthRequired, nil, nil)
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

	applyDB := newRobotApplyDB(bf.ctx)
	offset := (page - 1) * pageSize

	list, err := applyDB.queryPendingByOwner(loginUID, pageSize, offset)
	if err != nil {
		bf.Error("查询申请列表失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}

	count, err := applyDB.queryPendingCountByOwner(loginUID)
	if err != nil {
		bf.Error("查询申请数量失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}

	// 转换为响应格式
	respList := make([]*RobotApplyResp, 0, len(list))
	for _, apply := range list {
		// 获取申请人信息
		applicantName := apply.UID
		applicant, _ := bf.userService.GetUser(apply.UID)
		if applicant != nil {
			applicantName = applicant.Name
		}

		// 获取机器人信息
		robotName := apply.RobotUID
		robot, _ := bf.db.queryRobotByRobotID(apply.RobotUID)
		if robot != nil {
			robotName = robot.Username
		}

		respList = append(respList, &RobotApplyResp{
			ID:            apply.Id,
			UID:           apply.UID,
			RobotUID:      apply.RobotUID,
			RobotName:     robotName,
			ApplicantName: applicantName,
			OwnerUID:      apply.OwnerUID,
			Remark:        apply.Remark,
			Status:        apply.Status,
			CreatedAt:     apply.CreatedAt.String(),
		})
	}

	c.Response(&RobotApplyListResp{
		List:  respList,
		Count: count,
	})
}

// createFriendRelation 建立双向好友关系
func (bf *BotFather) createFriendRelation(userUID, robotUID string) error {
	// 用户 -> 机器人
	err := bf.userService.AddFriend(userUID, &user.FriendReq{
		UID:   userUID,
		ToUID: robotUID,
	})
	if err != nil {
		return err
	}

	// 机器人 -> 用户
	err = bf.userService.AddFriend(robotUID, &user.FriendReq{
		UID:   robotUID,
		ToUID: userUID,
	})
	if err != nil {
		return err
	}

	// 添加IM白名单（双向）— 同时添加裸 UID 和 Space 格式
	userChannelID := userUID
	robotChannelID := robotUID
	spaceID := space.GetCommonSpaceID(bf.ctx, userUID, robotUID)
	if spaceID != "" {
		userChannelID = fmt.Sprintf("s%s_%s", spaceID, userUID)
		robotChannelID = fmt.Sprintf("s%s_%s", spaceID, robotUID)
	}
	_ = bf.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   userChannelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{robotUID},
	})
	_ = bf.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   robotChannelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{userUID},
	})

	// 发送好友添加成功通知
	bfCmdParam := map[string]interface{}{
		"to_uid":   userUID,
		"from_uid": robotUID,
	}
	if spaceID != "" {
		bfCmdParam["space_id"] = spaceID
	}
	_ = bf.ctx.SendCMD(config.MsgCMDReq{
		CMD:         common.CMDFriendAccept,
		Subscribers: []string{userUID, robotUID},
		Param:       bfCmdParam,
	})

	// 发送欢迎消息：运营配置（Friend.AddedTipsText）优先，未配置时按收件人语言本地化
	content := bf.ctx.GetConfig().Friend.AddedTipsText
	if content == "" {
		lang := recipientLanguage(bf.cmdHandler.langSvc, userUID)
		if rendered, err := botMessages.Render(MsgFriendAddedTip, lang, nil); err != nil {
			bf.Error("渲染好友提示失败", zap.String("lang", lang), zap.Error(err))
		} else {
			content = rendered
		}
	}
	// Skip the tip when content is empty — a render failure (already logged
	// above) must not send a blank Tip. The friend relation / whitelist / CMD
	// are already done, so this only drops the cosmetic greeting.
	if content != "" {
		bfTipPayload := map[string]interface{}{
			"content": content,
			"type":    common.Tip,
		}
		// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM via NewPersonalMsgSendReq builder.
		_ = bf.ctx.SendMessage(config.NewPersonalMsgSendReq(
			userUID,
			robotUID,
			bfTipPayload,
			spaceID,
			config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
		))
	}

	return nil
}

// localizedMessage renders an outbound BotFather message for an HTTP
// success-response body. Unlike IM sends (which resolve language per recipient
// uid), a success body must follow the *request's* negotiated language exactly
// as the error envelope does: pkg/i18n's ErrorRenderer resolves it via
// LanguageOrDefault(c.Request.Context(), DefaultLanguage), honoring
// ?lang= / cookie / Accept-Language / X-Octo-Lang / user preference. Using the
// identical path keeps the success message and any error response on the same
// endpoint in one language, instead of diverging to a per-uid preference.
func (bf *BotFather) localizedMessage(c *wkhttp.Context, key string) string {
	lang := octoi18n.LanguageOrDefault(c.Request.Context(), octoi18n.DefaultLanguage)
	s, err := botMessages.Render(key, lang, nil)
	if err != nil {
		bf.Error("渲染响应消息失败", zap.String("key", key), zap.String("lang", lang), zap.Error(err))
		return ""
	}
	return s
}

// notifyOwnerNewApply 通知Owner有新的申请
func (bf *BotFather) notifyOwnerNewApply(applicantUID, robotUID, ownerUID, remark string, spaceID string) {
	applicantName := applicantUID
	applicant, _ := bf.userService.GetUser(applicantUID)
	if applicant != nil {
		applicantName = applicant.Name
	}

	lang := recipientLanguage(bf.cmdHandler.langSvc, ownerUID)
	content, err := botMessages.Render(MsgNotifyOwnerNewApply, lang, map[string]any{
		"ApplicantName": applicantName,
		"ApplicantUID":  applicantUID,
		"RobotUID":      robotUID,
		"Remark":        remark, // template renders the localized "Note:" line when non-empty
	})
	if err != nil {
		bf.Error("渲染申请通知失败", zap.String("lang", lang), zap.Error(err))
		return
	}

	notifyPayload := map[string]interface{}{
		"content": content,
		"type":    common.Text,
	}
	// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM via NewPersonalMsgSendReq builder.
	_ = bf.ctx.SendMessage(config.NewPersonalMsgSendReq(
		ownerUID,
		BotFatherUID,
		notifyPayload,
		spaceID,
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
	))
}

// notifyApplicantResult 通知申请人审批结果
func (bf *BotFather) notifyApplicantResult(applicantUID, robotUID string, approved bool, spaceID string) {
	key := MsgNotifyApplicantApproved
	if !approved {
		key = MsgNotifyApplicantRejected
	}
	lang := recipientLanguage(bf.cmdHandler.langSvc, applicantUID)
	content, err := botMessages.Render(key, lang, map[string]any{"RobotUID": robotUID})
	if err != nil {
		bf.Error("渲染申请结果通知失败", zap.String("lang", lang), zap.Error(err))
		return
	}

	resultPayload := map[string]interface{}{
		"content": content,
		"type":    common.Text,
	}
	// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM via NewPersonalMsgSendReq builder.
	_ = bf.ctx.SendMessage(config.NewPersonalMsgSendReq(
		applicantUID,
		BotFatherUID,
		resultPayload,
		spaceID,
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
	))
}

// setupApplyRoutes 注册apply相关路由（需要用户认证）
func (bf *BotFather) setupApplyRoutes(r *wkhttp.WKHttp) {
	applyAPI := r.Group("/v1/robot", bf.ctx.AuthMiddleware(r))
	{
		applyAPI.POST("/apply", bf.robotApply)
		applyAPI.POST("/apply/sure", bf.robotApplySure)
		applyAPI.PUT("/apply/refuse/:apply_id", bf.robotApplyRefuse)
		applyAPI.GET("/applies", bf.robotApplies)
	}
}
