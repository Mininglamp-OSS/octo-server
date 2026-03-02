package botfather

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/TangSengDaoDao/TangSengDaoDaoServer/modules/base/app"
	"github.com/TangSengDaoDao/TangSengDaoDaoServer/modules/user"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/common"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/config"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/pkg/log"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/pkg/util"
	"go.uber.org/zap"
)

// commandHandler 处理BotFather命令
type commandHandler struct {
	ctx          *config.Context
	db           *botfatherDB
	sm           *stateMachine
	userService  user.IService
	appService   app.IService
	log.Log
}

func newCommandHandler(ctx *config.Context) *commandHandler {
	return &commandHandler{
		ctx:         ctx,
		db:          newBotfatherDB(ctx),
		sm:          newStateMachine(ctx),
		userService: user.NewService(ctx),
		appService:  app.NewService(ctx),
		Log:         log.NewTLog("BotFather"),
	}
}

// HandleMessage 处理发送给BotFather的消息
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

func (h *commandHandler) handleCommand(fromUID string, cmd string) {
	// 规范化命令（只取第一个词）
	parts := strings.Fields(cmd)
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
	case CmdHelp, CmdStart:
		h.handleHelp(fromUID)
	default:
		h.reply(fromUID, "未知命令。发送 /help 查看可用命令。")
	}
}

func (h *commandHandler) handleStatefulInput(fromUID string, input string) {
	state, err := h.sm.GetState(fromUID)
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

// ========== 命令处理 ==========

func (h *commandHandler) handleCancel(fromUID string) {
	h.sm.Clear(fromUID)
	h.reply(fromUID, "操作已取消。")
}

func (h *commandHandler) handleNewBot(fromUID string) {
	h.sm.Clear(fromUID)
	h.sm.SetState(fromUID, StateWaitingBotName, CmdNewBot)
	h.reply(fromUID, "好的，请给你的机器人取一个名字：")
}

func (h *commandHandler) handleMyBots(fromUID string) {
	h.sm.Clear(fromUID)
	bots, err := h.db.queryRobotsByCreatorUID(fromUID)
	if err != nil {
		h.Error("查询机器人列表失败", zap.Error(err))
		h.reply(fromUID, "查询失败，请稍后重试。")
		return
	}
	if len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。发送 /newbot 创建一个。")
		return
	}

	var sb strings.Builder
	sb.WriteString("你的机器人列表：\n\n")
	for i, bot := range bots {
		name := bot.Username
		if name == "" {
			name = bot.RobotID
		}
		desc := bot.Description
		if desc == "" {
			desc = "无描述"
		}
		sb.WriteString(fmt.Sprintf("%d. %s (@%s)\n   %s\n\n", i+1, bot.RobotID, name, desc))
	}
	h.reply(fromUID, sb.String())
}

func (h *commandHandler) handleConnect(fromUID string) {
	bots, err := h.db.queryRobotsByCreatorUID(fromUID)
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
	h.sm.SetState(fromUID, StateWaitingSelectBot, CmdConnect)
	h.sendBotSelectionList(fromUID, bots, "请选择一个机器人获取连接 prompt：")
}

func (h *commandHandler) handleDisconnect(fromUID string) {
	bots, err := h.db.queryRobotsByCreatorUID(fromUID)
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
	h.sm.SetState(fromUID, StateWaitingSelectBot, CmdDisconnect)
	h.sendBotSelectionList(fromUID, bots, "请选择要断开连接的机器人：")
}

func (h *commandHandler) handleSetName(fromUID string) {
	bots, err := h.db.queryRobotsByCreatorUID(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.sm.SetState(fromUID, StateWaitingNewName, CmdSetName)
		h.sm.SetField(fromUID, FieldBotID, bots[0].RobotID)
		h.reply(fromUID, fmt.Sprintf("请输入 %s 的新名称：", bots[0].RobotID))
		return
	}
	h.sm.SetState(fromUID, StateWaitingSelectBot, CmdSetName)
	h.sendBotSelectionList(fromUID, bots, "请选择要改名的机器人：")
}

