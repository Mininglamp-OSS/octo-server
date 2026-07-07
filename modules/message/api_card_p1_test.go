package message

// card-message-protocol P1 集成测试（用户 ingress / 编辑路径 / 置顶文案）。
// spec: .octospec/tasks/card-message-protocol/brief.md；执行 brief:
// .octospec/tasks/card-message-p1-display/brief.md。
// 编辑路径用例需要 WuKongIM(:5001)——messageEdit 的属主校验先经
// IMGetWithChannelAndSeqs 查真实消息；缺席时 t.Skip 不破坏无 IM 环境。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
)

const cardTestHumanUID = "20001" // 非 bot 对端

// resetCardUIDRateLimit 清共享 UID 限流桶（模式同 modules/category api_test.go
// 的 resetUIDRateLimit —— 桶在 Redis 中跨测试存活，CleanAllTables 不清）。
func resetCardUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	if keys, err := rds.Keys("ratelimit:uid:*").Result(); err == nil && len(keys) > 0 {
		_ = rds.Del(keys...).Err()
	}
}

// cardEnvelopeJSON 构造合法 octo/v1 展示卡信封（P1 白名单：TextBlock + OpenUrl）。
func cardEnvelopeJSON(t *testing.T) []byte {
	t.Helper()
	env := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV1,
		"plain":        "client-forged plain",
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "审批单 #42"},
			},
			"actions": []interface{}{
				map[string]interface{}{"type": "Action.OpenUrl", "title": "查看", "url": "https://example.com/42"},
			},
		},
	}
	raw, err := json.Marshal(env)
	assert.NoError(t, err)
	return raw
}

// P1 Decision 2 layer (a)：用户 ingress 拒卡(经 /v1/message/send 代发口)。
//
// ⚠️ 不用 testutil.NewTestServer:register.GetModules 以 sync.Once 缓存模块
// 闭包,handler 绑定的是「进程内第一个测试」的 ctx —— 运行时改本测试 ctx 的
// SendMessageOn 对 handler 不可见。这里沿用包内旧式 newTestServer() 手动
// New(ctx)+Route,让 handler 与测试共享同一 ctx,config 开关可控。
// 该口不触 DB(拒绝发生在派发前),无需迁移建表。
func TestUserCardSendRejected(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	// 先经 testutil 跑一次迁移(friend 表等)——不依赖包内其它测试的运行顺序;
	// 其 route/ctx 不使用(sync.Once 陷阱见上)。
	_, migCtx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(migCtx) }()
	s, ctx := newTestServer()
	ctx.GetConfig().Message.SendMessageOn = true
	m := New(ctx)
	m.Route(s.GetRoute())
	resetCardUIDRateLimit(t, ctx)

	// person 频道的好友前置检查在卡片门禁之前 —— 种双向好友关系,让请求
	// 走到 Decision-2a 的 type-17 拒绝(表由同二进制内先跑的 testutil 迁移建出)。
	for _, pair := range [][2]string{{uid, cardTestHumanUID}, {cardTestHumanUID, uid}} {
		_, err := ctx.DB().InsertBySql(
			"insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, err)
	}

	var payload map[string]interface{}
	assert.NoError(t, json.Unmarshal(cardEnvelopeJSON(t), &payload))
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/message/send", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"token":                testutil.Token,
		"receive_channel_id":   cardTestHumanUID,
		"receive_channel_type": common.ChannelTypePerson.Uint8(),
		"payload":              payload,
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card messages can only be sent by bots or webhooks.")
}

// skipWithoutIM 探测 WuKongIM(:5001)健康端点,不可达则跳过。
func skipWithoutIM(t *testing.T) {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:5001/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skip("WuKongIM 未运行(需 :5001,见 CI 环境脚本),跳过 IM 集成用例")
	}
	_ = resp.Body.Close()
}

// waitIMMessageSeq 轮询 IMSearchMessages 直到消息完成异步持久化拿到 seq
// （官方 WuKongIM v2 的 send 响应只带 message_id,seq 由异步持久化后置分配）。
func waitIMMessageSeq(t *testing.T, ctx *config.Context, channelID string, channelType uint8, loginUID string, messageID int64) uint32 {
	t.Helper()
	for i := 0; i < 20; i++ {
		resp, err := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   channelID,
			ChannelType: channelType,
			MessageIds:  []int64{messageID},
			LoginUID:    loginUID,
		})
		if err == nil && resp != nil && len(resp.Messages) > 0 && resp.Messages[0].MessageSeq > 0 {
			return resp.Messages[0].MessageSeq
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("消息 %d 在 IM 中未完成持久化(拿不到 message_seq)", messageID)
	return 0
}

// TestUserCardEditRejectedIM:P1 Decision 7 —— 用户编辑路径对 type-17
// content_edit 一律拒绝(该路径对卡片永久关闭)。经真实 IM 链路:
// SendMessageWithResult 发真消息 → messageEdit 经 IM 属主校验 → 卡片门禁 400;
// 对照组:同一条消息的普通文本编辑放行(证明 IM 链路通、拒绝确因卡片门禁)。
func TestUserCardEditRejectedIM(t *testing.T) {
	skipWithoutIM(t)
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardUIDRateLimit(t, ctx)

	sendResp, err := ctx.SendMessageWithResult(&config.MsgSendReq{
		Header:      config.MsgHeader{RedDot: 1},
		ChannelID:   cardTestHumanUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		FromUID:     testutil.UID,
		Payload:     []byte(`{"type":1,"content":"hello card edit"}`),
	})
	assert.NoError(t, err)
	assert.NotNil(t, sendResp)
	assert.NotZero(t, sendResp.MessageID)
	seq := waitIMMessageSeq(t, ctx, cardTestHumanUID, common.ChannelTypePerson.Uint8(), testutil.UID, sendResp.MessageID)

	edit := func(contentEdit string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/message/edit", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"message_id":   fmt.Sprintf("%d", sendResp.MessageID),
			"message_seq":  seq,
			"channel_id":   cardTestHumanUID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"content_edit": contentEdit,
		}))))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// 卡片编辑体 → Decision 7 拒绝
	w := edit(string(cardEnvelopeJSON(t)))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card messages cannot be edited.")

	// 对照:普通文本编辑放行(IM 属主校验链路真实走通)
	w2 := edit("hello card edit (v2)")
	assert.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
}

// 验收(finding #3):置顶等「按内容类型描述消息」文案面经本地 helper,
// type-17 显示 [卡片] 而非「未知消息类型」。
func TestDisplayContentTypeText(t *testing.T) {
	if got := displayContentTypeText(cardmsg.InteractiveCard.Int()); got != cardmsg.PlaceholderCard {
		t.Errorf("type-17 置顶文案=%q want %q", got, cardmsg.PlaceholderCard)
	}
	// 其余类型透传 octo-lib（行为不变）
	if got := displayContentTypeText(common.Image.Int()); got != common.GetDisplayText(common.Image.Int()) {
		t.Errorf("非卡片类型应透传 GetDisplayText, got %q", got)
	}
}
