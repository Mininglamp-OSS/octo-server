package botfather

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
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
		c.ResponseError(errors.New("请先登录"))
		return
	}

	var req RobotApplyReq
	if err := c.BindJSON(&req); err != nil {
		bf.Error("数据格式有误", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误"))
		return
	}

	if req.RobotUID == "" {
		c.ResponseError(errors.New("robot_uid不能为空"))
		return
	}

	// 查询目标AI
	robot, err := bf.db.queryRobotByRobotID(req.RobotUID)
	if err != nil {
		bf.Error("查询机器人失败", zap.Error(err))
		c.ResponseError(errors.New("查询机器人失败"))
		return
	}
	if robot == nil || robot.Status != 1 {
		c.ResponseError(errors.New("机器人不存在或已被删除"))
		return
	}

	// 检查是否是自己的AI
	if robot.CreatorUID == loginUID {
		c.ResponseError(errors.New("无需申请使用自己的AI"))
		return
	}

	// 检查access_mode
	switch robot.AccessMode {
	case AccessModeForbidden:
		c.ResponseError(errors.New("该AI禁止申请"))
		return
	case AccessModeAutoApprove:
		// 自动通过：直接建立好友关系
		err = bf.createFriendRelation(loginUID, req.RobotUID)
		if err != nil {
			bf.Error("创建好友关系失败", zap.Error(err))
			c.ResponseError(errors.New("创建好友关系失败"))
			return
		}
		c.Response(map[string]interface{}{
			"status":  "approved",
			"message": "已自动通过，可以开始聊天",
		})
		return
	}

	// 需要审批：检查是否已经是好友
	isFriend, err := bf.userService.IsFriend(loginUID, req.RobotUID)
	if err != nil {
		bf.Error("检查好友关系失败", zap.Error(err))
		c.ResponseError(errors.New("检查好友关系失败"))
		return
	}
	if isFriend {
		c.ResponseError(errors.New("你们已经是好友了"))
		return
	}

	// 检查是否有待处理的申请
	applyDB := newRobotApplyDB(bf.ctx)
	existingApply, err := applyDB.queryPendingByUIDAndRobot(loginUID, req.RobotUID)
	if err != nil {
		bf.Error("查询申请记录失败", zap.Error(err))
		c.ResponseError(errors.New("查询申请记录失败"))
		return
	}
	if existingApply != nil {
		c.ResponseError(errors.New("你已提交过申请，请等待Owner审批"))
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
		c.ResponseError(errors.New("创建申请记录失败"))
		return
	}
	bf.notifyOwnerNewApply(loginUID, req.RobotUID, robot.CreatorUID, req.Remark, applySpaceID)

	c.Response(map[string]interface{}{
		"status":  "pending",
		"message": "申请已提交，等待Owner审批",
	})
}

