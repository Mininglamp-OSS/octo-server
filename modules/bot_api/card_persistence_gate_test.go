package bot_api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/gin-gonic/gin"
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
//
// 超时**判失败、不 skip**（review of #716）：调用方在此之前已经用 skipWithoutIMBot 确认过
// :5001 健康，所以到这里还等不到就是真问题。原来写 t.Skip 会让本 PR 唯一真正接线 guard 的
// 那条用例在负载高的 runner 上变成 pass-by-skip —— 与真通过在结果里分不出来，正是「测试
// 存在但什么都没保证」的那一类。预算也从 6s 抬到 15s，减少误判。
func awaitP1CardStored(t *testing.T, ctx *config.Context, clientMsgID int64) uint32 {
	t.Helper()
	var msgSeq uint32
	for i := 0; i < 50; i++ {
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
		t.Fatalf("消息未在 15s 内完成 IM 异步持久化（clientMsgID=%d）。IM 健康检查已通过，"+
			"所以这是真失败而不是环境缺失 —— 不能 skip，否则本 PR 唯一接线 guard 的用例"+
			"会以「通过」的形式静默失效", clientMsgID)
	}
	return msgSeq
}

// 模板发送预检（sendMessage 的模板分支）**必须驱动真 handler**。
//
// 这条用例的第一版只是渲染出最坏帧、然后自己比一次长度 —— 那是重新实现了预检的算术，
// 根本没走到预检：review 把 send.go 里整个 `if templateMode { … }` 块删掉，测试照样绿。
// 那正是本 PR 自己诊断的 #712 P0 的形状（断言判断、不断言接线），只是高了一层，所以必须
// 修成打 POST /v1/bot/sendMessage。不需要 DB —— dispatchOverride 就能截住派发。
func TestTemplateSendPrecheckRejectsUnstorableFrameThroughHandler(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	gin.SetMode(gin.TestMode)

	newAPI := func(dispatch *dispatchCapture) *BotAPI {
		catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
		if err != nil {
			t.Fatal(err)
		}
		return &BotAPI{
			Log:              log.NewTLog("BotAPI-precheck"),
			cardTemplates:    catalog,
			cardConfig:       allCardCapabilitiesOn(),
			spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space-authoritative"},
			dispatchOverride: dispatch.hook,
		}
	}

	// **正向对照，先跑**：出厂样本必须发得出去。没有它，下面那条「被拒」可能是任何原因
	// （模板未注册、策略门、Space 解析）造成的，归因不到列宽。
	control := &dispatchCapture{}
	recorder := invokeTemplateSend(t, newAPI(control),
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning")), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("出厂样本的模板发送应成功；这条不过说明 harness 有问题，后面无法归因: status=%d body=%s",
			recorder.Code, recorder.Body.String())
	}
	control.mu.Lock()
	controlDispatched := control.captured != nil
	control.mu.Unlock()
	if !controlDispatched {
		t.Fatal("正向对照应产生一次派发")
	}

	// 同一套 harness，只把 data 换成 schema 合法的最坏形状（实测首次编辑信封 151% 列宽）。
	blocked := &dispatchCapture{}
	recorder = invokeTemplateSend(t, newAPI(blocked),
		registrySendBody(t, "error", worstCaseReasoningData(t)), "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("超列宽的模板发送应被预检拒绝（否则卡片发出后第一次编辑就动不了）: status=%d body=%s",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), errcode.ErrBotAPICardInvalid.DefaultMessage) {
		t.Fatalf("应映射到 card-invalid: %s", recorder.Body.String())
	}
	// 关键：不得派发。预检必须挡在出站之前，而不是发出去再说。
	blocked.mu.Lock()
	dispatched := blocked.captured
	blocked.mu.Unlock()
	if dispatched != nil {
		t.Fatal("超列宽帧被派发了 —— 预检没有挡在出站之前，这张卡会发出去且永远编辑不动")
	}
}

