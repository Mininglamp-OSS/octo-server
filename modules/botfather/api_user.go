package botfather

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// botBoundAtFormat 与 octo-lib db.Time 的序列化格式一致，保证 bound_at 在列表/
// 占用响应里与其它时间字段同构。
const botBoundAtFormat = "2006-01-02 15:04:05"

// maxAgentRefLen 限制 agent_ref 长度，与 robot.bound_agent_ref 列宽（128）一致。
const maxAgentRefLen = 128

// bindMaxAttempts 限定 bind 在「CAS 与复查之间被并发 unbind 释放」竞态下的重试次数，
// 防止极端 bind/unbind 抖动时无限循环。正常路径一次成功。
const bindMaxAttempts = 3

// ========== User API Key 认证中间件 ==========

func (bf *BotFather) authUserAPIKey() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token := extractBotToken(c) // reuse Bearer extraction
		if token == "" || !strings.HasPrefix(token, UserAPIKeyPrefix) {
			respondBotfatherAuthFailed(c)
			return
		}

		keyModel, err := bf.apiKeyService.AuthByKey(token)
		if err != nil {
			bf.Error("查询User API Key失败", zap.Error(err))
			respondBotfatherAuthFailed(c)
			return
		}
		if keyModel == nil {
			// key 不存在或非 active（AuthByKey 已按 status=1 过滤）→ 统一 401。
			respondBotfatherAuthFailed(c)
			return
		}
		if keyModel.ClientID != clientIDBotFather {
			enabled, err := bf.db.isIntegrationClientEnabled(keyModel.ClientID)
			if err != nil {
				bf.Error("查询 integration client 状态失败", zap.String("client_id", keyModel.ClientID), zap.Error(err))
				httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
				c.Abort()
				return
			}
			if !enabled {
				httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
				c.Abort()
				return
			}
		}

		c.Set("uid", keyModel.UID)
		c.Set("api_key_uid", keyModel.UID)
		c.Set("api_key_space_id", keyModel.SpaceID)
		c.Set("api_key_id", keyModel.ID)
		c.Set("api_key_client_id", keyModel.ClientID)
		c.Next()
	}
}

