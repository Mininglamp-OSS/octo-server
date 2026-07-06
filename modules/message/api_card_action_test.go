package message

// card-message-interaction P2 集成测试（MySQL + Redis，无需 WuKongIM ——
// card/action 全链路不触碰 IM；这正是选型时"复用既有轨道"的直接收益）。
//
// 覆盖 brief 验收项：happy path（冻结 event_data 形状）、D4 幂等 replay、
// D3 信任模型（非 bot sender / 未知 action_id / 生效帧 fail-closed / 群成员
// 403）、rollout flag、用户 send ingress 拒卡（P1 Decision 2a）。
//
// 已知无法在无 WuKongIM 环境覆盖：用户/bot 编辑路径的卡片拒绝与解锁
// （messageEdit / botMessageEdit 在卡片门禁前先经 IMGetWithChannelAndSeqs
// 做属主校验）—— 留给带 IM 的 CI 环境。

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

const (
	cardTestBotUID   = "bot_card_1"
	cardTestHumanUID = "20001" // 非 bot 发送者
)

// resetCardUIDRateLimit 清共享 UID 限流桶（模式同 modules/category api_test.go
// 的 resetUIDRateLimit —— 桶在 Redis 中跨测试存活，CleanAllTables 不清）。
func resetCardUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	for _, pattern := range []string{"ratelimit:uid:*", "cardaction:*", "robotEvent:*", common.RobotEventSeqKey + "*"} {
		if keys, err := rds.Keys(pattern).Result(); err == nil && len(keys) > 0 {
			_ = rds.Del(keys...).Err()
		}
	}
}

// cardEnvelopeJSON 构造 octo/v2 审批卡信封（含指定 Submit action id）。
func cardEnvelopeJSON(t *testing.T, actionIDs ...string) []byte {
	t.Helper()
	actions := make([]interface{}, 0, len(actionIDs))
	for _, id := range actionIDs {
		actions = append(actions, map[string]interface{}{
			"type": "Action.Submit", "id": id, "title": id,
		})
	}
	env := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"plain":        "审批单 #42",
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "审批单 #42"},
			},
			"actions": actions,
		},
	}
	raw, err := json.Marshal(env)
	assert.NoError(t, err)
	return raw
}

func seedCardBot(t *testing.T, ctx *config.Context, robotID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql("insert into robot(robot_id,status) values(?,1)", robotID).Exec()
	assert.NoError(t, err)
}

func seedCardMessage(t *testing.T, ctx *config.Context, messageID int64, fromUID, channelID string, channelType uint8, payload []byte) {
	t.Helper()
	d := NewDB(ctx)
	err := d.insertMessage(&messageModel{
		MessageID:   messageID,
		MessageSeq:  1,
		ClientMsgNo: fmt.Sprintf("cmn-%d", messageID),
		FromUID:     fromUID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Timestamp:   time.Now().Unix(),
		Payload:     payload,
	})
	assert.NoError(t, err)
}

// 注:所有错误断言用「HTTP 400 + body 含错误码 ID」——httperr.ResponseErrorL
// 按 D14 契约把线上状态钉在 400,真实语义状态在 error.http_status(403 类的
// denied 码同样以 400 出线)。

