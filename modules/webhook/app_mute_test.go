package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/sideshow/apns2"
	"github.com/stretchr/testify/assert"
)

// captureAPNsPayload 把负载真正发一遍，抓取落到 HTTP body 上的原始字节。
// apns2 对 []byte 负载原样作为请求体发送（notification.go MarshalJSON），
// 因此这里断言的就是线上发给 APNs 的实际内容，而不是重新拼装的近似值。
func captureAPNsPayload(t *testing.T, p Payload) ([]byte, map[string]interface{}) {
	t.Helper()
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	push := &IOSPush{topic: "com.mininglamp.octo"}
	// 预置 client 以跳过证书加载；其余路径与线上一致。
	push.client = &apns2.Client{Host: srv.URL, HTTPClient: srv.Client()}

	if err := push.Push("a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4", p); err != nil {
		t.Fatalf("Push 失败: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("负载不是合法 JSON: %v (raw=%s)", err, raw)
	}
	return raw, decoded
}

func apsOf(t *testing.T, payload map[string]interface{}) map[string]interface{} {
	t.Helper()
	aps, ok := payload["aps"].(map[string]interface{})
	if !ok {
		t.Fatalf("负载缺少 aps 段: %v", payload)
	}
	return aps
}

func newTestPayloadInfo() *PayloadInfo {
	return &PayloadInfo{
		Title:       "产品讨论组",
		Content:     "张三: 明天的评审推迟到下午",
		Badge:       3,
		SpaceID:     "sp_1",
		ChannelID:   "g_1001",
		ChannelType: 2,
		MessageSeq:  42,
	}
}

// 未开启静音：保持改动前的行为，sound 仍为 default。
func TestIOSPayload_AudibleKeepsDefaultSound(t *testing.T) {
	_, decoded := captureAPNsPayload(t, NewIOSPayload(newTestPayloadInfo()))
	aps := apsOf(t, decoded)
	if got := aps["sound"]; got != "default" {
		t.Fatalf("未静音时应为 sound=default，实际 %v", got)
	}
}

// 静音生效：sound 键必须**整键消失**。
// 写空字符串是不可接受的 —— 部分 iOS 版本会把空串当成「找不到音频文件」而回落
// 默认音效，等于没静音。
func TestIOSPayload_SilentOmitsSoundKeyEntirely(t *testing.T) {
	p := NewIOSPayload(newTestPayloadInfo())
	p.(silenceable).Silence()

	_, decoded := captureAPNsPayload(t, p)
	aps := apsOf(t, decoded)

	if v, present := aps["sound"]; present {
		t.Fatalf("静音时 sound 键必须不存在，实际存在且为 %#v", v)
	}
}

// 静音只去声音：其余字段必须与有声负载逐字段一致。
func TestIOSPayload_SilentChangesNothingButSound(t *testing.T) {
	_, audible := captureAPNsPayload(t, NewIOSPayload(newTestPayloadInfo()))

	muted := NewIOSPayload(newTestPayloadInfo())
	muted.(silenceable).Silence()
	_, silent := captureAPNsPayload(t, muted)

	// 顶层路由字段必须完全一致
	for _, k := range []string{"space_id", "channel_id", "channel_type", "message_seq"} {
		if !jsonEqual(audible[k], silent[k]) {
			t.Errorf("顶层字段 %s 发生变化: 有声=%v 静音=%v", k, audible[k], silent[k])
		}
	}
	audibleAps, silentAps := apsOf(t, audible), apsOf(t, silent)
	for _, k := range []string{"alert", "badge"} {
		if !jsonEqual(audibleAps[k], silentAps[k]) {
			t.Errorf("aps.%s 发生变化: 有声=%v 静音=%v", k, audibleAps[k], silentAps[k])
		}
	}
	// 差异必须恰好是 sound 一个键
	if len(audibleAps) != len(silentAps)+1 {
		t.Errorf("aps 键数差异不止 sound: 有声=%v 静音=%v", audibleAps, silentAps)
	}
}

// RTC 来电穿透静音：与 allowPush 对 isVideoCall 的豁免保持一致。
func TestIOSPayload_RTCAlwaysAudible(t *testing.T) {
	info := newTestPayloadInfo()
	info.IsVideoCall = true
	info.FromUID = "u_888"
	info.Operation = "invoke"

	p := NewIOSPayload(info)
	if s, ok := p.(silenceable); ok {
		s.Silence() // 即便被标记静音
	}
	_, decoded := captureAPNsPayload(t, p)
	if got := apsOf(t, decoded)["sound"]; got != "default" {
		t.Fatalf("RTC 推送应始终有声，实际 sound=%v", got)
	}
}

func jsonEqual(a, b interface{}) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// --- resolveEffectiveAppMute ---

type stubOnlineChecker struct {
	online map[config.DeviceFlag]bool
	err    error
	calls  int
}

func (s *stubOnlineChecker) DeviceOnline(uid string, device config.DeviceFlag) (bool, error) {
	s.calls++
	if s.err != nil {
		return false, s.err
	}
	return s.online[device], nil
}

func mutedUser(uid string) *user.Resp  { return &user.Resp{UID: uid, MuteOfApp: 1} }
func normalUser(uid string) *user.Resp { return &user.Resp{UID: uid, MuteOfApp: 0} }

func TestResolveEffectiveAppMute(t *testing.T) {
	tests := []struct {
		name      string
		users     []*user.Resp
		checker   *stubOnlineChecker
		wantMuted []string
		wantCalls int
	}{
		{
			name:      "未开启静音：不查在线，直接有声",
			users:     []*user.Resp{normalUser("u1"), normalUser("u2")},
			checker:   &stubOnlineChecker{},
			wantMuted: nil,
			wantCalls: 0, // 关键：避免为未静音用户产生 N+1 查询
		},
		{
			name:      "静音 + PC 在线：生效",
			users:     []*user.Resp{mutedUser("u1")},
			checker:   &stubOnlineChecker{online: map[config.DeviceFlag]bool{config.PC: true}},
			wantMuted: []string{"u1"},
			wantCalls: 1, // 命中 PC 后不再查 Web
		},
		{
			name:      "静音 + 仅 Web 在线：生效",
			users:     []*user.Resp{mutedUser("u1")},
			checker:   &stubOnlineChecker{online: map[config.DeviceFlag]bool{config.Web: true}},
			wantMuted: []string{"u1"},
			wantCalls: 2,
		},
		{
			// 回归核心：Web 退出后 DB 里残留 mute_of_app=1，绝不能变成永久静音。
			name:      "静音但 PC/Web 均不在线：残留值失效，照常有声",
			users:     []*user.Resp{mutedUser("u1")},
			checker:   &stubOnlineChecker{online: map[config.DeviceFlag]bool{}},
			wantMuted: nil,
			wantCalls: 2,
		},
		{
			name:      "在线查询失败：fail-open 有声",
			users:     []*user.Resp{mutedUser("u1")},
			checker:   &stubOnlineChecker{err: errors.New("db down")},
			wantMuted: nil,
			wantCalls: 1,
		},
		{
			name:      "混合批次：只为静音用户查询",
			users:     []*user.Resp{normalUser("u1"), mutedUser("u2"), normalUser("u3")},
			checker:   &stubOnlineChecker{online: map[config.DeviceFlag]bool{config.PC: true}},
			wantMuted: []string{"u2"},
			wantCalls: 1,
		},
		{
			name:      "含 nil 用户不 panic",
			users:     []*user.Resp{nil, mutedUser("u1")},
			checker:   &stubOnlineChecker{online: map[config.DeviceFlag]bool{config.PC: true}},
			wantMuted: []string{"u1"},
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveEffectiveAppMute(tt.users, tt.checker, nil)
			if len(got) != len(tt.wantMuted) {
				t.Fatalf("静音用户数不符: 期望 %v, 实际 %v", tt.wantMuted, got)
			}
			for _, uid := range tt.wantMuted {
				if !got[uid] {
					t.Errorf("期望 %s 静音生效，实际未生效", uid)
				}
			}
			if tt.checker.calls != tt.wantCalls {
				t.Errorf("在线查询次数不符: 期望 %d, 实际 %d", tt.wantCalls, tt.checker.calls)
			}
		})
	}
}

