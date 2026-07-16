package notify

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/network"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

const (
	// NotifyBotUIDValue is the static UID for the notification system bot.
	NotifyBotUIDValue = "notification"
	// notifyBotName is the display name.
	notifyBotName = "通知助手"
)

// NotifyBotUID returns the static notification bot UID.
func NotifyBotUID() string {
	return NotifyBotUIDValue
}

// ensureNotifyBot creates the global notification bot if it doesn't exist (idempotent).
// Returns true if the bot is ready for use.
func (n *Notify) ensureNotifyBot() bool {
	return n.ensureBot(NotifyBotUIDValue, notifyBotName)
}

// ensureBot provisions a static full User Bot (user + app + robot.status=1) for
// botUID if absent (idempotent), repairing an orphan user, and returns true when
// the bot is ready.
func (n *Notify) ensureBot(botUID, botName string) bool {
	// Check if user already exists
	userResp, err := n.userService.GetUserWithUsername(botUID)
	if err != nil {
		n.Error("查询notify bot用户失败", zap.Error(err), zap.String("botUID", botUID))
		return false
	}
	if userResp != nil {
		// Bot exists — ensure name is correct and repair if needed
		if userResp.Name != botName {
			name := botName
			if err = n.userService.UpdateUser(user.UserUpdateReq{UID: botUID, Name: &name}); err != nil {
				n.Error("更新notify bot名称失败", zap.Error(err), zap.String("botUID", botUID))
			}
		}
		n.syncBotNameToWuKongIM(botUID, botName)
		n.repairBotIfNeeded(botUID)
		return true
	}

	// === Create flow (with compensation on failure) ===

	// Step 1: Create user
	if err = n.userService.AddUser(&user.AddUserReq{
		UID:      botUID,
		Username: botUID,
		Name:     botName,
		Robot:    1,
	}); err != nil {
		n.Error("创建notify bot用户失败", zap.Error(err), zap.String("botUID", botUID))
		return false
	}

	// Step 2: Create app
	appResp, err := n.appService.CreateApp(app.Req{AppID: botUID})
	if err != nil {
		n.Error("创建notify bot App失败，回滚user", zap.Error(err), zap.String("botUID", botUID))
		n.deleteUser(botUID)
		return false
	}

	// Step 3: Create robot record
	version, err := n.ctx.GenSeq(common.RobotSeqKey)
	if err != nil {
		n.Error("GenSeq failed，回滚app+user", zap.Error(err), zap.String("botUID", botUID))
		_ = n.appService.DeleteApp(botUID)
		n.deleteUser(botUID)
		return false
	}

	if _, err = n.db.InsertBySql(
		"INSERT IGNORE INTO robot (app_id, robot_id, username, token, version, status, auto_approve) VALUES (?, ?, ?, ?, ?, 1, 1)",
		appResp.AppID, botUID, botUID, appResp.AppKey, version,
	).Exec(); err != nil {
		n.Error("插入robot记录失败，回滚app+user", zap.Error(err), zap.String("botUID", botUID))
		_ = n.appService.DeleteApp(botUID)
		n.deleteUser(botUID)
		return false
	}

	// Step 4: Register IM token
	imToken := util.GenerUUID()
	if _, tokenErr := n.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         botUID,
		Token:       imToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	}); tokenErr != nil {
		n.Warn("notification bot UpdateIMToken failed — bot may be unable to deliver messages", zap.Error(tokenErr))
	}

	// Step 5: Sync bot name to WuKongIM
	n.syncBotNameToWuKongIM(botUID, botName)

	n.Info("Notify bot 创建成功", zap.String("botUID", botUID))
	return true
}

// repairBotIfNeeded repairs orphan user (has user but no robot record).
func (n *Notify) repairBotIfNeeded(botUID string) {
	var count int
	if _, err := n.db.SelectBySql(
		"SELECT COUNT(*) FROM robot WHERE robot_id = ? AND status = 1", botUID,
	).Load(&count); err != nil {
		return
	}
	if count > 0 {
		return
	}

	n.Warn("修复孤儿notify bot", zap.String("botUID", botUID))

	appResp, err := n.appService.CreateApp(app.Req{AppID: botUID})
	if err != nil {
		n.Error("修复: 创建App失败", zap.Error(err))
		return
	}

	version, err := n.ctx.GenSeq(common.RobotSeqKey)
	if err != nil {
		_ = n.appService.DeleteApp(botUID)
		return
	}

	_, _ = n.db.InsertBySql(
		"INSERT IGNORE INTO robot (app_id, robot_id, username, token, version, status, auto_approve) VALUES (?, ?, ?, ?, ?, 1, 1)",
		appResp.AppID, botUID, botUID, appResp.AppKey, version,
	).Exec()

	imToken := util.GenerUUID()
	if _, tokenErr := n.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         botUID,
		Token:       imToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	}); tokenErr != nil {
		n.Warn("notification bot UpdateIMToken failed", zap.Error(tokenErr))
	}
}

// deleteUser removes user record (only used for create compensation rollback).
func (n *Notify) deleteUser(uid string) {
	_, _ = n.db.DeleteFrom("user").Where("uid = ?", uid).Exec()
}

// syncBotNameToWuKongIM updates the bot display name in WuKongIM user store.
func (n *Notify) syncBotNameToWuKongIM(botUID, botName string) {
	cfg := n.ctx.GetConfig()
	headers := map[string]string{"Content-Type": "application/json"}
	if cfg.WuKongIM.ManagerToken != "" {
		headers["token"] = cfg.WuKongIM.ManagerToken
	}
	_, err := network.Post(cfg.WuKongIM.APIURL+"/user/update", []byte(util.ToJson(map[string]interface{}{
		"uid":  botUID,
		"name": botName,
	})), headers)
	if err != nil {
		n.Warn("更新WuKongIM用户名称失败", zap.Error(err), zap.String("botUID", botUID))
	}
}
