package message

// Decision 7 用户编辑路径的 WuKongIM 集成测试（需要 IM 运行在 :5001，
// 缺席时 t.Skip 不破坏无 IM 环境）。messageEdit 的属主校验先经
// IMGetWithChannelAndSeqs 查真实消息,故必须有 IM 才能走到卡片门禁。

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// skipWithoutIM 探测 WuKongIM(:5001)健康端点,不可达则跳过。
func skipWithoutIM(t *testing.T) {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:5001/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skip("WuKongIM 未运行(需 :5001,见 CI 或 poc 环境脚本),跳过 IM 集成用例")
	}
	_ = resp.Body.Close()
}

// waitIMMessageSeq 轮询 IMSearchMessages 直到消息完成异步持久化拿到 seq。
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

	// 以登录用户身份经 IM 发一条真实文本消息。
	// 注意:官方 WuKongIM v2 的 /message/send 响应只带 message_id(client_msg_no),
	// message_seq 由异步持久化后置分配 —— 须经 IMSearchMessages 轮询取回。
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
	w := edit(string(cardEnvelopeJSON(t, "sneak_btn")))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Card messages cannot be edited.")

	// 对照:普通文本编辑放行(IM 属主校验链路真实走通)
	w2 := edit("hello card edit (v2)")
	assert.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
}
