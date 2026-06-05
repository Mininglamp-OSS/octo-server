package botfather

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/botutil"
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
	groupService     group.IService
	apiKeyService    UserAPIKeyService
	userDB           *user.DB
	threadService    thread.IService
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
		groupService:     group.NewService(ctx),
		apiKeyService:    NewUserAPIKeyService(ctx),
		userDB:           user.NewDB(ctx),
		threadService:    thread.NewService(ctx),
		robotEventPrefix: "robotEvent:",
		msgSem:           make(chan struct{}, 100),
		Log:              log.NewTLog("BotFather"),
	}

	// 注册消息监听器
	ctx.AddMessagesListener(bf.messagesListen)

	// 注册好友申请通知回调
	RegisterFriendApplyHook(ctx)

	// 注册用户注册事件监听器，发送欢迎消息
	ctx.AddEventListener(event.EventUserRegister, bf.handleUserRegisterEvent)

	// 注册Space成员加入事件监听器，发送Space欢迎消息
	ctx.AddEventListener(event.SpaceMemberJoin, bf.handleSpaceMemberJoinEvent)

	return bf
}

// Route 路由配置
func (bf *BotFather) Route(r *wkhttp.WKHttp) {
	// NOTE: Bot API endpoints (/v1/bot/*) have been migrated to modules/bot_api/.
	// BotFather now only handles: documentation, User Bot management (BotFather commands),
	// User API Key endpoints, and Robot Apply endpoints.

	// 启动时批量同步所有 bot 的 token 到 WuKongIM（防止 WuKongIM 重启后 token 丢失）
	// TODO: Move to bot_api module after confirming no startup ordering issue.
	go bf.syncAllBotTokens()

	// 文档端点（无需认证）
	r.GET("/v1/bot/skill.md", bf.skillMD)
	r.GET("/v1/bot/cli-guide.md", bf.cliGuideMD)
	r.GET("/v1/bot/setup-install.md", bf.cliGuideMD)
	r.GET("/v1/bot/setup-newbot.md", bf.setupNewbotMD)
	r.GET("/v1/bot/setup-quickstart.md", bf.setupQuickstartMD)

	// User Bot API 端点（使用User API Key认证）
	bf.setupUserAPIRoutes(r)

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
	wsURL := botutil.DeriveWSURL(cfg)
	content := generateSkillMD(apiURL, wsURL)
	c.Header("Content-Type", "text/markdown; charset=utf-8")
	c.String(http.StatusOK, content)
}

// cliGuideMD 返回 CLI 使用指南
func (bf *BotFather) cliGuideMD(c *wkhttp.Context) {
	content := generateCLIGuideMD()
	c.Header("Content-Type", "text/markdown; charset=utf-8")
	c.String(http.StatusOK, content)
}

// setupNewbotMD 返回 /newbot 设置流程文档
func (bf *BotFather) setupNewbotMD(c *wkhttp.Context) {
	cfg := bf.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}
	content := generateSetupNewbotMD(apiURL)
	c.Header("Content-Type", "text/markdown; charset=utf-8")
	c.String(http.StatusOK, content)
}

// setupQuickstartMD 返回 /quickstart 设置流程文档
func (bf *BotFather) setupQuickstartMD(c *wkhttp.Context) {
	cfg := bf.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}
	content := generateSetupQuickstartMD(apiURL)
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

		// 从 payload 提取 space_id（前端注入，用于 DM 裸 UID 场景）
		spaceID := payloadValue.Get("space_id").String()

		// 处理命令（使用信号量限制并发数）
		select {
		case bf.msgSem <- struct{}{}:
			go func(uid, msg, chID, sid string) {
				defer func() { <-bf.msgSem }()
				cleanup := setSpacePrefixForUID(uid, chID)
				defer cleanup()
				cleanupSID := setSpaceIDFromPayload(uid, sid)
				defer cleanupSID()
				bf.cmdHandler.HandleMessage(uid, msg)
			}(message.FromUID, content, channelID, spaceID)
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
			if r := recover(); r != nil {
				tx.Rollback()
				bf.Error("panic in initBotFatherUser transaction, rolled back", zap.Any("recover", r))
			}
		}()

		robotVersion, err := bf.ctx.GenSeq(common.RobotSeqKey)
		if err != nil {
			tx.Rollback()
			bf.Error("GenSeq failed", zap.Error(err))
			return
		}
		err = bf.db.insertRobotTx(&robotModel{
			AppID:       appResp.AppID,
			RobotID:     BotFatherUID,
			Username:    BotFatherUID,
			Token:       appResp.AppKey,
			Version:     robotVersion,
			Status:      1,
			AutoApprove: 1,
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

	// 确保 BotFather auto_approve=1（修复已有部署）
	_, _ = bf.db.session.UpdateBySql("UPDATE robot SET auto_approve=1 WHERE robot_id=? AND auto_approve=0", BotFatherUID).Exec()

	// 修复孤儿 Bot — user 表有 robot=1 但 robot 表无记录（#234 遗留数据）
	bf.repairOrphanBots()

	// 注册BotFather自身的命令列表
	bf.registerBotFatherCommands()
}

// registerBotFatherCommands 注册BotFather自身的命令列表
func (bf *BotFather) registerBotFatherCommands() {
	commands := []map[string]string{
		{"command": CmdInstall, "description": "安装/更新 Octo 插件"},
		{"command": CmdQuickstart, "description": "AI Agent 快速入门"},
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

	// 恢复已删除 BotFather 好友关系的用户（用户删除后 is_deleted=1，服务重启时自动修复）
	_, err = bf.db.session.UpdateBySql("UPDATE friend SET is_deleted=0 WHERE to_uid=? AND is_deleted=1", BotFatherUID).Exec()
	if err != nil {
		bf.Warn("恢复已删除的BotFather好友关系失败", zap.Error(err))
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

// extractBotToken pulls the Bearer token from the Authorization header. Still
// LIVE: the User-API-Key auth middleware (api_user.go) reuses it.
func extractBotToken(c *wkhttp.Context) string {
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// syncAllBotTokens 启动时将所有活跃 bot 的 token 同步到 WuKongIM
// 使用旧 im_token_cache（兼容未重启的 adapter），新 register 后会切换到 bot_token
func (bf *BotFather) syncAllBotTokens() {
	robots, err := bf.db.queryAllActiveRobots()
	if err != nil {
		bf.Error("同步 bot token 失败: 查询 robot 出错", zap.Error(err))
		return
	}
	successCount := 0
	for _, robot := range robots {
		// 优先用旧 im_token_cache（兼容还没 re-register 的旧 adapter）
		// 旧 adapter 下次 register 后会自动切换到 bot_token
		token := robot.IMTokenCache
		if strings.TrimSpace(token) == "" {
			token = robot.BotToken
		}
		resp, tokenErr := bf.ctx.UpdateIMToken(config.UpdateIMTokenReq{
			UID:         robot.RobotID,
			Token:       token,
			DeviceFlag:  config.APP,
			DeviceLevel: config.DeviceLevelMaster,
		})
		if tokenErr != nil || resp.Status != config.UpdateTokenStatusSuccess {
			bf.Warn("同步 bot token 失败", zap.String("robotID", robot.RobotID), zap.Any("error", tokenErr), zap.Any("status", resp))
			continue
		}
		successCount++
	}
	bf.Info("Bot token 启动同步完成", zap.Int("total", len(robots)), zap.Int("success", successCount))
}
