package bot_api

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/botutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// BotRegisterReq is the optional request body for register.
type BotRegisterReq struct {
	AgentPlatform string `json:"agent_platform"`
	AgentVersion  string `json:"agent_version"`
	PluginVersion string `json:"plugin_version"`
}

// BotRegisterResp is the response for bot registration.
type BotRegisterResp struct {
	RobotID        string `json:"robot_id"`
	Name           string `json:"name"`
	IMToken        string `json:"im_token"`
	WSURL          string `json:"ws_url"`
	APIURL         string `json:"api_url"`
	OwnerUID       string `json:"owner_uid"`
	OwnerChannelID string `json:"owner_channel_id"`
}

// register handles POST /v1/bot/register for both User Bot and App Bot.
func (ba *BotAPI) register(c *wkhttp.Context) {
	token := extractBotToken(c)
	if token == "" {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthFailed, nil, nil)
		return
	}

	if strings.HasPrefix(token, "app_") {
		ba.registerAppBot(c, token)
	} else {
		ba.registerUserBot(c, token)
	}
}

// registerUserBot handles registration for User Bot (bf_ token).
func (ba *BotAPI) registerUserBot(c *wkhttp.Context, token string) {
	robot, err := ba.db.queryRobotByBotToken(token)
	if err != nil {
		ba.Error("查询机器人失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthCheckFailed, nil, nil)
		return
	}
	if robot == nil {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthFailed, nil, nil)
		return
	}

	// Use bot_token as im_token — single token design
	imToken := robot.BotToken
	resp, tokenErr := ba.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         robot.RobotID,
		Token:       imToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if tokenErr != nil || resp.Status != config.UpdateTokenStatusSuccess {
		ba.Error("获取IM Token失败", zap.Any("error", tokenErr), zap.String("robotID", robot.RobotID), zap.Any("status", resp))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIIMTokenFailed, nil, nil)
		return
	}
	if robot.IMTokenCache != imToken {
		ba.db.updateRobotIMTokenCache(robot.RobotID, imToken)
	}

	// Optional: parse agent version info
	var req BotRegisterReq
	_ = c.ShouldBindJSON(&req)
	if req.AgentPlatform != "" || req.AgentVersion != "" || req.PluginVersion != "" {
		merged := struct{ platform, version, plugin string }{
			platform: req.AgentPlatform,
			version:  req.AgentVersion,
			plugin:   req.PluginVersion,
		}
		if merged.platform == "" {
			merged.platform = robot.AgentPlatform
		}
		if merged.version == "" {
			merged.version = robot.AgentVersion
		}
		if merged.plugin == "" {
			merged.plugin = robot.PluginVersion
		}
		if robot.AgentPlatform != merged.platform ||
			robot.AgentVersion != merged.version ||
			robot.PluginVersion != merged.plugin {
			if updateErr := ba.db.updateRobotAgentInfo(robot.RobotID, merged.platform, merged.version, merged.plugin); updateErr != nil {
				ba.Warn("更新Agent信息失败", zap.Error(updateErr), zap.String("robotID", robot.RobotID))
			}
		}
	}

	cfg := ba.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}
	wsURL := botutil.DeriveWSURL(cfg)

	botName := ""
	if u, _ := ba.userService.GetUser(robot.RobotID); u != nil {
		botName = u.Name
	}

	c.Response(&BotRegisterResp{
		RobotID:        robot.RobotID,
		Name:           botName,
		IMToken:        imToken,
		WSURL:          wsURL,
		APIURL:         apiURL,
		OwnerUID:       robot.CreatorUID,
		OwnerChannelID: robot.CreatorUID,
	})
}

// registerAppBot handles registration for App Bot (app_ token).
func (ba *BotAPI) registerAppBot(c *wkhttp.Context, token string) {
	appBot, err := ba.db.queryAppBotByToken(token)
	if err != nil {
		ba.Error("查询App Bot失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthCheckFailed, nil, nil)
		return
	}
	if appBot == nil {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthFailed, nil, nil)
		return
	}

	// Only published App Bots can register
	if appBot.Status != 1 {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIAuthFailed, nil, nil)
		return
	}

	// Design: App Bot uses the same token for API auth and IM WebSocket connection.
	// This is intentional — simpler than managing two tokens, and the caller already
	// possesses the token (used it to authenticate this request). Token rotation via
	// the admin API invalidates both simultaneously. Tradeoff acknowledged:
	// intercepting the WS connection reveals the API credential.
	imToken := appBot.Token
	resp, tokenErr := ba.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         appBot.UID,
		Token:       imToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if tokenErr != nil || resp.Status != config.UpdateTokenStatusSuccess {
		ba.Error("App Bot IM Token注册失败", zap.Any("error", tokenErr), zap.String("uid", appBot.UID), zap.Any("status", resp))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIIMTokenFailed, nil, nil)
		return
	}

	cfg := ba.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}
	wsURL := botutil.DeriveWSURL(cfg)

	c.Response(&BotRegisterResp{
		RobotID:        appBot.UID,
		Name:           appBot.DisplayName,
		IMToken:        imToken,
		WSURL:          wsURL,
		APIURL:         apiURL,
		OwnerUID:       appBot.CreatedBy,
		OwnerChannelID: appBot.CreatedBy,
	})
}