// robotApplySure Owner通过申请
func (bf *BotFather) robotApplySure(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	if loginUID == "" {
		c.ResponseError(errors.New("请先登录"))
		return
	}

	var req RobotApplySureReq
	if err := c.BindJSON(&req); err != nil {
		bf.Error("数据格式有误", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误"))
		return
	}

	if req.ApplyID <= 0 {
		c.ResponseError(errors.New("apply_id无效"))
		return
	}

	applyDB := newRobotApplyDB(bf.ctx)
	apply, err := applyDB.queryByID(req.ApplyID)
	if err != nil {
		bf.Error("查询申请记录失败", zap.Error(err))
		c.ResponseError(errors.New("查询申请记录失败"))
		return
	}
	if apply == nil {
		c.ResponseError(errors.New("申请记录不存在"))
		return
	}

	// 验证Owner身份
	if apply.OwnerUID != loginUID {
		c.ResponseError(errors.New("你不是该AI的Owner"))
		return
	}

	// 检查状态
	if apply.Status != ApplyStatusPending {
		c.ResponseError(errors.New("该申请已被处理"))
		return
	}

	// 更新状态
	err = applyDB.updateStatus(req.ApplyID, ApplyStatusApproved)
	if err != nil {
		bf.Error("更新申请状态失败", zap.Error(err))
		c.ResponseError(errors.New("更新申请状态失败"))
		return
	}

	// 建立好友关系
	err = bf.createFriendRelation(apply.UID, apply.RobotUID)
	if err != nil {
		bf.Error("创建好友关系失败", zap.Error(err))
		c.ResponseError(errors.New("创建好友关系失败"))
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
		c.ResponseError(errors.New("请先登录"))
		return
	}

	applyIDStr := c.Param("apply_id")
	applyID, err := strconv.ParseInt(applyIDStr, 10, 64)
	if err != nil || applyID <= 0 {
		c.ResponseError(errors.New("apply_id无效"))
		return
	}

	applyDB := newRobotApplyDB(bf.ctx)
	apply, err := applyDB.queryByID(applyID)
	if err != nil {
		bf.Error("查询申请记录失败", zap.Error(err))
		c.ResponseError(errors.New("查询申请记录失败"))
		return
	}
	if apply == nil {
		c.ResponseError(errors.New("申请记录不存在"))
		return
	}

	// 验证Owner身份
	if apply.OwnerUID != loginUID {
		c.ResponseError(errors.New("你不是该AI的Owner"))
		return
	}

	// 检查状态
	if apply.Status != ApplyStatusPending {
		c.ResponseError(errors.New("该申请已被处理"))
		return
	}

	// 更新状态
	err = applyDB.updateStatus(applyID, ApplyStatusRejected)
	if err != nil {
		bf.Error("更新申请状态失败", zap.Error(err))
		c.ResponseError(errors.New("更新申请状态失败"))
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
		c.ResponseError(errors.New("请先登录"))
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
		c.ResponseError(errors.New("查询申请列表失败"))
		return
	}

	count, err := applyDB.queryPendingCountByOwner(loginUID)
	if err != nil {
		bf.Error("查询申请数量失败", zap.Error(err))
		c.ResponseError(errors.New("查询申请数量失败"))
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

	// 发送欢迎消息
	content := "我们已经是好友了，可以愉快的聊天了！"
	if bf.ctx.GetConfig().Friend.AddedTipsText != "" {
		content = bf.ctx.GetConfig().Friend.AddedTipsText
	}
	bfTipPayload := map[string]interface{}{
		"content": content,
		"type":    common.Tip,
	}
	if spaceID != "" {
		bfTipPayload["space_id"] = spaceID
	}
	payload := []byte(util.ToJson(bfTipPayload))
	_ = bf.ctx.SendMessage(&config.MsgSendReq{
		FromUID:     robotUID,
		ChannelID:   userUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     payload,
		Header: config.MsgHeader{
			RedDot: 1,
		},
	})

	return nil
}

// notifyOwnerNewApply 通知Owner有新的申请
func (bf *BotFather) notifyOwnerNewApply(applicantUID, robotUID, ownerUID, remark string, spaceID string) {
	applicantName := applicantUID
	applicant, _ := bf.userService.GetUser(applicantUID)
	if applicant != nil {
		applicantName = applicant.Name
	}

	remarkText := ""
	if remark != "" {
		remarkText = fmt.Sprintf("\n备注: %s", remark)
	}

	content := fmt.Sprintf("有人申请使用你的AI\n用户: %s (%s)\nAI: %s%s",
		applicantName, applicantUID, robotUID, remarkText)

	notifyPayload := map[string]interface{}{
		"content": content,
		"type":    common.Text,
	}
	if spaceID != "" {
		notifyPayload["space_id"] = spaceID
	}
	payload := []byte(util.ToJson(notifyPayload))
	_ = bf.ctx.SendMessage(&config.MsgSendReq{
		FromUID:     BotFatherUID,
		ChannelID:   ownerUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     payload,
		Header: config.MsgHeader{
			RedDot: 1,
		},
	})
}

// notifyApplicantResult 通知申请人审批结果
func (bf *BotFather) notifyApplicantResult(applicantUID, robotUID string, approved bool, spaceID string) {
	var content string
	if approved {
		content = fmt.Sprintf("你的AI使用申请已通过！\nAI: %s\n现在可以开始聊天了", robotUID)
	} else {
		content = fmt.Sprintf("你的AI使用申请被拒绝\nAI: %s", robotUID)
	}

	resultPayload := map[string]interface{}{
		"content": content,
		"type":    common.Text,
	}
	if spaceID != "" {
		resultPayload["space_id"] = spaceID
	}
	payload := []byte(util.ToJson(resultPayload))
	_ = bf.ctx.SendMessage(&config.MsgSendReq{
		FromUID:     BotFatherUID,
		ChannelID:   applicantUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     payload,
		Header: config.MsgHeader{
			RedDot: 1,
		},
	})
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
