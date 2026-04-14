package botfather

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"go.uber.org/zap"
)

// commandHandler 处理BotFather命令
type commandHandler struct {
	ctx          *config.Context
	db           *botfatherDB
	sm           *stateMachine
	userService  user.IService
	groupService group.IService
	appService   app.IService
	log.Log
}

func newCommandHandler(ctx *config.Context) *commandHandler {
	return &commandHandler{
		ctx:          ctx,
		db:           newBotfatherDB(ctx),
		sm:           newStateMachine(ctx),
		userService:  user.NewService(ctx),
		groupService: group.NewService(ctx),
		appService:   app.NewService(ctx),
		Log:          log.NewTLog("BotFather"),
	}
}

// spaceID 返回当前用户的 Space ID，用于状态机 key 隔离
func (h *commandHandler) spaceID(fromUID string) string {
	return getCurrentSpaceID(fromUID)
}

// HandleMessage 处理发送给BotFather的消息
// fromUID 可能是 Space 格式 (sminglue_default_uid)，内部用于回复；DB 查询需要 realUID
func (h *commandHandler) HandleMessage(fromUID string, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}

	// 命令优先处理
	if strings.HasPrefix(content, "/") {
		h.handleCommand(fromUID, content)
		return
	}

	// 非命令消息，检查是否在多轮对话中
	h.handleStatefulInput(fromUID, content)
}

// spacePrefixes stores per-fromUID Space prefix to avoid global mutable state.
// Each message-processing goroutine sets its prefix before handling and cleans
// up after, ensuring concurrent messages don't interfere with each other.
var spacePrefixes sync.Map

// spaceIDs stores per-fromUID space_id extracted from message payload.
// This is the primary source for DM scenarios where channel_id is a bare UID.
var spaceIDs sync.Map

// setSpacePrefixForUID extracts the Space prefix from channelID and stores it
// keyed by fromUID. Returns a cleanup function that must be deferred.
func setSpacePrefixForUID(fromUID, channelID string) func() {
	prefix := ""
	suffix := "_" + BotFatherUID
	idx := strings.Index(channelID, suffix)
	if idx > 0 {
		part := channelID[:idx+1] // include trailing "_"
		atIdx := strings.LastIndex(part, "@")
		if atIdx >= 0 {
			prefix = part[atIdx+1:]
		} else {
			prefix = part
		}
	}
	if prefix != "" {
		spacePrefixes.Store(fromUID, prefix)
	}
	return func() { spacePrefixes.Delete(fromUID) }
}

// getSpacePrefix returns the Space prefix for the given fromUID, or "".
func getSpacePrefix(fromUID string) string {
	if v, ok := spacePrefixes.Load(fromUID); ok {
		return v.(string)
	}
	return ""
}

// extractRealUID strips the Space prefix from a uid if present.
func extractRealUID(uid string) string {
	prefix := getSpacePrefix(uid)
	if prefix != "" && strings.HasPrefix(uid, prefix) {
		return uid[len(prefix):]
	}
	return uid
}

// setSpaceIDFromPayload stores the space_id from message payload for the given uid.
// Returns a cleanup function that must be deferred.
func setSpaceIDFromPayload(uid, spaceID string) func() {
	if spaceID != "" {
		spaceIDs.Store(uid, spaceID)
	}
	return func() { spaceIDs.Delete(uid) }
}

// getCurrentSpaceID returns the current Space ID for the given uid.
// Priority: payload space_id > channel_id Space prefix > empty.
func getCurrentSpaceID(uid string) string {
	// Priority 1: from payload space_id
	if v, ok := spaceIDs.Load(uid); ok {
		if sid, ok := v.(string); ok && sid != "" {
			return sid
		}
	}
	// Priority 2: from channel_id Space prefix (legacy)
	prefix := getSpacePrefix(uid)
	if prefix != "" && len(prefix) > 2 {
		// prefix format: "s{spaceId}_", strip leading "s" and trailing "_"
		return prefix[1 : len(prefix)-1]
	}
	return ""
}

func (h *commandHandler) handleCommand(fromUID string, cmd string) {
	// 规范化命令（只取第一个词）
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return
	}
	command := strings.ToLower(parts[0])

	switch command {
	case CmdCancel:
		h.handleCancel(fromUID)
	case CmdNewBot:
		h.handleNewBot(fromUID)
	case CmdMyBots:
		h.handleMyBots(fromUID)
	case CmdConnect:
		h.handleConnect(fromUID)
	case CmdDisconnect:
		h.handleDisconnect(fromUID)
	case CmdSetName:
		h.handleSetName(fromUID)
	case CmdSetDescription:
		h.handleSetDescription(fromUID)
	case CmdDeleteBot:
		h.handleDeleteBot(fromUID)
	case CmdToken:
		h.handleToken(fromUID)
	case CmdRevoke:
		h.handleRevoke(fromUID)
	case CmdQuickstart:
		h.handleQuickstart(fromUID)
	case CmdApprove:
		h.handleApprove(fromUID, strings.TrimPrefix(cmd, command+" "))
	case CmdReject:
		h.handleReject(fromUID, strings.TrimPrefix(cmd, command+" "))
	case CmdPending:
		h.handlePending(fromUID)
	case CmdHelp, CmdStart:
		h.handleHelp(fromUID)
	default:
		h.reply(fromUID, "未知命令。发送 /help 查看可用命令。")
	}
}

