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

func jsonEqual(a, b interface{}) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// --- resolveEffectiveAppMute ---

// stubLookup 记录批量在线查询的调用情况。
type stubLookup struct {
	online    map[string]bool
	err       error
	calls     int
	lastBatch []string
}

func (s *stubLookup) fn(uids []string) (map[string]bool, error) {
	s.calls++
	s.lastBatch = append([]string(nil), uids...)
	if s.err != nil {
		return nil, s.err
	}
	return s.online, nil
}

func mutedUser(uid string) *user.Resp  { return &user.Resp{UID: uid, MuteOfApp: 1} }
func normalUser(uid string) *user.Resp { return &user.Resp{UID: uid, MuteOfApp: 0} }

func TestResolveEffectiveAppMute(t *testing.T) {
	tests := []struct {
		name      string
		users     []*user.Resp
		lookup    *stubLookup
		wantMuted []string
		wantCalls int
		wantBatch []string
	}{
		{
			name:      "未开启静音：完全不查库",
			users:     []*user.Resp{normalUser("u1"), normalUser("u2")},
			lookup:    &stubLookup{},
			wantMuted: nil,
			wantCalls: 0,
		},
		{
			name:      "静音 + 桌面端在线：生效",
			users:     []*user.Resp{mutedUser("u1")},
			lookup:    &stubLookup{online: map[string]bool{"u1": true}},
			wantMuted: []string{"u1"},
			wantCalls: 1,
		},
		{
			// 回归核心：Web 退出后 DB 里残留 mute_of_app=1，绝不能变成永久静音。
			name:      "静音但桌面端不在线：残留值失效，照常有声",
			users:     []*user.Resp{mutedUser("u1")},
			lookup:    &stubLookup{online: map[string]bool{}},
			wantMuted: nil,
			wantCalls: 1,
		},
		{
			name:      "查询失败：整批 fail-open 有声",
			users:     []*user.Resp{mutedUser("u1"), mutedUser("u2")},
			lookup:    &stubLookup{err: errors.New("db down")},
			wantMuted: nil,
			wantCalls: 1,
		},
		{
			// 无论多少接收者，都只查一次，且只把静音候选人放进查询。
			name:      "混合批次：单次查询，且只带静音候选",
			users:     []*user.Resp{normalUser("u1"), mutedUser("u2"), normalUser("u3"), mutedUser("u4")},
			lookup:    &stubLookup{online: map[string]bool{"u2": true}},
			wantMuted: []string{"u2"},
			wantCalls: 1,
			wantBatch: []string{"u2", "u4"},
		},
		{
			name:      "含 nil 用户不 panic",
			users:     []*user.Resp{nil, mutedUser("u1")},
			lookup:    &stubLookup{online: map[string]bool{"u1": true}},
			wantMuted: []string{"u1"},
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveEffectiveAppMute(tt.users, tt.lookup.fn, nil)
			assert.Equal(t, len(tt.wantMuted), len(got), "静音用户数不符")
			for _, uid := range tt.wantMuted {
				assert.True(t, got[uid], "期望 %s 静音生效", uid)
			}
			assert.Equal(t, tt.wantCalls, tt.lookup.calls, "在线查询次数不符")
			if tt.wantBatch != nil {
				assert.Equal(t, tt.wantBatch, tt.lookup.lastBatch, "查询批次内容不符")
			}
		})
	}
}

// 一千个静音接收者也只能产生一次查询 —— 这是 N+1 的回归闸门。
func TestResolveEffectiveAppMute_SingleQueryForLargeBatch(t *testing.T) {
	users := make([]*user.Resp, 0, 1000)
	for i := 0; i < 1000; i++ {
		users = append(users, mutedUser(fmt.Sprintf("u%d", i)))
	}
	lookup := &stubLookup{online: map[string]bool{}}
	resolveEffectiveAppMute(users, lookup.fn, nil)
	assert.Equal(t, 1, lookup.calls, "整批必须只查一次，不得逐用户往返")
	assert.Len(t, lookup.lastBatch, 1000, "一次查询应覆盖全部静音候选")
}

// lookup 缺失时不得 panic，且按有声处理。
func TestResolveEffectiveAppMute_NilLookup(t *testing.T) {
	assert.Empty(t, resolveEffectiveAppMute([]*user.Resp{mutedUser("u1")}, nil, nil))
}

// 查询失败必须可观测，且整批只记一条（避免广播时刷屏）。
func TestResolveEffectiveAppMute_LogsOncePerBatch(t *testing.T) {
	wantErr := errors.New("db down")
	var calls, gotCount int
	var gotErr error
	users := []*user.Resp{mutedUser("u1"), mutedUser("u2"), mutedUser("u3")}
	resolveEffectiveAppMute(users, (&stubLookup{err: wantErr}).fn, func(n int, err error) {
		calls++
		gotCount, gotErr = n, err
	})
	assert.Equal(t, 1, calls, "整批只能记一条错误日志")
	assert.Equal(t, 3, gotCount, "日志应带上受影响的静音人数")
	assert.ErrorIs(t, gotErr, wantErr)
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
	// CleanAllTables 只清 MySQL，不清 Redis —— 设备令牌必须显式删除，
	// 否则会残留给后续（或乱序执行的）用例，导致不可复现的失败。
	defer func() { _ = ctx.GetRedisConn().Del(fmt.Sprintf("%s%s", common.UserDeviceTokenPrefix, uid)) }()
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
	defer func() { _ = ctx.GetRedisConn().Del(fmt.Sprintf("%s%s", common.UserDeviceTokenPrefix, uid)) }()
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
