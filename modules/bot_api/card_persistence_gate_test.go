package bot_api

import (
	"bytes"
	"context"
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
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/assert"
)

// 这两条闸都实现在 modules/bot_api 里，而在此之前**本包内零测试覆盖** —— 覆盖全在
// internal/carddispatch，靠的是那一层的 fake。这正是 #712 里那个 P0 之所以能溜过去的
// 形状：guard 在 A 包，测试用 B 包的替身（fakeBotCardMutator 只记请求、从不复校验），
// 于是 A 包这层接线没人守。所以这里刻意走真实 HTTP handler，不用 fake。
//
// ① raw 卡片编辑的列宽闸（send.go 的 raw 分支）：一处调用覆盖它下游三个写动作
//    （card_seq CAS / 非 card_seq LWW / 修订历史追加），三者消费同一份字节。
// ② 模板发送的列宽预检（sendMessage 的模板分支）。

// oversizedRawCard 造一个 cardmsg 完全合法、但比 TEXT 列宽的卡：单个 TextBlock 撑大。
// cardmsg 只有 512 KiB 的 payload 上限，是列宽的 8 倍，所以这个帧过得了 Validate。
func oversizedRawCard() map[string]interface{} {
	return map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV1,
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5",
			"body": []interface{}{
				map[string]interface{}{
					"type": "TextBlock",
					// 每个 CJK 字符 3 字节，取列宽的一半个字符 → 稳过列宽。
					"text": strings.Repeat("塞", carddispatch.MaxPersistedFrameBytes/2),
				},
			},
		},
	}
}

func TestBotRawCardEditRejectsFrameWiderThanTheColumn(t *testing.T) {
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

	// 先正常发一张卡，拿到可编辑的目标。
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

	msgSeq := awaitP1CardStored(t, ctx, sendResp.MessageID)
	messageID := fmt.Sprintf("%d", sendResp.MessageID)

	// 自证前提：这个超宽帧在 cardmsg 眼里是合法的，被拒只能是因为列宽。
	oversized := util.ToJson(oversizedRawCard())
	if _, err := cardmsg.NormalizeContentEdit(oversized); err != nil {
		t.Fatalf("测试前提不成立：帧本身就非法 (%v)，那它测不到列宽闸", err)
	}
	assert.Greater(t, len(oversized), carddispatch.MaxPersistedFrameBytes)

	editBody := func(contentEdit string) map[string]interface{} {
		return map[string]interface{}{
			"message_id":   messageID,
			"message_seq":  msgSeq,
			"channel_id":   testutil.UID,
			"channel_type": common.ChannelTypePerson.Uint8(),
			"content_edit": contentEdit,
		}
	}

	// **正向对照，先跑**。没有它，下面那条「被拒」什么也证明不了 —— token 错、消息查不到、
	// 策略门关着，任何一个都能让编辑失败。先证明这套 setup 下正常尺寸的编辑确实能成功，
	// 才能把随后的拒绝归因到列宽。
	w = do("/v1/bot/message/edit", editBody(util.ToJson(p1CardEnvelope())))
	assert.Equal(t, http.StatusOK, w.Code,
		"正常尺寸的 raw 卡片编辑应成功；这条不过说明 setup 有问题，后面的断言无法归因: %s", w.Body.String())

	var storedAfterControl int
	_, err := ctx.DB().SelectBySql(
		"select count(*) from message_extra where message_id=? and content_edit is not null and content_edit<>''",
		messageID).Load(&storedAfterControl)
	assert.NoError(t, err)
	assert.Equal(t, 1, storedAfterControl, "正向对照应留下一行 content_edit")

	// 现在同一条消息、同一套 setup，只把帧换成超宽的 —— 必须被拒。
	w = do("/v1/bot/message/edit", editBody(oversized))
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"超宽 raw 卡片编辑被接受了 —— 列宽闸没生效，这一帧会直达 MySQL 的 TEXT 列: %s", w.Body.String())
	// 断言 DefaultMessage 而不是 error.code：testutil.NewTestServer 不装 i18n
	// ErrorRenderer，响应体里没有 error.code 字段。DefaultMessage 取自 errcode 的注册处，
	// 所以这条仍然把拒绝归因到**这个**码，而不是任意一个 400。
	assert.Contains(t, w.Body.String(), errcode.ErrBotAPICardInvalid.DefaultMessage,
		"应映射到 card-invalid（ErrCardMutationTooLarge wrap 了 ErrCardMutationInvalid）: %s", w.Body.String())

	// 且不得覆盖已落库的那一行：闸挡在写之前，不是让 MySQL 去拒。
	var storedEdit string
	_, err = ctx.DB().SelectBySql(
		"select content_edit from message_extra where message_id=?", messageID).Load(&storedEdit)
	assert.NoError(t, err)
	assert.NotContains(t, storedEdit, strings.Repeat("塞", 64),
		"超宽帧不得触达写路径，落库内容应仍是正向对照那一帧")
	assert.LessOrEqual(t, len(storedEdit), carddispatch.MaxPersistedFrameBytes,
		"落库的 content_edit 不得超过列宽")
}

