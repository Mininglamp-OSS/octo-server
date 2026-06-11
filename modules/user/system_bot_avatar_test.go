package user

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSystemBotAvatarAssets 校验每个系统 Bot 专属头像：键是真正的系统 Bot、
// 资源可读、且为 512×512 PNG（与 13 色 bot 默认头像同规格）。
func TestSystemBotAvatarAssets(t *testing.T) {
	require.NotEmpty(t, systemBotAvatarFiles, "systemBotAvatarFiles must not be empty")
	for uid, path := range systemBotAvatarFiles {
		// 键必须是 pkg/space.SystemBots 里的系统 Bot——非系统账号不应走此分支
		// （普通 Bot 走 13 色头像，普通用户走昵称首字母）。
		require.Truef(t, spacepkg.IsSystemBot(uid), "systemBotAvatarFiles key %q is not a system bot", uid)

		data, ok := systemBotAvatar(uid)
		require.Truef(t, ok, "systemBotAvatar(%q) ok=false", uid)
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		require.NoErrorf(t, err, "decode %s", path)
		assert.Equalf(t, 512, cfg.Width, "%s width", path)
		assert.Equalf(t, 512, cfg.Height, "%s height", path)
	}
}

// TestSystemBotAvatarUnknown 未配专属图的 uid（含未配图的系统 Bot notification、
// 普通用户、空串）必须返回 ok=false，回退到默认头像逻辑。
func TestSystemBotAvatarUnknown(t *testing.T) {
	for _, uid := range []string{"notification", "u_normal", "27ba6or9nu_bot", ""} {
		_, ok := systemBotAvatar(uid)
		assert.Falsef(t, ok, "systemBotAvatar(%q) should be not found", uid)
	}
}

func TestSystemBotAvatarDeterministic(t *testing.T) {
	a, ok := systemBotAvatar("botfather")
	require.True(t, ok)
	b, ok := systemBotAvatar("botfather")
	require.True(t, ok)
	assert.True(t, bytes.Equal(a, b), "same system bot must resolve to the same avatar bytes")
}

// TestUserAvatarHandlerSystemBotBranding 端到端校验：UserAvatar 对配了专属图的
// 系统 Bot（botfather）返回该静态图，且在查库前返回——故意不往 DB 插 botfather。
func TestUserAvatarHandlerSystemBotBranding(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ctx.GetConfig().Avatar.Default = ""
	ctx.GetConfig().Avatar.DefaultBaseURL = ""

	resp := getAvatarForTest(t, s.GetRoute(), "botfather")
	want, ok := systemBotAvatar("botfather")
	require.True(t, ok)
	assert.Equal(t, want, resp.Body.Bytes())
	assert.Equal(t, "public, max-age=86400", resp.Header().Get("Cache-Control"))
}