func (h *commandHandler) handleStatefulInput(fromUID string, input string) {
	state, err := h.sm.GetState(fromUID, h.spaceID(fromUID))
	if err != nil {
		h.Error("获取用户状态失败", zap.Error(err))
		return
	}

	switch state {
	case StateWaitingBotName:
		h.onBotNameInput(fromUID, input)
	case StateWaitingBotUsername:
		h.onBotUsernameInput(fromUID, input)
	case StateWaitingSelectBot:
		h.onBotSelection(fromUID, input)
	case StateWaitingNewName:
		h.onNewNameInput(fromUID, input)
	case StateWaitingDescription:
		h.onDescriptionInput(fromUID, input)
	case StateWaitingDeleteConfirm:
		h.onDeleteConfirm(fromUID, input)
	case StateWaitingRevokeConfirm:
		h.onRevokeConfirm(fromUID, input)
	default:
		h.reply(fromUID, "发送 /help 查看可用命令。")
	}
}

// queryBotsForUser returns the creator's bots, filtered by current Space if available.
func (h *commandHandler) queryBotsForUser(fromUID string) ([]*robotModel, error) {
	realUID := extractRealUID(fromUID)
	spaceID := h.resolveSpaceID(fromUID)
	if spaceID != "" {
		return h.db.queryRobotsByCreatorUIDAndSpaceID(realUID, spaceID)
	}
	return h.db.queryRobotsByCreatorUID(realUID)
}

// ========== 命令处理 ==========

func (h *commandHandler) handleCancel(fromUID string) {
	h.sm.Clear(fromUID, h.spaceID(fromUID))
	h.reply(fromUID, "操作已取消。")
}

func (h *commandHandler) handleNewBot(fromUID string) {
	h.sm.Clear(fromUID, h.spaceID(fromUID))
	h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingBotName, CmdNewBot)
	h.reply(fromUID, "好的，请给你的机器人取一个名字：")
}

func (h *commandHandler) handleMyBots(fromUID string) {
	h.sm.Clear(fromUID, h.spaceID(fromUID))
	realUID := extractRealUID(fromUID)
	// Check if creator user exists and is active (helps diagnose /mybots failures)
	var userStatus int
	statusErr := h.db.session.SelectBySql("SELECT COALESCE((SELECT status FROM user WHERE uid = ?), -1)", realUID).LoadOne(&userStatus)
	if statusErr != nil {
		userStatus = -2 // query failed
	}
	// 提取当前 Space ID，用于过滤
	currentSpaceID := h.resolveSpaceID(fromUID)
	h.Info("/mybots query", zap.String("fromUID", fromUID), zap.String("realUID", realUID), zap.Int("creator_user_status", userStatus), zap.String("spaceID", currentSpaceID))
	var bots []*robotModel
	var err error
	if currentSpaceID != "" {
		bots, err = h.db.queryRobotsByCreatorUIDAndSpaceID(realUID, currentSpaceID)
	} else {
		bots, err = h.db.queryRobotsByCreatorUID(realUID)
	}
	if err != nil {
		h.Error("查询机器人列表失败", zap.Error(err), zap.String("realUID", realUID))
		h.reply(fromUID, "查询失败，请稍后重试。")
		return
	}
	// Filter out any bots with status != 1 as a defensive measure
	var activeBots []*robotModel
	for _, bot := range bots {
		if bot.Status == 1 {
			activeBots = append(activeBots, bot)
		} else {
			h.Warn("/mybots: filtered out bot with unexpected status",
				zap.String("robot_id", bot.RobotID),
				zap.Int("status", bot.Status))
		}
	}
	bots = activeBots

	if len(bots) == 0 {
		h.Info("/mybots returned 0 results", zap.String("realUID", realUID))
		h.reply(fromUID, "你还没有创建机器人。发送 /newbot 创建一个。")
		return
	}

	var sb strings.Builder
	sb.WriteString("**你的机器人列表：**\n\n")
	for i, bot := range bots {
		name := bot.Username
		if name == "" {
			name = bot.RobotID
		}
		desc := bot.Description
		if desc == "" {
			desc = "无描述"
		}
		sb.WriteString(fmt.Sprintf("%d. **%s** (@%s)\n   %s\n\n", i+1, bot.RobotID, name, desc))
	}
	h.reply(fromUID, sb.String())
}

func (h *commandHandler) handleConnect(fromUID string) {
	bots, err := h.queryBotsForUser(fromUID)
	if err != nil {
		h.Error("查询机器人列表失败", zap.Error(err))
		h.reply(fromUID, "查询失败，请稍后重试。")
		return
	}
	if len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。发送 /newbot 创建一个。")
		return
	}
	if len(bots) == 1 {
		h.sendConnectPrompt(fromUID, bots[0])
		return
	}
	h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingSelectBot, CmdConnect)
	h.sendBotSelectionList(fromUID, bots, "请选择一个机器人获取连接 prompt：")
}

