package botfather

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// BotFather BotFather模块
type BotFather struct {
	ctx              *config.Context
	db               *botfatherDB
	cmdHandler       *commandHandler
	userService      user.IService
	appService       app.IService
	fileService      file.IService
	robotEventPrefix string
	initOnce         sync.Once
	msgSem           chan struct{} // 限制并发消息处理的信号量
	log.Log
}

// New 创建BotFather实例
func New(ctx *config.Context) *BotFather {
	bf := &BotFather{
		ctx:              ctx,
		db:               newBotfatherDB(ctx),
		cmdHandler:       newCommandHandler(ctx),
		userService:      user.NewService(ctx),
		appService:       app.NewService(ctx),
		fileService:      file.NewService(ctx),
		robotEventPrefix: "robotEvent:",
		msgSem:           make(chan struct{}, 100), // 限制最多100个并发消息处理
		Log:              log.NewTLog("BotFather"),
	}

	// 注册消息监听器
	ctx.AddMessagesListener(bf.messagesListen)

	// 注册好友申请通知回调
	RegisterFriendApplyHook(ctx)

	// 注册用户注册事件监听器，发送欢迎消息
	ctx.AddEventListener(event.EventUserRegister, bf.handleUserRegisterEvent)

	return bf
}

// Route 路由配置
func (bf *BotFather) Route(r *wkhttp.WKHttp) {
	// 启动时批量同步所有 bot 的 token 到 WuKongIM（防止 WuKongIM 重启后 token 丢失）
	go bf.syncAllBotTokens()

	// skill.md 端点（无需认证）
	r.GET("/v1/bot/skill.md", bf.skillMD)

	// register 端点（只需bot token，不走authBot中间件组）
	r.POST("/v1/bot/register", bf.register)

	// Bot API 端点（使用bot token认证）
	botAPI := r.Group("/v1/bot", bf.authBot())
	{
		botAPI.POST("/sendMessage", bf.sendMessage)
		botAPI.POST("/typing", bf.typing)
		botAPI.POST("/readReceipt", bf.readReceipt)
		botAPI.POST("/events", bf.getEvents)
		botAPI.POST("/events/:event_id/ack", bf.eventAck)
		botAPI.POST("/stream/start", bf.streamStart)
		botAPI.POST("/stream/end", bf.streamEnd)
		botAPI.POST("/heartbeat", bf.heartbeat)
		botAPI.POST("/messages/sync", bf.syncMessages)
		botAPI.GET("/groups", bf.getGroups)
		botAPI.GET("/groups/:group_no", bf.getGroupInfo)
		botAPI.GET("/groups/:group_no/members", bf.getGroupMembers)
		botAPI.POST("/setCommands", bf.setCommands)
		// Bot File API (#433)
		botAPI.POST("/file/upload", bf.botUploadFile)
		botAPI.GET("/file/download/*path", bf.botFileDownload)
	}

	// Bot File API（独立路由组，避免 GIN wildcard 冲突）
	botFileAPI := r.Group("/v1/botfile", bf.authBot())
	{
		botFileAPI.GET("/*path", bf.botProxyFile)
		botFileAPI.POST("/upload", bf.botUploadFile)
	}

	// Robot Apply API 端点（使用用户认证）
	bf.setupApplyRoutes(r)

	// 初始化BotFather系统用户（使用sync.Once确保只执行一次）
	bf.initOnce.Do(func() {
		bf.initBotFatherUser()
	})
}

// skillMD 返回skill.md文档
func (bf *BotFather) skillMD(c *wkhttp.Context) {
	cfg := bf.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}
	wsURL := deriveWSURL(cfg)
	content := generateSkillMD(apiURL, wsURL)
	c.Header("Content-Type", "text/markdown; charset=utf-8")
	c.String(http.StatusOK, content)
}

// ========== 消息监听 ==========

