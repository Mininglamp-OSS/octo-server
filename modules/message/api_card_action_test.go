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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
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

// cardEnvelopeJSON 构造 octo/v2 审批卡信封（含指定 Submit action id；body 声明
// Input.Text "comment" —— D11 之后 inputs 只放行声明过的 id）。
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
				map[string]interface{}{"type": "Input.Text", "id": "comment"},
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

	// D4 幂等(round-3 P1-1):去重键是业务身份 (message_id, action_id,
	// operator_uid) —— 换一个 client_token 重放(模拟 D8 超时后的客户端重试)
	// 仍是 replay=true,队列恰好 1 条,绝不产生第二个事件。
	body["client_token"] = "tok-e2e-2"
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(body))))
	req2.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	assert.Contains(t, w2.Body.String(), `"replay":true`)
	entries2, _ := rds.ZRange("robotEvent:"+cardTestBotUID, 0, -1).Result()
	assert.Len(t, entries2, 1)

	// D4 claim 已 confirm:键值 = event_id(排障关联),TTL 升格为 24h 窗口
	claimVal, err := rds.Get(fmt.Sprintf("cardaction:%s:%s:%s", "9001", "approve_btn", testutil.UID)).Result()
	assert.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%d", ev.EventID), claimVal)
	claimTTL, err := rds.TTL(fmt.Sprintf("cardaction:%s:%s:%s", "9001", "approve_btn", testutil.UID)).Result()
	assert.NoError(t, err)
	assert.Greater(t, claimTTL, time.Hour, "confirm 后应为 24h 级 TTL,而非 60s pending")
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

	// ⑤ D11 inputs 信任边界(round-3 P1-3):未声明键 / 非字符串值 → 400
	for i, badInputs := range []map[string]interface{}{
		{"undeclared": "x"},       // 生效帧(done_btn 新帧)只声明了 comment
		{"comment": float64(123)}, // 值必须是字符串(AC submit 线上语义)
	} {
		w = do(map[string]interface{}{
			"message_id": "9102", "channel_id": cardTestBotUID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"action_id":    "done_btn", "inputs": badInputs,
			"client_token": fmt.Sprintf("tok-t6-%d", i),
		})
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "Invalid card action.")
	}

	// ⑥ 跨频道 IDOR(round-3 P1-4):操作者是 A 群成员、消息在 B 群(非成员)。
	//    拿 A 的 channel_id 指 B 的 message_id → 频道绑定源自存储行,查不到即
	//    400;拿 B 的真实 channel_id → 成员资格对存储频道校验,403。两条路都
	//    不产生 bot 事件。person 频道变体:fake id 含操作者,天然指不到别人的会话。
	_, err = ctx.DB().InsertBySql("insert into group_member(group_no,uid) values(?,?)", "g_idor_a", testutil.UID).Exec()
	assert.NoError(t, err)
	seedCardMessage(t, ctx, 9104, cardTestBotUID, "g_idor_b", common.ChannelTypeGroup.Uint8(), cardEnvelopeJSON(t, "approve_btn"))
	idorBody := func(chID, tok string) map[string]interface{} {
		return map[string]interface{}{
			"message_id": "9104", "channel_id": chID,
			"channel_type": common.ChannelTypeGroup.Uint8(),
			"action_id":    "approve_btn", "client_token": tok,
		}
	}
	w = do(idorBody("g_idor_a", "tok-t7"))
	assert.Equal(t, http.StatusBadRequest, w.Code, "A 群 channel_id 指 B 群消息应 400")
	assert.Contains(t, w.Body.String(), "Invalid card action.")
	w = do(idorBody("g_idor_b", "tok-t8"))
	assert.Equal(t, http.StatusBadRequest, w.Code, "B 群非成员应 denied")
	assert.Contains(t, w.Body.String(), "You cannot act on this card.")
	w = do(map[string]interface{}{ // person 变体:声明与 bot 的会话,指群消息 id
		"message_id": "9104", "channel_id": cardTestBotUID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"action_id":    "approve_btn", "client_token": "tok-t9",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, "person fake 频道指群消息应 400")
	assert.Contains(t, w.Body.String(), "Invalid card action.")
	// 整个 ⑥ 未投递任何事件
	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	n, err := rds.ZCard("robotEvent:" + cardTestBotUID).Result()
	assert.NoError(t, err)
	assert.Equal(t, int64(1), n, "只应有 ③ done_btn 放行产生的那 1 条事件")
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

// flakyRobotService 包装真实 robot 服务,按开关注入 EnqueueBotTypedEvent 失败
// (D4 验收:入队失败必须释放幂等 claim,不得造成 24h 锁死)。
type flakyRobotService struct {
	robot.IService
	fail bool
}

func (f *flakyRobotService) EnqueueBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (int64, error) {
	if f.fail {
		return 0, errors.New("injected enqueue failure")
	}
	return f.IService.EnqueueBotTypedEvent(robotID, eventType, eventData)
}

// 验收(P2 D4, round-3 P1-1):入队失败 → 5xx 内部封套 + 补偿释放 claim,同一
// 操作者立即重试成功 —— 半途而废的请求不锁死动作。
//
// 与 TestUserCardSendRejected 同理由使用包内 newTestServer + New(ctx):需要拿到
// Message 实例注入 flaky robotService(testutil 路由绑定的是 sync.Once 缓存的
// 全局实例,不可注入)。表由同二进制内先跑的 testutil 迁移建出。
func TestCardActionEnqueueFailureReleasesClaim(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := newTestServer()
	m := New(ctx)
	flaky := &flakyRobotService{IService: m.robotService, fail: true}
	m.robotService = flaky
	m.Route(s.GetRoute())
	resetCardUIDRateLimit(t, ctx)
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const failBot = "bot_card_fail"
	seedCardBot(t, ctx, failBot)
	fake := common.GetFakeChannelIDWith(uid, failBot)
	seedCardMessage(t, ctx, 9301, failBot, fake, common.ChannelTypePerson.Uint8(), cardEnvelopeJSON(t, "approve_btn"))

	body := map[string]interface{}{
		"message_id": "9301", "channel_id": failBot,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"action_id":    "approve_btn", "client_token": "tok-fail-1",
	}
	do := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// ①入队失败:内部错误封套(D14 线上仍 400),未 accepted
	w := do()
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), `"accepted":true`)

	// ②claim 已补偿释放(键不存在 —— 不是 24h 锁死,也不是 60s pending 残留)
	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	claimKey := fmt.Sprintf("cardaction:%s:%s:%s", "9301", "approve_btn", uid)
	exists, err := rds.Exists(claimKey).Result()
	assert.NoError(t, err)
	assert.Equal(t, int64(0), exists, "入队失败后 claim 应被释放")

	// ③恢复入队 → 同一 client_token 立即重试成功,事件恰好 1 条
	flaky.fail = false
	w = do()
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"replay":false`)
	entries, err := rds.ZRange("robotEvent:"+failBot, 0, -1).Result()
	assert.NoError(t, err)
	assert.Len(t, entries, 1)
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
