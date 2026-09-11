package webhook

import (
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

// 这一组覆盖 pushTo 的**装配**：判定结果如何跨 PushPool 边界到达负载。
// 三层各自的单测（resolveEffectiveAppMute / push→Silence / IOSPayload→无 sound）
// 都可能全绿而装配是断的 —— job Data 的 key 拼错、传错标记、或 RTC 豁免被删，
// 在这一层之外都看不出来。

// capturingPusher 记录最终交给厂商 SDK 的负载是否被静音。
type capturingPusher struct {
	done    chan struct{}
	payload *capturedPayload
}

func newCapturingPusher() *capturingPusher {
	return &capturingPusher{done: make(chan struct{}, 4)}
}

func (c *capturingPusher) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	c.payload = &capturedPayload{Payload: &BasePayload{}}
	return c.payload, nil
}

func (c *capturingPusher) Push(deviceToken string, payload Payload) error {
	c.done <- struct{}{}
	return nil
}

// waitPushed 等待 PushPool 上的异步任务真正把推送发出去。
func (c *capturingPusher) waitPushed(t *testing.T) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("超时：推送任务未在 PushPool 上完成")
	}
}

type capturedPayload struct {
	Payload
	silenced bool
}

func (p *capturedPayload) Silence() { p.silenced = true }

// seedMuteRecipient 造一个开启了「手机静音」的接收者，并按需给他一个桌面端在线会话。
func seedMuteRecipient(t *testing.T, ctx *config.Context, desktopOnline bool) string {
	t.Helper()
	uid := "u_" + util.GenerUUID()[:12]

	_, err := ctx.DB().InsertInto("user").
		Columns("uid", "name", "mute_of_app", "new_msg_notice").
		Values(uid, "静音用户", 1, 1).Exec()
	assert.NoError(t, err)

	if desktopOnline {
		_, err = ctx.DB().InsertInto("user_online").
			Columns("uid", "device_flag", "online").
			Values(uid, config.PC.Uint8(), 1).Exec()
		assert.NoError(t, err)
	}

	key := fmt.Sprintf("%s%s", common.UserDeviceTokenPrefix, uid)
	assert.NoError(t, ctx.GetRedisConn().Hmset(key,
		"device_token", "tok_"+uid,
		"device_type", string(common.DeviceTypeIOS),
		"bundle_id", "com.test.app"))
	t.Cleanup(func() { _ = ctx.GetRedisConn().Del(key) }) // CleanAllTables 不清 Redis

	return uid
}

func newWebhookWithPusher(ctx *config.Context, pusher Push) *Webhook {
	w := New(ctx)
	w.pushMap[common.DeviceTypeIOS] = map[string]Push{"com.test.app": pusher}
	return w
}

func textMessage(toUID string) msgOfflineNotify {
	msg := msgOfflineNotify{}
	msg.FromUID = "u_sender"
	msg.ToUID = toUID
	msg.ChannelID = "u_sender"
	msg.ChannelType = common.ChannelTypePerson.Uint8()
	msg.Payload = []byte(`{"type":1,"content":"hi"}`)
	return msg
}

// 开启静音 + 桌面端在线 → 负载必须一路带着静音标记抵达厂商 SDK。
func TestPushTo_MutedWithDesktopSession_DeliversSilentPayload(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	uid := seedMuteRecipient(t, ctx, true)
	pusher := newCapturingPusher()

	assert.NoError(t, newWebhookWithPusher(ctx, pusher).pushTo(textMessage(uid), []string{uid}))
	pusher.waitPushed(t)

	assert.True(t, pusher.payload.silenced,
		"mute_of_app=1 且桌面端在线时，静音标记必须跨 PushPool 抵达负载")
}

// 开启静音但桌面端不在线（Web 已退出，DB 残留 1）→ 必须照常有声。
func TestPushTo_MutedWithoutDesktopSession_DeliversAudiblePayload(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	uid := seedMuteRecipient(t, ctx, false)
	pusher := newCapturingPusher()

	assert.NoError(t, newWebhookWithPusher(ctx, pusher).pushTo(textMessage(uid), []string{uid}))
	pusher.waitPushed(t)

	assert.False(t, pusher.payload.silenced,
		"桌面端不在线时，残留的 mute_of_app 不得让推送静音")
}

// RTC 来电穿透静音。真正的保证是 pushTo 里的 `if !isVideoCall`（isVideoCall 由
// payload 的 cmd 解析得到），而不是负载层的 RTC 分支 —— 后者的 PayloadInfo.IsVideoCall
// 在生产代码里从未被赋值。删掉那行豁免，本用例必须变红。
func TestPushTo_RTCPiercesMute(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	uid := seedMuteRecipient(t, ctx, true) // 静音生效的最强前提
	pusher := newCapturingPusher()

	msg := textMessage(uid)
	msg.Payload = []byte(`{"type":1,"cmd":"room.invoke"}`)

	assert.NoError(t, newWebhookWithPusher(ctx, pusher).pushTo(msg, []string{uid}))
	pusher.waitPushed(t)

	assert.False(t, pusher.payload.silenced,
		"RTC 来电必须穿透「手机静音」，与 allowPush 对 isVideoCall 的豁免一致")
}
