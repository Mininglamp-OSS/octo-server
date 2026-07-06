package bot_api

// card-message-interaction P2 D6/D9 的 WuKongIM 集成测试(最小闭环):
// bot 经 /v1/bot/sendMessage 发卡(真实 IM 派发)→ /v1/bot/message/edit 整卡
// 替换帧(cardmsg 校验 + 权威 plain 重算 + message_extra 落库)→ 跨类型变异
// 拒绝(D6 不变量 a)→ card_seq CAS 乱序帧拒绝(D9)。
// IM(:5001)缺席时 t.Skip,不破坏无 IM 环境。
//
// bot 鉴权走 robot 表 token 列(authUserBot → queryRobotByBotToken),测试
// 直接种行获得身份;Authorization: Bearer <token>。

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
	"github.com/stretchr/testify/assert"

	// 注册 message 模块:botMessageEdit 落库到 message_extra,该表由 message
	// 模块迁移创建,bot_api 测试二进制默认不含它。
	_ "github.com/Mininglamp-OSS/octo-server/modules/message"
)

const (
	imCardBotID    = "bot_card_im"
	imCardBotToken = "bf_card_im_token"
)

func skipWithoutIMBot(t *testing.T) {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:5001/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skip("WuKongIM 未运行(需 :5001),跳过 IM 集成用例")
	}
	_ = resp.Body.Close()
}

// imCardEnvelope 构造 octo/v2 卡片信封;cardSeq<0 表示不带 card_seq。
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

func TestBotCardSendAndEditIM(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	// 种 bot(带 bot_token → 直接获得 HTTP 身份,authUserBot 查 bot_token 列)
	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", imCardBotID, imCardBotToken).Exec()
	assert.NoError(t, err)
	// user bot 的 DM 发送有好友门禁 —— 种双向好友关系
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

	// ① bot 发卡(真实 IM 派发;send 返回 message_id/message_seq 即 P2 的"显示句柄")
	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload":      imCardEnvelope("approve_btn", -1),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID  int64  `json:"message_id"`
		MessageSeq uint32 `json:"message_seq"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	// 官方 WuKongIM v2:send 响应只带 message_id(=显示句柄),message_seq 由
	// 异步持久化后置分配 —— 编辑前经 IMSearchMessages 轮询等 seq 就绪。
	assert.NotZero(t, sendResp.MessageID, "send 响应应携带 message_id(进度回显模式的句柄)")
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
	sendResp.MessageSeq = msgSeq

	editBody := func(env map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"message_id":   msgID,
			"message_seq":  sendResp.MessageSeq,
			"channel_id":   testutil.UID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"content_edit": util.ToJson(env),
		}
	}

	// ② D6 happy:整卡替换为新帧(card_seq=2),校验 + plain 权威重算 + 落库
	w = do("/v1/bot/message/edit", editBody(imCardEnvelope("done_btn", 2)))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var stored string
	err = ctx.DB().Select("content_edit").From("message_extra").Where("message_id=?", msgID).LoadOne(&stored)
	assert.NoError(t, err)
	assert.Contains(t, stored, "done_btn", "message_extra 应存最新帧")
	assert.NotContains(t, stored, "forged-by-client", "plain 必须被服务端重算覆盖")
	assert.Contains(t, stored, "审批单 #7 状态卡", "权威 plain 来自卡片内容")

	// ③ D9 CAS:乱序帧(card_seq=1 ≤ 已存 2)→ 409 conflict(D14 线上 400 + 文案)
	w = do("/v1/bot/message/edit", editBody(imCardEnvelope("stale_btn", 1)))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card was updated concurrently")
	var after string
	_ = ctx.DB().Select("content_edit").From("message_extra").Where("message_id=?", msgID).LoadOne(&after)
	assert.Contains(t, after, "done_btn", "乱序帧不得覆盖已存帧")

	// ④ D6 跨类型变异:卡片消息被"编辑"为纯文本体 → 拒绝
	w = do("/v1/bot/message/edit", map[string]interface{}{
		"message_id":   msgID,
		"message_seq":  sendResp.MessageSeq,
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"content_edit": "plain text takeover",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid card payload.")

	// ⑤ 脏卡片帧(javascript: URL)→ 白名单拒绝,不落库
	dirty := imCardEnvelope("x_btn", 3)
	dirty["card"].(map[string]interface{})["body"] = []interface{}{
		map[string]interface{}{"type": "Image", "url": "javascript:alert(1)"},
	}
	w = do("/v1/bot/message/edit", editBody(dirty))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var count int
	_ = ctx.DB().Select("count(*)").From("message_extra").Where("message_id=? and content_edit like ?", msgID, "%javascript%").LoadOne(&count)
	assert.Zero(t, count, "脏帧不得落库")
}