func (bf *BotFather) messagesListen(messages []*config.MessageResp) {
	for _, message := range messages {
		if message.ChannelType != common.ChannelTypePerson.Uint8() {
			continue
		}

		// 检查是否是发给BotFather的DM
		rawToUID := common.GetToChannelIDWithFakeChannelID(message.ChannelID, message.FromUID)
		// Space channel_id 格式: s{spaceId}_{botfather}
		// 用 HasSuffix 匹配 "_botfather"，避免 ParseChannelID 下划线歧义
		isBotFather := rawToUID == BotFatherUID || strings.HasSuffix(rawToUID, "_"+BotFatherUID)
		if !isBotFather {
			continue
		}

		// 提取 Space 前缀（用于后续 extractRealUID）
		channelID := message.ChannelID

		// 解析消息内容
		payloadValue := gjson.ParseBytes(message.Payload)
		if !payloadValue.Exists() {
			continue
		}
		contentType := payloadValue.Get("type").Int()
		if contentType != int64(common.Text) {
			continue
		}
		content := payloadValue.Get("content").String()
		if content == "" {
			continue
		}

		// 处理命令（使用信号量限制并发数）
		select {
		case bf.msgSem <- struct{}{}:
			go func(uid, msg, chID string) {
				defer func() { <-bf.msgSem }()
				cleanup := setSpacePrefixForUID(uid, chID)
				defer cleanup()
				bf.cmdHandler.HandleMessage(uid, msg)
			}(message.FromUID, content, channelID)
		default:
			bf.Warn("消息处理并发数已达上限，丢弃消息", zap.String("fromUID", message.FromUID))
		}
	}
}

// ========== BotFather用户初始化 ==========

func (bf *BotFather) initBotFatherUser() {
	// 检查BotFather用户是否存在
	userResp, err := bf.userService.GetUserWithUsername(BotFatherUID)
	if err != nil {
		bf.Error("查询BotFather用户失败", zap.Error(err))
	}
	if userResp == nil {
		// 创建BotFather用户
		err = bf.userService.AddUser(&user.AddUserReq{
			UID:      BotFatherUID,
			Username: BotFatherUID,
			Name:     BotFatherName,
		})
		if err != nil {
			bf.Error("创建BotFather用户失败", zap.Error(err))
			return
		}
		bf.Info("BotFather用户创建成功")
	}

	// 确保BotFather在robot表中有记录
	robot, err := bf.db.queryRobotByRobotID(BotFatherUID)
	if err != nil {
		bf.Error("查询BotFather机器人记录失败", zap.Error(err))
	}
	if robot == nil {
		// 创建App
		appResp, err := bf.appService.CreateApp(app.Req{AppID: BotFatherUID})
		if err != nil {
			bf.Error("创建BotFather App失败", zap.Error(err))
			return
		}

		tx, err := bf.db.session.Begin()
		if err != nil {
			bf.Error("开启事务失败", zap.Error(err))
			return
		}
		defer func() {
			if err := recover(); err != nil {
				tx.Rollback()
				fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
			}
		}()

		robotVersion, err := bf.ctx.GenSeq(common.RobotSeqKey)
		if err != nil {
			tx.Rollback()
			bf.Error("GenSeq failed", zap.Error(err))
			return
		}
		err = bf.db.insertRobotTx(&robotModel{
			AppID:    appResp.AppID,
			RobotID:  BotFatherUID,
			Username: BotFatherUID,
			Token:    appResp.AppKey,
			Version:  robotVersion,
			Status:   1,
		}, tx)
		if err != nil {
			tx.Rollback()
			bf.Error("插入BotFather机器人记录失败", zap.Error(err))
			return
		}
		err = tx.Commit()
		if err != nil {
			bf.Error("提交事务失败", zap.Error(err))
			return
		}
		bf.Info("BotFather机器人记录创建成功")
	}

	// 确保BotFather与所有用户建立好友关系
	bf.ensureBotFatherFriends()

	// 修复孤儿 Bot — user 表有 robot=1 但 robot 表无记录（#234 遗留数据）
	bf.repairOrphanBots()

	// 注册BotFather自身的命令列表
	bf.registerBotFatherCommands()
}

// registerBotFatherCommands 注册BotFather自身的命令列表
func (bf *BotFather) registerBotFatherCommands() {
	commands := []map[string]string{
		{"command": CmdNewBot, "description": "创建新机器人"},
		{"command": CmdMyBots, "description": "查看我的机器人"},
		{"command": CmdConnect, "description": "获取连接 prompt"},
		{"command": CmdDisconnect, "description": "断开 Agent 连接"},
		{"command": CmdSetName, "description": "修改机器人名称"},
		{"command": CmdSetDescription, "description": "修改机器人描述"},
		{"command": CmdDeleteBot, "description": "删除机器人"},
		{"command": CmdToken, "description": "查看 Token"},
		{"command": CmdRevoke, "description": "重置 Token"},
		{"command": CmdApprove, "description": "通过好友申请"},
		{"command": CmdReject, "description": "拒绝好友申请"},
		{"command": CmdPending, "description": "查看待审批好友申请"},
		{"command": CmdHelp, "description": "显示帮助"},
		{"command": CmdCancel, "description": "取消当前操作"},
	}
	commandsJSON, err := json.Marshal(commands)
	if err != nil {
		bf.Error("序列化BotFather命令列表失败", zap.Error(err))
		return
	}
	err = bf.db.updateBotCommands(BotFatherUID, string(commandsJSON))
	if err != nil {
		bf.Error("注册BotFather命令列表失败", zap.Error(err))
		return
	}
	bf.Info("BotFather命令列表注册成功")
}

