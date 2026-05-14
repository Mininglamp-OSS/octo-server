package incomingwebhook

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/assert"
)

func TestGenerateToken(t *testing.T) {
	tok1, h1, err := generateToken()
	assert.NoError(t, err)
	assert.Equal(t, 64, len(tok1), "32 bytes hex = 64 chars")
	assert.Equal(t, 64, len(h1), "sha256 hex = 64 chars")
	assert.Equal(t, h1, hashToken(tok1))

	tok2, h2, err := generateToken()
	assert.NoError(t, err)
	assert.NotEqual(t, tok1, tok2, "token should be random")
	assert.NotEqual(t, h1, h2)
}

func TestHashTokenIsDeterministic(t *testing.T) {
	assert.Equal(t, hashToken("foo"), hashToken("foo"))
	assert.NotEqual(t, hashToken("foo"), hashToken("bar"))
}

func TestBuildPayload_DefaultsToWebhookNameAvatar(t *testing.T) {
	m := &incomingWebhookModel{
		WebhookID: "iwh_x",
		Name:      "WH",
		Avatar:    "https://avatar/x.png",
	}
	req := &pushPayloadReq{Content: "hi"}
	p := buildPayload(m, req)

	assert.Equal(t, int(common.Text), p["type"])
	assert.Equal(t, "hi", p["content"])
	from, _ := p["from"].(map[string]interface{})
	assert.Equal(t, "webhook", from["kind"])
	assert.Equal(t, "iwh_x", from["webhook_id"])
	assert.Equal(t, "WH", from["name"])
	assert.Equal(t, "https://avatar/x.png", from["avatar"])
}

func TestBuildPayload_OverrideUsernameAndAvatar(t *testing.T) {
	m := &incomingWebhookModel{WebhookID: "iwh_x", Name: "default"}
	req := &pushPayloadReq{
		Content:  "hi",
		Username: "GitHub Bot", AvatarURL: "https://gh/a.png",
	}
	from := buildPayload(m, req)["from"].(map[string]interface{})
	assert.Equal(t, "GitHub Bot", from["name"])
	assert.Equal(t, "https://gh/a.png", from["avatar"])
}

// TestBuildPayload_DropsAllExtra 锁定 Extra 一律丢弃的语义：调用方任何 key
// 都不能写入持久化 payload。重点防御 visibles —— message 模块把它当作服务端
// 强制的可见性白名单解释（不在列表内的用户会被同步标删 + 单消息 404），允许
// token 持有者写就等于给了一个"对管理员隐身的群消息"通道。
func TestBuildPayload_DropsAllExtra(t *testing.T) {
	m := &incomingWebhookModel{WebhookID: "iwh_x", Name: "WH"}
	req := &pushPayloadReq{
		Content: "real",
		Extra: map[string]interface{}{
			"visibles":   []string{"attacker_uid"}, // 关键：访问控制字段必须被丢弃
			"mention":    map[string]interface{}{"all": true},
			"reminder":   "fake",
			"link":       "https://x",
			"type":       9999,
			"content":    "fake",
			"from":       "fake",
			"space_id":   "forged",
			"anything":   "else",
		},
	}
	p := buildPayload(m, req)
	// 核心字段保持服务端值
	assert.Equal(t, int(common.Text), p["type"])
	assert.Equal(t, "real", p["content"])
	_, isMap := p["from"].(map[string]interface{})
	assert.True(t, isMap, "from must remain the structured object")
	// Extra 中任何 key 都不能写入 payload
	for k := range req.Extra {
		if k == "type" || k == "content" || k == "from" || k == "space_id" {
			continue // 这些是服务端字段，断言已在上面
		}
		_, exists := p[k]
		assert.Falsef(t, exists, "extra key %q must not leak into payload", k)
	}
}

func TestBuildPayload_SpaceIDFromModelNotExtra(t *testing.T) {
	m := &incomingWebhookModel{WebhookID: "iwh_x", Name: "WH", SpaceID: "real_space"}
	req := &pushPayloadReq{
		Content: "hi",
		Extra: map[string]interface{}{
			"space_id": "forged_space",
		},
	}
	p := buildPayload(m, req)
	assert.Equal(t, "real_space", p["space_id"], "space_id must come from model, not Extra")
}

func TestBuildPayload_SpaceIDSetEvenWhenExtraOmitsIt(t *testing.T) {
	m := &incomingWebhookModel{WebhookID: "iwh_x", Name: "WH", SpaceID: "real_space"}
	req := &pushPayloadReq{Content: "hi"}
	p := buildPayload(m, req)
	assert.Equal(t, "real_space", p["space_id"])
}

// TestBuildPayload_TruncatesLongUsernameAvatar 锁定 push 路径 username / avatar_url
// 服务端裁剪：调用方塞 KB 级字符串时，from.name / from.avatar 被截断到 create 侧
// 的列长度上限（64 / 255 字节），防止污染所有客户端渲染。原回归来自 PR #31
// yujiawei review P2-3。
func TestBuildPayload_TruncatesLongUsernameAvatar(t *testing.T) {
	m := &incomingWebhookModel{WebhookID: "iwh_x", Name: "WH", Avatar: "https://default"}
	longName := strings.Repeat("A", 10000)
	longAvatar := "https://" + strings.Repeat("x", 10000)
	req := &pushPayloadReq{
		Content:   "hi",
		Username:  longName,
		AvatarURL: longAvatar,
	}
	p := buildPayload(m, req)
	from := p["from"].(map[string]interface{})
	gotName := from["name"].(string)
	gotAvatar := from["avatar"].(string)

	assert.Equalf(t, 64, len(gotName), "from.name capped at 64 bytes; got %d", len(gotName))
	assert.Equalf(t, 255, len(gotAvatar), "from.avatar capped at 255 bytes; got %d", len(gotAvatar))
}

// TestBuildPayload_TruncateRespectsUTF8 验证截断在 rune 边界回退，不会把多字节字符
// 切成乱码字节。中文字符 3 字节，64 字节上限最多保留 21 个完整中文。
func TestBuildPayload_TruncateRespectsUTF8(t *testing.T) {
	m := &incomingWebhookModel{WebhookID: "iwh_x", Name: "WH"}
	req := &pushPayloadReq{
		Content:  "hi",
		Username: strings.Repeat("一", 100), // 100 × 3 = 300 字节
	}
	p := buildPayload(m, req)
	from := p["from"].(map[string]interface{})
	gotName := from["name"].(string)
	assert.LessOrEqualf(t, len(gotName), 64, "byte length capped; got %d", len(gotName))
	assert.Truef(t, utf8.ValidString(gotName), "truncated name must remain valid UTF-8; got %q", gotName)
}

func TestPublicURL(t *testing.T) {
	got := publicURL("iwh_abc", "tk")
	assert.Equal(t, "/v1/incoming-webhooks/iwh_abc/tk", got)
}

func TestGenerateWebhookID_HasPrefix(t *testing.T) {
	id := generateWebhookID()
	assert.Truef(t, len(id) > 4 && id[:4] == "iwh_", "id should start with iwh_, got %s", id)
}
