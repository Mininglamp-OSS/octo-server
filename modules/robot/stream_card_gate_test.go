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

	"github.com/Mininglamp-OSS/octo-lib/common"
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
func streamGateHarness(t *testing.T) (*wkhttp.WKHttp, *stubGroupService, *libconfig.Context) {
	t.Helper()
	_, ctx := testutil.NewTestServer()

	gs := &stubGroupService{}
	rb := &Robot{ctx: ctx, Log: log.NewTLog("RobotStreamGateTest"), groupService: gs}
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.POST("/v1/robots/:robot_id/:app_key/stream/start", rb.streamStart)
	return r, gs, ctx
}

// seedGroup inserts the parent group row the disband guard reads. Needed only by
// the subarea cases: the guard is skipped for Person and, for Group, these cases
// never get past the channel stub.
func seedGroup(t *testing.T, ctx *libconfig.Context, groupNo string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, ?, ?, '')",
		groupNo, groupNo, status,
	).Exec()
	assert.NoError(t, err)
}

// stubGroupService records which uid allowSendToChannel asked about, which is
// the only way to observe the FromUID pin from outside the handler: the pin is a
// single assignment and IMStreamStart's response does not echo it back.
//
// The interface is embedded rather than fully implemented — every other method
// is nil and would panic if reached, which is the point: if this stub ever needs
// a second method, the handler started doing something this test does not model.
//
// **The two membership predicates return separate fields on purpose.** They are
// not interchangeable: ExistMember filters is_deleted=0 only, ExistMemberActive
// also requires status=Normal, and the subarea gate must use the latter so a
// blacklisted bot cannot post (round-8 review P1). A stub with one shared result
// would answer both identically, so a test could not tell which one the handler
// called and swapping them back would stay green. askedMethod records the choice
// itself rather than only its outcome.
type stubGroupService struct {
	group.IService
	askedUID     string
	askedGroup   string
	askedMethod  string // "ExistMember" | "ExistMemberActive"
	member       bool   // ExistMember result — is_deleted=0 only
	activeMember bool   // ExistMemberActive result — also status=Normal
}

func (s *stubGroupService) ExistMember(groupNo string, uid string) (bool, error) {
	s.askedUID, s.askedGroup, s.askedMethod = uid, groupNo, "ExistMember"
	return s.member, nil
}

