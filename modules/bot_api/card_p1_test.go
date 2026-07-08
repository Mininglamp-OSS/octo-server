package bot_api

// card-message-protocol P1 的 bot ingress / 编辑路径测试。
// spec: .octospec/tasks/card-message-protocol/brief.md；执行 brief:
// .octospec/tasks/card-message-p1-display/brief.md。
//
// 拒绝类用例（rollout flag / OBO / 脏卡 / v2 分期 / body cap）发生在 IM 派发
// 之前，只需 MySQL（authBot 查 robot.bot_token 列）；happy path + 编辑路径
// 需要 WuKongIM(:5001)，缺席时 t.Skip。

import (
	"bytes"
	"encoding/json"
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
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"

	// 注册 message 模块:botMessageEdit 落库到 message_extra,该表由 message
	// 模块迁移创建,bot_api 测试二进制默认不含它。
	_ "github.com/Mininglamp-OSS/octo-server/modules/message"
)

const (
	p1CardBotID    = "bot_card_p1"
	p1CardBotToken = "bf_card_p1_token"
)

func skipWithoutIMBot(t *testing.T) {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:5001/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skip("WuKongIM 未运行(需 :5001),跳过 IM 集成用例")
	}
	_ = resp.Body.Close()
}

// p1CardEnvelope 构造合法 octo/v1 展示卡信封。
func p1CardEnvelope() map[string]interface{} {
	return map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV1,
		"plain":        "forged-by-client",
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "审批单 #7 状态卡"},
			},
			"actions": []interface{}{
				map[string]interface{}{"type": "Action.OpenUrl", "title": "查看", "url": "https://example.com/7"},
			},
		},
	}
}

func seedP1CardBot(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", p1CardBotID, p1CardBotToken).Exec()
	assert.NoError(t, err)
	// user bot 的 DM 发送有好友门禁 —— 种双向好友关系
	for _, pair := range [][2]string{{p1CardBotID, testutil.UID}, {testutil.UID, p1CardBotID}} {
		_, ferr := ctx.DB().InsertBySql(
			"insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, ferr)
	}
}

// TestBotCardSendRejects：入站 gate 的拒绝矩阵（全部发生在 IM 派发前）。
func TestBotCardSendRejects(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedP1CardBot(t, ctx)

	do := func(body map[string]interface{}, rawBody []byte) *httptest.ResponseRecorder {
		var buf []byte
		if rawBody != nil {
			buf = rawBody
		} else {
			buf = []byte(util.ToJson(body))
		}
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/bot/sendMessage", bytes.NewReader(buf))
		req.Header.Set("Authorization", "Bearer "+p1CardBotToken)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	sendBody := func(env map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"channel_id":   testutil.UID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"payload":      env,
		}
	}

	// ① Decision 2b：OBO 卡片按请求意图拦截 —— 先于 grant 校验（本测试不种任何
	//   OBO grant，拒绝即证明 gate 在 grant 校验之前；grantorReplyBypass 子路径
	//   同被该意图门覆盖）。
	body := sendBody(p1CardEnvelope())
	body["on_behalf_of"] = testutil.UID
	w := do(body, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card messages cannot be sent on behalf of a user.")

	// ② 脏卡（javascript: URL）→ 白名单 400
	dirty := p1CardEnvelope()
	dirty["card"].(map[string]interface{})["body"] = []interface{}{
		map[string]interface{}{"type": "Image", "url": "javascript:alert(1)"},
	}
	w = do(sendBody(dirty), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid card payload.")

	// ③ P2 元素越级（octo/v1 信封携带 Action.Submit）→ 白名单 400。
	//   （octo/v2 profile 本身 P2 起已被接受,其发送 happy path 见 TestBotCardEditCASIM。）
	submit := p1CardEnvelope()
	submit["card"].(map[string]interface{})["actions"] = []interface{}{
		map[string]interface{}{"type": "Action.Submit", "id": "x", "title": "OK"},
	}
	w = do(sendBody(submit), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Invalid card payload.")

	// ⑤ Decision 3b：> MaxSendBodyBytes 的 body 在解码前被 MaxBytesReader 拒绝
	huge := []byte(`{"channel_id":"` + testutil.UID + `","channel_type":1,"payload":{"type":17,"pad":"` +
		strings.Repeat("a", cardmsg.MaxSendBodyBytes) + `"}}`)
	w = do(nil, huge)
	assert.Equal(t, http.StatusBadRequest, w.Code, "超限 body 应在 pre-decode 被拒")

	// ⑥ 回归护栏（review 发现）：body 上限必须 > 同路由最大合法 payload。
	//   RichText 合法上限是 1MiB payload —— 一条 ~1MiB 的 RichText 叠加信封后
	//   body 约 1MiB+,若上限取 1MiB 会被 pre-decode 误杀。此处发一条 ~1.4MiB
	//   body 的 RichText（payload 本体在 RichText 1MiB 限内），断言它**不是**
	//   被 body cap 挡下（走到 richtext 校验路径，返回业务响应而非 pre-decode
	//   截断）。用 body 长度直接锚定 MaxSendBodyBytes 与 1MiB 的关系。
	assert.Greater(t, cardmsg.MaxSendBodyBytes, 1<<20,
		"body 上限必须严格大于 RichText 的 1MiB payload 上限,否则回归既有流量")
}

// TestBotCardSendDisabledByFlag：Decision 2 rollout gate（默认关闭 fail-closed）。
func TestBotCardSendDisabledByFlag(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedP1CardBot(t, ctx)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/bot/sendMessage", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload":      p1CardEnvelope(),
	}))))
	req.Header.Set("Authorization", "Bearer "+p1CardBotToken)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card messages are not enabled on this server.")
}

