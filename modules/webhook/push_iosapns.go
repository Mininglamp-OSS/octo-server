package webhook

import (
	"errors"
	"fmt"
	"sync"

	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/certificate"
)

// IOSPayload iOS负载
type IOSPayload struct {
	Payload
	spaceID string
}

// NewIOSPayload NewIOSPayload
func NewIOSPayload(payloadInfo *PayloadInfo) Payload {

	return &IOSPayload{
		Payload: payloadInfo.toPayload(),
		spaceID: payloadInfo.SpaceID,
	}
}

// IOSPush IOSPush
type IOSPush struct {
	client    *apns2.Client
	clientMu  sync.Mutex
	topic       string
	password    string
	p12FilePath string
	dev         bool // 是否是开发环境
	log.Log
}

// NewIOSPush NewIOSPush
func NewIOSPush(topic string, dev bool, p12FilePath string, password string) *IOSPush {
	return &IOSPush{
		topic:       topic,
		dev:         dev,
		p12FilePath: p12FilePath,
		password:    password,
		Log:         log.NewTLog("IOSPush"),
	}
}

func (p *IOSPush) createClient() (*apns2.Client, error) {
	cert, err := certificate.FromP12File(p.p12FilePath, p.password)
	if err != nil {
		return nil, err
	}
	var client *apns2.Client
	if p.dev {
		client = apns2.NewClient(cert).Development()
	} else {
		client = apns2.NewClient(cert).Production()
	}
	return client, nil
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
		fmt.Println("音视频推送。。。。。")
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
		if iosPayload.spaceID != "" {
			data["space_id"] = iosPayload.spaceID
		}
		notification.Payload = []byte(util.ToJson(data))
	} else {
		fmt.Println("普通推送。。。。。")
		data := map[string]interface{}{
			"aps": map[string]interface{}{
				"alert": map[string]interface{}{
					"title": payload.GetTitle(),
					"body":  payload.GetContent(),
				},
				"badge": payload.GetBadge(),
				"sound": "default",
			},
		}
		if iosPayload.spaceID != "" {
			data["space_id"] = iosPayload.spaceID
		}
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