// checker 缺失时不得 panic，且按有声处理。
func TestResolveEffectiveAppMute_NilChecker(t *testing.T) {
	if got := resolveEffectiveAppMute([]*user.Resp{mutedUser("u1")}, nil, nil); len(got) != 0 {
		t.Fatalf("checker 为 nil 时应无人静音，实际 %v", got)
	}
}

// 查询失败必须可观测：logger 被调用且带上 uid 与原始错误。
func TestResolveEffectiveAppMute_LogsLookupFailure(t *testing.T) {
	wantErr := errors.New("db down")
	var gotUID string
	var gotErr error
	resolveEffectiveAppMute(
		[]*user.Resp{mutedUser("u1")},
		&stubOnlineChecker{err: wantErr},
		func(uid string, err error) { gotUID, gotErr = uid, err },
	)
	if gotUID != "u1" || !errors.Is(gotErr, wantErr) {
		t.Fatalf("期望记录 uid=u1 与原始错误，实际 uid=%q err=%v", gotUID, gotErr)
	}
}

// --- push() 的静音传递（端到端连接点）---

// fakePusher 记录负载是否在 Push 前被要求静音。
type fakePusher struct {
	payload *fakeSilenceablePayload
}

func (f *fakePusher) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	f.payload = &fakeSilenceablePayload{Payload: &BasePayload{}}
	return f.payload, nil
}