func (h *commandHandler) handleDisconnect(fromUID string) {
	bots, err := h.queryBotsForUser(fromUID)
	if err != nil {
		h.Error("查询机器人列表失败", zap.Error(err))
		h.reply(fromUID, "查询失败，请稍后重试。")
		return
	}
	if len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。发送 /newbot 创建一个。")
		return
	}
	if len(bots) == 1 {
		h.disconnectBot(fromUID, bots[0])
		return
	}
	h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingSelectBot, CmdDisconnect)
	h.sendBotSelectionList(fromUID, bots, "请选择要断开连接的机器人：")
}

func (h *commandHandler) handleSetName(fromUID string) {
	bots, err := h.queryBotsForUser(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingNewName, CmdSetName)
		h.sm.SetField(fromUID, h.spaceID(fromUID), FieldBotID, bots[0].RobotID)
		h.reply(fromUID, fmt.Sprintf("请输入 %s 的新名称：", bots[0].RobotID))
		return
	}
	h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingSelectBot, CmdSetName)
	h.sendBotSelectionList(fromUID, bots, "请选择要改名的机器人：")
}

func (h *commandHandler) handleSetDescription(fromUID string) {
	bots, err := h.queryBotsForUser(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingDescription, CmdSetDescription)
		h.sm.SetField(fromUID, h.spaceID(fromUID), FieldBotID, bots[0].RobotID)
		h.reply(fromUID, fmt.Sprintf("请输入 %s 的新描述：", bots[0].RobotID))
		return
	}
	h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingSelectBot, CmdSetDescription)
	h.sendBotSelectionList(fromUID, bots, "请选择要设置描述的机器人：")
}

func (h *commandHandler) handleDeleteBot(fromUID string) {
	bots, err := h.queryBotsForUser(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingDeleteConfirm, CmdDeleteBot)
		h.sm.SetField(fromUID, h.spaceID(fromUID), FieldBotID, bots[0].RobotID)
		h.reply(fromUID, fmt.Sprintf("确定要删除机器人 %s 吗？输入 \"Yes, delete it\" 确认：", bots[0].RobotID))
		return
	}
	h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingSelectBot, CmdDeleteBot)
	h.sendBotSelectionList(fromUID, bots, "请选择要删除的机器人：")
}

func (h *commandHandler) handleToken(fromUID string) {
	bots, err := h.queryBotsForUser(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.reply(fromUID, fmt.Sprintf("机器人 **%s** 的 Token：\n\n`%s`\n\n请妥善保管，不要泄露。", bots[0].RobotID, bots[0].BotToken))
		return
	}
	h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingSelectBot, CmdToken)
	h.sendBotSelectionList(fromUID, bots, "请选择要查看 Token 的机器人：")
}

func (h *commandHandler) handleRevoke(fromUID string) {
	bots, err := h.queryBotsForUser(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingRevokeConfirm, CmdRevoke)
		h.sm.SetField(fromUID, h.spaceID(fromUID), FieldBotID, bots[0].RobotID)
		h.reply(fromUID, fmt.Sprintf("确定要重置 %s 的 Token 吗？旧 Token 将立即失效。输入 \"Yes, revoke it\" 确认：", bots[0].RobotID))
		return
	}
	h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingSelectBot, CmdRevoke)
	h.sendBotSelectionList(fromUID, bots, "请选择要重置 Token 的机器人：")
}

func (h *commandHandler) handleQuickstart(fromUID string) {
	h.sm.Clear(fromUID, h.spaceID(fromUID))
	realUID := extractRealUID(fromUID)

	// 获取当前 Space ID，绑定到 API Key
	spaceID := h.resolveSpaceID(fromUID)

	// 查找当前 Space 的名称（用于展示）
	spaceName := ""
	if spaceID != "" {
		if name, err := h.db.querySpaceNameByID(spaceID); err == nil && name != "" {
			spaceName = name
		}
	}

	// 获取或创建 User API Key（每个 Space 独立一把 Key）
	var apiKey string
	if spaceID != "" {
		// 优先查当前 Space 是否已有 Key
		existing, err := h.db.queryUserAPIKeyByUIDAndSpaceID(realUID, spaceID)
		if err != nil {
			h.Error("查询User API Key失败", zap.Error(err))
			h.reply(fromUID, "操作失败，请稍后重试。")
			return
		}
		if existing != nil {
			apiKey = existing.APIKey
		} else {
			apiKey, err = generateUserAPIKey()
			if err != nil {
				h.Error("生成User API Key失败", zap.Error(err))
				h.reply(fromUID, "操作失败，请稍后重试。")
				return
			}
			err = h.db.insertUserAPIKey(realUID, apiKey, spaceID)
			if err != nil {
				h.Error("保存User API Key失败", zap.Error(err))
				h.reply(fromUID, "操作失败，请稍后重试。")
				return
			}
		}
	} else {
		// 无 Space 场景：回退到按 UID 查询（兼容旧数据）
		existing, err := h.db.queryUserAPIKeyByUID(realUID)
		if err != nil {
			h.Error("查询User API Key失败", zap.Error(err))
			h.reply(fromUID, "操作失败，请稍后重试。")
			return
		}
		if existing != nil {
			apiKey = existing.APIKey
		} else {
			apiKey, err = generateUserAPIKey()
			if err != nil {
				h.Error("生成User API Key失败", zap.Error(err))
				h.reply(fromUID, "操作失败，请稍后重试。")
				return
			}
			err = h.db.insertUserAPIKey(realUID, apiKey, "")
			if err != nil {
				h.Error("保存User API Key失败", zap.Error(err))
				h.reply(fromUID, "操作失败，请稍后重试。")
				return
			}
		}
	}

	cfg := h.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}

	// 构造 Space 提示行
	spaceInfo := ""
	if spaceName != "" {
		spaceInfo = fmt.Sprintf("\n📌 当前 Space：%s", spaceName)
	} else if spaceID != "" {
		spaceInfo = fmt.Sprintf("\n📌 当前 Space ID：%s", spaceID)
	}

	apiKeyFormatted := "`" + apiKey + "`"
	h.reply(fromUID, fmt.Sprintf(`🚀 **Quickstart**

将下面的提示词复制发送给你的 OpenClaw：

---
Read %s/v1/bot/skill.md to learn the DMWork Bot API (includes User API, multi-bot config, and OpenClaw setup).

My User API Key: %s
API server: %s

Create a bot, get the bot_token, then follow the skill.md instructions to connect.
All User API endpoints require: Authorization: Bearer %s
---

💡 User API Key 可反复使用，用于管理你的所有 Bot（Bot 会自动加入你当前的 Space）%s
🔑 你的 API Key: %s`,
		apiURL, apiKey, apiURL, apiKey, spaceInfo, apiKeyFormatted))
}