// ensureBotFatherFriends 批量为缺少BotFather好友关系的用户添加
func (bf *BotFather) ensureBotFatherFriends() {
	_, err := bf.db.session.InsertBySql(`
		INSERT IGNORE INTO friend (uid, to_uid, version)
		SELECT u.uid, ?, 1 FROM user u
		WHERE u.uid NOT IN (?, ?, ?)
		AND u.status = 1
		AND NOT EXISTS (
			SELECT 1 FROM friend f WHERE f.uid = u.uid AND f.to_uid = ?
		)
	`, BotFatherUID, systemExcludedUIDs[0], systemExcludedUIDs[1], systemExcludedUIDs[2], BotFatherUID).Exec()
	if err != nil {
		bf.Warn("批量添加BotFather好友关系失败", zap.Error(err))
	}
}

// repairOrphanBots finds users with robot=1 that have no corresponding robot
// table record (caused by the pre-#289 non-atomic createBot flow) and creates
// the missing robot records so /mybots and the sidebar can find them.
func (bf *BotFather) repairOrphanBots() {
	type orphan struct {
		UID      string `db:"uid"`
		Username string `db:"username"`
	}
	var orphans []orphan
	_, err := bf.db.session.SelectBySql(`
		SELECT u.uid, u.username FROM user u
		WHERE u.robot = 1
		AND u.uid NOT IN (?, ?, ?)
		AND NOT EXISTS (
			SELECT 1 FROM robot r WHERE r.robot_id = u.uid
		)
	`, systemExcludedUIDs[0], systemExcludedUIDs[1], systemExcludedUIDs[2]).Load(&orphans)
	if err != nil {
		bf.Warn("查询孤儿Bot失败", zap.Error(err))
		return
	}
	if len(orphans) == 0 {
		return
	}

	bf.Info("发现孤儿Bot，开始修复", zap.Int("count", len(orphans)))
	for _, o := range orphans {
		// Try to find the creator from friend table (the non-bot user who friended this bot)
		var creatorUID string
		err := bf.db.session.SelectBySql(`
			SELECT f.uid FROM friend f
			INNER JOIN user u ON f.uid = u.uid AND u.robot = 0
			WHERE f.to_uid = ? AND f.is_deleted = 0
			ORDER BY f.id ASC
			LIMIT 1
		`, o.UID).LoadOne(&creatorUID)
		if err != nil || creatorUID == "" {
			bf.Warn("无法确定孤儿Bot的创建者，跳过", zap.String("bot_uid", o.UID), zap.Error(err))
			continue
		}
		bf.Info("孤儿Bot创建者推断自friend表", zap.String("bot_uid", o.UID), zap.String("inferred_creator", creatorUID))

		// Create app if missing. CreateApp is idempotent: if the app already
		// exists (which is expected for orphan bots — createBot calls CreateApp
		// before the failing robot insert), it returns the existing record.
		appResp, err := bf.appService.CreateApp(app.Req{AppID: o.UID})
		if err != nil {
			bf.Warn("修复孤儿Bot：创建App失败", zap.String("bot_uid", o.UID), zap.Error(err))
			continue
		}

		tx, err := bf.db.session.Begin()
		if err != nil {
			bf.Warn("修复孤儿Bot：开启事务失败", zap.String("bot_uid", o.UID), zap.Error(err))
			continue
		}

		version, err := bf.ctx.GenSeq(common.RobotSeqKey)
		if err != nil {
			tx.Rollback()
			bf.Warn("修复孤儿Bot：GenSeq失败", zap.String("bot_uid", o.UID), zap.Error(err))
			continue
		}

		err = bf.db.insertRobotTx(&robotModel{
			AppID:      appResp.AppID,
			RobotID:    o.UID,
			Username:   o.Username,
			Token:      appResp.AppKey,
			Version:    version,
			Status:     1,
			CreatorUID: creatorUID,
		}, tx)
		if err != nil {
			tx.Rollback()
			bf.Warn("修复孤儿Bot：插入robot记录失败", zap.String("bot_uid", o.UID), zap.Error(err))
			continue
		}

		if err = tx.Commit(); err != nil {
			bf.Warn("修复孤儿Bot：提交事务失败", zap.String("bot_uid", o.UID), zap.Error(err))
			continue
		}

		bf.Info("修复孤儿Bot成功", zap.String("bot_uid", o.UID), zap.String("creator", creatorUID))
	}
}

