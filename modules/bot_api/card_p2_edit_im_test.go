package bot_api

// card-message-interaction P2 D6/D9 集成测试（需 WuKongIM :5001）。
// spec: .octospec/tasks/card-message-interaction/brief.md；执行 brief:
// .octospec/tasks/card-message-p2-action-loop/brief.md。
//
// 覆盖：bot 卡片编辑解锁（D6 整卡替换 + 权威 plain 重算）→ 跨类型变异拒绝
// （D6 不变量 a）→ card_seq CAS 乱序帧拒绝（D9）→ 脏帧白名单拒绝。
// send 响应只带 message_id（官方 WuKongIM v2 语义），编辑前经 IMSearchMessages
// 轮询等 message_seq 就绪。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
)

const (
	imCardBotID    = "bot_card_im"
	imCardBotToken = "bf_card_im_token"
)

// imCardEnvelope 构造 octo/v2 卡片信封；cardSeq<0 表示不带 card_seq。
func imCardEnvelope(actionID string, cardSeq int64) map[string]interface{} {
	env := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV2,
		"plain":        "forged-by-client",
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "审批单 #7 状态卡"},
			},
			"actions": []interface{}{
				map[string]interface{}{"type": "Action.Submit", "id": actionID, "title": actionID},
			},
		},
	}
	if cardSeq >= 0 {
		env["card_seq"] = cardSeq
	}
	return env
}

func TestBotCardEditCASIM(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", imCardBotID, imCardBotToken).Exec()
	assert.NoError(t, err)
	for _, pair := range [][2]string{{imCardBotID, testutil.UID}, {testutil.UID, imCardBotID}} {
		_, ferr := ctx.DB().InsertBySql(
			"insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, ferr)
	}

	do := func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", path, bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("Authorization", "Bearer "+imCardBotToken)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// ① bot 发卡（真实 IM 派发）
	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload":      imCardEnvelope("approve_btn", -1),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	assert.NotZero(t, sendResp.MessageID)
	msgID := fmt.Sprintf("%d", sendResp.MessageID)

	var msgSeq uint32
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   testutil.UID,
			ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds:  []int64{sendResp.MessageID},
			LoginUID:    imCardBotID,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, msgSeq, "消息未完成 IM 异步持久化")

	editBody := func(env map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"message_id":   msgID,
			"message_seq":  msgSeq,
			"channel_id":   testutil.UID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"content_edit": util.ToJson(env),
		}
	}

	// ② D6 happy：整卡替换为新帧（card_seq=2），校验 + plain 权威重算 + 落库
	w = do("/v1/bot/message/edit", editBody(imCardEnvelope("done_btn", 2)))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var stored string
	err = ctx.DB().Select("content_edit").From("message_extra").Where("message_id=?", msgID).LoadOne(&stored)
	assert.NoError(t, err)
	assert.Contains(t, stored, "done_btn", "message_extra 应存最新帧")
	assert.NotContains(t, stored, "forged-by-client", "plain 必须被服务端重算覆盖")

	// ③ D9 CAS：乱序帧（card_seq=1 ≤ 已存 2）→ 冲突（D14 线上 400 + 文案），不覆盖
	w = do("/v1/bot/message/edit", editBody(imCardEnvelope("stale_btn", 1)))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card update rejected: stale card_seq.")
	var after string
	_ = ctx.DB().Select("content_edit").From("message_extra").Where("message_id=?", msgID).LoadOne(&after)
	assert.Contains(t, after, "done_btn", "乱序帧不得覆盖已存帧")

	// ④ D6 跨类型变异：卡片消息被"编辑"为纯文本体 → 拒绝
	w = do("/v1/bot/message/edit", map[string]interface{}{
		"message_id":   msgID,
		"message_seq":  msgSeq,
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"content_edit": "plain text takeover",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid request.")

	// ⑤ 脏卡片帧（javascript: URL）→ 白名单拒绝，不落库
	dirty := imCardEnvelope("x_btn", 3)
	dirty["card"].(map[string]interface{})["body"] = []interface{}{
		map[string]interface{}{"type": "Image", "url": "javascript:alert(1)"},
	}
	w = do("/v1/bot/message/edit", editBody(dirty))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid card payload.")
	var count int
	_ = ctx.DB().Select("count(*)").From("message_extra").Where("message_id=? and content_edit like ?", msgID, "%javascript%").LoadOne(&count)
	assert.Zero(t, count, "脏帧不得落库")
}

// TestBotCardEditConcurrentCASIM 验证 D9 CAS 在并发下无 lost-update：并发发若干
// 递增 card_seq 的编辑帧,不论到达顺序,最终 stored 必为最大 seq 那一帧 —— 一旦
// 最大 seq 帧落库,任何更小 seq 都被拒；而最大 seq 帧到达时 stored 必 < 它,故必被
// 应用。SELECT ... FOR UPDATE 的 next-key 锁把并发首帧也串行化。
func TestBotCardEditConcurrentCASIM(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const casBot = "bot_card_cas"
	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", casBot, "bf_card_cas_token").Exec()
	assert.NoError(t, err)
	for _, pair := range [][2]string{{casBot, testutil.UID}, {testutil.UID, casBot}} {
		_, ferr := ctx.DB().InsertBySql(
			"insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, ferr)
	}
	do := func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", path, bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("Authorization", "Bearer bf_card_cas_token")
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
		"payload": imCardEnvelope("f0", -1),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	msgID := fmt.Sprintf("%d", sendResp.MessageID)
	var msgSeq uint32
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID: testutil.UID, ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds: []int64{sendResp.MessageID}, LoginUID: casBot,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, msgSeq)

	const n = 8
	var wg sync.WaitGroup
	for seq := 1; seq <= n; seq++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			do("/v1/bot/message/edit", map[string]interface{}{
				"message_id": msgID, "message_seq": msgSeq,
				"channel_id": testutil.UID, "channel_type": common.ChannelTypePerson.Uint8(),
				"content_edit": util.ToJson(imCardEnvelope(fmt.Sprintf("f%d", seq), int64(seq))),
			})
		}(seq)
	}
	wg.Wait()

	// 不变量:最终 stored 必为最大 seq(n)的帧,不论并发到达顺序。
	var storedSeq int64
	err = ctx.DB().Select("card_seq").From("message_extra").Where("message_id=?", msgID).LoadOne(&storedSeq)
	assert.NoError(t, err)
	assert.Equal(t, int64(n), storedSeq, "并发 CAS 后最终 card_seq 必为最大值(无 lost-update/stale-overwrite)")
	var stored string
	_ = ctx.DB().Select("content_edit").From("message_extra").Where("message_id=?", msgID).LoadOne(&stored)
	assert.Contains(t, stored, fmt.Sprintf("f%d", n), "最终帧必为最大 seq 那一帧")
}