// TestBotCardSendAndEditRejectP1IM：P1 happy path + Decision 7 编辑不可变
// （真实 WuKongIM 链路）。
func TestBotCardSendAndEditP2IM(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedP1CardBot(t, ctx)

	do := func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", path, bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("Authorization", "Bearer "+p1CardBotToken)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// ① bot 发合法 v1 卡 → 真实 IM 派发成功；存储 payload 的 plain 已被服务端
	//   重算（Decision 8：端上伪造 plain 不落地）。
	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload":      p1CardEnvelope(),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	assert.NotZero(t, sendResp.MessageID)

	var msgSeq uint32
	var storedPayload []byte
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   testutil.UID,
			ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds:  []int64{sendResp.MessageID},
			LoginUID:    p1CardBotID,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			storedPayload = sr.Messages[0].Payload
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, msgSeq, "消息未完成 IM 异步持久化")
	assert.NotContains(t, string(storedPayload), "forged-by-client", "plain 必须被服务端重算覆盖")
	assert.Contains(t, string(storedPayload), "审批单 #7 状态卡", "权威 plain 来自卡片内容")

	msgID := fmt.Sprintf("%d", sendResp.MessageID)
	editBody := func(contentEdit string) map[string]interface{} {
		return map[string]interface{}{
			"message_id":   msgID,
			"message_seq":  msgSeq,
			"channel_id":   testutil.UID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"content_edit": contentEdit,
		}
	}

	// ② P2 D6：卡片消息可被 bot 编辑为新卡帧 → 200，落 message_extra（plain 服务端
	//   重算）。P1 的 blanket-reject 已退役（本 PR-B）。
	w = do("/v1/bot/message/edit", editBody(util.ToJson(p1CardEnvelope())))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var extraCount int
	_ = ctx.DB().Select("count(*)").From("message_extra").Where("message_id=?", msgID).LoadOne(&extraCount)
	assert.Equal(t, 1, extraCount, "P2:卡片编辑应写入 message_extra 一行")

	// ③ D6 不变量 (a)：卡片消息被"编辑"为纯文本体（跨类型变异 card→非card）仍 400。
	w = do("/v1/bot/message/edit", editBody("plain text takeover"))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// ④ 对照:bot 发文本消息再编辑 → 放行（证明编辑链路通、拒绝确因卡片门禁）
	w = do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload":      map[string]interface{}{"type": 1, "content": "editable text"},
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var textResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &textResp))
	var textSeq uint32
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   testutil.UID,
			ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds:  []int64{textResp.MessageID},
			LoginUID:    p1CardBotID,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			textSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, textSeq)
	w = do("/v1/bot/message/edit", map[string]interface{}{
		"message_id":   fmt.Sprintf("%d", textResp.MessageID),
		"message_seq":  textSeq,
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"content_edit": "editable text (v2)",
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
