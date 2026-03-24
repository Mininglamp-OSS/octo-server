package webhook

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/network"
	"go.uber.org/zap"
)

// MIPush 小米推送
type MIPush struct {
	appID       string // 小米app id
	appSecret   string // 小米app secret
	packageName string // android包名
	channelID   string // 频道id 如果有则填写
	log.Log
}

// NewMIPush NewMIPush
func NewMIPush(appID string, appSecret string, packageName string, channelID string) *MIPush {
	return &MIPush{
		appID:       appID,
		appSecret:   appSecret,
		packageName: packageName,
		channelID:   channelID,
		Log:         log.NewTLog("MIPush"),
	}
}

// MIPayload 小米负载
type MIPayload struct {
	Payload
	notifyID string
	spaceID  string
}

// NewMIPayload NewMIPayload
func NewMIPayload(payloadInfo *PayloadInfo, notifyID string) *MIPayload {
	return &MIPayload{
		Payload:  payloadInfo.toPayload(),
		notifyID: notifyID,
		spaceID:  payloadInfo.SpaceID,
	}
}

// GetPayload 获取推送负载
func (m *MIPush) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	payloadInfo, err := ParsePushInfo(msg, ctx, toUser)
	if err != nil {
		return nil, err
	}
	return NewMIPayload(payloadInfo, fmt.Sprintf("%d", msg.MessageSeq)), nil
}

// Push 推送
func (m *MIPush) Push(deviceToken string, payload Payload) error {
	miPayload := payload.(*MIPayload)

	// 文档 https://dev.mi.com/console/doc/detail?pId=1163

	params := map[string]string{
		"registration_id":         deviceToken,
		"payload":                 url.QueryEscape(miPayload.GetContent()), //消息的内容。（注意：需要对payload字符串做urlencode处理）
		"restricted_package_name": m.packageName,
		"pass_through":            "0",
		"notify_type":             "-1",
		"title":                   miPayload.GetTitle(),
		"notify_id":               miPayload.notifyID,
		"description":             miPayload.GetContent(),
		"extra.sound_uri":         fmt.Sprintf("android.resource://%s/raw/newmsg", m.packageName),
		"extra.badge":             fmt.Sprintf("%d", payload.GetBadge()),
		"extra.notify_effect":     "1",
		"extra.channel_id":        m.channelID,
	}
	if miPayload.spaceID != "" {
		params["extra.space_id"] = miPayload.spaceID
	}

	result, err := network.PostForWWWForm("https://api.xmpush.xiaomi.com/v4/message/regid", params, map[string]string{
		"Authorization": fmt.Sprintf("key=%s", m.appSecret),
	})
	if err != nil {
		return err
	}
	m.Debug("返回", zap.Any("data", result))
	return checkMIPushResult(result)
}

// checkMIPushResult validates the MI push API response with safe type assertions
func checkMIPushResult(result map[string]interface{}) error {
	if result != nil {
		if resultStr, ok := result["result"].(string); ok && resultStr != "ok" {
			if reason, ok := result["reason"].(string); ok {
				return errors.New(reason)
			}
			if desc, ok := result["description"].(string); ok {
				return errors.New(desc)
			}
			return errors.New("MI push failed with unknown error")
		}
	}
	return nil
}