func getAPIKeyUID(c *wkhttp.Context) string {
	v, _ := c.Get("api_key_uid")
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func getAPIKeySpaceID(c *wkhttp.Context) string {
	v, _ := c.Get("api_key_space_id")
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// isBotInSpace checks whether a bot belongs to the given (active) Space.
//
// Delegates to bf.db.isBotInSpace, which additionally joins space.status=1 so a
// bot whose space_member row is still active but whose Space is disabled does
// NOT pass. Kept as a thin wrapper so the existing handler call sites
// (update/delete/getToken/bind/unbind) need no churn.
func (bf *BotFather) isBotInSpace(botID, spaceID string) (bool, error) {
	return bf.db.isBotInSpace(botID, spaceID)
}

// setupUserAPIRoutes 注册 User API Key 认证的路由
func (bf *BotFather) setupUserAPIRoutes(r *wkhttp.WKHttp) {
	userAPI := r.Group("/v1/user", bf.authUserAPIKey(), appwkhttp.SharedUIDRateLimiter(r, bf.ctx))
	{
		userAPI.POST("/bots", bf.createUserBot)
		userAPI.GET("/bots", bf.listUserBots)
		userAPI.PUT("/bots/:bot_id", bf.updateUserBot)
		userAPI.DELETE("/bots/:bot_id", bf.deleteUserBot)
		userAPI.GET("/bots/:bot_id/token", bf.getUserBotToken)
		userAPI.POST("/bots/:bot_id/bind", bf.bindUserBot)
		userAPI.DELETE("/bots/:bot_id/bind", bf.unbindUserBot)
	}
}

// ========== User Bot CRUD APIs ==========

// createUserBot POST /v1/user/bots
func (bf *BotFather) createUserBot(c *wkhttp.Context) {
	uid := getAPIKeyUID(c)
	var req CreateBotReq
	if err := c.BindJSON(&req); err != nil {
		respondBotfatherRequestInvalid(c, "")
		return
	}

	// Validate name
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 64 {
		respondBotfatherRequestInvalid(c, "name")
		return
	}

	// Generate bot token
	botToken, err := bf.cmdHandler.generateUniqueBotToken()
	if err != nil {
		bf.Error("生成Bot Token失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}
	description := ""
	if req.Description != nil {
		description = *req.Description
	}

	// 向后兼容：如果客户端传了 username，走原始逻辑（normalize + 校验 + 查重 + 用作 robot_id）；
	// username 为空时走自动生成（新行为）。
	var robotID string
	var createErr error
	reqUsername := strings.TrimSpace(strings.ToLower(req.Username))
	if reqUsername != "" {
		reqUsername = strings.TrimSuffix(reqUsername, BotUsernameSuffix)
		if len(reqUsername) == 0 || len(reqUsername) > 20 {
			respondBotfatherRequestInvalid(c, "username")
			return
		}
		for _, r := range reqUsername {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
				respondBotfatherRequestInvalid(c, "username")
				return
			}
		}
		// 禁止 app_ 前缀以避免与 App Bot UID 命名空间冲突
		if strings.HasPrefix(reqUsername, "app_") {
			respondBotfatherRequestInvalid(c, "username")
			return
		}
		reqUsername = reqUsername + BotUsernameSuffix

		// 唯一性预检
		exists, _ := bf.db.existRobotByUsername(reqUsername)
		if exists {
			respondBotfatherUsernameTaken(c, reqUsername)
			return
		}
		u, _ := bf.userService.GetUserWithUsername(reqUsername)
		if u != nil {
			respondBotfatherUsernameTaken(c, reqUsername)
			return
		}

		createErr = bf.cmdHandler.tryCreateBotCore(uid, name, reqUsername, botToken, reqUsername)
		robotID = reqUsername
	} else {
		robotID, createErr = bf.cmdHandler.createBotCoreWithRetry(uid, name, botToken)
	}
	if createErr != nil {
		bf.Error("创建Bot失败", zap.Error(createErr))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}
	username := robotID

	// description 写入（tryCreateBotCore 不处理 description）
	if description != "" {
		if descErr := bf.db.updateRobotDescription(robotID, description); descErr != nil {
			bf.Warn("写入description失败", zap.Error(descErr), zap.String("robotID", robotID))
		}
	}

	// Resolve Space ID: API Key binding takes authority; fall back to request
	spaceID := getAPIKeySpaceID(c)
	if spaceID == "" {
		spaceID = req.SpaceID
	}

	// Add bot to Space (best-effort, non-critical)
	// Verify caller belongs to the Space before adding bot (prevent cross-Space injection)
	if spaceID != "" {
		var memberCount int
		_, countErr := bf.db.session.SelectBySql(
			"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
			spaceID, uid,
		).Load(&memberCount)
		if countErr != nil {
			bf.Error("校验Space归属失败", zap.String("spaceID", spaceID), zap.Error(countErr))
		} else if memberCount > 0 {
			_, spErr := bf.db.session.InsertBySql(
				"INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
				spaceID, robotID,
			).Exec()
			if spErr != nil {
				bf.Error("Bot加入Space失败", zap.String("spaceID", spaceID), zap.Error(spErr))
			}
		} else {
			bf.Warn("用户不属于指定Space，跳过", zap.String("uid", uid), zap.String("spaceID", spaceID))
		}
	}

	// Add friend relationships (non-critical — partial state is acceptable if these fail)
	bf.userService.AddFriend(uid, &user.FriendReq{UID: uid, ToUID: robotID})
	bf.userService.AddFriend(robotID, &user.FriendReq{UID: robotID, ToUID: uid})
	// Fix friend version so WuKongIM SDK incremental sync picks up the relationship
	bf.cmdHandler.fixFriendVersion(uid, robotID)
	bf.cmdHandler.fixFriendVersion(robotID, uid)

	// Add IM whitelist (both directions) — with Space prefix if applicable
	userChannelID := uid
	robotChannelID := robotID
	spaceID = space.GetCommonSpaceID(bf.ctx, uid, robotID)
	if spaceID != "" {
		userChannelID = fmt.Sprintf("s%s_%s", spaceID, uid)
		robotChannelID = fmt.Sprintf("s%s_%s", spaceID, robotID)
	}
	_ = bf.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   userChannelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{robotID},
	})
	_ = bf.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   robotChannelID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{uid},
	})

	// Send friend accept notification so client updates conversation list
	_ = bf.ctx.SendCMD(config.MsgCMDReq{
		CMD:         common.CMDFriendAccept,
		Subscribers: []string{uid, robotID},
		Param: map[string]interface{}{
			"to_uid":   uid,
			"from_uid": robotID,
		},
	})

	c.Response(&CreateBotResp{
		RobotID:     robotID,
		Username:    username,
		Name:        name,
		Description: description,
		BotToken:    botToken,
	})
}

