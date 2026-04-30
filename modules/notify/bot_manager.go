package notify

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/network"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

// NotifyBotUID 返回 Space 通知 Bot 的 UID。导出供 space 模块引用，避免字符串重复。
func NotifyBotUID(spaceID string) string {
	return fmt.Sprintf("notify_%s_bot", spaceID)
}

// ensureNotifyBots 启动时为所有活跃 Space 补建通知 Bot。
func (n *Notify) ensureNotifyBots() {
	var spaces []struct {
		SpaceID string `db:"space_id"`
		Name    string `db:"name"`
	}
	if _, err := n.db.SelectBySql(
		"SELECT space_id, name FROM space WHERE status = 1",
	).Load(&spaces); err != nil {
		n.Error("查询所有Space失败", zap.Error(err))
		return
	}

	for _, sp := range spaces {
		if n.ensureNotifyBot(sp.SpaceID, sp.Name) {
			n.botReady.Store(sp.SpaceID, true)
		}
	}
	n.Info("Notify bot 初始化完成", zap.Int("spaces", len(spaces)))
}

// ensureNotifyBot 为指定 Space 创建通知 Bot（幂等）。
// 带补偿逻辑：任何步骤失败回滚前序操作。返回 true 表示 bot 已就绪。
func (n *Notify) ensureNotifyBot(spaceID string, spaceName string) bool {
	botUID := NotifyBotUID(spaceID)
	// spaceName preserved for future use (e.g. multi-tenant bot naming)
	botName := "通知助手"

	// 检查 user 是否已存在
	userResp, err := n.userService.GetUserWithUsername(botUID)
	if err != nil {
		n.Error("查询notify bot用户失败", zap.Error(err), zap.String("botUID", botUID))
		return false
	}
	if userResp != nil {
		needChannelUpdate := false
		if userResp.Name != botName {
			if err = n.userService.UpdateUser(user.UserUpdateReq{UID: botUID, Name: &botName}); err != nil {
				n.Error("更新notify bot名称失败", zap.Error(err), zap.String("botUID", botUID))
			} else {
				needChannelUpdate = true
			}
		}
		// Update bot name in WuKongIM user store (person channel names come from here)
		n.syncBotNameToWuKongIM(botUID, botName)
		// Notify space members to refresh channel info
		if needChannelUpdate {
			n.notifySpaceMembersChannelUpdate(spaceID, botUID, botName)
		}
		n.ensureBotSpaceMember(spaceID, botUID)
		n.repairBotIfNeeded(botUID)
		return true
	}

	// === 创建流程（带补偿） ===

	// Step 1: 创建 user
	if err = n.userService.AddUser(&user.AddUserReq{
		UID:      botUID,
		Username: botUID,
		Name:     botName,
		Robot:    1,
	}); err != nil {
		n.Error("创建notify bot用户失败", zap.Error(err), zap.String("botUID", botUID))
		return false
	}

	// Step 2: 创建 app
	appResp, err := n.appService.CreateApp(app.Req{AppID: botUID})
	if err != nil {
		n.Error("创建notify bot App失败，回滚user", zap.Error(err), zap.String("botUID", botUID))
		n.deleteUser(botUID)
		return false
	}

	// Step 3: 创建 robot 记录
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

	// Step 4: 加入 Space
	n.ensureBotSpaceMember(spaceID, botUID)

	// Step 5: 注册 IM token
	imToken := util.GenerUUID()
	_, _ = n.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         botUID,
		Token:       imToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})

	// Step 6: Sync bot name to WuKongIM user store
	n.syncBotNameToWuKongIM(botUID, botName)

	n.Info("Notify bot 创建成功", zap.String("spaceID", spaceID), zap.String("botUID", botUID))
	return true
}

// repairBotIfNeeded 修复孤儿 user（有 user 但无 robot 记录）。
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
	_, _ = n.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         botUID,
		Token:       imToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
}

// ensureBotSpaceMember 确保 Bot 是 Space 成员。
func (n *Notify) ensureBotSpaceMember(spaceID string, botUID string) {
	if _, err := n.db.InsertBySql(
		"INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
		spaceID, botUID,
	).Exec(); err != nil {
		n.Error("notify bot 加入Space失败", zap.Error(err),
			zap.String("spaceID", spaceID), zap.String("botUID", botUID))
	}
}

// deleteUser 直接 DB 删除 user（user.IService 无 Delete 方法，仅用于创建补偿回滚）。
func (n *Notify) deleteUser(uid string) {
	_, _ = n.db.DeleteFrom("user").Where("uid = ?", uid).Exec()
}

// syncBotNameToWuKongIM updates the bot display name in WuKongIM user store.
// Person channel names are served from WuKongIM user store, not channel info.
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
	} else {
		n.Info("notify bot名称已同步到WuKongIM", zap.String("botUID", botUID), zap.String("name", botName))
	}
}

// notifySpaceMembersChannelUpdate sends CMDChannelUpdate to all space members
// so connected clients refresh the bot's channel info immediately.
func (n *Notify) notifySpaceMembersChannelUpdate(spaceID, botUID, botName string) {
	var memberUIDs []string
	if _, err := n.db.Select("uid").From("space_member").
		Where("space_id = ? AND status = 1", spaceID).
		Load(&memberUIDs); err == nil && len(memberUIDs) > 0 {
		_ = n.ctx.SendCMD(config.MsgCMDReq{
			CMD:         common.CMDChannelUpdate,
			Subscribers: memberUIDs,
			Param: map[string]interface{}{
				"channel_id":   botUID,
				"channel_type": common.ChannelTypePerson,
			},
		})
		n.Info("notify bot频道更新已推送到Space成员", zap.String("botUID", botUID), zap.Int("members", len(memberUIDs)))
	}
}