// ========== Bot Token 认证中间件 ==========

func (bf *BotFather) authBot() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token := extractBotToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "缺少Authorization头或token无效"})
			return
		}

		robot, err := bf.db.queryRobotByBotToken(token)
		if err != nil {
			bf.Error("查询机器人失败", zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "认证失败"})
			return
		}
		if robot == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "无效的bot token"})
			return
		}

		// 将robot信息存入上下文
		c.Set("robot_id", robot.RobotID)
		c.Set("robot", robot)
		c.Next()
	}
}

func extractBotToken(c *wkhttp.Context) string {
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func getRobotFromContext(c *wkhttp.Context) *robotModel {
	v, exists := c.Get("robot")
	if !exists {
		return nil
	}
	rm, ok := v.(*robotModel)
	if !ok {
		return nil
	}
	return rm
}

func getRobotIDFromContext(c *wkhttp.Context) string {
	v, _ := c.Get("robot_id")
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// resolveSpaceChannelID 处理 Bot API 的 channel_id
// DM(channel_type=1): WuKongIM 只认裸 uid，不加 Space 前缀
// Group: 原样返回
func (bf *BotFather) resolveSpaceChannelID(robotID, channelID string, channelType uint8) string {
	// DM 场景：始终使用裸 uid（WuKongIM DM 不认 Space 前缀）
	// 非 DM 场景：原样返回
	return channelID
}

// ========== Bot Register API ==========

func (bf *BotFather) register(c *wkhttp.Context) {
	token := extractBotToken(c)
	if token == "" {
		c.ResponseError(errors.New("缺少Authorization头"))
		return
	}

	robot, err := bf.db.queryRobotByBotToken(token)
	if err != nil {
		bf.Error("查询机器人失败", zap.Error(err))
		c.ResponseError(errors.New("认证失败"))
		return
	}
	if robot == nil {
		c.ResponseError(errors.New("无效的bot token"))
		return
	}

	// 直接用 bot_token 作为 im_token — 只有一个 token，永远不会不一致
	imToken := robot.BotToken
	// 无论是否有缓存，都向 WuKongIM 注册（保证 WuKongIM 内存中有该 token）
	resp, tokenErr := bf.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         robot.RobotID,
		Token:       imToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if tokenErr != nil || resp.Status != config.UpdateTokenStatusSuccess {
		bf.Error("获取IM Token失败", zap.Any("error", tokenErr), zap.String("robotID", robot.RobotID), zap.Any("status", resp))
		c.ResponseError(errors.New("获取IM Token失败"))
		return
	}
	// 更新缓存
	if robot.IMTokenCache != imToken {
		bf.db.updateRobotIMTokenCache(robot.RobotID, imToken)
	}

	cfg := bf.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}
	wsURL := deriveWSURL(cfg)

	c.Response(&BotRegisterResp{
		RobotID:        robot.RobotID,
		IMToken:        imToken,
		WSURL:          wsURL,
		APIURL:         apiURL,
		OwnerUID:       robot.CreatorUID,
		OwnerChannelID: robot.CreatorUID,
	})
}

func (bf *BotFather) getOrCreateIMToken(robotID string) (string, error) {
	token := util.GenerUUID()
	resp, err := bf.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         robotID,
		Token:       token,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if err != nil {
		return "", err
	}
	if resp.Status != config.UpdateTokenStatusSuccess {
		return "", fmt.Errorf("更新IM Token状态异常: %d", resp.Status)
	}
	return token, nil
}

// ========== Bot Send Message API ==========

func (bf *BotFather) sendMessage(c *wkhttp.Context) {
	var req BotSendMessageReq
	if err := c.BindJSON(&req); err != nil {
		bf.Error("数据格式有误", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		c.ResponseError(errors.New("channel_id不能为空"))
		return
	}
	if req.ChannelType == 0 {
		c.ResponseError(errors.New("channel_type不能为空"))
		return
	}
	if len(req.Payload) == 0 {
		c.ResponseError(errors.New("payload不能为空"))
		return
	}

	robotID := getRobotIDFromContext(c)
	channelID := bf.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)
	result, err := bf.ctx.SendMessageWithResult(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 1,
		},
		StreamNo:    req.StreamNo,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
		FromUID:     robotID,
		Payload:     []byte(util.ToJson(req.Payload)),
	})
	if err != nil {
		bf.Error("发送消息失败", zap.Error(err))
		c.ResponseError(errors.New("发送消息失败"))
		return
	}
	c.Response(result)
}