func (s *stubGroupService) ExistMemberActive(groupNo string, uid string) (bool, error) {
	s.askedUID, s.askedGroup, s.askedMethod = uid, groupNo, "ExistMemberActive"
	return s.activeMember, nil
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

// postStreamStartTo is postStreamStart with the channel under test's control,
// for the subarea cases.
func postStreamStartTo(t *testing.T, handler http.Handler, channelID string, channelType uint8, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(libconfig.MessageStreamStartReq{
		ClientMsgNo: "streamgate-topic",
		FromUID:     streamGateRobotID,
		ChannelID:   channelID,
		ChannelType: channelType,
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
	handler, _, _ := streamGateHarness(t)

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
	handler, _, _ := streamGateHarness(t)

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
	handler, _, _ := streamGateHarness(t)

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
	handler, gs, _ := streamGateHarness(t)
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
	handler, gs, _ := streamGateHarness(t)
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
	handler, gs, _ := streamGateHarness(t)
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

// TestAllowSendToChannel_CommunityTopicResolvesParentGroup —— 子区按父群判成员。
//
// 直接测这份共用判定，而不是只从 streamStart 打进去：三个 robot ingress
// （sendMessage / typing / streamStart）都调它，这里少认一种频道类型，那种类型就在三条
// 路上同时不可用。round-8 评审正是因为 streamStart 补了频道校验才让这个洞浮出水面——
// 而它先前已经让 sendMessage 与 typing 的子区解散守卫永远不可达。
//
// 断言按父群查：子区 channelID 形如 `<parentGroupNo>____<topicNo>`，成员表挂在父群上，
// 拿整个 channelID 去查成员会永远查不到，那是「看起来实现了、实际全拒」的失败形态。
func TestAllowSendToChannel_CommunityTopicResolvesParentGroup(t *testing.T) {
	_, ctx := testutil.NewTestServer()

	gs := &stubGroupService{member: true, activeMember: true}
	rb := &Robot{ctx: ctx, Log: log.NewTLog("RobotAllowSendTest"), groupService: gs}
	// 用常量而非字面量 5：常量若改值，字面量会让这条测试静默改测另一种频道。
	topicType := common.ChannelTypeCommunityTopic.Uint8()

	assert.True(t, rb.allowSendToChannel("bot_a", "parent_group_1____topic_9", topicType),
		"父群成员必须被允许向子区发送")
	assert.Equal(t, "parent_group_1", gs.askedGroup,
		"成员查询用的不是父群号——拿整个子区 channelID 去查会永远落空")

	gs.activeMember = false
	assert.False(t, rb.allowSendToChannel("bot_a", "parent_group_1____topic_9", topicType),
		"非父群成员必须被拒")

	// 形状不合法 → fail-closed，且不得把畸形串当群号去查。
	//
	// 这几种形状全仓标准解析器 thread.ParseChannelID 都拒（恰好两段、两段非空），
	// 而原来那版 SplitN(...,2) 只拒第一种：后两种会切出 parts[0]="groupA" 通过成员
	// 校验，再把非规范 channelID 原样投出去——下游同样走 ParseChannelID，于是门按
	// groupA 放行、消息却投不到任何订阅者。门与投递目标对「合法子区 ID」判断不一致，
	// 正是 round-9 评审 P2-2（与刚修的 P1 同一族：本模块把全仓判定又实现了一遍且更松）。
	for _, malformed := range []string{
		"no_separator_here",        // 没有分隔符
		"groupA____topic____extra", // 多一个分隔符：SplitN 会放行
		"groupA____",               // 尾段为空：SplitN 会放行
		"____topic_1",              // 群号为空：SplitN 会放行且把 "" 当群号
	} {
		gs.activeMember = true
		gs.askedGroup = ""
		assert.False(t, rb.allowSendToChannel("bot_a", malformed, topicType),
			"子区 channelID %q 形状不合法时必须拒绝", malformed)
		assert.Empty(t, gs.askedGroup,
			"%q 形状不合法却发起了成员查询——说明解析器放行了一个下游会拒的 ID", malformed)
	}

	// 未知类型仍然一律拒——放宽只针对 type 5。
	assert.False(t, rb.allowSendToChannel("bot_a", "whatever", 99),
		"未知频道类型必须继续拒绝")
}

// TestStreamStart_CommunityTopicFlowsThroughBothGates —— round-8 评审的回归。
//
// 加频道校验之前，streamStart 是三条 robot 路里唯一子区能发的（因为它压根没校验）。
// 补上校验后 allowSendToChannel 不认 type 5，子区流式立刻 403 —— 而子区助手用流式增量
// 回复正是最自然的用法。原有用例只覆盖 type 1 和 2，所以没人拦住。
//
// **父群行必须先插进去**：修好频道门之后，那个此前不可达的子区解散守卫第一次被执行，
// 它会去查 `group` 表。不插这一行的话，请求会在守卫里以 query_failed 结束——本用例仍会
// 因为断言的是「没被频道门拒」而通过，但它证明的东西比名字承诺的少。第一版就是这样过的。
func TestStreamStart_CommunityTopicFlowsThroughBothGates(t *testing.T) {
	handler, gs, ctx := streamGateHarness(t)
	gs.activeMember = true // 父群活跃成员
	seedGroup(t, ctx, "streamgate_group", 1)

	w := postStreamStartTo(t, handler, "streamgate_group____topic_1",
		common.ChannelTypeCommunityTopic.Uint8(), []byte(`{"type":1,"content":"hello"}`))

	assert.Equal(t, "streamgate_group", gs.askedGroup,
		"子区成员判定必须落在父群上")
	for _, refusal := range []string{
		"err.server.robot.channel_send_forbidden",
		"err.server.robot.group_disbanded",
		"err.server.robot.query_failed",
	} {
		assert.NotEqual(t, refusal, streamGateErrorCode(w),
			"父群成员向未解散子区开流被 %s 拦下了，body=%s", refusal, w.Body.String())
	}
}

// TestStreamStart_CommunityTopicDisbandGuardIsLive —— 覆盖被本次改动**激活**的代码。
//
// 三个 handler 各有一个解析父群的子区解散守卫，在 allowSendToChannel 认识 type 5 之前
// 全是死代码——一行都跑不到。修好频道门等于把它们同时启用了，而启用一段从未执行过的分支
// 却不测它，正是「改动看起来没风险」最容易出事的地方。
//
// 与上一条配对：上一条证明未解散的子区能过，这一条证明已解散的过不去。少了任何一条，
// 「守卫存在」和「守卫永远放行」就分不出来。
func TestStreamStart_CommunityTopicDisbandGuardIsLive(t *testing.T) {
	handler, gs, ctx := streamGateHarness(t)
	gs.activeMember = true // 成员判定放行，只留解散这一个变量
	seedGroup(t, ctx, "streamgate_dead", group.GroupStatusDisband)

	w := postStreamStartTo(t, handler, "streamgate_dead____topic_1",
		common.ChannelTypeCommunityTopic.Uint8(), []byte(`{"type":1,"content":"hello"}`))

	assert.Equal(t, "err.server.robot.group_disbanded", streamGateErrorCode(w),
		"父群已解散仍放行开流，body=%s", w.Body.String())
}

// TestAllowSendToChannel_CommunityTopicRefusesBlacklistedMember —— round-8 评审 P1。
//
// 这条用例把两个谓词的差别做成**唯一变量**：`member=true, activeMember=false` 正是被拉黑
// 成员在库里的样子（`is_deleted=0` 通过、`status=Blacklist` 不通过）。用 ExistMember 判定
// 会放行，用 ExistMemberActive 会拒绝——所以它守的不是「拒绝」这个结果，而是**选了哪个
// 谓词**。只断言结果的话，把代码换回 ExistMember 而 stub 两个字段又恰好同值时，测试照绿。
//
// 为什么这条路必须用严格的那个：robot ingress 是服务端直发，不经过 IM datasource，拿不到
// thread 模块的子区拉黑继承，本地这道门就是唯一防线。group/service.go 上 ExistMemberActive
// 的注释点名了子区场景，thread / message / messages_search / bot_api 的每一处子区门禁也都
// 用它——本函数曾是全仓唯一的例外。
func TestAllowSendToChannel_CommunityTopicRefusesBlacklistedMember(t *testing.T) {
	_, ctx := testutil.NewTestServer()

	gs := &stubGroupService{member: true, activeMember: false} // 在群里，但被拉黑
	rb := &Robot{ctx: ctx, Log: log.NewTLog("RobotBlacklistTest"), groupService: gs}

	allowed := rb.allowSendToChannel("bot_a", "parent_group_1____topic_9",
		common.ChannelTypeCommunityTopic.Uint8())

	assert.False(t, allowed,
		"被拉黑成员被放行进子区——说明走的是 ExistMember（只看 is_deleted）而非 ExistMemberActive")
	assert.Equal(t, "ExistMemberActive", gs.askedMethod,
		"子区门禁必须调 ExistMemberActive；调 ExistMember 会让被拉黑成员通过，"+
			"且这条路绕过 IM 层的子区拉黑继承，本地判定是唯一防线")
	assert.Equal(t, "parent_group_1", gs.askedGroup, "活跃成员判定必须落在父群上")
}
