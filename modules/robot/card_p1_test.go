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
	// octo/v2 在 P1 是未知 profile → 拒绝（Decision 10 分期）
	assert.False(t, rb.payloadIsVail(maputil.Data(p1RobotCardPayload("octo/v2", ""))))
}