// ========== Bot Typing API ==========

func (bf *BotFather) typing(c *wkhttp.Context) {
	var req BotTypingReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		c.ResponseError(errors.New("channel_id不能为空"))
		return
	}
	if req.ChannelType == 0 {
		c.ResponseError(errors.New("channel_type不能为空"))
		return
	}

	robotID := getRobotIDFromContext(c)
	channelID := bf.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)
	// DM 场景：param.channel_id 必须是 bot 自身 UID（或 Space 前缀），
	// 因为客户端用 param.channel_id 匹配会话
	paramChannelID := channelID
	if req.ChannelType == uint8(common.ChannelTypePerson) {
		// DM 场景：paramChannelID 用裸 robotID（WuKongIM 不认 Space 前缀）
		paramChannelID = robotID
	}
	err := bf.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		CMD:         common.CMDTyping,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
		Param: map[string]interface{}{
			"from_uid":     robotID,
			"channel_id":   paramChannelID,
			"channel_type": req.ChannelType,
		},
	})
	if err != nil {
		bf.Error("发送typing失败", zap.Error(err))
		c.ResponseError(errors.New("发送typing失败"))
		return
	}
	c.ResponseOK()
}

// ========== Bot Read Receipt API ==========

func (bf *BotFather) readReceipt(c *wkhttp.Context) {
	var req BotReadReceiptReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		c.ResponseError(errors.New("channel_id不能为空"))
		return
	}

	robotID := getRobotIDFromContext(c)
	channelType := uint8(common.ChannelTypePerson)
	if req.ChannelType > 0 {
		channelType = req.ChannelType
	}
	channelID := bf.resolveSpaceChannelID(robotID, req.ChannelID, channelType)

	// 1. 清除会话未读角标
	err := bf.ctx.IMClearConversationUnread(config.ClearConversationUnreadReq{
		UID:         robotID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Unread:      0,
	})
	if err != nil {
		bf.Warn("清除未读计数失败", zap.Error(err))
	}

	// 2. 消息级已读回执：写入 member_readed + Redis 缓存，触发发送者看到"已读"
	if len(req.MessageIDs) > 0 {
		// 解析字符串消息 ID 为 int64（避免 JS 大数精度丢失）
		messageIDs := make([]int64, 0, len(req.MessageIDs))
		for _, idStr := range req.MessageIDs {
			mid, parseErr := strconv.ParseInt(idStr, 10, 64)
			if parseErr != nil {
				bf.Warn("解析消息ID失败", zap.String("id", idStr), zap.Error(parseErr))
				continue
			}
			messageIDs = append(messageIDs, mid)
		}
		if len(messageIDs) == 0 {
			c.ResponseOK()
			return
		}

		fakeChannelID := channelID
		if channelType == common.ChannelTypePerson.Uint8() {
			fakeChannelID = common.GetFakeChannelIDWith(channelID, robotID)
		}

		// 查询消息详情（需要 FromUID、MessageSeq）
		// DM 场景：用户发给 bot 的消息存储在 channel_id=robotID，
		// 但 adapter 传入的 channel_id 是用户 UID（回复目标），需要交换为 robotID 来搜索
		searchChannelID := channelID
		if channelType == common.ChannelTypePerson.Uint8() {
			searchChannelID = robotID
		}
		syncMsg, err := bf.ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   searchChannelID,
			ChannelType: channelType,
			MessageIds:  messageIDs,
			LoginUID:    robotID,
		})
		if err != nil {
			bf.Warn("查询消息失败", zap.Error(err))
		} else if syncMsg != nil && len(syncMsg.Messages) > 0 {
			// 批量插入 member_readed
			valueStrings := make([]string, 0, len(syncMsg.Messages))
			valueArgs := make([]interface{}, 0, len(syncMsg.Messages)*4)
			for _, msg := range syncMsg.Messages {
				valueStrings = append(valueStrings, "(?, ?, ?, ?)")
				valueArgs = append(valueArgs, msg.MessageID, fakeChannelID, channelType, robotID)
			}
			stmt := fmt.Sprintf(`INSERT INTO member_readed (message_id, channel_id, channel_type, uid) VALUES %s ON DUPLICATE KEY UPDATE message_id=VALUES(message_id)`,
				strings.Join(valueStrings, ","))
			_, err = bf.db.session.InsertBySql(stmt, valueArgs...).Exec()
			if err != nil {
				bf.Warn("插入已读记录失败", zap.Error(err))
			}

			// 写入 Redis 缓存，定时任务会聚合并通知发送者
			go func() {
				for _, msg := range syncMsg.Messages {
					messageIDStr := strconv.FormatInt(msg.MessageID, 10)
					cacheData := map[string]interface{}{
						"MessageID":      msg.MessageID,
						"MessageIDStr":   messageIDStr,
						"MessageSeq":     msg.MessageSeq,
						"FromUID":        msg.FromUID,
						"ChannelID":      fakeChannelID,
						"ChannelType":    channelType,
						"LoginUID":       robotID,
						"ReqChannelID":   channelID,
						"ReqChannelType": channelType,
					}
					jsonStr, err := json.Marshal(cacheData)
					if err != nil {
						bf.Error("序列化消息已读缓存失败", zap.Error(err))
						continue
					}
					err = bf.ctx.GetRedisConn().SetAndExpire(
						fmt.Sprintf("readedCount:%s", messageIDStr),
						string(jsonStr),
						time.Hour*24*7,
					)
					if err != nil {
						bf.Error("写入已读缓存失败", zap.Error(err), zap.Int64("messageID", msg.MessageID))
					}
				}
			}()
		}
	}

	c.ResponseOK()
}

