package incomingwebhook

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/assert"
)

func TestIsWebhookUID(t *testing.T) {
	assert.True(t, isWebhookUID("iwh_d1fc093bc19c8a7fca754bcf607562d3"))
	assert.False(t, isWebhookUID("d1fc093bc19c8a7fca754bcf607562d3"))
	assert.False(t, isWebhookUID(""))
	assert.False(t, isWebhookUID("Iwh_x")) // 前缀大小写敏感
}

func TestWebhookDisplayName(t *testing.T) {
	assert.Equal(t, "GitHub Bot", webhookDisplayName(&incomingWebhookModel{Name: "GitHub Bot"}))
	// 空 / 纯空白名兜底到默认展示名。
	assert.Equal(t, defaultWebhookDisplayName, webhookDisplayName(&incomingWebhookModel{Name: ""}))
	assert.Equal(t, defaultWebhookDisplayName, webhookDisplayName(&incomingWebhookModel{Name: "   "}))
}

func TestNewWebhookChannelResp(t *testing.T) {
	m := &incomingWebhookModel{
		WebhookID: "iwh_abc",
		Name:      "GitHub Bot",
		Avatar:    "https://example.com/a.png",
		GroupNo:   "g_123",
		TokenHash: "SECRET_SHOULD_NOT_LEAK",
	}
	resp := newWebhookChannelResp(m)

	assert.Equal(t, "iwh_abc", resp.Channel.ChannelID)
	assert.Equal(t, common.ChannelTypePerson.Uint8(), resp.Channel.ChannelType)
	assert.Equal(t, "GitHub Bot", resp.Name)
	assert.Equal(t, "users/iwh_abc/avatar", resp.Logo)
	assert.Equal(t, extraKindValue, resp.Category)
	assert.Equal(t, 1, resp.Status)
	assert.Equal(t, extraKindValue, resp.Extra[extraKindKey])
	assert.Equal(t, "https://example.com/a.png", resp.Extra[extraAvatarKey])
	assert.Equal(t, "g_123", resp.Extra[extraGroupKey])
	// token_hash 绝不能进入对外频道详情。
	for k, v := range resp.Extra {
		assert.NotEqualf(t, "SECRET_SHOULD_NOT_LEAK", v, "token leaked via extra[%s]", k)
	}
}
