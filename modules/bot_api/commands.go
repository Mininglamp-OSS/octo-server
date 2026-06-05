package bot_api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// setCommands handles POST /v1/bot/setCommands.
func (ba *BotAPI) setCommands(c *wkhttp.Context) {
	var req struct {
		Commands []struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		} `json:"commands"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "")
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
		ba.Error("序列化命令列表失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}

	err = ba.db.updateBotCommands(robotID, string(commandsJSON))
	if err != nil {
		ba.Error("保存命令列表失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIStoreFailed, nil, nil)
		return
	}

	c.ResponseOK()
}

// spaceUIDPattern matches space-prefixed UIDs: s{digits}_{baseUID}
var spaceUIDPattern = regexp.MustCompile(`^s\d+_(.+)$`)

// stripSpacePrefix extracts the base UID from a space-prefixed UID.
func stripSpacePrefix(uid string) string {
	if m := spaceUIDPattern.FindStringSubmatch(uid); len(m) == 2 {
		return m[1]
	}
	return uid
}

// getUserInfo handles GET /v1/bot/user/info?uid=xxx.
func (ba *BotAPI) getUserInfo(c *wkhttp.Context) {
	uid := strings.TrimSpace(c.Query("uid"))
	if uid == "" {
		respondBotAPIRequestInvalid(c, "uid")
		return
	}

	bareUID := stripSpacePrefix(uid)

	userResp, err := ba.userService.GetUser(bareUID)
	if err != nil {
		ba.Error("query user info failed", zap.Error(err), zap.String("uid", bareUID))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	if userResp == nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIUserNotFound, nil, nil)
		return
	}

	cfg := ba.ctx.GetConfig()
	apiURL := cfg.External.BaseURL
	if strings.TrimSpace(apiURL) == "" {
		apiURL = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}

	c.JSON(http.StatusOK, gin.H{
		"uid":    userResp.UID,
		"name":   userResp.Name,
		"avatar": fmt.Sprintf("%s/users/%s/avatar", apiURL, userResp.UID),
	})
}