// ========== Bot Set Commands API ==========

func (bf *BotFather) setCommands(c *wkhttp.Context) {
	var req struct {
		Commands []struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		} `json:"commands"`
	}
	if err := c.BindJSON(&req); err != nil {
		bf.Error("数据格式有误", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误"))
		return
	}

	robotID := getRobotIDFromContext(c)

	if req.Commands == nil {
		req.Commands = make([]struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		}, 0)
	}
	commandsJSON, err := json.Marshal(req.Commands)
	if err != nil {
		bf.Error("序列化命令列表失败", zap.Error(err))
		c.ResponseError(errors.New("序列化命令列表失败"))
		return
	}

	err = bf.db.updateBotCommands(robotID, string(commandsJSON))
	if err != nil {
		bf.Error("保存命令列表失败", zap.Error(err))
		c.ResponseError(errors.New("保存命令列表失败"))
		return
	}

	c.ResponseOK()
}

// ========== Bot Events API (轮询消息) ==========

func (bf *BotFather) getEvents(c *wkhttp.Context) {
	var req BotEventsReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}

	robotID := getRobotIDFromContext(c)
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	results, err := bf.getEventsResult(robotID, req.EventID, limit)
	if err != nil {
		c.Response(gin.H{
			"status": 0,
			"msg":    err.Error(),
		})
		return
	}
	c.Response(gin.H{
		"status":  1,
		"results": results,
	})
}

func (bf *BotFather) getEventsResult(robotID string, eventID int64, limit int64) ([]*eventResp, error) {
	key := fmt.Sprintf("%s%s", bf.robotEventPrefix, robotID)
	robotEventJsons, err := bf.ctx.GetRedisConn().ZRangeByScore(key, redis.ZRangeBy{
		Max:   "+inf",
		Min:   fmt.Sprintf("(%d", eventID),
		Count: limit,
	})
	if err != nil {
		return nil, err
	}

	results := make([]*eventResp, 0)
	if len(robotEventJsons) > 0 {
		type robotEvent struct {
			EventID   int64                  `json:"event_id,omitempty"`
			Message   *config.MessageResp    `json:"message,omitempty"`
			EventType string                 `json:"event_type,omitempty"`
			EventData map[string]interface{} `json:"event_data,omitempty"`
			Expire    int64                  `json:"expire,omitempty"`
		}

		events := make([]*robotEvent, 0)
		for _, jsonStr := range robotEventJsons {
			var ev robotEvent
			err = util.ReadJsonByByte([]byte(jsonStr), &ev)
			if err != nil {
				bf.Error("解码事件失败", zap.Error(err))
				continue
			}
			events = append(events, &ev)
		}

		sort.Slice(events, func(i, j int) bool {
			return events[i].EventID < events[j].EventID
		})

		for _, ev := range events {
			resp := &eventResp{
				EventID: ev.EventID,
			}
			if ev.Message != nil {
				resp.Message = &messageResp{
					MessageID:  ev.Message.MessageID,
					MessageSeq: ev.Message.MessageSeq,
					FromUID:    ev.Message.FromUID,
					Timestamp:  ev.Message.Timestamp,
				}
				if ev.Message.ChannelType != common.ChannelTypePerson.Uint8() {
					resp.Message.ChannelID = ev.Message.ChannelID
					resp.Message.ChannelType = ev.Message.ChannelType
				}
				var payloadMap map[string]interface{}
				if err := util.ReadJsonByByte(ev.Message.Payload, &payloadMap); err == nil {
					resp.Message.Payload = payloadMap
				}
			}
			if ev.EventType != "" {
				resp.EventType = ev.EventType
				resp.EventData = ev.EventData
			}
			results = append(results, resp)
		}
	}
	return results, nil
}

