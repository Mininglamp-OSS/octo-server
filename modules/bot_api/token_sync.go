package bot_api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

func (ba *BotAPI) setIMToken(req config.UpdateIMTokenReq) (*config.UpdateIMTokenResp, error) {
	if ba.updateIMToken != nil {
		return ba.updateIMToken(req)
	}
	return ba.ctx.UpdateIMToken(req)
}

func (ba *BotAPI) installIMToken(uid, token string) error {
	resp, err := ba.setIMToken(config.UpdateIMTokenReq{
		UID:         uid,
		Token:       token,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if err != nil {
		return err
	}
	if resp == nil || resp.Status != config.UpdateTokenStatusSuccess {
		return fmt.Errorf("unexpected IM token update response: %#v", resp)
	}
	return nil
}

// matchingBinding returns the binding for this exact bot identity. An
// inconsistent row is a conflict, never permission to install a caller-owned
// credential.
func (ba *BotAPI) matchingBinding(botToken, botKind, robotID string) (*botInstanceBinding, error) {
	binding, err := ba.db.queryBotInstanceBinding(botBindingFingerprint(botToken))
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if binding.BotKind != botKind || binding.RobotID != robotID {
		return nil, errBotInstanceConflict
	}
	return binding, nil
}

// reconcileBindingAfterUnboundUpdate closes the race where a modern client
// claims the token after a legacy registration's first lookup but before its
// WuKongIM update. The owner's credential is restored before the legacy
// request receives a conflict response.
func (ba *BotAPI) reconcileBindingAfterUnboundUpdate(
	botToken string,
	botKind string,
	robotID string,
	installedToken string,
) (bool, error) {
	binding, err := ba.matchingBinding(botToken, botKind, robotID)
	if err != nil {
		return false, err
	}
	if binding == nil {
		return false, nil
	}
	if binding.IMToken != installedToken {
		if err := ba.installIMToken(robotID, binding.IMToken); err != nil {
			return true, err
		}
	}
	return true, nil
}

// resolveRobotSyncToken reads current robot state at push time instead of
// trusting the startup snapshot. Once a binding exists, its IM token is the
// sole restore credential; the raw Bot API token must never be reinstalled.
func (ba *BotAPI) resolveRobotSyncToken(robotID string) (string, bool, error) {
	robot, err := ba.db.queryRobotByRobotID(robotID)
	if err != nil {
		return "", false, err
	}
	if robot == nil || robot.Status != 1 || strings.TrimSpace(robot.BotToken) == "" {
		return "", false, nil
	}
	binding, err := ba.matchingBinding(robot.BotToken, botKindUser, robot.RobotID)
	if err != nil {
		return "", false, err
	}
	if binding != nil {
		return binding.IMToken, true, nil
	}
	if strings.TrimSpace(robot.IMTokenCache) != "" {
		return robot.IMTokenCache, true, nil
	}
	return robot.BotToken, true, nil
}

func (ba *BotAPI) syncRobotIMToken(robotID string) error {
	token, active, err := ba.resolveRobotSyncToken(robotID)
	if err != nil || !active {
		return err
	}
	if err := ba.installIMToken(robotID, token); err != nil {
		return err
	}

	// A first claim or token rotation may have committed while the external
	// update was in flight. Resolve once more and repair before declaring this
	// robot synchronized; the latest database state stays authoritative.
	latest, stillActive, err := ba.resolveRobotSyncToken(robotID)
	if err != nil || !stillActive {
		return err
	}
	if latest != token {
		return ba.installIMToken(robotID, latest)
	}
	return nil
}

// syncAllBotTokens restores User Bot credentials after WuKongIM/server
// restarts. This lives in bot_api because binding ownership is authoritative
// here; BotFather must not independently restore a raw bearer token.
func (ba *BotAPI) syncAllBotTokens() {
	robots, err := ba.db.queryAllActiveRobots()
	if err != nil {
		ba.Error("同步 bot token 失败: 查询 robot 出错", zap.Error(err))
		return
	}
	successCount := 0
	for _, robot := range robots {
		if err := ba.syncRobotIMToken(robot.RobotID); err != nil {
			ba.Warn("同步 bot token 失败", zap.String("robotID", robot.RobotID), zap.Error(err))
			continue
		}
		successCount++
	}
	ba.Info("Bot token 启动同步完成", zap.Int("total", len(robots)), zap.Int("success", successCount))
}
