package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/network"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"go.uber.org/zap"
)

// HMSPayload 华为负载
type HMSPayload struct {
	Payload
	accessToken string
	spaceID     string
}

// NewHMSPayload NewHMSPayload
func NewHMSPayload(payloadInfo *PayloadInfo, accessToken string) *HMSPayload {
	return &HMSPayload{
		Payload:     payloadInfo.toPayload(),
		accessToken: accessToken,
		spaceID:     payloadInfo.SpaceID,
	}
}

// HMSPush 华为推送
type HMSPush struct {
	appID       string // 华为app id
	appSecret   string // 华为app secret
	packageName string // android包名
	log.Log
	hmsAccessTokenCachePrefix string
}

// NewHMSPush NewHMSPush
func NewHMSPush(appID string, appSecret string, packageName string) *HMSPush {
	return &HMSPush{
		appID:                     appID,
		appSecret:                 appSecret,
		packageName:               packageName,
		hmsAccessTokenCachePrefix: "hms_accesstoken",
		Log:                       log.NewTLog("HMSPush"),
	}
}

// GetPayload 获取推送负载
func (h *HMSPush) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	payloadInfo, err := ParsePushInfo(msg, ctx, toUser)
	if err != nil {
		log.Warn("推送失败！", zap.Error(err))
		return nil, err
	}
	accessToken, err := ctx.GetRedisConn().GetString(h.hmsAccessTokenCachePrefix)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(accessToken) == "" {
		var expire time.Duration
		accessToken, expire, err = h.GetHMSAccessToken()
		if err != nil {
			return nil, err
		}
		err = ctx.GetRedisConn().SetAndExpire(h.hmsAccessTokenCachePrefix, accessToken, expire)
		if err != nil {
			return nil, err
		}
	}
	return NewHMSPayload(payloadInfo, accessToken), nil
}

// Push 推送
func (h *HMSPush) Push(deviceToken string, payload Payload) error {
	hmsPayload := payload.(*HMSPayload)
	channelID := "wk_new_msg_notification"
	sound := "/raw/newmsg"
	category := "IM"
	if hmsPayload.GetRTCPayload() != nil && hmsPayload.GetRTCPayload().GetOperation() != "cancel" {
		channelID = "wk_new_rtc_notification"
		sound = "/raw/newrtc"
		category = "VOIP"
	}

	messagePayload := map[string]interface{}{
		"token": []string{deviceToken},
		"android": map[string]interface{}{
			"category": category,
			"notification": map[string]interface{}{
				"visibility":    "PUBLIC",
				"title":         payload.GetTitle(),
				"body":          payload.GetContent(),
				"sound":         sound,
				"importance":    "NORMAL",
				"default_sound": false,
				"channel_id":    channelID,
				"click_action": map[string]interface{}{
					"type": 3,
				},
				"badge": map[string]interface{}{
					"add_num": 1,
					"class":   fmt.Sprintf("%s%s", h.packageName, ".MainActivity"),
				},
			},
		},
	}
	if hmsPayload.spaceID != "" {
		messagePayload["data"] = util.ToJson(map[string]string{
			"space_id": hmsPayload.spaceID,
		})
	}

	resp, err := network.Post(fmt.Sprintf("https://push-api.cloud.huawei.com/v1/%s/messages:send", h.appID), []byte(util.ToJson(map[string]interface{}{
		"validate_only": false,
		"message":       messagePayload,
	})), map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", hmsPayload.accessToken),
	})

	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("华为推送返回错误！-> %s", resp.Body)
	}
	h.Debug("返回", zap.String("body", resp.Body))
	resultMap, err := util.JsonToMap(resp.Body)
	if err != nil {
		return err
	}
	if resultMap != nil && resultMap["code"] != nil {
		code, ok := resultMap["code"].(string)
		if !ok {
			return fmt.Errorf("HMS push: unexpected code type %T", resultMap["code"])
		}
		if code != "80000000" {
			if msg, ok := resultMap["msg"].(string); ok {
				return errors.New(msg)
			}
			return fmt.Errorf("HMS push failed with code %s", code)
		}
	}
	return nil
}

// parseHMSAuthResponse 解析华为认证响应，返回 accessToken 和过期时间
func parseHMSAuthResponse(resultMap map[string]interface{}) (string, time.Duration, error) {
	if resultMap == nil {
		return "", 0, errors.New("HMS auth: empty response")
	}
	accessToken, ok := resultMap["access_token"].(string)
	if !ok {
		return "", 0, fmt.Errorf("HMS auth: unexpected access_token type %T", resultMap["access_token"])
	}
	var expiresIn int64 = 3600 // default 1 hour
	if expiresInVal, ok := resultMap["expires_in"].(json.Number); ok {
		if parsed, err := expiresInVal.Int64(); err == nil && parsed > 0 {
			expiresIn = parsed
		}
	}
	return accessToken, time.Duration(expiresIn) * time.Second, nil
}

// GetHMSAccessToken 获取华为的访问Token
func (h *HMSPush) GetHMSAccessToken() (string, time.Duration, error) {

	resultMap, err := network.PostForWWWForm("https://oauth-login.cloud.huawei.com/oauth2/v2/token", map[string]string{
		"grant_type":    "client_credentials",
		"client_secret": h.appSecret,
		"client_id":     h.appID,
	}, nil)
	if err != nil {
		return "", 0, err
	}
	return parseHMSAuthResponse(resultMap)

}