func (h *commandHandler) handleSetDescription(fromUID string) {
	bots, err := h.db.queryRobotsByCreatorUID(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.sm.SetState(fromUID, StateWaitingDescription, CmdSetDescription)
		h.sm.SetField(fromUID, FieldBotID, bots[0].RobotID)
		h.reply(fromUID, fmt.Sprintf("请输入 %s 的新描述：", bots[0].RobotID))
		return
	}
	h.sm.SetState(fromUID, StateWaitingSelectBot, CmdSetDescription)
	h.sendBotSelectionList(fromUID, bots, "请选择要设置描述的机器人：")
}

func (h *commandHandler) handleDeleteBot(fromUID string) {
	bots, err := h.db.queryRobotsByCreatorUID(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.sm.SetState(fromUID, StateWaitingDeleteConfirm, CmdDeleteBot)
		h.sm.SetField(fromUID, FieldBotID, bots[0].RobotID)
		h.reply(fromUID, fmt.Sprintf("确定要删除机器人 %s 吗？输入 \"Yes, delete it\" 确认：", bots[0].RobotID))
		return
	}
	h.sm.SetState(fromUID, StateWaitingSelectBot, CmdDeleteBot)
	h.sendBotSelectionList(fromUID, bots, "请选择要删除的机器人：")
}

func (h *commandHandler) handleToken(fromUID string) {
	bots, err := h.db.queryRobotsByCreatorUID(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.reply(fromUID, fmt.Sprintf("机器人 %s 的 Token：\n\n%s\n\n请妥善保管，不要泄露。", bots[0].RobotID, bots[0].BotToken))
		return
	}
	h.sm.SetState(fromUID, StateWaitingSelectBot, CmdToken)
	h.sendBotSelectionList(fromUID, bots, "请选择要查看 Token 的机器人：")
}

func (h *commandHandler) handleRevoke(fromUID string) {
	bots, err := h.db.queryRobotsByCreatorUID(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "你还没有创建机器人。")
		return
	}
	if len(bots) == 1 {
		h.sm.SetState(fromUID, StateWaitingRevokeConfirm, CmdRevoke)
		h.sm.SetField(fromUID, FieldBotID, bots[0].RobotID)
		h.reply(fromUID, fmt.Sprintf("确定要重置 %s 的 Token 吗？旧 Token 将立即失效。输入 \"Yes, revoke it\" 确认：", bots[0].RobotID))
		return
	}
	h.sm.SetState(fromUID, StateWaitingSelectBot, CmdRevoke)
	h.sendBotSelectionList(fromUID, bots, "请选择要重置 Token 的机器人：")
}

func (h *commandHandler) handleHelp(fromUID string) {
	h.sm.Clear(fromUID)
	h.reply(fromUID, `BotFather 可以帮你创建和管理机器人。

可用命令：
/newbot - 创建新机器人
/mybots - 查看我的机器人
/connect - 获取连接 prompt
/disconnect - 断开 Agent 连接
/setname - 修改机器人名称
/setdescription - 修改机器人描述
/deletebot - 删除机器人
/token - 查看 Token
/revoke - 重置 Token
/cancel - 取消当前操作
/help - 显示帮助`)
}

// ========== 状态输入处理 ==========

