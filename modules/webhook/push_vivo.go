package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/network"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"go.uber.org/zap"
)

// VIVO 推送
type VIVOPush struct {
	appID                string
	appKey               string
	appSecret            string
	authTokenCachePrefix string
	log.Log
	ctx *config.Context
}

// NewVIVOPush NewVIVOPush
func NewVIVOPush(appID, appKey, appSecret string, ctx *config.Context) *VIVOPush {
	return &VIVOPush{
		appID:                appID,
		appKey:               appKey,
		appSecret:            appSecret,
		authTokenCachePrefix: "vivo_auth_token",
		Log:                  log.NewTLog("vivopush"),
		ctx:                  ctx,
	}
}

// VIVOPayload VIVO负载
type VIVOPayload struct {
	Payload
	notifyID string
	spaceID  string
}

// NewVIVOPayload NewVIVOPayload
func NewVIVOPayload(payloadInfo *PayloadInfo, notifyID string) *VIVOPayload {
	return &VIVOPayload{
		Payload:  payloadInfo.toPayload(),
		notifyID: notifyID,
		spaceID:  payloadInfo.SpaceID,
	}
}

// GetPayload GetPayload
func (v *VIVOPush) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	payloadInfo, err := ParsePushInfo(msg, ctx, toUser)
	if err != nil {
		return nil, err
	}
	return NewVIVOPayload(payloadInfo, fmt.Sprintf("%d", msg.MessageSeq)), nil
}

// Push Push
func (v *VIVOPush) Push(deviceToken string, payload Payload) error {
	// 推送文档 https://dev.vivo.com.cn/documentCenter/doc/362
	authToken := v.getAuthToken()
	vivoPayload := payload.(*VIVOPayload)

	pushData := map[string]interface{}{
		"regId":          deviceToken,
		"notifyType":     "4",
		"title":          vivoPayload.GetTitle(),
		"content":        vivoPayload.GetContent(),
		"skipType":       "1",
		"classification": "1",
		"pushMode":       "1",
		"requestId":      util.GenerUUID(),
	}
	if vivoPayload.spaceID != "" {
		pushData["clientCustomMap"] = map[string]string{
			"space_id": vivoPayload.spaceID,
		}
	}

	resp, err := network.Post("https://api-push.vivo.com.cn/message/send", []byte(util.ToJson(pushData)), map[string]string{
		"authToken": authToken,
	})

	if err != nil {
		println("推送错误")
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vivo推送返回错误！-> %s", resp.Body)
	}

	resultMap, err := util.JsonToMap(resp.Body)
	if err != nil {
		return fmt.Errorf("解析vivo推送返回错误！-> %s", resp.Body)
	}

	if resultMap != nil && resultMap["result"] != nil {
		if num, ok := resultMap["result"].(json.Number); ok {
			code, _ := num.Int64()
			if code != 0 {
				if desc, ok := resultMap["desc"].(string); ok {
					return errors.New(desc)
				}
				return fmt.Errorf("VIVO push failed with code %d", code)
			}
		}
	}
	return nil
}

// getAuthToken 获取推送鉴权令牌
func (v *VIVOPush) getAuthToken() string {
	authToken, _ := v.ctx.GetRedisConn().GetString(v.authTokenCachePrefix)
	if authToken != "" {
		return authToken
	}
	timestamp := time.Now().Local().UnixNano() / 1e6
	sign := util.MD5(fmt.Sprintf("%s%s%d%s", v.appID, v.appKey, timestamp, v.appSecret))
	resp, err := network.Post("https://api-push.vivo.com.cn/message/auth", []byte(util.ToJson(map[string]interface{}{
		"appId":     v.appID,
		"appKey":    v.appKey,
		"sign":      sign,
		"timestamp": fmt.Sprintf("%d", timestamp),
	})), nil)
	if err != nil {
		v.Error("获取VIVO推送鉴权错误", zap.Error(err))
		return ""
	}
	if resp.StatusCode != http.StatusOK {
		return authToken
	}

	resultMap, err := util.JsonToMap(resp.Body)
	if err != nil {
		return authToken
	}
	if resultMap != nil && resultMap["result"] != nil {
		if num, ok := resultMap["result"].(json.Number); ok {
			code, _ := num.Int64()
			message, _ := resultMap["desc"].(string)
			if code != 0 {
				v.Error("VIVO鉴权返回错误数据", zap.String("错误信息", message))
			} else {
				if token, ok := resultMap["authToken"].(string); ok {
					authToken = token
				}
			}
		}
	}
	if authToken != "" {
		_ = v.ctx.GetRedisConn().SetAndExpire(v.authTokenCachePrefix, authToken, time.Hour*20)
	}
	return authToken
}
