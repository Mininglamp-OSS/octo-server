//go:build integration

// 构建约束（PR#548 CI 修复）：本文件经 testutil.NewTestServer 跑 sql-migrate，必须带
// integration tag —— modules/message 默认构建混有「手建最小表」的单元测试
// （channel_files_blacklist_test.go 等），跑迁移的用例与之在 -race -shuffle 下并存会撞
// Error 1050 Table already exists（约定见 thread_ext_blacklist_filter_test.go：包内
// NewTestServer 用例一律 tag/skip；本包 e2e 13+ 文件皆 //go:build integration，
// api_card_p1_test.go 则改为不触 NewTestServer）。带 -tags integration 运行；bot 侧
// D6/D9 CAS 由 modules/bot_api 的 IM 用例在 CI 覆盖。
package message

// card-message-interaction P2 集成测试（MySQL + Redis，无需 WuKongIM ——
// card/action 全链路不触碰 IM；这正是选型时"复用既有轨道"的直接收益）。
// spec: .octospec/tasks/card-message-interaction/brief.md；执行 brief:
// .octospec/tasks/card-message-p2-action-loop/brief.md。
//
// 覆盖 brief 验收项：happy path（冻结 event_data 形状，含 D11 server-extracted
// data）、D4 幂等 replay + 入队失败释放 claim、D3 信任模型（非 bot sender /
// 未知 action_id / 生效帧 fail-closed / 群成员 / 跨频道 IDOR）、D11 inputs、
// rollout flag。
//
// 无法在无 WuKongIM 环境覆盖的 bot 编辑路径（D6/D9 CAS）由 bot_api 的
// IM-gated 用例承接。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	_ "github.com/Mininglamp-OSS/octo-server/modules/app_bot"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cardActionBotUID = "bot_card_1"

