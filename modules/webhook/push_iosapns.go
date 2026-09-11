package webhook

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/certificate"
	"github.com/sideshow/apns2/token"
)

// IOSPayload iOS负载
type IOSPayload struct {
	Payload
	spaceID     string
	channelID   string
	channelType uint8
	messageSeq  uint32
	// silent 表示该接收者的「手机静音」已生效（判定见 resolveEffectiveAppMute）。
	// 仅作用于普通消息推送的声音，不影响横幅/角标，也不影响 RTC 来电。
	silent bool
}

// NewIOSPayload NewIOSPayload
func NewIOSPayload(payloadInfo *PayloadInfo) Payload {

	return &IOSPayload{
		Payload:     payloadInfo.toPayload(),
		spaceID:     payloadInfo.SpaceID,
		channelID:   payloadInfo.ChannelID,
		channelType: payloadInfo.ChannelType,
		messageSeq:  payloadInfo.MessageSeq,
	}
}

// applyRouting 将 space_id 与会话路由字段附加到 APNs payload 顶层，
// 供客户端点击通知时跳转到对应会话使用。
func (p *IOSPayload) applyRouting(data map[string]interface{}) {
	if p.spaceID != "" {
		data["space_id"] = p.spaceID
	}
	if p.channelID == "" {
		return
	}
	data["channel_id"] = p.channelID
	// ChannelType 0 不是合法枚举值（1=单聊 2=群聊 5=子区），视为未设置跳过。
	if p.channelType != 0 {
		data["channel_type"] = p.channelType
	}
	// channel_id 存在时 message_seq 一起下发，避免客户端拿不到定位序号。
	data["message_seq"] = p.messageSeq
}

// Silence 实现 silenceable：让本次 APNs 负载以无声形态下发。
func (p *IOSPayload) Silence() {
	p.silent = true
}

// IOSPush IOSPush
type IOSPush struct {
	client      *apns2.Client
	clientMu    sync.Mutex
	topic       string
	password    string
	p12FilePath string
	// p8 Token 认证字段
	p8FilePath string
	keyID      string
	teamID     string
	dev        bool // 是否是开发环境
	log.Log
}

// NewIOSPush NewIOSPush (p12 证书方式)
func NewIOSPush(topic string, dev bool, p12FilePath string, password string) *IOSPush {
	return &IOSPush{
		topic:       topic,
		dev:         dev,
		p12FilePath: p12FilePath,
		password:    password,
		Log:         log.NewTLog("IOSPush"),
	}
}

// NewIOSPushWithToken 使用 p8 Token 认证方式创建 IOSPush
func NewIOSPushWithToken(topic string, dev bool, p8FilePath, keyID, teamID string) *IOSPush {
	return &IOSPush{
		topic:      topic,
		dev:        dev,
		p8FilePath: p8FilePath,
		keyID:      keyID,
		teamID:     teamID,
		Log:        log.NewTLog("IOSPush"),
	}
}

func (p *IOSPush) createClient() (*apns2.Client, error) {
	var client *apns2.Client

	// 优先使用 p8 Token 认证
	if p.p8FilePath != "" && p.keyID != "" && p.teamID != "" {
		authKey, err := token.AuthKeyFromFile(p.p8FilePath)
		if err != nil {
			return nil, fmt.Errorf("failed to load p8 auth key: %w", err)
		}
		apnsToken := &token.Token{
			AuthKey: authKey,
			KeyID:   p.keyID,
			TeamID:  p.teamID,
		}
		if p.dev {
			client = apns2.NewTokenClient(apnsToken).Development()
		} else {
			client = apns2.NewTokenClient(apnsToken).Production()
		}
		return client, nil
	}

	// Fallback 到 p12 证书认证
	if p.p12FilePath == "" {
		return nil, errors.New("no APNs credentials configured (need p8 or p12)")
	}
	cert, err := certificate.FromP12File(p.p12FilePath, p.password)
	if err != nil {
		return nil, err
	}
	if p.dev {
		client = apns2.NewClient(cert).Development()
	} else {
		client = apns2.NewClient(cert).Production()
	}
	return client, nil
}

// loadAPNsP8Config 从环境变量加载 p8 配置
func loadAPNsP8Config() (p8Path, keyID, teamID string) {
	return os.Getenv("DM_PUSH_APNS_P8_PATH"),
		os.Getenv("DM_PUSH_APNS_KEY_ID"),
		os.Getenv("DM_PUSH_APNS_TEAM_ID")
}

// GetPayload 获取推送负载
func (p *IOSPush) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	pushInfo, err := ParsePushInfo(msg, ctx, toUser)
	if err != nil {
		return nil, err
	}
	return NewIOSPayload(pushInfo), nil
}

// Push iOS推送
func (p *IOSPush) Push(deviceToken string, payload Payload) error {
	notification := &apns2.Notification{}
	notification.DeviceToken = deviceToken
	notification.Topic = p.topic

	iosPayload := payload.(*IOSPayload)
	rtcPayload := payload.GetRTCPayload()
	if rtcPayload != nil {
		data := map[string]interface{}{
			"aps": map[string]interface{}{
				"content-available": 1,
				"alert":             "",
				"badge":             payload.GetBadge(),
				"sound":             "default",
			},
			"content":   payload.GetContent(),
			"call_type": rtcPayload.GetCallType(),
			"from_uid":  rtcPayload.GetFromUID(),
		}
		iosPayload.applyRouting(data)
		notification.Payload = []byte(util.ToJson(data))
	} else {
		aps := map[string]interface{}{
			"alert": map[string]interface{}{
				"title": payload.GetTitle(),
				"body":  payload.GetContent(),
			},
			"badge": payload.GetBadge(),
		}
		// 静音生效时**整键省略** sound，而不是写空字符串 —— 部分 iOS 版本会把
		// 空串当成「找不到该音频文件」从而回落默认音效，等于没静音。
		// 其余字段（alert/badge/路由字段）保持不变：静音只去声音，横幅与角标照常。
		if !iosPayload.silent {
			aps["sound"] = "default"
		}
		data := map[string]interface{}{
			"aps": aps,
		}
		iosPayload.applyRouting(data)
		notification.Payload = []byte(util.ToJson(data))
	}

	p.clientMu.Lock()
	if p.client == nil {
		client, err := p.createClient()
		if err != nil {
			p.clientMu.Unlock()
			return err
		}
		p.client = client
	}
	p.clientMu.Unlock()
	res, err := p.client.Push(notification)
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return errors.New(res.Reason)
	}
	return nil
}
