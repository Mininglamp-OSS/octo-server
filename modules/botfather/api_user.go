package botfather

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// ========== User API Key 认证中间件 ==========

func (bf *BotFather) authUserAPIKey() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token := extractBotToken(c) // reuse Bearer extraction
		if token == "" || !strings.HasPrefix(token, UserAPIKeyPrefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "缺少Authorization头或API Key无效"})
			return
		}

		keyModel, err := bf.db.queryUserAPIKeyByKey(token)
		if err != nil {
			bf.Error("查询User API Key失败", zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "认证失败"})
			return
		}
		if keyModel == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "无效的API Key"})
			return
		}

		c.Set("api_key_uid", keyModel.UID)
		c.Set("api_key_space_id", keyModel.SpaceID)
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

// isBotInSpace checks whether a bot belongs to the given Space.
func (bf *BotFather) isBotInSpace(botID, spaceID string) (bool, error) {
	var count int
	_, err := bf.db.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		spaceID, botID,
	).Load(&count)
	return count > 0, err
}

// setupUserAPIRoutes 注册 User API Key 认证的路由
func (bf *BotFather) setupUserAPIRoutes(r *wkhttp.WKHttp) {
	userAPI := r.Group("/v1/user", bf.authUserAPIKey())
	{
		userAPI.POST("/bots", bf.createUserBot)
		userAPI.GET("/bots", bf.listUserBots)
		userAPI.PUT("/bots/:bot_id", bf.updateUserBot)
		userAPI.DELETE("/bots/:bot_id", bf.deleteUserBot)
		userAPI.GET("/bots/:bot_id/token", bf.getUserBotToken)
	}
}

// ========== User Bot CRUD APIs ==========

// createUserBot POST /v1/user/bots
func (bf *BotFather) createUserBot(c *wkhttp.Context) {
	uid := getAPIKeyUID(c)
	var req CreateBotReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}

	// Validate name
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 64 {
		c.ResponseError(errors.New("name 长度需要在 1-64 个字符之间"))
		return
	}

	// username 已废弃：接受但忽略，始终自动生成 Bot ID
	robotID := generateBotID()
	username := robotID

	// Generate bot token
	botToken, err := bf.cmdHandler.generateUniqueBotToken()
	if err != nil {
		bf.Error("生成Bot Token失败", zap.Error(err))
		c.ResponseError(errors.New("创建失败，请稍后重试"))
		return
	}
	description := ""
	if req.Description != nil {
		description = *req.Description
	}

	// Create App
	appResp, err := bf.appService.CreateApp(app.Req{AppID: robotID})
	if err != nil {
		bf.Error("创建App失败", zap.Error(err))
		c.ResponseError(errors.New("创建失败"))
		return
	}

	// Create robot record
	tx, err := bf.db.session.Begin()
	if err != nil {
		bf.Error("开启事务失败", zap.Error(err))
		c.ResponseError(errors.New("创建失败"))
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	version, err := bf.ctx.GenSeq(common.RobotSeqKey)
	if err != nil {
		tx.Rollback()
		c.ResponseError(errors.New("创建失败"))
		return
	}
	err = bf.db.insertRobotTx(&robotModel{
		AppID:       appResp.AppID,
		RobotID:     robotID,
		Username:    username,
		Token:       appResp.AppKey,
		Version:     version,
		Status:      1,
		CreatorUID:  uid,
		BotToken:    botToken,
		Description: description,
	}, tx)
	if err != nil {
		tx.Rollback()
		bf.Error("插入机器人记录失败", zap.Error(err))
		c.ResponseError(errors.New("创建失败"))
		return
	}
	if err = tx.Commit(); err != nil {
		bf.Error("提交事务失败", zap.Error(err))
		c.ResponseError(errors.New("创建失败"))
		return
	}

	// Create user
	err = bf.userService.AddUser(&user.AddUserReq{
		UID:      robotID,
		Username: username,
		Name:     name,
		ShortNo:  username,
		Robot:    1,
	})
	if err != nil {
		// Rollback robot record
		if delErr := bf.db.deleteRobot(robotID); delErr != nil {
			bf.Error("回滚robot记录失败", zap.Error(delErr))
		}
		bf.Error("创建用户失败", zap.Error(err))
		c.ResponseError(errors.New("创建失败"))
		return
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
		c.ResponseError(errors.New("查询失败"))
		return
	}

	list := make([]*UserBotResp, 0, len(bots))
	for _, bot := range bots {
		if bot.Status != 1 {
			continue
		}
		// Get display name from user table
		name := bot.Username
		userResp, _ := bf.userService.GetUserWithUsername(bot.Username)
		if userResp != nil {
			name = userResp.Name
		}
		list = append(list, &UserBotResp{
			RobotID:       bot.RobotID,
			Username:      bot.Username,
			Name:          name,
			Description:   bot.Description,
			BotToken:      bot.BotToken,
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
		c.ResponseError(errors.New("查询失败"))
		return
	}
	if bot == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"msg": "Bot不存在或无权限"})
		return
	}

	// Space isolation: if the API Key is bound to a Space, verify the bot belongs to it
	if spaceID != "" {
		inSpace, sErr := bf.isBotInSpace(botID, spaceID)
		if sErr != nil {
			bf.Error("校验Bot Space归属失败", zap.Error(sErr))
			c.ResponseError(errors.New("查询失败"))
			return
		}
		if !inSpace {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "该Bot不属于当前Space"})
			return
		}
	}

	var req UpdateBotReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 64 {
			c.ResponseError(errors.New("name 长度需要在 1-64 个字符之间"))
			return
		}
		err = bf.userService.UpdateUser(user.UserUpdateReq{
			UID:  botID,
			Name: &name,
		})
		if err != nil {
			bf.Error("更新Bot名称失败", zap.Error(err))
			c.ResponseError(errors.New("更新失败"))
			return
		}
	}

	if req.Description != nil {
		desc := strings.TrimSpace(*req.Description)
		if len(desc) > 500 {
			c.ResponseError(errors.New("description 不能超过 500 个字符"))
			return
		}
		err = bf.db.updateRobotDescription(botID, desc)
		if err != nil {
			bf.Error("更新Bot描述失败", zap.Error(err))
			c.ResponseError(errors.New("更新失败"))
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
		c.ResponseError(errors.New("查询失败"))
		return
	}
	if bot == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"msg": "Bot不存在或无权限"})
		return
	}

	// Space isolation: if the API Key is bound to a Space, verify the bot belongs to it
	if spaceID != "" {
		inSpace, sErr := bf.isBotInSpace(botID, spaceID)
		if sErr != nil {
			bf.Error("校验Bot Space归属失败", zap.Error(sErr))
			c.ResponseError(errors.New("查询失败"))
			return
		}
		if !inSpace {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "该Bot不属于当前Space"})
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
		c.ResponseError(errors.New("删除失败"))
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
		c.ResponseError(errors.New("查询失败"))
		return
	}
	if bot == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"msg": "Bot不存在或无权限"})
		return
	}

	// Space isolation: if the API Key is bound to a Space, verify the bot belongs to it
	if spaceID != "" {
		inSpace, sErr := bf.isBotInSpace(botID, spaceID)
		if sErr != nil {
			bf.Error("校验Bot Space归属失败", zap.Error(sErr))
			c.ResponseError(errors.New("查询失败"))
			return
		}
		if !inSpace {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "该Bot不属于当前Space"})
			return
		}
	}

	c.Response(gin.H{
		"robot_id":  bot.RobotID,
		"bot_token": bot.BotToken,
	})
}
