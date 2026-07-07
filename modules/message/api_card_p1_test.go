package message

// card-message-protocol P1 集成测试（用户 ingress / 编辑路径 / 置顶文案）。
// spec: .octospec/tasks/card-message-protocol/brief.md；执行 brief:
// .octospec/tasks/card-message-p1-display/brief.md。
// 用户 send / edit 拒卡门均置于频道/好友/属主等 DB·IM 前置检查之前（拒卡是与
// 收件人/归属无关的绝对策略），故本文件用例不触 IM/DB，也不经 testutil.NewTestServer
// —— 避免受 modules/message 包在 -shuffle 下的迁移账本脆弱性（issue #17）牵连。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
//
// 拒卡门现置于频道成员/好友前置检查之前(api.go sendMsg),故本测试完全不触库:
// 不种好友、不需迁移建表,也就不调 testutil.NewTestServer —— 这既反映了「拒卡是
// 与收件人无关的绝对策略」的语义,也让本测试不再受 modules/message 包在 -shuffle
// 下的迁移账本脆弱性(issue #17)牵连(PR#543 review:原先经 testutil 建 friend 表
// 会在坏 seed 下 panic 于 group_member 重复建表)。
func TestUserCardSendRejected(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := newTestServer()
	ctx.GetConfig().Message.SendMessageOn = true
	m := New(ctx)
	m.Route(s.GetRoute())
	resetCardUIDRateLimit(t, ctx)

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

// TestUserCardEditRejected: P1 Decision 7 —— 用户编辑路径对 type-17 content_edit
// 一律拒绝(该路径对卡片永久关闭,与 rollout flag 无关)。拒卡门现置于 IM 属主校验
// 之前(api.go messageEdit),是与消息归属无关的绝对策略,故本测试不触 IM/DB:
// bare newTestServer + New(ctx)+Route,POST 卡片 content_edit → 400 CardEditForbidden。
// 不经 testutil.NewTestServer,从而不受 modules/message 包在 -shuffle 下的迁移账本
// 脆弱性(issue #17)牵连(PR#543 review:原 IM 版在 CI 坏 seed 下 panic 于
// group_member 重复建表)。
func TestUserCardEditRejected(t *testing.T) {
	s, ctx := newTestServer()
	m := New(ctx)
	m.Route(s.GetRoute())
	resetCardUIDRateLimit(t, ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/message/edit", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"message_id":   "1",
		"message_seq":  1,
		"channel_id":   cardTestHumanUID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"content_edit": string(cardEnvelopeJSON(t)),
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card messages cannot be edited.")
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
