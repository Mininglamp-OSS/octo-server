package robot

// card-message-protocol P1：robot ingress 的对称校验（payloadIsVail /
// supportContentType 的卡片分支）。构造 *Robot 直接打单元面 —— HTTP 层的
// robot 鉴权与本 gate 正交，send 的错误形状（单一 content-invalid 400）由
// respondRobotContentInvalid 既有测试覆盖。

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/gookit/goutil/maputil"
	"github.com/stretchr/testify/assert"
)

func p1RobotCardPayload(profile string, imageURL string) map[string]interface{} {
	body := []interface{}{
		map[string]interface{}{"type": "TextBlock", "text": "状态卡"},
	}
	if imageURL != "" {
		body = append(body, map[string]interface{}{"type": "Image", "url": imageURL})
	}
	return map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      profile,
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": "1.5", "body": body,
		},
	}
}

func TestRobotCardIngress(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	rb := New(ctx)

	// ContentType 17 进入支持集（Decision：robot 是三个生产者入口之一）
	assert.True(t, rb.supportContentType(cardmsg.InteractiveCard))

	// rollout gate 关闭（缺省）→ fail-closed
	t.Setenv(cardmsg.EnvEnabled, "")
	assert.False(t, rb.payloadIsVail(maputil.Data(p1RobotCardPayload(cardmsg.ProfileV1, ""))),
		"flag 关闭时 robot ingress 应拒卡")

	t.Setenv(cardmsg.EnvEnabled, "true")
	// 合法 octo/v1 卡放行
	assert.True(t, rb.payloadIsVail(maputil.Data(p1RobotCardPayload(cardmsg.ProfileV1, "https://cdn.example.com/a.png"))))
	// 脏 URL → 白名单拒绝
	assert.False(t, rb.payloadIsVail(maputil.Data(p1RobotCardPayload(cardmsg.ProfileV1, "javascript:alert(1)"))))
	// octo/v2（P2 D2）已被接受：展示型 body 无交互元素时同样是合法 v2 卡。
	assert.True(t, rb.payloadIsVail(maputil.Data(p1RobotCardPayload("octo/v2", ""))))
}

// TestRobotCardIngress_StringTypeIsNotACard 钉住路由谓词与校验谓词的同源性
// （round-3 评审 P2-1）。
//
// 本 ingress 用 maputil.Data.Int("type") 分发，Int 会对字符串跑 strconv.Atoi；
// cardmsg.IsCardPayload 只认 JSON 数字。两者不一致时 {"type":"17"} 会落进一条
// 谁都不管的夹缝：Validate 因 IsCardPayload=false 直接 return nil（白名单、
// 节点/深度上限、profile 协商全部不执行），rejectRobotCardBySetting 同样判 false
// 而跳过全部 per-Bot 开关，Finalize 也 no-op。断言用的卡带 javascript: URL——
// 上面那条同样的 URL 走数字 type 时是被拒的，这里若放行就等于**校验被绕过**，
// 而不只是类型宽松。
func TestRobotCardIngress_StringTypeIsNotACard(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	rb := New(ctx)
	t.Setenv(cardmsg.EnvEnabled, "true")

	payload := p1RobotCardPayload(cardmsg.ProfileV1, "javascript:alert(1)")
	payload["type"] = "17" // 字符串而非数字

	// 前提复核：分发确实把它当成 17（否则本测试没有守住任何东西）。
	assert.Equal(t, cardmsg.InteractiveCard.Int(), maputil.Data(payload).Int("type"),
		"maputil 不再强转字符串，本测试守护的夹缝已不存在，可以删除")
	assert.False(t, cardmsg.IsCardPayload(payload),
		"IsCardPayload 开始接受字符串 type，请改用它作为唯一谓词")

	assert.False(t, rb.payloadIsVail(maputil.Data(payload)),
		"字符串 type 的 payload 进了卡片分支却跳过校验——URL 白名单被绕过")
}