// listUserBots GET /v1/user/bots
func (bf *BotFather) listUserBots(c *wkhttp.Context) {
	uid := getAPIKeyUID(c)
	spaceID := getAPIKeySpaceID(c)

	var bots []*robotModel
	var err error
	if spaceID != "" {
		bots, err = bf.db.queryRobotsByCreatorUIDAndSpaceID(uid, spaceID)
	} else {
		bots, err = bf.db.queryRobotsByCreatorUID(uid)
	}
	if err != nil {
		bf.Error("查询Bot列表失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}

	usernames := make([]string, 0, len(bots))
	seenUsername := make(map[string]struct{}, len(bots))
	for _, bot := range bots {
		if bot.Username == "" {
			continue
		}
		if _, ok := seenUsername[bot.Username]; ok {
			continue
		}
		seenUsername[bot.Username] = struct{}{}
		usernames = append(usernames, bot.Username)
	}
	nameByUsername, err := bf.db.queryUserNamesByUsernames(usernames)
	if err != nil {
		bf.Warn("批量查询Bot用户名失败，使用username兜底", zap.Error(err))
		nameByUsername = map[string]string{}
	}

	list := make([]*UserBotResp, 0, len(bots))
	for _, bot := range bots {
		if bot.Status != 1 {
			continue
		}
		name := bot.Username
		if displayName := nameByUsername[bot.Username]; displayName != "" {
			name = displayName
		}
		var boundAt *string
		if bot.BoundAt.Valid {
			s := bot.BoundAt.Time.Format(botBoundAtFormat)
			boundAt = &s
		}
		list = append(list, &UserBotResp{
			RobotID:       bot.RobotID,
			Username:      bot.Username,
			Name:          name,
			Description:   bot.Description,
			BoundAgentRef: bot.BoundAgentRef,
			BoundAt:       boundAt,
			AgentPlatform: bot.AgentPlatform,
			AgentVersion:  bot.AgentVersion,
			PluginVersion: bot.PluginVersion,
		})
	}
	c.Response(list)
}

// updateUserBot PUT /v1/user/bots/:bot_id
func (bf *BotFather) updateUserBot(c *wkhttp.Context) {
	uid := getAPIKeyUID(c)
	spaceID := getAPIKeySpaceID(c)
	botID := c.Param("bot_id")

	bot, err := bf.db.queryRobotByRobotIDAndCreator(botID, uid)
	if err != nil {
		bf.Error("查询Bot失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	if bot == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherBotNotFound, nil, nil)
		c.Abort()
		return
	}

	// Space isolation: if the API Key is bound to a Space, verify the bot belongs to it
	if spaceID != "" {
		inSpace, sErr := bf.isBotInSpace(botID, spaceID)
		if sErr != nil {
			bf.Error("校验Bot Space归属失败", zap.Error(sErr))
			httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
			return
		}
		if !inSpace {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherBotNotInSpace, nil, nil)
			c.Abort()
			return
		}
	}

	var req UpdateBotReq
	if err := c.BindJSON(&req); err != nil {
		respondBotfatherRequestInvalid(c, "")
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 64 {
			respondBotfatherRequestInvalid(c, "name")
			return
		}
		err = bf.userService.UpdateUser(user.UserUpdateReq{
			UID:  botID,
			Name: &name,
		})
		if err != nil {
			bf.Error("更新Bot名称失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
			return
		}
	}

	if req.Description != nil {
		desc := strings.TrimSpace(*req.Description)
		if len(desc) > 500 {
			respondBotfatherRequestInvalid(c, "description")
			return
		}
		err = bf.db.updateRobotDescription(botID, desc)
		if err != nil {
			bf.Error("更新Bot描述失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
			return
		}
	}

	c.ResponseOK()
}

// deleteUserBot DELETE /v1/user/bots/:bot_id
func (bf *BotFather) deleteUserBot(c *wkhttp.Context) {
	uid := getAPIKeyUID(c)
	spaceID := getAPIKeySpaceID(c)
	botID := c.Param("bot_id")

	bot, err := bf.db.queryRobotByRobotIDAndCreator(botID, uid)
	if err != nil {
		bf.Error("查询Bot失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	if bot == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherBotNotFound, nil, nil)
		c.Abort()
		return
	}

	// Space isolation: if the API Key is bound to a Space, verify the bot belongs to it
	if spaceID != "" {
		inSpace, sErr := bf.isBotInSpace(botID, spaceID)
		if sErr != nil {
			bf.Error("校验Bot Space归属失败", zap.Error(sErr))
			httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
			return
		}
		if !inSpace {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherBotNotInSpace, nil, nil)
			c.Abort()
			return
		}
	}

	// Clean up IM connection: invalidate token to kick existing WS sessions
	newIMToken := util.GenerUUID()
	_, imErr := bf.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         botID,
		Token:       newIMToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if imErr != nil {
		bf.Error("撤销IM Token失败", zap.Error(imErr))
	}
	bf.db.updateRobotIMTokenCache(botID, "")

	// Clear heartbeat and event queue
	bf.ctx.GetRedisConn().Del(fmt.Sprintf("%s%s", heartbeatKeyPrefix, botID))
	bf.ctx.GetRedisConn().Del(fmt.Sprintf("robotEvent:%s", botID))

	// Remove friend records (both directions) with version for client sync
	friendVersion, verErr := bf.ctx.GenSeq(common.FriendSeqKey)
	if verErr != nil {
		bf.Error("GenSeq failed for friend", zap.Error(verErr))
	} else {
		_, fErr := bf.ctx.DB().Update("friend").
			Set("is_deleted", 1).
			Set("version", friendVersion).
			Where("(uid=? or to_uid=?) and is_deleted=0", botID, botID).
			Exec()
		if fErr != nil {
			bf.Error("删除Bot好友记录失败", zap.Error(fErr))
		}
	}

	// Remove from Spaces
	_, spErr := bf.ctx.DB().UpdateBySql(
		"UPDATE space_member SET status=0 WHERE uid=? AND status=1", botID,
	).Exec()
	if spErr != nil {
		bf.Error("移除Bot的Space成员记录失败", zap.Error(spErr))
	}

	// Soft-delete robot record
	err = bf.db.deleteRobot(botID)
	if err != nil {
		bf.Error("删除Bot失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherStoreFailed, nil, nil)
		return
	}

	// Release username and short_no so the identifier can be reused
	_, relErr := bf.ctx.DB().Update("user").
		Set("username", "").
		Set("short_no", "").
		Where("uid=?", botID).
		Exec()
	if relErr != nil {
		bf.Error("释放Bot用户名失败", zap.String("botID", botID), zap.Error(relErr))
	}

	c.ResponseOK()
}

// getUserBotToken GET /v1/user/bots/:bot_id/token
func (bf *BotFather) getUserBotToken(c *wkhttp.Context) {
	uid := getAPIKeyUID(c)
	spaceID := getAPIKeySpaceID(c)
	botID := c.Param("bot_id")

	bot, err := bf.db.queryRobotByRobotIDAndCreator(botID, uid)
	if err != nil {
		bf.Error("查询Bot失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
		return
	}
	if bot == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherBotNotFound, nil, nil)
		c.Abort()
		return
	}

	// Space isolation: if the API Key is bound to a Space, verify the bot belongs to it
	if spaceID != "" {
		inSpace, sErr := bf.isBotInSpace(botID, spaceID)
		if sErr != nil {
			bf.Error("校验Bot Space归属失败", zap.Error(sErr))
			httperr.ResponseErrorL(c, errcode.ErrBotfatherQueryFailed, nil, nil)
			return
		}
		if !inSpace {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotfatherBotNotInSpace, nil, nil)
			c.Abort()
			return
		}
	}

	c.Response(gin.H{
		"robot_id":  bot.RobotID,
		"bot_token": bot.BotToken,
	})
}

// ========== Bot 占用 / 绑定 ==========

// bindUserBot POST /v1/user/bots/:bot_id/bind
//
// 占用一个自有 Bot。creator 级权限 + 行级 CAS 互斥：多个 Agent 并发抢同一空闲
// Bot 时 Octo 侧只放行一个；重复传同一 agent_ref 幂等成功；已被他人占用返回
// 409 并在 error.details.occupied_by 透传当前占用方。
func (bf *BotFather) bindUserBot(c *wkhttp.Context) {
	uid := getAPIKeyUID(c)
	spaceID := getAPIKeySpaceID(c)
	botID := c.Param("bot_id")

	var req BindBotReq
	if err := c.BindJSON(&req); err != nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "agent_ref"})
		return
	}
	agentRef := strings.TrimSpace(req.AgentRef)
	if agentRef == "" || len(agentRef) > maxAgentRefLen {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "agent_ref"})
		return
	}

	// 1) 存在性 + creator 校验：他人创建的 Bot 不可见（按 PM creator 级边界）。
	bot, err := bf.db.queryRobotByRobotIDAndCreator(botID, uid)
	if err != nil {
		bf.Error("查询Bot失败", zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	if bot == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
		return
	}

	// 2) Space 隔离：key 绑定到 Space 时校验 Bot 属于该 Space。
	if spaceID != "" {
		inSpace, sErr := bf.isBotInSpace(botID, spaceID)
		if sErr != nil {
			bf.Error("校验Bot Space归属失败", zap.Error(sErr))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			return
		}
		if !inSpace {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
			return
		}
	}

	// 3) 行级 CAS 占用。带有界重试，原因见下方各分支注释。
	for attempt := 0; attempt < bindMaxAttempts; attempt++ {
		affected, err := bf.db.bindRobotCAS(botID, uid, agentRef)
		if err != nil {
			bf.Error("占用Bot失败", zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			return
		}

		// affected==1：CAS 命中，本次占用确定成功（写入即真相，无需复查——复查会被
		// 并发 unbind 污染成「已释放」的陈旧视图）。直接按 agentRef 回包；bound_at
		// best-effort 回读。
		if affected == 1 {
			var boundAt *string
			if cur, rErr := bf.db.queryRobotByRobotIDAndCreator(botID, uid); rErr == nil && cur != nil && cur.BoundAt.Valid {
				s := cur.BoundAt.Time.Format(botBoundAtFormat)
				boundAt = &s
			}
			c.Response(&BindBotResp{RobotID: botID, BoundAgentRef: agentRef, BoundAt: boundAt})
			return
		}

		// affected==0：不一定是冲突——MySQL 按「实际改动行数」计，幂等 re-bind（占用方
		// 未变、同秒 NOW() 也不变）同样返回 0。复查 bound_agent_ref 区分：
		current, err := bf.db.queryRobotByRobotIDAndCreator(botID, uid)
		if err != nil {
			bf.Error("回读占用状态失败", zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			return
		}
		switch {
		case current == nil:
			// 1)~3) 之间 Bot 被并发删除 → 404（而非 500）。
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
			return
		case current.BoundAgentRef == agentRef:
			// 已是自己持有（幂等成功）。
			var boundAt *string
			if current.BoundAt.Valid {
				s := current.BoundAt.Time.Format(botBoundAtFormat)
				boundAt = &s
			}
			c.Response(&BindBotResp{RobotID: botID, BoundAgentRef: current.BoundAgentRef, BoundAt: boundAt})
			return
		case current.BoundAgentRef == "":
			// CAS 时被他人持有（故 affected=0），复查时又被并发 unbind 释放成空闲。
			// 这是可恢复的竞态：重试 CAS 抢占，而非把空占用方塞进 409。
			continue
		default:
			// 被其他 agent_ref 占用。
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotOccupied, nil, i18n.Details{"occupied_by": current.BoundAgentRef})
			return
		}
	}

	// 重试用尽仍未抢到（极端高频 bind/unbind 抖动）：按冲突返回，让调用方退避重试。
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotOccupied, nil, nil)
}