// awaitP1CardStored 等 IM 异步持久化完成，返回可用于编辑的 messageSeq。
func awaitP1CardStored(t *testing.T, ctx *config.Context, clientMsgID int64) uint32 {
	t.Helper()
	var msgSeq uint32
	for i := 0; i < 20; i++ {
		sr, err := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   testutil.UID,
			ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds:  []int64{clientMsgID},
			LoginUID:    p1CardBotID,
		})
		if err == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if msgSeq == 0 {
		t.Skip("消息未在预期时间内完成 IM 异步持久化，跳过（非本用例断言的对象）")
	}
	return msgSeq
}

// 模板发送预检（sendMessage 的模板分支）：send 侧原本只查 512 KiB、编辑侧查 TEXT 列宽，
// 两者不一致就能造出「发得出去、第一次编辑就被拒」的卡。这条用例证明**schema 合法**的
// 输入确实能渲染出超列宽的帧（所以预检不是死代码），并且预检用的常量与写入侧闸同源。
//
// 刻意不打 HTTP：这里要钉的是「渲染结果 vs 列宽」这个判断，用真实 catalog 渲染 + 真实
// 常量即可，不必拖上 DB/IM。raw 编辑那条闸的接线才需要真实 handler（见上）。
func TestTemplateSendPrecheckCatchesSchemaValidButUnstorableFrame(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}

	// 最坏形状：6 阶段 / 13 条聚合 action（schema 的聚合上限）/ 每个自由字符串顶到上限，
	// 且全部用会被 Go 转义成 6 字节的 `<`。thought 给到受理上限，由引擎截到展示上限。
	filler := func(n int) string { return strings.Repeat("<", n) }
	phases := make([]any, 0, 6)
	actionCounts := []int{3, 2, 2, 2, 2, 2}
	for _, count := range actionCounts {
		actions := make([]any, 0, count)
		for i := 0; i < count; i++ {
			actions = append(actions, map[string]any{
				"tool": filler(81), "detail": filler(192),
				"statusGlyph": "●", "statusTone": "Good",
			})
		}
		phases = append(phases, map[string]any{"thought": filler(4001), "actions": actions})
	}
	fields := map[string]any{
		"reasoningId": strings.Repeat("r", 512), "state": "error",
		"title": filler(64), "statusLabel": filler(32), "statusTone": "Attention",
		"traceExpanded": true, "traceCollapsed": false,
		"collapsedSummary": filler(160), "progressText": filler(160),
		"errorTitle": filler(64), "errorMessage": filler(121),
		"phases": phases,
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	rendered, err := catalog.RenderPayload(context.Background(), map[string]any{
		"type": float64(17),
		"template_ref": map[string]any{
			"id": "ai.reasoning-process", "version": testReasoningVersionCurrent,
		},
		"state": "error",
		"data":  rawJSONToMap(t, raw),
	}, cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
	if err != nil {
		t.Fatalf("最坏形状必须 schema 合法、渲染得出来（否则测不到预检）: %v", err)
	}
	// 与 sendMessage 同口径：预检量的是 Finalize 之后的出站帧。
	if err := cardmsg.Finalize(rendered); err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}

	// ① 这一帧过得了 cardmsg 的 512 KiB 上限 —— 所以 send 侧靠 cardmsg 是拦不住的。
	assert.NoError(t, cardmsg.RecheckPayloadSize(rendered),
		"最坏帧应仍在 cardmsg.MaxPayloadBytes 内，否则它在渲染阶段就挂了，预检永远走不到")
	// ② 但它存不下 —— 预检必须拦住，否则卡片发出后第一次编辑就被写入侧的闸拒掉。
	assert.Greater(t, len(frame), carddispatch.MaxPersistedFrameBytes,
		"schema 允许的最坏帧竟然装得下列宽 —— 若模板收窄到这个程度，预检可以移除；"+
			"在那之前它是活的，不能当死代码删掉")

	t.Logf("最坏帧 %d B = 列宽 %d B 的 %.0f%%（cardmsg 上限 %d B 拦不住它）",
		len(frame), carddispatch.MaxPersistedFrameBytes,
		100*float64(len(frame))/float64(carddispatch.MaxPersistedFrameBytes), cardmsg.MaxPayloadBytes)
}
