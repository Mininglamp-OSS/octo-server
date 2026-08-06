package robot

// task bot-setting-store round-7 —— 流式入口的卡片拒绝门。
//
// 打真实 handler 而不是抽一个纯函数来测：被守护的性质是「streamStart 这个 handler 里
// 有这道门」，纯函数测试在有人把调用点删掉之后照样全绿——本分支已经出过两个「名字里的
// 东西坏了它照样过」的测试（round-1 的原子性测试、round-7 评审指出的编辑门测试），
// 不再添第三个。
//
// 不挂 robotAuth：`app` 表的迁移在 modules/base，不在 modules/robot 测试二进制的迁移
// 集里（这也正是本包既有的 robot ingress 测试一律绕开该中间件的原因）。鉴权与本门正交
// ——门在 BindJSON 之后、任何业务判断之前，鉴权通过与否不改变它的结论。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	libconfig "github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
)

const streamGateRobotID = "streamgate_bot"

// streamGateHarness mounts streamStart on its real route shape, minus the auth
// middleware. The path keeps :robot_id so the handler's own c.Param read is
// exercised rather than stubbed.
//
// **Constructs *Robot as a struct literal rather than calling New(ctx)**, and
// that is load-bearing, not style. New() registers the module's event listeners
// and background wiring — the doc comment on Service says it exists precisely so
// callers can re-New *it* without duplicating them, which names *Robot as the
// one you must not. An earlier draft of this file called New() per test and made
// the bot_setting integration cases fail in 2 of 5 shuffled runs, always the
// unrelated ones; dropping New() removed it. The handler needs only ctx (for
// IMStreamStart) and the embedded logger, and the disband guards are skipped
// entirely for ChannelTypePerson, which is what these cases use.
//
// Setup-time CleanAllTables only, never on exit: every sibling setup in this
// package cleans before it runs, and cleaning afterwards instead puts the wipe
// between some other test's setup and its assertions once the order shuffles.
func streamGateHarness(t *testing.T) (*wkhttp.WKHttp, *stubGroupService) {
	t.Helper()
	_, ctx := testutil.NewTestServer()

	gs := &stubGroupService{}
	rb := &Robot{ctx: ctx, Log: log.NewTLog("RobotStreamGateTest"), groupService: gs}
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.POST("/v1/robots/:robot_id/:app_key/stream/start", rb.streamStart)
	return r, gs
}

// stubGroupService records which uid allowSendToChannel asked about, which is
// the only way to observe the FromUID pin from outside the handler: the pin is a
// single assignment and IMStreamStart's response does not echo it back.
//
// The interface is embedded rather than fully implemented — every other method
// is nil and would panic if reached, which is the point: if this stub ever needs
// a second method, the handler started doing something this test does not model.
type stubGroupService struct {
	group.IService
	askedUID string
	member   bool
}

func (s *stubGroupService) ExistMember(groupNo string, uid string) (bool, error) {
	s.askedUID = uid
	return s.member, nil
}

// postStreamStart sends one stream/start carrying the given raw payload bytes.
// MessageStreamStartReq.Payload is []byte, so encoding/json renders it base64 on
// the wire — going through the real struct rather than a hand-written literal
// keeps the test honest about the shape the handler actually receives.
func postStreamStart(t *testing.T, handler http.Handler, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(libconfig.MessageStreamStartReq{
		ClientMsgNo: "streamgate-1",
		FromUID:     streamGateRobotID,
		ChannelID:   "streamgate_peer",
		ChannelType: 1,
		Payload:     payload,
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST",
		"/v1/robots/"+streamGateRobotID+"/appkey/stream/start", bytes.NewReader(body))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)
	return w
}

func streamGateErrorCode(w *httptest.ResponseRecorder) string {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		return ""
	}
	return envelope.Error.Code
}