// resetCardActionState 清 P2 用例跨测试存活的 Redis 状态：共享 UID 限流桶、D4
// 幂等 claim、bot 事件队列与其 seq（CleanAllTables 不清 Redis）。
func resetCardActionState(t *testing.T, ctx *config.Context) {
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

// cardV2EnvelopeJSON 构造 octo/v2 审批卡信封（含指定 Submit action id；body 声明
// Input.Text "comment" —— D11 之后 inputs 只放行声明过的 id）。approve_btn 带静态
// data，用于验证 D11 server-extracted event_data.data。
func cardV2EnvelopeJSON(t *testing.T, actionIDs ...string) []byte {
	t.Helper()
	actions := make([]interface{}, 0, len(actionIDs))
	for _, id := range actionIDs {
		act := map[string]interface{}{"type": "Action.Submit", "id": id, "title": id}
		if id == "approve_btn" {
			act["data"] = map[string]interface{}{"action": "approve", "record_id": float64(42)}
		}
		actions = append(actions, act)
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

func seedCardAppBot(t *testing.T, ctx *config.Context, id, uid, token string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(`
		INSERT INTO app_bot(id,uid,display_name,scope,space_id,status,token,created_by)
		VALUES(?,?,?,'platform','',?,?,?)`, id, uid, uid, status, token, "owner").Exec()
	require.NoError(t, err)
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

// 注:错误断言用「HTTP 400 + body 含 DefaultMessage」——testutil.NewTestServer 不
// 装 i18n renderer，body 走 legacy {msg,status} 封套携带英文 DefaultMessage；且
// httperr.ResponseErrorL 按 D14 把线上状态钉 400（denied 的真实 403 在
// error.http_status）。

func TestCardActionEndToEndAndIdempotency(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)

	seedCardBot(t, ctx, cardActionBotUID)
	fake := common.GetFakeChannelIDWith(testutil.UID, cardActionBotUID)
	seedCardMessage(t, ctx, 9001, cardActionBotUID, fake, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn", "reject_btn"))

	body := map[string]interface{}{
		"message_id":   "9001",
		"channel_id":   cardActionBotUID, // person 频道:对端 = bot
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

	// 冻结形状断言:bot 事件队列恰好 1 条 card_action,event_data 字段齐全（含
	// D11 server-extracted data）。
	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	entries, err := rds.ZRange("robotEvent:"+cardActionBotUID, 0, -1).Result()
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
	assert.Equal(t, cardActionBotUID, ev.EventData["channel_id"])
	assert.Equal(t, float64(common.ChannelTypePerson.Uint8()), ev.EventData["channel_type"])
	assert.Equal(t, "approve_btn", ev.EventData["action_id"])
	assert.Equal(t, testutil.UID, ev.EventData["operator_uid"])
	assert.Equal(t, "tok-e2e-1", ev.EventData["client_token"])
	assert.Equal(t, map[string]interface{}{"comment": "LGTM"}, ev.EventData["inputs"])
	assert.NotNil(t, ev.EventData["acted_at"])
	// D11：data 是服务端从生效帧提取的作者静态对象（不可伪造）。
	assert.Equal(t, map[string]interface{}{"action": "approve", "record_id": float64(42)}, ev.EventData["data"])

	// P2-b 线冻结（PR#548 review）：event_data 键集是对 bot 的稳定线契约(additive-only)。
	// 全量比对(键数 + 每个键都在白名单)——任何漏加断言的新键 / 改名 / 删键都在此暴露,
	// 而非等到 bot 端解析出错。新增字段须同步更新 bot 契约文档与此白名单。
	frozenEventDataKeys := map[string]bool{
		"message_id": true, "channel_id": true, "channel_type": true,
		"action_id": true, "operator_uid": true, "client_token": true,
		"inputs": true, "acted_at": true, "data": true,
	}
	assert.Equal(t, len(frozenEventDataKeys), len(ev.EventData), "event_data 键数变更需同步 bot 线契约与本冻结")
	for k := range ev.EventData {
		assert.Truef(t, frozenEventDataKeys[k], "event_data 出现未冻结的新键: %s", k)
	}

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
	entries2, _ := rds.ZRange("robotEvent:"+cardActionBotUID, 0, -1).Result()
	assert.Len(t, entries2, 1)

	// D4 claim 已 confirm:键值 = event_id(排障关联),TTL 升格为 24h 窗口
	claimKey := fmt.Sprintf("cardaction:%s:%s:%s", "9001", "approve_btn", testutil.UID)
	claimVal, err := rds.Get(claimKey).Result()
	assert.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%d", ev.EventID), claimVal)
	claimTTL, err := rds.TTL(claimKey).Result()
	assert.NoError(t, err)
	assert.Greater(t, claimTTL, time.Hour, "confirm 后应为 24h 级 TTL,而非 60s pending")
}

func TestCardActionAppBotEventPollAndAckLifecycle(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)

	const (
		appID    = "card_action_app"
		appUID   = "card_action_app_bot"
		appToken = "app_card_action_lifecycle_token"
	)
	seedCardAppBot(t, ctx, appID, appUID, appToken, 1)
	fake := common.GetFakeChannelIDWith(testutil.UID, appUID)
	seedCardMessage(t, ctx, 9051, appUID, fake, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))

	actionBody := map[string]interface{}{
		"message_id":   "9051",
		"channel_id":   appUID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"action_id":    "approve_btn",
		"inputs":       map[string]interface{}{"comment": "ship it"},
		"client_token": "app-action-1",
	}
	postAction := func() *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req, err := http.NewRequest(http.MethodPost, "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(actionBody))))
		require.NoError(t, err)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	poll := func() (*httptest.ResponseRecorder, struct {
		Status  int `json:"status"`
		Results []struct {
			EventID   int64                  `json:"event_id"`
			EventType string                 `json:"event_type"`
			EventData map[string]interface{} `json:"event_data"`
		} `json:"results"`
	}) {
		t.Helper()
		w := httptest.NewRecorder()
		req, err := http.NewRequest(http.MethodPost, "/v1/bot/events", strings.NewReader(`{"event_id":0,"limit":20}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+appToken)
		s.GetRoute().ServeHTTP(w, req)
		var body struct {
			Status  int `json:"status"`
			Results []struct {
				EventID   int64                  `json:"event_id"`
				EventType string                 `json:"event_type"`
				EventData map[string]interface{} `json:"event_data"`
			} `json:"results"`
		}
		if w.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		}
		return w, body
	}

	// Live authorization: unpublish must block a first attempt immediately and
	// must not enqueue or claim the action.
	_, err := ctx.DB().Exec("UPDATE app_bot SET status=2 WHERE uid=?", appUID)
	require.NoError(t, err)
	w := postAction()
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid card action.")
	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	n, err := rds.ZCard("robotEvent:" + appUID).Result()
	require.NoError(t, err)
	assert.Zero(t, n)

	// Re-publishing permits the same previously unclaimed action.
	_, err = ctx.DB().Exec("UPDATE app_bot SET status=1 WHERE uid=?", appUID)
	require.NoError(t, err)
	w = postAction()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"accepted":true`)
	assert.Contains(t, w.Body.String(), `"replay":false`)
	n, err = rds.ZCard("robotEvent:" + appUID).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "exactly one App Bot event must be queued")

	pollW, events := poll()
	require.Equal(t, http.StatusOK, pollW.Code, pollW.Body.String())
	require.Equal(t, 1, events.Status)
	require.Len(t, events.Results, 1)
	event := events.Results[0]
	assert.Equal(t, cardmsg.EventTypeCardAction, event.EventType)
	assert.Equal(t, "9051", event.EventData["message_id"])
	assert.Equal(t, appUID, event.EventData["channel_id"])
	assert.Equal(t, testutil.UID, event.EventData["operator_uid"])

	ackW := httptest.NewRecorder()
	ackReq, err := http.NewRequest(http.MethodPost, fmt.Sprintf("/v1/bot/events/%d/ack", event.EventID), nil)
	require.NoError(t, err)
	ackReq.Header.Set("Authorization", "Bearer "+appToken)
	s.GetRoute().ServeHTTP(ackW, ackReq)
	require.Equal(t, http.StatusOK, ackW.Code, ackW.Body.String())

	pollW, events = poll()
	require.Equal(t, http.StatusOK, pollW.Code, pollW.Body.String())
	assert.Empty(t, events.Results, "ACKed App Bot event must not be returned again")
}

func TestCardActionTrustModel(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)

	seedCardBot(t, ctx, cardActionBotUID)

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
	seedCardMessage(t, ctx, 9101, cardTestHumanUID, fakeHuman, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	w := do(baseBody("9101", cardTestHumanUID, "approve_btn", "tok-t1"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid card action.")

	// ② action_id 不存在于卡片(防伪造)
	fakeBot := common.GetFakeChannelIDWith(testutil.UID, cardActionBotUID)
	seedCardMessage(t, ctx, 9102, cardActionBotUID, fakeBot, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	w = do(baseBody("9102", cardActionBotUID, "forged_btn", "tok-t2"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid card action.")

	// ③ D3 生效帧 fail-closed:content_edit 重写为只含 done_btn 的新帧后,旧帧按钮
	//    approve_btn 迟到点击 → 400;新帧 done_btn → 放行
	newFrame := cardV2EnvelopeJSON(t, "done_btn")
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version) VALUES (?,?,?,?,?,?,?,?)",
		"9102", 1, fakeBot, common.ChannelTypePerson.Uint8(), string(newFrame), util.MD5(string(newFrame)), int(time.Now().Unix()), 1,
	).Exec()
	assert.NoError(t, err)
	w = do(baseBody("9102", cardActionBotUID, "approve_btn", "tok-t3"))
	assert.Equal(t, http.StatusBadRequest, w.Code, "旧帧按钮应 fail-closed")
	w = do(baseBody("9102", cardActionBotUID, "done_btn", "tok-t4"))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// ④ 群频道非成员 → denied(群存在且 Normal、但无成员记录 —— 隔离成员门禁;
	//    群状态门禁 H1/P1-a 由 TestCardActionVisibilityParity 覆盖)
	_, err = ctx.DB().InsertBySql("INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'gc', ?, '')", "g_card_test", group.GroupStatusNormal).Exec()
	assert.NoError(t, err)
	seedCardMessage(t, ctx, 9103, cardActionBotUID, "g_card_test", common.ChannelTypeGroup.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	w = do(map[string]interface{}{
		"message_id": "9103", "channel_id": "g_card_test",
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"action_id":    "approve_btn", "client_token": "tok-t5",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "You are not allowed to act on this card.")

	// ⑤ D11 inputs 信任边界(round-3 P1-3):未声明键 / 非字符串值 → 400。
	//    P1-4 后幂等先于校验:必须用一张**未被 claim** 的新卡(复用已 claim 的 done_btn
	//    会先回 replay 而非跑校验)。校验失败会释放 claim,故两条 bad-input 各自重新
	//    claim 并 400。
	seedCardMessage(t, ctx, 9109, cardActionBotUID, fakeBot, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	for i, badInputs := range []map[string]interface{}{
		{"undeclared": "x"},       // 生效帧只声明了 comment
		{"comment": float64(123)}, // 值必须是字符串(AC submit 线上语义)
	} {
		w = do(map[string]interface{}{
			"message_id": "9109", "channel_id": cardActionBotUID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"action_id":    "approve_btn", "inputs": badInputs,
			"client_token": fmt.Sprintf("tok-t6-%d", i),
		})
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "Invalid card action.")
	}

	// ⑥ 跨频道 IDOR(round-3 P1-4):操作者是 A 群成员、消息在 B 群(非成员)。
	//    拿 A 的 channel_id 指 B 的 message_id → 频道绑定源自存储行,查不到即
	//    400;拿 B 的真实 channel_id → 成员资格对存储频道校验,denied。两条路都
	//    不产生 bot 事件。person 频道变体:fake id 含操作者,天然指不到别人的会话。
	_, err = ctx.DB().InsertBySql("insert into group_member(group_no,uid) values(?,?)", "g_idor_a", testutil.UID).Exec()
	assert.NoError(t, err)
	// g_idor_b 存在且 Normal —— 隔离:该分支唯一拒因是操作者非 B 群成员(denied),而非群状态。
	_, err = ctx.DB().InsertBySql("INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'gb', ?, '')", "g_idor_b", group.GroupStatusNormal).Exec()
	assert.NoError(t, err)
	seedCardMessage(t, ctx, 9104, cardActionBotUID, "g_idor_b", common.ChannelTypeGroup.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
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
	assert.Contains(t, w.Body.String(), "You are not allowed to act on this card.")
	w = do(map[string]interface{}{ // person 变体:声明与 bot 的会话,指群消息 id
		"message_id": "9104", "channel_id": cardActionBotUID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"action_id":    "approve_btn", "client_token": "tok-t9",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, "person fake 频道指群消息应 400")
	assert.Contains(t, w.Body.String(), "Invalid card action.")

	// ⑦ D7：iwh_ webhook 发送者的卡片 → 400（webhook 无事件消费端，与非 bot 同路径
	//    fail-closed；ExistRobot 对 iwh_ 合成发送者返 false）。
	fakeIWH := common.GetFakeChannelIDWith(testutil.UID, "iwh_hook1")
	seedCardMessage(t, ctx, 9105, "iwh_hook1", fakeIWH, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	w = do(baseBody("9105", "iwh_hook1", "approve_btn", "tok-t10"))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid card action.")

	// ⑧ D3：> 64KiB 请求体 pre-decode 即被 MaxBytesReader 拒（BindJSON 失败 → invalid）。
	w = do(map[string]interface{}{
		"message_id": "9102", "channel_id": cardActionBotUID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"action_id":    "done_btn", "client_token": "tok-t11",
		"inputs": map[string]interface{}{"comment": strings.Repeat("a", 70<<10)},
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, "超 64KiB body 应 pre-decode 被拒")

	// ⑨ 撤回门禁（PR#548 review 阻断项）：message_extra.revoke=1 的卡片 → 400，
	//    不触发 bot 副作用（与单条读 api_message_get.go 同口径）。
	fakeBotR := common.GetFakeChannelIDWith(testutil.UID, cardActionBotUID)
	seedCardMessage(t, ctx, 9106, cardActionBotUID, fakeBotR, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,`revoke`,version) VALUES (?,?,?,?,?,?)",
		"9106", 1, fakeBotR, common.ChannelTypePerson.Uint8(), 1, 1).Exec()
	assert.NoError(t, err)
	w = do(baseBody("9106", cardActionBotUID, "approve_btn", "tok-t12"))
	assert.Equal(t, http.StatusBadRequest, w.Code, "已撤回卡片应拒")
	assert.Contains(t, w.Body.String(), "Invalid card action.")

	// ⑩ 全局删除门禁：message_extra.is_deleted=1 的卡片 → 400。
	seedCardMessage(t, ctx, 9107, cardActionBotUID, fakeBotR, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,is_deleted,version) VALUES (?,?,?,?,?,?)",
		"9107", 1, fakeBotR, common.ChannelTypePerson.Uint8(), 1, 1).Exec()
	assert.NoError(t, err)
	w = do(baseBody("9107", cardActionBotUID, "approve_btn", "tok-t13"))
	assert.Equal(t, http.StatusBadRequest, w.Code, "全局删除卡片应拒")
	assert.Contains(t, w.Body.String(), "Invalid card action.")

	// ⑪ 操作者本地删除门禁：message_user_extra.message_is_deleted=1 → 400
	//    （该操作者已从自己视图删除这张卡）。
	seedCardMessage(t, ctx, 9108, cardActionBotUID, fakeBotR, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO message_user_extra (uid,message_id,message_seq,channel_id,channel_type,message_is_deleted) VALUES (?,?,?,?,?,?)",
		testutil.UID, "9108", 1, fakeBotR, common.ChannelTypePerson.Uint8(), 1).Exec()
	assert.NoError(t, err)
	w = do(baseBody("9108", cardActionBotUID, "approve_btn", "tok-t14"))
	assert.Equal(t, http.StatusBadRequest, w.Code, "操作者本地删除卡片应拒")
	assert.Contains(t, w.Body.String(), "Invalid card action.")

	// 整个 ⑥–⑪ 未投递任何事件
	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	n, err := rds.ZCard("robotEvent:" + cardActionBotUID).Result()
	assert.NoError(t, err)
	assert.Equal(t, int64(1), n, "只应有 ③ done_btn 放行产生的那 1 条事件")
}

func TestCardActionDisabledByFlag(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "") // rollout gate 默认关闭(fail-closed)
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)

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

func TestCardActionD8SharedWindowConstant(t *testing.T) {
	// D8（PR#548 review）：card_action 去重键 TTL 必须与事件可操作窗口
	// (Robot.MessageExpire) **同源同值** —— 否则两值之间的窗口里去重键先过期，窗口内
	// re-tap 会造出第二条 card_action 事件（yujiawei：需 asserted by test）。
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	store := newCardActionClaimStore(ctx)
	if store.idemTTL != ctx.GetConfig().Robot.MessageExpire {
		t.Errorf("幂等 TTL 应 == 可操作窗口 Robot.MessageExpire: idemTTL=%v window=%v",
			store.idemTTL, ctx.GetConfig().Robot.MessageExpire)
	}
	if store.idemTTL <= 0 {
		t.Errorf("幂等 TTL 应为正, got %v", store.idemTTL)
	}
}

func TestCardActionClaimConfirmedState(t *testing.T) {
	// PR#548 review P2：Confirmed 区分 pending（首请求在途、可重试）与已 confirm
	// （回 replay:true）—— 只有 confirmed 才让并发的第二请求回 replay，避免首请求校验
	// 失败释放后第二请求拿着 pending 得到虚假成功却无事件入队而丢有效动作。
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)
	store := newCardActionClaimStore(ctx)
	key := cardActionClaimKey("m1", "a1", "u1")

	if c, err := store.Confirmed(key); err != nil || c {
		t.Errorf("不存在的 key 应未确认, c=%v err=%v", c, err)
	}
	if ok, err := store.Claim(key); err != nil || !ok {
		t.Fatalf("首次 Claim 应成功, ok=%v err=%v", ok, err)
	}
	if c, err := store.Confirmed(key); err != nil || c {
		t.Errorf("pending 占位应未确认（可重试，不回虚假 replay）, c=%v err=%v", c, err)
	}
	if ok, err := store.Confirm(key, 12345); err != nil || !ok {
		t.Fatalf("Confirm 应成功, ok=%v err=%v", ok, err)
	}
	if c, err := store.Confirmed(key); err != nil || !c {
		t.Errorf("confirm 后应已确认, c=%v err=%v", c, err)
	}
}

func TestCardActionReleaseOnlyIfPending(t *testing.T) {
	// PR#548 review P2-c：Release 必须「仅当仍是 pending 才删」(CAS-del)。否则「首请求
	// stall >60s → pending 过期 → 重试 claim+confirm」后,原请求的补偿 Release 会误删已
	// confirm 键、重开去重窗口 → 同一动作二次入队。
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)
	store := newCardActionClaimStore(ctx)

	// ① pending 态 Release → 删除(正常补偿路径:校验失败释放,纠正后可重新 claim)。
	pendKey := cardActionClaimKey("m_rel", "a", "pending")
	if ok, err := store.Claim(pendKey); err != nil || !ok {
		t.Fatalf("Claim 应成功, ok=%v err=%v", ok, err)
	}
	if err := store.Release(pendKey); err != nil {
		t.Fatalf("Release(pending) 应成功, err=%v", err)
	}
	if ok, err := store.Claim(pendKey); err != nil || !ok {
		t.Errorf("pending 被释放后应可重新 Claim, ok=%v err=%v", ok, err)
	}

	// ② confirmed 态 Release → **不删**(模拟 stale 请求误删已确认键的场景)。
	confKey := cardActionClaimKey("m_rel", "a", "confirmed")
	if ok, err := store.Claim(confKey); err != nil || !ok {
		t.Fatalf("Claim 应成功, ok=%v err=%v", ok, err)
	}
	if ok, err := store.Confirm(confKey, 999); err != nil || !ok {
		t.Fatalf("Confirm 应成功, ok=%v err=%v", ok, err)
	}
	if err := store.Release(confKey); err != nil {
		t.Fatalf("Release(confirmed) 不应报错(只是不删), err=%v", err)
	}
	// 键仍在且仍是 confirmed:去重窗口未被重开 —— 新 Claim(SET NX)应失败。
	if c, err := store.Confirmed(confKey); err != nil || !c {
		t.Errorf("confirmed 键不应被 Release 误删, c=%v err=%v", c, err)
	}
	if ok, err := store.Claim(confKey); err != nil || ok {
		t.Errorf("confirmed 键仍在 → 新 Claim 应失败(去重窗口未重开), ok=%v err=%v", ok, err)
	}
}

func TestCardActionVisibilityParity(t *testing.T) {
	// PR#548 review：card/action 必须与单条读 respondSingleMessage 的 canonical 可见性
	// 同口径。读路径 404 的不可见状态 —— visibles 白名单排除(round-3 P1)、群被管理员禁用
	// (H1/P1-a)、子区已删除(P2-a) —— 触发 card/action 都必须拒,否则能对不可见卡片触发
	// bot 副作用。
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)

	seedCardBot(t, ctx, cardActionBotUID)
	// g_vis 正常群(status=Normal)+ 操作者为成员 —— 让 9601/9602 的唯一区分点落在 visibles。
	_, err := ctx.DB().InsertBySql("INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'vis', ?, '')", "g_vis", group.GroupStatusNormal).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertBySql("insert into group_member(group_no,uid) values(?,?)", "g_vis", testutil.UID).Exec()
	assert.NoError(t, err)

	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()

	do := func(msgID, channelID string, channelType uint8, tok string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"message_id": msgID, "channel_id": channelID,
			"channel_type": channelType,
			"action_id":    "approve_btn", "client_token": tok,
		}))))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	withVisibles := func(uids ...string) []byte {
		var env map[string]interface{}
		assert.NoError(t, json.Unmarshal(cardV2EnvelopeJSON(t, "approve_btn"), &env))
		vis := make([]interface{}, 0, len(uids))
		for _, u := range uids {
			vis = append(vis, u)
		}
		env["visibles"] = vis
		b, _ := json.Marshal(env)
		return b
	}
	assertNoNewEvent := func(before int64, label string) {
		after, _ := rds.ZCard("robotEvent:" + cardActionBotUID).Result()
		assert.Equal(t, before, after, label+"：拒绝不得新增 bot 事件")
	}

	// ① visibles 只含别人(不含操作者)→ 虽是群成员 + bot 发送 + 未撤回，仍必须拒。
	seedCardMessage(t, ctx, 9601, cardActionBotUID, "g_vis", common.ChannelTypeGroup.Uint8(), withVisibles("someone_else"))
	before, _ := rds.ZCard("robotEvent:" + cardActionBotUID).Result()
	w := do("9601", "g_vis", common.ChannelTypeGroup.Uint8(), "tok-vis1")
	assert.Equal(t, http.StatusBadRequest, w.Code, "visibles 排除的成员触发 card/action 应拒")
	assert.Contains(t, w.Body.String(), "Invalid card action.")
	assertNoNewEvent(before, "visibles 排除")

	// ② 对照:visibles 含操作者 → 放行(不误伤合法可见成员)。
	seedCardMessage(t, ctx, 9602, cardActionBotUID, "g_vis", common.ChannelTypeGroup.Uint8(), withVisibles(testutil.UID))
	w = do("9602", "g_vis", common.ChannelTypeGroup.Uint8(), "tok-vis2")
	assert.Equal(t, http.StatusOK, w.Code, "visibles 含操作者应放行, body=%s", w.Body.String())

	// ③ H1/P1-a：管理员禁用群(GroupStatusDisabled)—— 禁用只置 group.Status=Disabled、
	//    成员行仍 Normal、ExistMemberActive 照过;读路径 requireGroupMember 已 404,动作
	//    路径必须同样拒(在成员门禁之前先判群状态,归并 invalid 防枚举)。
	_, err = ctx.DB().InsertBySql("INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'dis', ?, '')", "g_dis", group.GroupStatusDisabled).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertBySql("insert into group_member(group_no,uid) values(?,?)", "g_dis", testutil.UID).Exec()
	assert.NoError(t, err)
	seedCardMessage(t, ctx, 9603, cardActionBotUID, "g_dis", common.ChannelTypeGroup.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	before, _ = rds.ZCard("robotEvent:" + cardActionBotUID).Result()
	w = do("9603", "g_dis", common.ChannelTypeGroup.Uint8(), "tok-dis")
	assert.Equal(t, http.StatusBadRequest, w.Code, "禁用群触发 card/action 应拒")
	assert.Contains(t, w.Body.String(), "Invalid card action.")
	assertNoNewEvent(before, "禁用群")

	// ④ P2-a：CommunityTopic 子区已删除(ThreadStatusDeleted)—— 父群 Normal + 成员齐备,
	//    唯一拒因是子区状态(隔离该门禁);读路径 getThreadMessage 同样 404。归档子区不拒。
	const topicGroup = "g_topic"
	const topicShort = "100000000000001" // 15 位纯数字,满足 IsValidShortID
	_, err = ctx.DB().InsertBySql("INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'tg', ?, '')", topicGroup, group.GroupStatusNormal).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertBySql("insert into group_member(group_no,uid) values(?,?)", topicGroup, testutil.UID).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO thread(short_id, group_no, name, creator_uid, status, version) VALUES(?, ?, 'del', ?, ?, 1)", topicShort, topicGroup, cardActionBotUID, thread.ThreadStatusDeleted).Exec()
	assert.NoError(t, err)
	topicChannel := thread.BuildChannelID(topicGroup, topicShort)
	seedCardMessage(t, ctx, 9604, cardActionBotUID, topicChannel, common.ChannelTypeCommunityTopic.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	before, _ = rds.ZCard("robotEvent:" + cardActionBotUID).Result()
	w = do("9604", topicChannel, common.ChannelTypeCommunityTopic.Uint8(), "tok-thr")
	assert.Equal(t, http.StatusBadRequest, w.Code, "已删除子区触发 card/action 应拒, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid card action.")
	assertNoNewEvent(before, "已删除子区")
}

// 验收(P2 D4, round-3 P1-1):入队失败 → 内部封套 + 补偿释放 claim,同一操作者
// 立即重试成功 —— 半途而废的请求不锁死动作。
//
// 用包内 newTestServer + New(ctx):需要拿到 Message 实例注入 flaky robotService
// (testutil 路由绑定的是 sync.Once 缓存的全局实例,不可注入)。表由同二进制内先
// 跑的 testutil 迁移建出。
func TestCardActionEnqueueFailureReleasesClaim(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := newTestServer()
	m := New(ctx)
	flaky := &flakyRobotService{IService: m.robotService, fail: true}
	m.robotService = flaky
	m.Route(s.GetRoute())
	resetCardActionState(t, ctx)
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const failBot = "bot_card_fail"
	seedCardBot(t, ctx, failBot)
	fake := common.GetFakeChannelIDWith(uid, failBot)
	seedCardMessage(t, ctx, 9301, failBot, fake, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))

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

// cardV2EnvelopeWithSpaceID 构造带顶层 space_id 的 octo/v2 卡信封(模拟 send 出口
// 把权威 SpaceID 注入进存储 payload —— DM 无 Space 路由,payload 是收端唯一信号源)。
func cardV2EnvelopeWithSpaceID(t *testing.T, spaceID, actionID string) []byte {
	t.Helper()
	env := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"plain":        "审批单",
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "审批单"},
			},
			"actions": []interface{}{
				map[string]interface{}{"type": "Action.Submit", "id": actionID, "title": actionID},
			},
		},
	}
	if spaceID != "" {
		env["space_id"] = spaceID
	}
	raw, err := json.Marshal(env)
	assert.NoError(t, err)
	return raw
}

// TestCardActionSpaceIDFromCardOrigin 验收 P1-3(PR#548 review):event_data.space_id
// 取自卡片的**权威来源 Space**(存储 payload / 群表),而非操作者请求上下文的 Space;
// 卡片无权威 Space 时 fail-closed 省略该键(与 send 出口无权威值即 strip 同口径)。
func TestCardActionSpaceIDFromCardOrigin(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)

	seedCardBot(t, ctx, cardActionBotUID)
	fake := common.GetFakeChannelIDWith(testutil.UID, cardActionBotUID)
	// 9401:payload 顶层带发送时注入的权威 space_id="sp_origin"。
	seedCardMessage(t, ctx, 9401, cardActionBotUID, fake, common.ChannelTypePerson.Uint8(), cardV2EnvelopeWithSpaceID(t, "sp_origin", "approve_btn"))
	// 9402:payload 无 space_id(孤儿 bot / 非 Space 部署)。
	seedCardMessage(t, ctx, 9402, cardActionBotUID, fake, common.ChannelTypePerson.Uint8(), cardV2EnvelopeWithSpaceID(t, "", "approve_btn"))

	act := func(msgID string) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"message_id": msgID, "channel_id": cardActionBotUID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"action_id":    "approve_btn", "client_token": "tok-" + msgID,
		}))))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	eventDataFor := func(msgID string) map[string]interface{} {
		entries, err := rds.ZRange("robotEvent:"+cardActionBotUID, 0, -1).Result()
		assert.NoError(t, err)
		for _, e := range entries {
			var ev struct {
				EventData map[string]interface{} `json:"event_data"`
			}
			if json.Unmarshal([]byte(e), &ev) == nil && ev.EventData["message_id"] == msgID {
				return ev.EventData
			}
		}
		return nil
	}

	act("9401")
	ed := eventDataFor("9401")
	assert.NotNil(t, ed)
	assert.Equal(t, "sp_origin", ed["space_id"], "space_id 必取卡片来源 Space,而非操作者请求上下文")

	act("9402")
	ed = eventDataFor("9402")
	assert.NotNil(t, ed)
	_, hasSpace := ed["space_id"]
	assert.False(t, hasSpace, "无权威来源 Space 时必须省略 space_id 键(fail-closed)")
}

