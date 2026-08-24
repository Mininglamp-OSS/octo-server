package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/network"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

const (
	oppoAPIBaseURL             = "https://api-push-cn.heytapmobi.com"
	oppoAuthURL                = oppoAPIBaseURL + "/server/v1/auth"
	oppoNotificationUnicastURL = oppoAPIBaseURL + "/server/v1/message/notification/unicast"
)

// OPPO 推送
type OPPOPush struct {
	appID                string
	appKey               string
	appSecret            string
	masterSecret         string // 服务端密钥
	authTokenCachePrefix string
	log.Log
	ctx *config.Context
}

// NewOPPOPush NewOPPOPush
func NewOPPOPush(appID, appKey, appSecret, masterSecret string, ctx *config.Context) *OPPOPush {
	return &OPPOPush{
		appID:                appID,
		appKey:               appKey,
		appSecret:            appSecret,
		masterSecret:         masterSecret,
		authTokenCachePrefix: "oppo_auth_token",
		Log:                  log.NewTLog("oppopush"),
		ctx:                  ctx,
	}
}

// OPPOPayload oppo负载
type OPPOPayload struct {
	Payload
	notifyID string
}

// NewOPPOPayload NewOPPOPayload
func NewOPPOPayload(payloadInfo *PayloadInfo, notifyID string) *OPPOPayload {
	return &OPPOPayload{
		Payload:  payloadInfo.toPayload(),
		notifyID: notifyID,
	}
}

// GetPayload GetPayload
func (o *OPPOPush) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	payloadInfo, err := ParsePushInfo(msg, ctx, toUser)
	if err != nil {
		return nil, err
	}
	return NewOPPOPayload(payloadInfo, fmt.Sprintf("%d", msg.MessageSeq)), nil
}

// Push Push
func (o *OPPOPush) Push(deviceToken string, payload Payload) error {
	// 推送文档 https://open.oppomobile.com/new/developmentDoc/info?id=11238
	authToken := o.getAuthToken()
	oppoPayload := payload.(*OPPOPayload)
	message := map[string]interface{}{
		"target_type":  2,
		"target_value": deviceToken,
		"notification": map[string]string{
			"title":   oppoPayload.GetTitle(),
			"content": oppoPayload.GetContent(),
		},
	}
	dataType, _ := json.Marshal(message)
	dataString := string(dataType)
	resp, err := network.PostForWWWForm(oppoNotificationUnicastURL, map[string]string{
		"auth_token": authToken,
		"message":    dataString,
	}, nil)

	if err != nil {
		o.Warn("OPPO推送错误", zap.Error(err))
		return err
	}
	if resp != nil && resp["code"] != nil {
		if codeNum, ok := resp["code"].(json.Number); ok {
			code, _ := codeNum.Int64()
			if code != 0 {
				if msg, ok := resp["message"].(string); ok {
					return errors.New(msg)
				}
				return fmt.Errorf("OPPO push failed with code %d", code)
			}
		}
	}
	return nil
}

// getAuthToken 获取推送鉴权令牌
func (o *OPPOPush) getAuthToken() string {
	authToken, _ := o.ctx.GetRedisConn().GetString(o.authTokenCachePrefix)
	if authToken != "" {
		return authToken
	}
	timestamp := time.Now().Local().UnixNano() / 1e6
	sign := o.SHA256(fmt.Sprintf("%s%d%s", o.appKey, timestamp, o.masterSecret))
	resp, err := network.PostForWWWForm(oppoAuthURL, map[string]string{
		"app_key":   o.appKey,
		"sign":      sign,
		"timestamp": fmt.Sprintf("%d", timestamp),
	}, nil)
	if err != nil {
		o.Error("获取OPPO推送鉴权错误", zap.Error(err))
		return ""
	}

	if resp != nil && resp["code"] != nil {
		if codeNum, ok := resp["code"].(json.Number); ok {
			code, _ := codeNum.Int64()
			message, _ := resp["message"].(string)
			if code != 0 {
				o.Error("OPPO鉴权返回错误数据", zap.String("错误信息", message))
			} else {
				if data, ok := resp["data"].(map[string]interface{}); ok {
					if token, ok := data["auth_token"].(string); ok {
						authToken = token
					}
				}
			}
		}
	}
	if authToken != "" {
		_ = o.ctx.GetRedisConn().SetAndExpire(o.authTokenCachePrefix, authToken, time.Hour*20)
	}
	return authToken
}

func (o *OPPOPush) SHA256(password string) string {
	hash := sha256.New()
	hash.Write([]byte(password))
	return hex.EncodeToString(hash.Sum(nil))
}

// parseOPPOAuthResponse 解析 OPPO 认证响应，返回 authToken
// 如果响应格式错误或认证失败，返回空字符串和错误信息
func parseOPPOAuthResponse(resp map[string]interface{}) (string, error) {
	if resp == nil || resp["code"] == nil {
		return "", errors.New("OPPO auth: empty response")
	}
	codeNum, ok := resp["code"].(json.Number)
	if !ok {
		return "", fmt.Errorf("OPPO auth: unexpected code type %T", resp["code"])
	}
	code, _ := codeNum.Int64()
	if code != 0 {
		message, _ := resp["message"].(string)
		return "", fmt.Errorf("OPPO auth failed: code=%d, message=%s", code, message)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("OPPO auth: unexpected data type %T", resp["data"])
	}
	token, ok := data["auth_token"].(string)
	if !ok {
		return "", fmt.Errorf("OPPO auth: unexpected auth_token type %T", data["auth_token"])
	}
	return token, nil
}