// TestStreamStart_RefusesCardPayload —— 流式入口不得成为第四条发卡路径。
//
// 本 handler 把 Payload 裸转给 WuKongIM：没有 payloadIsVail、没有 cardmsg.Validate、
// 没有 BotEnabled() 总闸、也没有 per-Bot 门。没有这道拒绝，owner 关掉展示/交互卡之后，
// 同一个 Bot 换这个端点仍能把 type:17 送出去——本任务承诺「每条已鉴权的发卡路径都按有效
// 配置校验」，少一条这个承诺就是假的。
func TestStreamStart_RefusesCardPayload(t *testing.T) {
	handler, _ := streamGateHarness(t)

	card := []byte(`{"type":17,"card_version":"1.0","profile":"octo/v1",` +
		`"card":{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"x"}]}}`)
	w := postStreamStart(t, handler, card)

	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "err.server.robot.content_invalid", streamGateErrorCode(w),
		"卡片必须走本 ingress 既有的单一泛化码（防枚举），body=%s", w.Body.String())
}

// TestStreamStart_TextPayloadIsNotRefusedByTheCardGate —— 这道门只拦卡片。
//
// 断言「没有被**这道门**拒掉」，而不是「返回 200」：放行之后请求会一路走到 WuKongIM，
// 其成败取决于 IM 侧状态，拿它当断言会把一条门禁测试变成环境测试。
func TestStreamStart_TextPayloadIsNotRefusedByTheCardGate(t *testing.T) {
	handler, _ := streamGateHarness(t)

	w := postStreamStart(t, handler, []byte(`{"type":1,"content":"hello"}`))

	assert.NotEqual(t, "err.server.robot.content_invalid", streamGateErrorCode(w),
		"普通文本被卡片门拦下了，body=%s", w.Body.String())
}

// TestStreamStart_StringTypedPayloadIsNotACard —— 与 payloadIsVail 同一个谓词，
// 且这里把它的**边界**钉成显式契约而不是留作默认。
//
// IsCardRawPayload 是 IsCardPayload 的 []byte 形态，同样只认 JSON 数字，所以
// `{"type":"17"}` 在本门是放行的：它不被判作卡片，会当普通 payload 转给 IM。
// legacy sendMessage 那条路上这个夹缝是被堵住的（round-3 P2-1），因为那里同时存在一个
// 会强转字符串的分发谓词；本 handler 不做类型分发，没有那个夹缝可堵。
//
// 写下这条是为了让「本门覆盖到哪」有据可查：它的职责是不让**卡片**从流式路径出去，而
// 字符串 type 能否被客户端渲染成卡片是仓外事实（见 journal known-gaps）。哪天答案是
// 「能」，本用例会连同 sendMessage 侧一起改，而不是让人误以为这里已经覆盖了。
func TestStreamStart_StringTypedPayloadIsNotACard(t *testing.T) {
	handler, _ := streamGateHarness(t)

	w := postStreamStart(t, handler, []byte(`{"type":"17","card":{}}`))

	assert.NotEqual(t, "err.server.robot.content_invalid", streamGateErrorCode(w),
		"字符串 type 目前不被 IsCardRawPayload 认作卡片；若这里开始拒绝，说明谓词已变，"+
			"sendMessage 侧的同型判定需一并复核。body=%s", w.Body.String())
}