func (h *commandHandler) handleHelp(fromUID string) {
	h.sm.Clear(fromUID, h.spaceID(fromUID))
	h.reply(fromUID, `BotFather 可以帮你创建和管理机器人。

**可用命令：**

- /quickstart — AI Agent 快速入门（推荐）
- /newbot — 创建新机器人
- /mybots — 查看我的机器人
- /connect — 获取连接 prompt
- /disconnect — 断开 Agent 连接
- /setname — 修改机器人名称
- /setdescription — 修改机器人描述
- /deletebot — 删除机器人
- /token — 查看 Token
- /revoke — 重置 Token
- /pending — 查看待处理的好友申请
- /approve — 通过好友申请
- /reject — 拒绝好友申请
- /cancel — 取消当前操作
- /help — 显示帮助`)
}

// ========== 状态输入处理 ==========

func (h *commandHandler) onBotNameInput(fromUID string, name string) {
	name = strings.TrimSpace(name)
	if len(name) == 0 || len(name) > 64 {
		h.reply(fromUID, "名称长度需要在 1-64 个字符之间，请重新输入：")
		return
	}

	// 保存名字，进入下一步：要求输入唯一标识符
	h.sm.SetField(fromUID, h.spaceID(fromUID), FieldBotName, name)
	h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingBotUsername, CmdNewBot)
	h.reply(fromUID, fmt.Sprintf("好的，名字是「%s」。\n\n现在请为它设置一个唯一标识符（英文/数字/下划线，如 xiaobao）。\n其他用户可以通过这个标识符搜索并添加你的机器人。\n系统会自动添加 _bot 后缀。", name))
}