func (f *fakePusher) Push(deviceToken string, payload Payload) error { return nil }

type fakeSilenceablePayload struct {
	Payload
	silenced bool
}

func (p *fakeSilenceablePayload) Silence() { p.silenced = true }

// 不支持静音的厂商负载：不实现 silenceable。
type fakePlainPusher struct{ pushed bool }

func (f *fakePlainPusher) GetPayload(msg msgOfflineNotify, ctx *config.Context, toUser *user.Resp) (Payload, error) {
	return &BasePayload{}, nil
}
func (f *fakePlainPusher) Push(deviceToken string, payload Payload) error {
	f.pushed = true
	return nil
}

// push() 必须把静音标记真正施加到负载上 —— 这是判定逻辑与 payload 之间的连接点，
// 断掉的话前两组测试仍全绿，但线上依旧响铃。
func TestPush_AppliesSilenceToPayload(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const uid = "u_mute_apply"
	err := ctx.GetRedisConn().Hmset(fmt.Sprintf("%s%s", common.UserDeviceTokenPrefix, uid),
		"device_token", "tok_1",
		"device_type", string(common.DeviceTypeIOS),
		"bundle_id", "com.test.app")
	assert.NoError(t, err)

	pusher := &fakePusher{}
	w := &Webhook{
		ctx:     ctx,
		Log:     log.NewTLog("Webhook-test"),
		pushMap: map[common.DeviceType]map[string]Push{common.DeviceTypeIOS: {"com.test.app": pusher}},
	}

	// silent=true → 负载必须被静音
	_, err = w.push(&user.Resp{UID: uid}, msgOfflineNotify{}, true)
	assert.NoError(t, err)
	assert.True(t, pusher.payload.silenced, "silent=true 时必须调用 Silence()")

	// silent=false → 负载保持有声
	_, err = w.push(&user.Resp{UID: uid}, msgOfflineNotify{}, false)
	assert.NoError(t, err)
	assert.False(t, pusher.payload.silenced, "silent=false 时不得调用 Silence()")
}

// 不支持静音的厂商：静音标记被安全忽略，推送照常发出（不得报错或丢弃）。
func TestPush_UnsupportedVendorStillPushes(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const uid = "u_mute_plain"
	err := ctx.GetRedisConn().Hmset(fmt.Sprintf("%s%s", common.UserDeviceTokenPrefix, uid),
		"device_token", "tok_2",
		"device_type", string(common.DeviceTypeHMS),
		"bundle_id", "com.test.app")
	assert.NoError(t, err)

	pusher := &fakePlainPusher{}
	w := &Webhook{
		ctx:     ctx,
		Log:     log.NewTLog("Webhook-test"),
		pushMap: map[common.DeviceType]map[string]Push{common.DeviceTypeHMS: {"com.test.app": pusher}},
	}

	_, err = w.push(&user.Resp{UID: uid}, msgOfflineNotify{}, true)
	assert.NoError(t, err)
	assert.True(t, pusher.pushed, "不支持静音的厂商仍必须正常推送")
}