func (bf *BotFather) eventAck(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	eventID, _ := strconv.ParseInt(c.Param("event_id"), 10, 64)

	key := fmt.Sprintf("%s%s", bf.robotEventPrefix, robotID)
	err := bf.ctx.GetRedisConn().ZRemRangeByScore(key, fmt.Sprintf("%d", eventID), fmt.Sprintf("%d", eventID))
	if err != nil {
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// ========== Bot Stream API ==========

func (bf *BotFather) streamStart(c *wkhttp.Context) {
	var req BotStreamStartReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}

	robotID := getRobotIDFromContext(c)
	channelID := bf.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)
	streamNo, err := bf.ctx.IMStreamStart(config.MessageStreamStartReq{
		Header: config.MsgHeader{
			RedDot: 1,
		},
		FromUID:     robotID,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
		Payload:     req.Payload,
	})
	if err != nil {
		bf.Error("stream start失败", zap.Error(err))
		c.ResponseError(errors.New("stream start失败"))
		return
	}
	c.Response(gin.H{
		"stream_no": streamNo,
	})
}

func (bf *BotFather) streamEnd(c *wkhttp.Context) {
	var req BotStreamEndReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}

	robotID := getRobotIDFromContext(c)
	channelID := bf.resolveSpaceChannelID(robotID, req.ChannelID, req.ChannelType)
	err := bf.ctx.IMStreamEnd(config.MessageStreamEndReq{
		StreamNo:    req.StreamNo,
		ChannelID:   channelID,
		ChannelType: req.ChannelType,
	})
	if err != nil {
		bf.Error("stream end失败", zap.Error(err))
		c.ResponseError(errors.New("stream end失败"))
		return
	}
	c.ResponseOK()
}

// ========== Bot Heartbeat API ==========

func (bf *BotFather) heartbeat(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	key := fmt.Sprintf("%s%s", heartbeatKeyPrefix, robotID)
	err := bf.ctx.GetRedisConn().SetAndExpire(key, "1", time.Second*heartbeatTTL)
	if err != nil {
		bf.Error("设置心跳失败", zap.Error(err))
		c.ResponseError(errors.New("设置心跳失败"))
		return
	}
	c.ResponseOK()
}

// ========== Bot File API ==========

// botProxyFile Bot文件下载代理 — 后端读取文件内容写入response（避免MinIO签名问题）
func (bf *BotFather) botProxyFile(c *wkhttp.Context) {
	ph := c.Param("path")
	if ph == "" {
		c.ResponseError(errors.New("文件路径不能为空"))
		return
	}
	ph = strings.TrimPrefix(ph, "/")
	// Strip file storage prefix to avoid double "file/" in path
	ph = strings.TrimPrefix(ph, "file/")

	filename := c.Query("filename")
	if filename == "" {
		parts := strings.Split(ph, "/")
		if len(parts) > 0 {
			filename = parts[len(parts)-1]
		}
	}

	reader, contentType, err := bf.fileService.GetFile(ph)
	if err != nil {
		bf.Error("读取文件失败", zap.Error(err), zap.String("path", ph))
		c.ResponseError(errors.New("获取文件失败"))
		return
	}
	defer reader.Close()

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(filename)))

	c.Status(http.StatusOK)
	io.Copy(c.Writer, reader)
}

// botUploadFile Bot文件上传
func (bf *BotFather) botUploadFile(c *wkhttp.Context) {
	fileType := c.DefaultQuery("type", "chat")
	uploadPath := c.Query("path")

	multipartFile, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		bf.Error("读取上传文件失败", zap.Error(err))
		c.ResponseError(errors.New("读取文件失败"))
		return
	}
	defer multipartFile.Close()

	const maxSize int64 = 100 * 1024 * 1024
	if fileHeader.Size > maxSize {
		c.ResponseError(fmt.Errorf("文件大小不能超过%dMB", maxSize/1024/1024))
		return
	}

	fileName := fileHeader.Filename
	path := uploadPath
	if path == "" {
		path = fmt.Sprintf("/%d/%s", time.Now().Unix(), fileName)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	storagePath := fmt.Sprintf("%s%s", fileType, path)
	contentType := "application/octet-stream"
	_, err = bf.fileService.UploadFile(storagePath, contentType, func(w io.Writer) error {
		_, err := io.Copy(w, multipartFile)
		return err
	})
	if err != nil {
		bf.Error("上传文件失败", zap.Error(err))
		c.ResponseError(errors.New("上传文件失败"))
		return
	}

	previewPath := fmt.Sprintf("file/preview/%s%s", fileType, path)
	c.Response(gin.H{
		"url":  previewPath,
		"name": fileName,
		"size": fileHeader.Size,
	})
}

