package incomingwebhook

import (
	"testing"

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

func TestBuildPayload_ExtraDoesNotOverrideKeyFields(t *testing.T) {
	m := &incomingWebhookModel{WebhookID: "iwh_x", Name: "WH"}
	req := &pushPayloadReq{
		Content: "real",
		Extra: map[string]interface{}{
			"type":    9999,        // 不能覆盖
			"content": "fake",      // 不能覆盖
			"from":    "fake",      // 不能覆盖
			"mention": "fake",      // 不能覆盖
			"link":    "https://x", // 允许透传
		},
	}
	p := buildPayload(m, req)
	assert.Equal(t, int(common.Text), p["type"])
	assert.Equal(t, "real", p["content"])
	_, isMap := p["from"].(map[string]interface{})
	assert.True(t, isMap, "from must remain the structured object")
	assert.Equal(t, "https://x", p["link"])
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

func TestPublicURL(t *testing.T) {
	got := publicURL("iwh_abc", "tk")
	assert.Equal(t, "/v1/incoming-webhooks/iwh_abc/tk", got)
}

func TestGenerateWebhookID_HasPrefix(t *testing.T) {
	id := generateWebhookID()
	assert.Truef(t, len(id) > 4 && id[:4] == "iwh_", "id should start with iwh_, got %s", id)
}