// worstCaseReasoningData 构造 schema 允许的最坏形状：6 阶段 / 13 条聚合 action（聚合上限）/
// 每个自由字符串顶到上限，且全用会被 Go 转义成 6 字节的 `<`。thought 给到受理上限，由引擎
// 截到展示上限。实测渲染后的首次编辑信封是列宽的 151%。
func worstCaseReasoningData(t *testing.T) json.RawMessage {
	t.Helper()
	filler := func(n int) string { return strings.Repeat("<", n) }
	phases := make([]any, 0, 6)
	for _, count := range []int{3, 2, 2, 2, 2, 2} {
		actions := make([]any, 0, count)
		for i := 0; i < count; i++ {
			actions = append(actions, map[string]any{
				"tool": filler(81), "detail": filler(192),
				"statusGlyph": "●", "statusTone": "Good",
			})
		}
		phases = append(phases, map[string]any{"thought": filler(4001), "actions": actions})
	}
	raw, err := json.Marshal(map[string]any{
		"reasoningId": strings.Repeat("r", 512), "state": "error",
		"title": filler(64), "statusLabel": filler(32), "statusTone": "Attention",
		"traceExpanded": true, "traceCollapsed": false,
		"collapsedSummary": filler(160), "progressText": filler(160),
		"errorTitle": filler(64), "errorMessage": filler(121),
		"phases": phases,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 保留这条只量字节的用例作为**活性事实**：证明 schema 合法的输入确实能渲染出超列宽的帧，
// 所以上面那条 handler 用例测的不是死代码。它单独不足以守住 guard（review P1 已证），
// 两条一起才完整。
func TestTemplateSendPrecheckTargetIsReachableFromSchemaValidInput(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := catalog.RenderPayload(context.Background(), map[string]any{
		"type": float64(17),
		"template_ref": map[string]any{
			"id": "ai.reasoning-process", "version": testReasoningVersionCurrent,
		},
		"state": "error",
		"data":  rawJSONToMap(t, worstCaseReasoningData(t)),
	}, cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
	if err != nil {
		t.Fatalf("最坏形状必须 schema 合法、渲染得出来: %v", err)
	}
	if err := cardmsg.Finalize(rendered); err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	// ① 过得了 cardmsg 的 512 KiB 上限 —— 所以 send 侧靠 cardmsg 拦不住。
	assert.NoError(t, cardmsg.RecheckPayloadSize(rendered),
		"最坏帧应仍在 cardmsg.MaxPayloadBytes 内，否则它在渲染阶段就挂了，预检永远走不到")
	// ② 但存不下。
	assert.Greater(t, len(frame), carddispatch.MaxPersistedFrameBytes,
		"schema 允许的最坏帧竟然装得下列宽 —— 若模板收窄到这个程度，预检可以移除；在那之前它是活的")
	t.Logf("最坏帧 %d B = 列宽 %d B 的 %.0f%%（cardmsg 上限 %d B 拦不住它）",
		len(frame), carddispatch.MaxPersistedFrameBytes,
		100*float64(len(frame))/float64(carddispatch.MaxPersistedFrameBytes), cardmsg.MaxPayloadBytes)
}

// 预检必须按**首次编辑信封**量，而不是按发送帧量。编辑路径必然追加 card_seq、进度帧还会
// 追加 transient（send.go 的 registry edit 分支）。所以按发送帧长度
// 放行会留下一个与该开销同宽的窗口：落在其中的帧发得出去、第一次编辑就被写入侧的
// 闸拒掉 —— 正是这道预检存在的理由（follow-up review）。字段长度连续可变，区间可达。
func TestTemplateSendPrecheckAccountsForTheFirstEditEnvelope(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	catalog, err := newBotCardTemplateCatalog(testBotTemplateRegistry(t), defaultBotTemplateRefs())
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := catalog.RenderPayload(context.Background(), map[string]any{
		"type": float64(17),
		"template_ref": map[string]any{
			"id": "ai.reasoning-process", "version": testReasoningVersionCurrent,
		},
		"state": "reasoning",
		"data":  rawJSONToMap(t, testReasoningData(t, "reasoning")),
	}, cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cardmsg.Finalize(rendered); err != nil {
		t.Fatal(err)
	}
	sendFrame, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}

	// 复刻编辑路径的信封改写，取最坏值（int64 上界的 card_seq + transient）。
	probe := make(map[string]interface{}, len(rendered)+2)
	for k, v := range rendered {
		probe[k] = v
	}
	probe["card_seq"] = int64(math.MaxInt64)
	probe["transient"] = true
	editFrame, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}

	overhead := len(editFrame) - len(sendFrame)
	assert.Positive(t, overhead,
		"首次编辑信封必须比发送帧大，否则这条用例测的东西不存在")
	t.Logf("发送帧 %d B → 首次编辑信封 %d B，开销 %d B（这就是按发送帧量会漏掉的窗口宽度）",
		len(sendFrame), len(editFrame), overhead)

	// 关键断言：一个恰好落在窗口里的帧 —— 发送帧装得下、编辑信封装不下 —— 必须被预检
	// 用的那个判据拒掉。这里直接对判据下手，不必构造一张真到 65,491 B 的卡。
	windowSend := strings.Repeat("x", carddispatch.MaxPersistedFrameBytes-overhead/2)
	assert.LessOrEqual(t, len(windowSend), carddispatch.MaxPersistedFrameBytes,
		"窗口内的发送帧按定义应装得下列宽")
	assert.Greater(t, len(windowSend)+overhead, carddispatch.MaxPersistedFrameBytes,
		"窗口内的帧加上编辑信封开销后应超出列宽 —— 否则窗口是空的，这条用例无意义")

	// 同一份渲染结果，两种量法给出不同答案，这就是修复前后的差别。
	assert.LessOrEqual(t, len(sendFrame), carddispatch.MaxPersistedFrameBytes,
		"出厂样本的发送帧本来就远在列宽内（此处只是钉住前提）")
	if _, err := carddispatch.NormalizeFrameForPersistence(string(editFrame)); err != nil {
		t.Fatalf("出厂样本的首次编辑信封应通过判据: %v", err)
	}
}