func TestCardActionEndToEndAndIdempotency(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardUIDRateLimit(t, ctx)

	seedCardBot(t, ctx, cardTestBotUID)
	fake := common.GetFakeChannelIDWith(testutil.UID, cardTestBotUID)
	seedCardMessage(t, ctx, 9001, cardTestBotUID, fake, common.ChannelTypePerson.Uint8(), cardEnvelopeJSON(t, "approve_btn", "reject_btn"))

	body := map[string]interface{}{
		"message_id":   "9001",
		"channel_id":   cardTestBotUID, // person 频道:对端 = bot
		"channel_type": common.ChannelTypePerson.Uint8(),
		"action_id":    "approve_btn",
		"inputs":       map[string]interface{}{"comment": "LGTM"},
		"client_token": "tok-e2e-1",
	}
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(body))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"accepted":true`)
	assert.Contains(t, w.Body.String(), `"replay":false`)

	// 冻结形状断言:bot 事件队列恰好 1 条 card_action,event_data 字段齐全
	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	entries, err := rds.ZRange("robotEvent:"+cardTestBotUID, 0, -1).Result()
	assert.NoError(t, err)
	assert.Len(t, entries, 1)
	var ev struct {
		EventID   int64                  `json:"event_id"`
		EventType string                 `json:"event_type"`
		EventData map[string]interface{} `json:"event_data"`
		Expire    int64                  `json:"expire"`
	}
	assert.NoError(t, json.Unmarshal([]byte(entries[0]), &ev))
	assert.Equal(t, cardmsg.EventTypeCardAction, ev.EventType)
	assert.Greater(t, ev.EventID, int64(0))
	assert.Greater(t, ev.Expire, time.Now().Unix())
	assert.Equal(t, "9001", ev.EventData["message_id"])
	assert.Equal(t, cardTestBotUID, ev.EventData["channel_id"])
	assert.Equal(t, float64(common.ChannelTypePerson.Uint8()), ev.EventData["channel_type"])
	assert.Equal(t, "approve_btn", ev.EventData["action_id"])
	assert.Equal(t, testutil.UID, ev.EventData["operator_uid"])
	assert.Equal(t, "tok-e2e-1", ev.EventData["client_token"])
	assert.Equal(t, map[string]interface{}{"comment": "LGTM"}, ev.EventData["inputs"])
	assert.NotNil(t, ev.EventData["acted_at"])

	// D4 幂等:同键重放 → replay=true,队列仍恰好 1 条(绝不产生第二个事件)
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(body))))
	req2.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	assert.Contains(t, w2.Body.String(), `"replay":true`)
	entries2, _ := rds.ZRange("robotEvent:"+cardTestBotUID, 0, -1).Result()
	assert.Len(t, entries2, 1)
}

func TestCardActionTrustModel(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardUIDRateLimit(t, ctx)

	seedCardBot(t, ctx, cardTestBotUID)

	do := func(body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	baseBody := func(msgID, peer, actionID, tok string) map[string]interface{} {
		return map[string]interface{}{
			"message_id": msgID, "channel_id": peer,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"action_id":    actionID, "client_token": tok,
		}
	}

	// ① sender 非 bot(layer-c fail-closed:iwh_/人类发送者同路径)
	fakeHuman := common.GetFakeChannelIDWith(testutil.UID, cardTestHumanUID)
	seedCardMessage(t, ctx, 9101, cardTestHumanUID, fakeHuman, common.ChannelTypePerson.Uint8(), cardEnvelopeJSON(t, "approve_btn"))
	w := do(baseBody("9101", cardTestHumanUID, "approve_btn", "tok-t1"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid card action.")

	// ② action_id 不存在于卡片(防伪造)
	fakeBot := common.GetFakeChannelIDWith(testutil.UID, cardTestBotUID)
	seedCardMessage(t, ctx, 9102, cardTestBotUID, fakeBot, common.ChannelTypePerson.Uint8(), cardEnvelopeJSON(t, "approve_btn"))
	w = do(baseBody("9102", cardTestBotUID, "forged_btn", "tok-t2"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid card action.")

	// ③ D3 生效帧 fail-closed:content_edit 重写为只含 done_btn 的新帧后,
	//    旧帧按钮 approve_btn 迟到点击 → 400;新帧 done_btn → 放行
	newFrame := cardEnvelopeJSON(t, "done_btn")
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version) VALUES (?,?,?,?,?,?,?,?)",
		"9102", 1, fakeBot, common.ChannelTypePerson.Uint8(), string(newFrame), util.MD5(string(newFrame)), int(time.Now().Unix()), 1,
	).Exec()
	assert.NoError(t, err)
	w = do(baseBody("9102", cardTestBotUID, "approve_btn", "tok-t3"))
	assert.Equal(t, http.StatusBadRequest, w.Code, "旧帧按钮应 fail-closed")
	w = do(baseBody("9102", cardTestBotUID, "done_btn", "tok-t4"))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// ④ 群频道非成员 → 403 denied(群无成员记录)
	seedCardMessage(t, ctx, 9103, cardTestBotUID, "g_card_test", common.ChannelTypeGroup.Uint8(), cardEnvelopeJSON(t, "approve_btn"))
	w = do(map[string]interface{}{
		"message_id": "9103", "channel_id": "g_card_test",
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"action_id":    "approve_btn", "client_token": "tok-t5",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "You cannot act on this card.")
}

func TestCardActionDisabledByFlag(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "") // rollout gate 默认关闭(fail-closed)
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardUIDRateLimit(t, ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"message_id": "1", "channel_id": "x", "channel_type": 1,
		"action_id": "a", "client_token": "t",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid card action.")
}

// P1 Decision 2 layer (a):用户 ingress 拒卡(经 /v1/message/send 代发口)。
//
// ⚠️ 不用 testutil.NewTestServer:register.GetModules 以 sync.Once 缓存模块
// 闭包,handler 绑定的是「进程内第一个测试」的 ctx —— 运行时改本测试 ctx 的
// SendMessageOn 对 handler 不可见。这里沿用包内旧式 newTestServer() 手动
// New(ctx)+Route,让 handler 与测试共享同一 ctx,config 开关可控。
// 该口不触 DB(拒绝发生在派发前),无需迁移建表。
func TestUserCardSendRejected(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
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
	assert.NoError(t, json.Unmarshal(cardEnvelopeJSON(t, "approve_btn"), &payload))
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