// botFileDownload Bot文件下载 — 302重定向到presigned URL（/v1/bot/file/download/*path）
func (bf *BotFather) botFileDownload(c *wkhttp.Context) {
	ph := c.Param("path")
	if ph == "" {
		c.ResponseError(errors.New("文件路径不能为空"))
		return
	}
	ph = strings.TrimPrefix(ph, "/")

	// 路径清洗：防止路径遍历攻击（defense-in-depth）
	ph, err := sanitizeBotFilePath(ph)
	if err != nil {
		c.ResponseErrorWithStatus(errors.New("文件路径无效"), http.StatusBadRequest)
		return
	}

	filename := c.Query("filename")
	if filename == "" {
		parts := strings.Split(ph, "/")
		if len(parts) > 0 {
			filename = parts[len(parts)-1]
		}
	}

	downloadURL, err := bf.fileService.DownloadURL(ph, filename)
	if err != nil {
		bf.Error("获取文件下载URL失败", zap.Error(err), zap.String("path", ph))
		c.ResponseErrorWithStatus(errors.New("获取文件失败"), http.StatusNotFound)
		return
	}
	c.Redirect(http.StatusFound, downloadURL)
}

// sanitizeBotFilePath 规范化文件路径，防止路径遍历攻击
func sanitizeBotFilePath(p string) (string, error) {
	// 循环解码防止双重/多重 URL 编码绕过
	decoded := p
	for i := 0; i < 3; i++ {
		next, err := url.QueryUnescape(decoded)
		if err != nil {
			return "", errors.New("路径包含无效字符")
		}
		if next == decoded {
			break
		}
		decoded = next
	}
	// 禁止包含 .. 的路径遍历
	cleaned := filepath.Clean(decoded)
	if strings.Contains(cleaned, "..") {
		return "", errors.New("路径不允许包含目录遍历字符")
	}
	return cleaned, nil
}

// ========== 响应模型 ==========

type eventResp struct {
	EventID   int64                  `json:"event_id"`
	Message   *messageResp           `json:"message,omitempty"`
	EventType string                 `json:"event_type,omitempty"` // 自定义事件类型（如 bot_joined_group）
	EventData map[string]interface{} `json:"event_data,omitempty"` // 自定义事件数据
}

type messageResp struct {
	MessageID   int64       `json:"message_id"`
	MessageSeq  uint32      `json:"message_seq"`
	FromUID     string      `json:"from_uid"`
	ChannelID   string      `json:"channel_id,omitempty"`
	ChannelType uint8       `json:"channel_type,omitempty"`
	Timestamp   int32       `json:"timestamp"`
	Payload     interface{} `json:"payload"`
}

// syncAllBotTokens 启动时将所有活跃 bot 的 bot_token 同步到 WuKongIM
// 确保 WuKongIM 重启后所有 bot 都能正常连接
func (bf *BotFather) syncAllBotTokens() {
	robots, err := bf.db.queryAllActiveRobots()
	if err != nil {
		bf.Error("同步 bot token 失败: 查询 robot 出错", zap.Error(err))
		return
	}
	successCount := 0
	for _, robot := range robots {
		resp, tokenErr := bf.ctx.UpdateIMToken(config.UpdateIMTokenReq{
			UID:         robot.RobotID,
			Token:       robot.BotToken,
			DeviceFlag:  config.APP,
			DeviceLevel: config.DeviceLevelMaster,
		})
		if tokenErr != nil || resp.Status != config.UpdateTokenStatusSuccess {
			bf.Warn("同步 bot token 失败", zap.String("robotID", robot.RobotID), zap.Any("error", tokenErr), zap.Any("status", resp))
			continue
		}
		// 同步 DB cache
		if robot.IMTokenCache != robot.BotToken {
			bf.db.updateRobotIMTokenCache(robot.RobotID, robot.BotToken)
		}
		successCount++
	}
	bf.Info("Bot token 启动同步完成", zap.Int("total", len(robots)), zap.Int("success", successCount))
}