func (h *commandHandler) onBotUsernameInput(fromUID string, input string) {
	input = strings.TrimSpace(input)
	input = strings.ToLower(input)

	// 去掉用户可能手动加的 _bot 后缀，后面统一加
	input = strings.TrimSuffix(input, BotUsernameSuffix)

	// 校验格式：只允许英文字母、数字、下划线
	if len(input) == 0 || len(input) > 20 {
		h.reply(fromUID, "标识符长度需要在 1-20 个字符之间，请重新输入：")
		return
	}
	for _, r := range input {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			h.reply(fromUID, "标识符只能包含英文字母、数字和下划线，请重新输入：")
			return
		}
	}

	username := input + BotUsernameSuffix

	// 检查唯一性
	exists, _ := h.db.existRobotByUsername(username)
	if exists {
		h.reply(fromUID, fmt.Sprintf("标识符「%s」已被占用，请换一个：", username))
		return
	}
	u, _ := h.userService.GetUserWithUsername(username)
	if u != nil {
		h.reply(fromUID, fmt.Sprintf("标识符「%s」已被占用，请换一个：", username))
		return
	}

	// 获取之前保存的名字
	name, _ := h.sm.GetField(fromUID, h.spaceID(fromUID), FieldBotName)
	if name == "" {
		h.reply(fromUID, "操作异常，已取消。请重新发送 /newbot")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	// 生成 bot token 并创建（含碰撞检测）
	botToken, err := h.generateUniqueBotToken()
	if err != nil {
		h.Error("生成Bot Token失败", zap.Error(err))
		h.reply(fromUID, "创建失败，请稍后重试。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}
	err = h.createBot(extractRealUID(fromUID), fromUID, name, username, botToken)
	if err != nil {
		h.Error("创建机器人失败", zap.Error(err))
		h.reply(fromUID, "创建失败，请稍后重试。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	h.sm.Clear(fromUID, h.spaceID(fromUID))

	// 生成连接 prompt
	bot, err := h.db.queryRobotByBotToken(botToken)
	if err != nil || bot == nil {
		h.reply(fromUID, fmt.Sprintf("机器人「%s」(%s) 创建成功！但获取信息失败，请使用 /connect 获取连接信息。", name, username))
		return
	}
	h.sendCreatedPrompt(fromUID, name, bot)
}

func (h *commandHandler) onBotSelection(fromUID string, input string) {
	input = strings.TrimSpace(input)

	// 查找机器人
	bots, err := h.queryBotsForUser(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "查询失败，操作已取消。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	var selectedBot *robotModel
	// 支持按序号或名称选择
	for i, bot := range bots {
		if fmt.Sprintf("%d", i+1) == input || bot.RobotID == input || bot.Username == input {
			selectedBot = bot
			break
		}
	}
	if selectedBot == nil {
		h.reply(fromUID, "未找到该机器人，请输入序号或机器人ID：")
		return
	}

	cmd, _ := h.sm.GetCommand(fromUID, h.spaceID(fromUID))
	h.sm.SetField(fromUID, h.spaceID(fromUID), FieldBotID, selectedBot.RobotID)

	switch cmd {
	case CmdConnect:
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		h.sendConnectPrompt(fromUID, selectedBot)
	case CmdDisconnect:
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		h.disconnectBot(fromUID, selectedBot)
	case CmdSetName:
		h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingNewName, CmdSetName)
		h.reply(fromUID, fmt.Sprintf("请输入 %s 的新名称：", selectedBot.RobotID))
	case CmdSetDescription:
		h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingDescription, CmdSetDescription)
		h.reply(fromUID, fmt.Sprintf("请输入 %s 的新描述：", selectedBot.RobotID))
	case CmdDeleteBot:
		h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingDeleteConfirm, CmdDeleteBot)
		h.reply(fromUID, fmt.Sprintf("确定要删除 %s 吗？输入 \"Yes, delete it\" 确认：", selectedBot.RobotID))
	case CmdToken:
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		h.reply(fromUID, fmt.Sprintf("机器人 **%s** 的 Token：\n\n`%s`\n\n请妥善保管，不要泄露。", selectedBot.RobotID, selectedBot.BotToken))
	case CmdRevoke:
		h.sm.SetState(fromUID, h.spaceID(fromUID), StateWaitingRevokeConfirm, CmdRevoke)
		h.reply(fromUID, fmt.Sprintf("确定要重置 %s 的 Token 吗？输入 \"Yes, revoke it\" 确认：", selectedBot.RobotID))
	default:
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		h.reply(fromUID, "操作异常，已取消。")
	}
}

func (h *commandHandler) onNewNameInput(fromUID string, name string) {
	name = strings.TrimSpace(name)
	if len(name) == 0 || len(name) > 64 {
		h.reply(fromUID, "名称长度需要在 1-64 个字符之间，请重新输入：")
		return
	}

	botID, _ := h.sm.GetBotID(fromUID, h.spaceID(fromUID))
	if botID == "" {
		h.reply(fromUID, "操作异常，已取消。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	// 更新用户表中的名称
	err := h.userService.UpdateUser(user.UserUpdateReq{
		UID:  botID,
		Name: &name,
	})
	if err != nil {
		h.Error("更新机器人名称失败", zap.Error(err))
		h.reply(fromUID, "更新失败，请稍后重试。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	h.sm.Clear(fromUID, h.spaceID(fromUID))
	h.reply(fromUID, fmt.Sprintf("机器人名称已更新为：%s", name))
}

func (h *commandHandler) onDescriptionInput(fromUID string, desc string) {
	desc = strings.TrimSpace(desc)
	if len(desc) > 500 {
		h.reply(fromUID, "描述不能超过 500 个字符，请重新输入：")
		return
	}

	botID, _ := h.sm.GetBotID(fromUID, h.spaceID(fromUID))
	if botID == "" {
		h.reply(fromUID, "操作异常，已取消。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	err := h.db.updateRobotDescription(botID, desc)
	if err != nil {
		h.Error("更新描述失败", zap.Error(err))
		h.reply(fromUID, "更新失败，请稍后重试。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	h.sm.Clear(fromUID, h.spaceID(fromUID))
	h.reply(fromUID, "描述已更新。")
}

func (h *commandHandler) onDeleteConfirm(fromUID string, input string) {
	botID, _ := h.sm.GetBotID(fromUID, h.spaceID(fromUID))
	if botID == "" {
		h.reply(fromUID, "操作异常，已取消。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	if strings.TrimSpace(input) != "Yes, delete it" {
		h.reply(fromUID, "删除已取消。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	// 先清理 IM 连接和缓存，再做软删除
	newIMToken := util.GenerUUID()
	_, err := h.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         botID,
		Token:       newIMToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if err != nil {
		h.Error("撤销IM Token失败", zap.Error(err))
	}

	// 清空缓存的 IM Token
	h.db.updateRobotIMTokenCache(botID, "")

	// 清除心跳
	heartbeatKey := fmt.Sprintf("%s%s", heartbeatKeyPrefix, botID)
	h.ctx.GetRedisConn().Del(heartbeatKey)

	// 清除事件队列
	eventKey := fmt.Sprintf("robotEvent:%s", botID)
	h.ctx.GetRedisConn().Del(eventKey)

	// Remove bot from all groups
	groups, err := h.groupService.GetGroupsWithMemberUID(botID)
	if err != nil {
		h.Error("查询Bot所在群失败", zap.Error(err))
	} else {
		for _, g := range groups {
			// Remove from IM channel
			err = h.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
				ChannelID:   g.GroupNo,
				ChannelType: uint8(common.ChannelTypeGroup),
				Subscribers: []string{botID},
			})
			if err != nil {
				h.Error("从IM频道移除Bot失败", zap.String("groupNo", g.GroupNo), zap.Error(err))
			}
		}
	}

	// Remove bot from all group_member records with version for client sync
	if groups != nil {
		for _, g := range groups {
			memberVersion, err := h.ctx.GenSeq(common.GroupMemberSeqKey)
			if err != nil {
				h.Error("GenSeq failed for group member", zap.String("groupNo", g.GroupNo), zap.Error(err))
				continue
			}
			_, err = h.ctx.DB().Update("group_member").
				Set("is_deleted", 1).
				Set("version", memberVersion).
				Where("group_no=? and uid=? and is_deleted=0", g.GroupNo, botID).
				Exec()
			if err != nil {
				h.Error("删除Bot群成员记录失败", zap.String("groupNo", g.GroupNo), zap.Error(err))
			}
		}
	}

	// Remove bot from all Spaces
	_, err = h.ctx.DB().UpdateBySql(
		"UPDATE space_member SET status=0 WHERE uid=? AND status=1", botID,
	).Exec()
	if err != nil {
		h.Error("移除Bot的Space成员记录失败", zap.Error(err))
	}

	// Remove bot from friend records with version for client sync (both directions)
	friendVersion, err := h.ctx.GenSeq(common.FriendSeqKey)
	if err != nil {
		h.Error("GenSeq failed for friend", zap.Error(err))
	} else {
		_, err = h.ctx.DB().Update("friend").
			Set("is_deleted", 1).
			Set("version", friendVersion).
			Where("(uid=? or to_uid=?) and is_deleted=0", botID, botID).
			Exec()
		if err != nil {
			h.Error("删除Bot好友记录失败", zap.Error(err))
		}
	}

	err = h.db.deleteRobot(botID)
	if err != nil {
		h.Error("删除机器人失败", zap.Error(err))
		h.reply(fromUID, "删除失败，请稍后重试。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	// 释放用户表中的 username 和 short_no，允许后续复用该标识符
	_, err = h.ctx.DB().Update("user").
		Set("username", "").
		Set("short_no", "").
		Where("uid=?", botID).
		Exec()
	if err != nil {
		h.Error("释放Bot用户名失败", zap.String("botID", botID), zap.Error(err))
	}

	h.sm.Clear(fromUID, h.spaceID(fromUID))
	h.reply(fromUID, fmt.Sprintf("机器人 %s 已删除。", botID))
}

func (h *commandHandler) onRevokeConfirm(fromUID string, input string) {
	botID, _ := h.sm.GetBotID(fromUID, h.spaceID(fromUID))
	if botID == "" {
		h.reply(fromUID, "操作异常，已取消。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	if strings.TrimSpace(input) != "Yes, revoke it" {
		h.reply(fromUID, "操作已取消。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	newToken, err := h.generateUniqueBotToken()
	if err != nil {
		h.Error("生成Bot Token失败", zap.Error(err))
		h.reply(fromUID, "重置失败，请稍后重试。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}
	err = h.db.updateRobotBotToken(botID, newToken)
	if err != nil {
		h.Error("重置Token失败", zap.Error(err))
		h.reply(fromUID, "重置失败，请稍后重试。")
		h.sm.Clear(fromUID, h.spaceID(fromUID))
		return
	}

	// 撤销旧 IM Token，踢掉现有连接
	newIMToken := util.GenerUUID()
	_, err = h.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         botID,
		Token:       newIMToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if err != nil {
		h.Error("撤销IM Token失败", zap.Error(err))
	}

	// 清空缓存的 IM Token
	h.db.updateRobotIMTokenCache(botID, "")

	// 清除心跳
	heartbeatKey := fmt.Sprintf("%s%s", heartbeatKeyPrefix, botID)
	h.ctx.GetRedisConn().Del(heartbeatKey)

	// 清除事件队列
	eventKey := fmt.Sprintf("robotEvent:%s", botID)
	h.ctx.GetRedisConn().Del(eventKey)

	h.sm.Clear(fromUID, h.spaceID(fromUID))
	h.reply(fromUID, fmt.Sprintf("Token 已重置。新 Token：\n\n`%s`\n\n旧 Token 已失效，已连接的 Agent 已被踢下线。", newToken))
}

// disconnectBot 断开机器人的 Agent 连接
func (h *commandHandler) disconnectBot(fromUID string, bot *robotModel) {
	// 1. 更新 IM Token，旧 Token 立即失效，WS 连接被踢
	newToken := util.GenerUUID()
	_, err := h.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         bot.RobotID,
		Token:       newToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if err != nil {
		h.Error("断开连接失败: 更新IM Token", zap.Error(err))
		h.reply(fromUID, "断开连接失败，请稍后重试。")
		return
	}

	// 2. 清除缓存的 IM Token
	h.db.updateRobotIMTokenCache(bot.RobotID, "")

	// 3. 清除心跳
	heartbeatKey := fmt.Sprintf("%s%s", heartbeatKeyPrefix, bot.RobotID)
	h.ctx.GetRedisConn().Del(heartbeatKey)

	// 4. 清除待处理事件队列
	eventKey := fmt.Sprintf("robotEvent:%s", bot.RobotID)
	h.ctx.GetRedisConn().Del(eventKey)

	h.reply(fromUID, fmt.Sprintf("已断开机器人「%s」的连接。\n\n已连接的 Agent 会被踢下线，待处理的消息队列已清空。\n使用 /connect 可重新获取连接信息。", bot.RobotID))
}

// ========== 辅助方法 ==========

func (h *commandHandler) createBot(creatorUID, fromUID, name, username, botToken string) (retErr error) {
	robotID := username // 机器人的UID就是用户名

	// 1. 创建 App
	appResp, err := h.appService.CreateApp(app.Req{AppID: robotID})
	if err != nil {
		return fmt.Errorf("创建app失败: %w", err)
	}

	// 2. 创建机器人记录（优先于用户/好友，确保 BotFather 能查到）
	tx, err := h.db.session.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			h.Error("panic in createBot transaction, rolled back", zap.Any("recover", r))
			retErr = fmt.Errorf("panic in createBot: %v", r)
		}
	}()

	version, err := h.ctx.GenSeq(common.RobotSeqKey)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("GenSeq failed: %w", err)
	}
	err = h.db.insertRobotTx(&robotModel{
		AppID:      appResp.AppID,
		RobotID:    robotID,
		Username:   username,
		Token:      appResp.AppKey,
		Version:    version,
		Status:     1,
		CreatorUID: creatorUID,
		BotToken:   botToken,
	}, tx)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("插入机器人记录失败: %w", err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}

	// 3. 创建用户
	err = h.userService.AddUser(&user.AddUserReq{
		UID:      robotID,
		Username: username,
		Name:     name,
		ShortNo:  username,
		Robot:    1,
	})
	if err != nil {
		// 回滚 robot 记录，避免孤儿数据
		if delErr := h.db.deleteRobot(robotID); delErr != nil {
			h.Error("回滚 robot 记录失败", zap.Error(delErr), zap.String("robot_id", robotID))
		}
		h.Error("创建用户失败，已回滚 robot 记录", zap.Error(err), zap.String("robot_id", robotID))
		return fmt.Errorf("创建用户失败: %w", err)
	}

	// 4. 将 Bot 加入当前 Space（而非创建者所在的所有 Space）
	targetSpaceID := h.resolveSpaceID(fromUID)
	if targetSpaceID == "" {
		// 无 Space 信息（legacy），回退到创建者的第一个 Space
		h.Warn("createBot: no space_id from client, falling back to creator's first Space",
			zap.String("fromUID", fromUID), zap.String("creatorUID", creatorUID))
		creatorSpaces, err := h.getCreatorSpaceIDs(creatorUID)
		if err != nil {
			h.Warn("查询创建者Space失败", zap.Error(err))
		}
		if len(creatorSpaces) > 0 {
			targetSpaceID = creatorSpaces[0]
		}
	}
	if targetSpaceID != "" {
		_, err = h.ctx.DB().InsertBySql(
			"INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
			targetSpaceID, robotID,
		).Exec()
		if err != nil {
			h.Warn("Bot加入Space失败", zap.Error(err), zap.String("space_id", targetSpaceID))
		}
	}
	// 兼容：仍添加好友关系（过渡期）
	err = h.userService.AddFriend(creatorUID, &user.FriendReq{
		UID:   creatorUID,
		ToUID: robotID,
	})
	if err != nil {
		h.Warn("添加好友关系(creator->bot)失败", zap.Error(err))
	}
	err = h.userService.AddFriend(robotID, &user.FriendReq{
		UID:   robotID,
		ToUID: creatorUID,
	})
	if err != nil {
		h.Warn("添加好友关系(bot->creator)失败", zap.Error(err))
	}
	h.fixFriendVersion(creatorUID, robotID)
	h.fixFriendVersion(robotID, creatorUID)

	return nil
}

func (h *commandHandler) reply(toUID string, content string) {
	channelID := toUID
	fromUID := BotFatherUID
	// Space 模式：BotFather 也需要加 Space 前缀
	if sp := getSpacePrefix(toUID); sp != "" {
		fromUID = sp + BotFatherUID
	}
	payload := map[string]interface{}{
		"content": content,
		"type":    common.Text,
	}
	// 写入 space_id，前端按当前 Space 过滤 BotFather 聊天历史
	if spaceID := h.resolveSpaceID(toUID); spaceID != "" {
		payload["space_id"] = spaceID
	}
	h.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 1,
		},
		FromUID:     fromUID,
		ChannelID:   channelID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     []byte(util.ToJson(payload)),
	})
}

func (h *commandHandler) sendBotSelectionList(fromUID string, bots []*robotModel, prompt string) {
	var sb strings.Builder
	sb.WriteString(prompt + "\n\n")
	for i, bot := range bots {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, bot.RobotID))
	}
	sb.WriteString("\n请输入序号：")
	h.reply(fromUID, sb.String())
}

func (h *commandHandler) sendConnectPrompt(toUID string, bot *robotModel) {
	cfg := h.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}

	prompt := fmt.Sprintf(`📋 机器人「**%s**」的连接信息：

**Bot Name:** %s
**Bot Token:** %s
**API Server:** %s

**安装插件并配置 bot：**
`+"```"+`
npx -y openclaw-channel-dmwork install --bot-token %s --api-url %s --account-id %s
`+"```"+`

**CLI 命令说明：** %s/v1/bot/cli-guide.md
**OpenClaw API 文档：** %s/v1/bot/skill.md

⚙️ 群聊默认只有 @机器人 时才回复（可在配置中修改）
🔌 断开连接请发送 /disconnect`,
		bot.RobotID, bot.RobotID, bot.BotToken, apiURL,
		bot.BotToken, apiURL, bot.RobotID,
		apiURL, apiURL)

	h.reply(toUID, prompt)
}

func (h *commandHandler) sendCreatedPrompt(toUID string, name string, bot *robotModel) {
	cfg := h.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}

	msg := fmt.Sprintf(`✅ 机器人「**%s**」创建成功！

**Bot Name:** %s
**Bot Token:** %s
**API Server:** %s

📋 **连接 OpenClaw**

**安装插件并配置 bot：**
`+"```"+`
npx -y openclaw-channel-dmwork install --bot-token %s --api-url %s --account-id %s
`+"```"+`

**CLI 命令说明：** %s/v1/bot/cli-guide.md
**OpenClaw API 文档：** %s/v1/bot/skill.md

⚙️ 群聊默认只有 @机器人 时才回复（可在配置中修改）
🔌 断开连接请发送 /disconnect`,
		name, bot.RobotID, bot.BotToken, apiURL,
		bot.BotToken, apiURL, bot.RobotID,
		apiURL, apiURL)

	h.reply(toUID, msg)
}

// resolveSpaceID returns the current Space ID for the user.
// Returns empty string if no space_id is available (callers must handle this).
// DB fallback removed: ORDER BY created_at DESC LIMIT 1 would pick the wrong
// Space when the client payload omits space_id (production bug: munger_bot).
func (h *commandHandler) resolveSpaceID(fromUID string) string {
	sid := getCurrentSpaceID(fromUID)
	if sid != "" {
		return sid
	}
	// 不再使用 DB fallback 猜测 Space（ORDER BY created_at DESC 不可靠，
	// 会导致 bot 创建到错误 Space）。返回空字符串，让各调用方走无 Space 分支。
	h.Info("resolveSpaceID: no space_id in payload or channel prefix",
		zap.String("fromUID", fromUID))
	return ""
}

// fixFriendVersion 修复好友 version=0 的问题（WKSDK 增量同步需要 version > 0）
// getCreatorSpaceIDs returns all active Space IDs the creator belongs to
func (h *commandHandler) getCreatorSpaceIDs(uid string) ([]string, error) {
	var ids []string
	_, err := h.db.session.SelectBySql(
		"SELECT space_id FROM space_member WHERE uid=? AND status=1", uid,
	).Load(&ids)
	return ids, err
}

func (h *commandHandler) fixFriendVersion(uid, toUID string) {
	var maxVer int64
	err := h.db.session.SelectBySql("SELECT IFNULL(MAX(version),0) FROM friend WHERE uid=?", uid).LoadOne(&maxVer)
	if err != nil {
		h.Warn("查询好友最大version失败", zap.Error(err))
		return
	}
	_, err = h.db.session.UpdateBySql("UPDATE friend SET version=? WHERE uid=? AND to_uid=? AND version=0", maxVer+1, uid, toUID).Exec()
	if err != nil {
		h.Warn("更新好友version失败", zap.Error(err))
	}
}

// generateBotToken 生成Bot Token
func generateBotToken() (string, error) {
	hex, err := randomHex(16)
	if err != nil {
		return "", err
	}
	return BotTokenPrefix + hex, nil
}

// generateUniqueBotToken 生成唯一的Bot Token（最多重试3次）
func (h *commandHandler) generateUniqueBotToken() (string, error) {
	for i := 0; i < 3; i++ {
		token, err := generateBotToken()
		if err != nil {
			return "", fmt.Errorf("生成Token失败: %w", err)
		}
		existing, err := h.db.queryRobotByBotToken(token)
		if err != nil {
			return "", fmt.Errorf("检查Token唯一性失败: %w", err)
		}
		if existing == nil {
			return token, nil
		}
	}
	return "", fmt.Errorf("生成唯一Token失败，已重试3次")
}

// generateUserAPIKey 生成User API Key
func generateUserAPIKey() (string, error) {
	hex, err := randomHex(16)
	if err != nil {
		return "", err
	}
	return UserAPIKeyPrefix + hex, nil
}

// randomHex 生成随机十六进制字符串
func randomHex(n int) (string, error) {
	bytes := make([]byte, n)
	_, err := rand.Read(bytes)
	if err != nil {
		return "", fmt.Errorf("随机数生成失败: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}