// TestCardActionReplayAfterButtonRemoved 验收 P1-4(PR#548 review):已受理的动作在
// bot 重写移除该按钮后被重试,必须回 replay(幂等**先于** stale-frame 校验),不撞
// stale-frame 误判 400、不产生第二个事件;而一个**从未 claim** 的按钮迟到点击仍
// fail-closed 400(不弱化首次点击的 stale 保护)。
func TestCardActionReplayAfterButtonRemoved(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)

	seedCardBot(t, ctx, cardActionBotUID)
	fake := common.GetFakeChannelIDWith(testutil.UID, cardActionBotUID)
	seedCardMessage(t, ctx, 9501, cardActionBotUID, fake, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn", "reject_btn"))

	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	do := func(actionID, tok string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"message_id": "9501", "channel_id": cardActionBotUID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"action_id":    actionID, "client_token": tok,
		}))))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// ① 首次点 approve_btn → 受理 + 入队 1 条
	w := do("approve_btn", "tok-p4-1")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"replay":false`)
	n, _ := rds.ZCard("robotEvent:" + cardActionBotUID).Result()
	assert.Equal(t, int64(1), n)

	// ② bot 重写卡片,把 approve_btn / reject_btn 全移除(新帧只剩 done_btn)——
	//    模拟"点完即置灰/消失"。
	newFrame := cardV2EnvelopeJSON(t, "done_btn")
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version) VALUES (?,?,?,?,?,?,?,?)",
		"9501", 1, fake, common.ChannelTypePerson.Uint8(), string(newFrame), util.MD5(string(newFrame)), int(time.Now().Unix()), 1,
	).Exec()
	assert.NoError(t, err)

	// ③ 丢 ack 后客户端重试同一 (message_id, action_id, operator)(换 token)——
	//    approve_btn 已从生效帧移除。P1-4:必须回 replay(不是 stale-frame 400),
	//    且不产生第二个事件。
	w = do("approve_btn", "tok-p4-2")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"replay":true`, "已 claim 的重试即使按钮被移除也必须回 replay")
	n, _ = rds.ZCard("robotEvent:" + cardActionBotUID).Result()
	assert.Equal(t, int64(1), n, "重试不得产生第二个事件")

	// ④ 对照:一个**从未 claim** 的按钮(reject_btn)在被移除后首次点击 → 仍 fail-closed
	//    400(P1-4 只放行已受理动作的重试,不放行全新的迟到点击)。
	w = do("reject_btn", "tok-p4-3")
	assert.Equal(t, http.StatusBadRequest, w.Code, "从未 claim 的按钮迟到点击仍 fail-closed 400")
	assert.Contains(t, w.Body.String(), "Invalid card action.")
	n, _ = rds.ZCard("robotEvent:" + cardActionBotUID).Result()
	assert.Equal(t, int64(1), n, "对照组不得产生事件")
}