func (h *commandHandler) onBotNameInput(fromUID string, name string) {
	name = strings.TrimSpace(name)
	if len(name) == 0 || len(name) > 64 {
		h.reply(fromUID, "名称长度需要在 1-64 个字符之间，请重新输入：")
		return
	}

	// 保存名字，进入下一步：要求输入唯一标识符
	h.sm.SetField(fromUID, FieldBotName, name)
	h.sm.SetState(fromUID, StateWaitingBotUsername, CmdNewBot)
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
	name, _ := h.sm.GetField(fromUID, FieldBotName)
	if name == "" {
		h.reply(fromUID, "操作异常，已取消。请重新发送 /newbot")
		h.sm.Clear(fromUID)
		return
	}

	// 生成 bot token 并创建（含碰撞检测）
	botToken, err := h.generateUniqueBotToken()
	if err != nil {
		h.Error("生成Bot Token失败", zap.Error(err))
		h.reply(fromUID, "创建失败，请稍后重试。")
		h.sm.Clear(fromUID)
		return
	}
	err = h.createBot(fromUID, name, username, botToken)
	if err != nil {
		h.Error("创建机器人失败", zap.Error(err))
		h.reply(fromUID, "创建失败，请稍后重试。")
		h.sm.Clear(fromUID)
		return
	}

	h.sm.Clear(fromUID)

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
	bots, err := h.db.queryRobotsByCreatorUID(fromUID)
	if err != nil || len(bots) == 0 {
		h.reply(fromUID, "查询失败，操作已取消。")
		h.sm.Clear(fromUID)
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

	cmd, _ := h.sm.GetCommand(fromUID)
	h.sm.SetField(fromUID, FieldBotID, selectedBot.RobotID)

	switch cmd {
	case CmdConnect:
		h.sm.Clear(fromUID)
		h.sendConnectPrompt(fromUID, selectedBot)
	case CmdDisconnect:
		h.sm.Clear(fromUID)
		h.disconnectBot(fromUID, selectedBot)
	case CmdSetName:
		h.sm.SetState(fromUID, StateWaitingNewName, CmdSetName)
		h.reply(fromUID, fmt.Sprintf("请输入 %s 的新名称：", selectedBot.RobotID))
	case CmdSetDescription:
		h.sm.SetState(fromUID, StateWaitingDescription, CmdSetDescription)
		h.reply(fromUID, fmt.Sprintf("请输入 %s 的新描述：", selectedBot.RobotID))
	case CmdDeleteBot:
		h.sm.SetState(fromUID, StateWaitingDeleteConfirm, CmdDeleteBot)
		h.reply(fromUID, fmt.Sprintf("确定要删除 %s 吗？输入 \"Yes, delete it\" 确认：", selectedBot.RobotID))
	case CmdToken:
		h.sm.Clear(fromUID)
		h.reply(fromUID, fmt.Sprintf("机器人 %s 的 Token：\n\n%s\n\n请妥善保管，不要泄露。", selectedBot.RobotID, selectedBot.BotToken))
	case CmdRevoke:
		h.sm.SetState(fromUID, StateWaitingRevokeConfirm, CmdRevoke)
		h.reply(fromUID, fmt.Sprintf("确定要重置 %s 的 Token 吗？输入 \"Yes, revoke it\" 确认：", selectedBot.RobotID))
	default:
		h.sm.Clear(fromUID)
		h.reply(fromUID, "操作异常，已取消。")
	}
}

func (h *commandHandler) onNewNameInput(fromUID string, name string) {
	name = strings.TrimSpace(name)
	if len(name) == 0 || len(name) > 64 {
		h.reply(fromUID, "名称长度需要在 1-64 个字符之间，请重新输入：")
		return
	}

	botID, _ := h.sm.GetBotID(fromUID)
	if botID == "" {
		h.reply(fromUID, "操作异常，已取消。")
		h.sm.Clear(fromUID)
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
		h.sm.Clear(fromUID)
		return
	}

	h.sm.Clear(fromUID)
	h.reply(fromUID, fmt.Sprintf("机器人名称已更新为：%s", name))
}

func (h *commandHandler) onDescriptionInput(fromUID string, desc string) {
	desc = strings.TrimSpace(desc)
	if len(desc) > 500 {
		h.reply(fromUID, "描述不能超过 500 个字符，请重新输入：")
		return
	}

	botID, _ := h.sm.GetBotID(fromUID)
	if botID == "" {
		h.reply(fromUID, "操作异常，已取消。")
		h.sm.Clear(fromUID)
		return
	}

	err := h.db.updateRobotDescription(botID, desc)
	if err != nil {
		h.Error("更新描述失败", zap.Error(err))
		h.reply(fromUID, "更新失败，请稍后重试。")
		h.sm.Clear(fromUID)
		return
	}

	h.sm.Clear(fromUID)
	h.reply(fromUID, "描述已更新。")
}

func (h *commandHandler) onDeleteConfirm(fromUID string, input string) {
	botID, _ := h.sm.GetBotID(fromUID)
	if botID == "" {
		h.reply(fromUID, "操作异常，已取消。")
		h.sm.Clear(fromUID)
		return
	}

	if strings.TrimSpace(input) != "Yes, delete it" {
		h.reply(fromUID, "删除已取消。")
		h.sm.Clear(fromUID)
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

	err = h.db.deleteRobot(botID)
	if err != nil {
		h.Error("删除机器人失败", zap.Error(err))
		h.reply(fromUID, "删除失败，请稍后重试。")
		h.sm.Clear(fromUID)
		return
	}

	h.sm.Clear(fromUID)
	h.reply(fromUID, fmt.Sprintf("机器人 %s 已删除。", botID))
}

func (h *commandHandler) onRevokeConfirm(fromUID string, input string) {
	botID, _ := h.sm.GetBotID(fromUID)
	if botID == "" {
		h.reply(fromUID, "操作异常，已取消。")
		h.sm.Clear(fromUID)
		return
	}

	if strings.TrimSpace(input) != "Yes, revoke it" {
		h.reply(fromUID, "操作已取消。")
		h.sm.Clear(fromUID)
		return
	}

	newToken, err := h.generateUniqueBotToken()
	if err != nil {
		h.Error("生成Bot Token失败", zap.Error(err))
		h.reply(fromUID, "重置失败，请稍后重试。")
		h.sm.Clear(fromUID)
		return
	}
	err = h.db.updateRobotBotToken(botID, newToken)
	if err != nil {
		h.Error("重置Token失败", zap.Error(err))
		h.reply(fromUID, "重置失败，请稍后重试。")
		h.sm.Clear(fromUID)
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

	h.sm.Clear(fromUID)
	h.reply(fromUID, fmt.Sprintf("Token 已重置。新 Token：\n\n%s\n\n旧 Token 已失效，已连接的 Agent 已被踢下线。", newToken))
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

func (h *commandHandler) createBot(creatorUID, name, username, botToken string) error {
	robotID := username // 机器人的UID就是用户名

	// 1. 创建 App
	appResp, err := h.appService.CreateApp(app.Req{AppID: robotID})
	if err != nil {
		return fmt.Errorf("创建app失败: %w", err)
	}

	// 2. 创建用户
	err = h.userService.AddUser(&user.AddUserReq{
		UID:      robotID,
		Username: username,
		Name:     name,
		ShortNo:  username,
		Robot:    1,
	})
	if err != nil {
		return fmt.Errorf("创建用户失败: %w", err)
	}

	// 3. 添加创建者为好友（双向）
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

	// 4. 创建机器人记录
	tx, err := h.db.session.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
		}
	}()

	version := h.ctx.GenSeq(common.RobotSeqKey)
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

	return nil
}

func (h *commandHandler) reply(toUID string, content string) {
	channelID := toUID
	h.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 1,
		},
		FromUID:     BotFatherUID,
		ChannelID:   channelID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"content": content,
			"type":    common.Text,
		})),
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

	prompt := fmt.Sprintf(`📋 机器人「%s」的连接信息：

将下面的提示词复制发给你的 AI Agent：

---
Read %s/v1/bot/skill.md to connect to DMWork messaging.
My bot token is: %s
API server: %s
Install adapter: openclaw plugins install openclaw-channel-dmwork
Or install manually: npm i openclaw-channel-dmwork
DM me on DMWork when you're ready.
---

💡 支持 OpenClaw、Claude Code、及任何能读取 URL 的 AI Agent
🔌 断开连接请发送 /disconnect`,
		bot.RobotID, apiURL, bot.BotToken, apiURL)

	h.reply(toUID, prompt)
}

func (h *commandHandler) sendCreatedPrompt(toUID string, name string, bot *robotModel) {
	cfg := h.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}

	msg := fmt.Sprintf(`✅ 机器人「%s」创建成功！

📋 连接 Agent
将下面的提示词复制发送给你的 AI Agent：

---
Read %s/v1/bot/skill.md to connect to DMWork messaging.
My bot token is: %s
API server: %s
Install adapter: openclaw plugins install openclaw-channel-dmwork
Or install manually: npm i openclaw-channel-dmwork
DM me on DMWork when you're ready.
---

💡 支持 OpenClaw、Claude Code、及任何能读取 URL 的 AI Agent
🔌 断开连接请发送 /disconnect`,
		name, apiURL, bot.BotToken, apiURL)

	h.reply(toUID, msg)
}

// generateBotToken 生成Bot Token
func generateBotToken() string {
	return BotTokenPrefix + randomHex(16)
}

// generateUniqueBotToken 生成唯一的Bot Token（最多重试3次）
func (h *commandHandler) generateUniqueBotToken() (string, error) {
	for i := 0; i < 3; i++ {
		token := generateBotToken()
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

// randomHex 生成随机十六进制字符串
func randomHex(n int) string {
	bytes := make([]byte, n)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