// TestStreamStart_PinsFromUIDToTheAuthenticatedRobot —— 身份取自路径，不信请求体。
//
// 这是 round-7 评审的**核心**阻塞项，评审明确说它「与客户端是否渲染卡片无关」：同一个
// robotAuth 组里 sendMessage 与 typing 都把 FromUID 钉成 c.Param("robot_id")，只有
// streamStart 直接用调用方传入的值——鉴权回答了「你是谁」，请求体却能改写「你说你是谁」，
// 于是一个已鉴权的 Bot 能以任意 uid 开流。
//
// 请求体带一个与路径**不同**的 from_uid，再看频道校验实际按哪个身份去查群成员——
// 这是从 handler 外部唯一能观察到该赋值的地方（IMStreamStart 的响应不回显 FromUID）。
// 断言的是 stub 记下的 uid，而不是最终状态码：拿状态码当证据的话，「拒绝」既可能因为
// 钉住生效、也可能因为 victim 恰好也不在群里，两种原因给出同一个绿灯。
func TestStreamStart_PinsFromUIDToTheAuthenticatedRobot(t *testing.T) {
	handler, gs := streamGateHarness(t)
	gs.member = true // 成员判定放行，把频道门从断言里摘掉，只留身份这一个变量

	body, err := json.Marshal(libconfig.MessageStreamStartReq{
		ClientMsgNo: "streamgate-forge",
		FromUID:     "victim_uid_not_this_bot", // 冒充：与路径里的 robot_id 不同
		ChannelID:   "streamgate_group",
		ChannelType: 2, // 群：allowSendToChannel 会真的按身份查成员
		Payload:     []byte(`{"type":1,"content":"hello"}`),
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST",
		"/v1/robots/"+streamGateRobotID+"/appkey/stream/start", bytes.NewReader(body))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	assert.Equal(t, streamGateRobotID, gs.askedUID,
		"频道校验按请求体里的 from_uid 做的——身份未被钉成已鉴权的 robot_id，冒充仍然成立")
}

// TestStreamStart_ChecksChannelPermission —— 频道校验存在，且拒绝走既有码。
//
// 与上一条分开：上一条问「按谁的身份查」，这条问「查不过时会不会拒」。合成一条的话，
// 任何一半坏掉都被另一半的绿灯掩盖。
func TestStreamStart_ChecksChannelPermission(t *testing.T) {
	handler, gs := streamGateHarness(t)
	gs.member = false // 非群成员

	body, err := json.Marshal(libconfig.MessageStreamStartReq{
		ClientMsgNo: "streamgate-nonmember",
		FromUID:     streamGateRobotID,
		ChannelID:   "streamgate_group",
		ChannelType: 2,
		Payload:     []byte(`{"type":1,"content":"hello"}`),
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST",
		"/v1/robots/"+streamGateRobotID+"/appkey/stream/start", bytes.NewReader(body))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	assert.Equal(t, "err.server.robot.channel_send_forbidden", streamGateErrorCode(w),
		"非群成员必须被拒，且与 sendMessage 用同一个码，body=%s", w.Body.String())
}

// TestStreamStart_ChannelCheckPrecedesTheCardGate —— 顺序也是契约。
//
// 一个**非群成员**发一张卡：两道门都会拒，但必须是频道门先答。sendMessage 的顺序就是
// allowSendToChannel 在 payloadIsVail 之前——先确定有没有资格往这里发，再看发的是什么。
// 本分支为同一条原则改过一次：settings 端点的属主校验被连提三轮后前移到形状校验之前，
// 理由是「对无权资源先就内容表态，与端点自述的 403 语义矛盾」。
//
// 没有这条用例，两道门的相对位置就是无人防守的：任谁为了省一次 DB 查询把拒卡挪到前面，
// 全部既有用例照样绿——因为每一条都只触发其中一道门。
func TestStreamStart_ChannelCheckPrecedesTheCardGate(t *testing.T) {
	handler, gs := streamGateHarness(t)
	gs.member = false // 非群成员：频道门会拒

	card := []byte(`{"type":17,"card_version":"1.0","profile":"octo/v1",` +
		`"card":{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"x"}]}}`)
	body, err := json.Marshal(libconfig.MessageStreamStartReq{
		ClientMsgNo: "streamgate-order",
		FromUID:     streamGateRobotID,
		ChannelID:   "streamgate_group",
		ChannelType: 2,
		Payload:     card, // 卡片门也会拒
	})
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST",
		"/v1/robots/"+streamGateRobotID+"/appkey/stream/start", bytes.NewReader(body))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(w, req)

	assert.Equal(t, "err.server.robot.channel_send_forbidden", streamGateErrorCode(w),
		"卡片门抢在频道门前面答了：等于在鉴权之前就对内容表态，与 sendMessage 的顺序相反。"+
			"body=%s", w.Body.String())
}
