package user

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webhook 合成发送者（iwh_）无自定义头像时，兜底必须复用 bot 默认头像逻辑：
// crc32(uid) 确定性选取 13 色内置 PNG（与 Robot==1 用户一致），而非通用生成式
// 默认头像——webhook 与 bot 同属"非真实用户"，视觉口径保持一致。
func TestWebhookAvatarFallback_UsesBotDefaultPalette(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ctx.GetConfig().Avatar.Default = ""
	ctx.GetConfig().Avatar.DefaultBaseURL = ""

	const uid = "iwh_palette_test_0001"
	resp := getAvatarForTest(t, s.GetRoute(), uid)
	want, err := readBotDefaultAvatar(uid)
	require.NoError(t, err)
	assert.Equal(t, want, resp.Body.Bytes(),
		"webhook avatar fallback must serve the deterministic bot-palette PNG")
}