// unbindUserBot DELETE /v1/user/bots/:bot_id/bind
//
// 释放占用。creator 级权限 + **agent_ref 归属校验**：只有当前占用方本人（或 Bot
// 已空闲）才能释放，否则 409。这与 bind 对称，共同保证「一个 Bot 同时只被一个
// Agent 占用」——否则同一用户的另一 Agent 能用共享 uk_ 把别人的占用清掉再自占。
// 对占用方幂等（已空闲再次调用仍成功）。
func (bf *BotFather) unbindUserBot(c *wkhttp.Context) {
	uid := getAPIKeyUID(c)
	spaceID := getAPIKeySpaceID(c)
	botID := c.Param("bot_id")

	var req BindBotReq
	if err := c.BindJSON(&req); err != nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "agent_ref"})
		return
	}
	agentRef := strings.TrimSpace(req.AgentRef)
	if agentRef == "" || len(agentRef) > maxAgentRefLen {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "agent_ref"})
		return
	}

	bot, err := bf.db.queryRobotByRobotIDAndCreator(botID, uid)
	if err != nil {
		bf.Error("查询Bot失败", zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	if bot == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
		return
	}

	if spaceID != "" {
		inSpace, sErr := bf.isBotInSpace(botID, spaceID)
		if sErr != nil {
			bf.Error("校验Bot Space归属失败", zap.Error(sErr))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			return
		}
		if !inSpace {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
			return
		}
	}

	affected, err := bf.db.unbindRobotCAS(botID, uid, agentRef)
	if err != nil {
		bf.Error("释放Bot占用失败", zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	// affected=0 不一定是冲突：Bot 已空闲（幂等）时 bound_agent_ref 未变也返回 0。
	// 复查 bound_agent_ref 区分「已空闲（幂等成功）」「被他人占用（409）」「被删（404）」。
	if affected == 0 {
		current, reErr := bf.db.queryRobotByRobotIDAndCreator(botID, uid)
		if reErr != nil {
			bf.Error("回读占用状态失败", zap.Error(reErr))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
			return
		}
		switch {
		case current == nil:
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
			return
		case current.BoundAgentRef != "" && current.BoundAgentRef != agentRef:
			// 被其他 Agent 占用：不允许越权释放。
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotOccupied, nil, i18n.Details{"occupied_by": current.BoundAgentRef})
			return
		}
		// 否则 Bot 已空闲 → 幂等成功，落到下方统一响应。
	}

	// bound_at 显式置 null：释放后无占用时间，与 docs §6 的响应形状及 bind 路径
	// 的 bound_at 字段对齐，避免严格按文档编码的客户端读到 undefined。
	c.Response(gin.H{
		"robot_id":        botID,
		"bound_agent_ref": "",
		"bound_at":        nil,
	})
}
