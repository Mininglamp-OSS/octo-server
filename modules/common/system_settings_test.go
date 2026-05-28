package common

import (
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullOIDCTestEnv 是测试用的"完整 OIDC 配置最小集",用法是
// for k, v := range fullOIDCTestEnv { t.Setenv(k, v) }。把 OIDC 切换为
// "完整可用"状态(对应 modules/oidc/config.go:loadProvider 的所有 required
// 必填项 + 32 字节 RT 加密 key),从而让 isOIDCFullyConfigured() 返回 true。
//
// 这是 system_settings_test.go 和 api_test.go 共用的 fixture。修改
// modules/oidc 的 required 列表时,这张表也要同步;反之亦然 —— 该常量缺项
// 会导致原本应通过的守卫用例静默回退,排查路径明显。
var fullOIDCTestEnv = map[string]string{
	"DM_OIDC_ENABLED":                "true",
	"DM_OIDC_PROVIDER_ISSUER":        "https://idp.example.com",
	"DM_OIDC_PROVIDER_CLIENT_ID":     "test-client",
	"DM_OIDC_PROVIDER_CLIENT_SECRET": "test-secret",
	"DM_OIDC_PROVIDER_REDIRECT_URI":  "https://app.example.com/oidc/callback",
	// 32 字节全零的 base64 编码 —— 仅供测试,不是真实密钥。
	"DM_OIDC_RT_ENC_KEY": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
}

// enableFullOIDCForTest 调用 t.Setenv 把整张 fullOIDCTestEnv 写进当前测试
// 的环境,Setenv 在 t.Cleanup 自动复原。先 unset alias(AEGIS_*)防止外部
// 残留绑定到测试场景,影响断言。
func enableFullOIDCForTest(t *testing.T) {
	t.Helper()
	for _, alias := range []string{
		"DM_OIDC_AEGIS_ISSUER",
		"DM_OIDC_AEGIS_CLIENT_ID",
		"DM_OIDC_AEGIS_CLIENT_SECRET",
		"DM_OIDC_AEGIS_REDIRECT_URI",
	} {
		t.Setenv(alias, "")
	}
	for k, v := range fullOIDCTestEnv {
		t.Setenv(k, v)
	}
}

// helper to construct a SystemSettings backed by the test DB plus the given
// yaml-side defaults applied to the context's config.
func newTestSystemSettings(t *testing.T, apply func(s *SystemSettings)) *SystemSettings {
	t.Helper()
	// Defensive reset: key_encryption_test.go intentionally Unsetenvs the
	// master key without restoring it, so any test running after it would
	// panic when NewTestServer triggers RSA private-key encryption. Reset
	// here so test order is irrelevant.
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := newSystemSettingDB(ctx)
	s := NewSystemSettings(ctx, db)
	require.NoError(t, s.Load())
	if apply != nil {
		apply(s)
	}
	return s
}

func TestSystemSettings_BoolFallsBackToYamlWhenUnset(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Register.EmailOn = true
	s.ctx.GetConfig().Register.Off = false
	require.NoError(t, s.Reload())

	assert.True(t, s.RegisterEmailOn(), "DB empty -> fall back to yaml true")
	assert.False(t, s.RegisterOff(), "DB empty -> fall back to yaml false")
}

func TestSystemSettings_BoolOverridesYamlWhenSet(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Register.EmailOn = true // yaml says on
	s.ctx.GetConfig().Register.Off = false    // yaml says open

	// Admin disables both via DB.
	require.NoError(t, s.db.upsert("register", "email_on", "0", settingTypeBool, ""))
	require.NoError(t, s.db.upsert("register", "off", "1", settingTypeBool, ""))
	require.NoError(t, s.Reload())

	assert.False(t, s.RegisterEmailOn(), "DB 0 must override yaml true")
	assert.True(t, s.RegisterOff(), "DB 1 must override yaml false")
}

func TestSystemSettings_LocalLoginOff_DefaultsFalse(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	assert.False(t, s.LocalLoginOff(), "DB 缺字段时默认 false（保持本地登录可用）")
}

func TestSystemSettings_LocalLoginOff_DBValueWins(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	// 让 OIDC 完整可用,DB local_off=1 才会通过安全回退实际生效。
	enableFullOIDCForTest(t)

	require.NoError(t, s.db.upsert("login", "local_off", "1", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.True(t, s.LocalLoginOff(), "DB=1 + OIDC 已配置 → 关闭本地登录")

	require.NoError(t, s.db.upsert("login", "local_off", "0", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.False(t, s.LocalLoginOff(), "DB=0 → 启用本地登录")
}

// 安全回退：DB local_off=1 但没有任何第三方登录配置时，LocalLoginOff() 必须
// 返回 false，否则会把整个系统锁死（前端隐藏本地登录卡片 + 后端拒绝本地登录
// 请求 = 无人可登录）。守卫此处而不是 panic：让服务能起来，admin 上去看日志
// 再修复 SSO 配置；管理面写入也按这个语义验证。
func TestSystemSettings_LocalLoginOff_AutoFalseWhenNoThirdPartyConfigured(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	t.Setenv("DM_OIDC_ENABLED", "")
	s.ctx.GetConfig().Github.ClientID = ""
	s.ctx.GetConfig().Github.ClientSecret = ""
	s.ctx.GetConfig().Gitee.ClientID = ""
	s.ctx.GetConfig().Gitee.ClientSecret = ""

	require.NoError(t, s.db.upsert("login", "local_off", "1", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.False(t, s.LocalLoginOff(),
		"DB=1 但无任何第三方登录配置 → 自动回退为 false，避免锁死")
}

// DM_OIDC_ENABLED=true 只是 OIDC 的开关位,真正能用还需要 issuer / client_id
// / client_secret / redirect_uri / rt_enc_key 等一批 env 齐备(详见
// modules/oidc/config.go:LoadConfig)。任一缺失,callback 在请求时会 404/500,
// 实际上不存在可用的第三方登录入口 —— 此时若 LocalLoginOff() 仍生效,前端隐藏
// 本地登录 + 后端拒绝本地登录 + SSO 跑不通 = 全员锁死。安全回退必须看到
// "OIDC 启用 但 config 残缺" 也算"无可用第三方登录"。
func TestSystemSettings_LocalLoginOff_AutoFalseWhenOIDCEnabledButMisconfigured(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	t.Setenv("DM_OIDC_ENABLED", "true")
	// 故意只开 ENABLED,不配 issuer / client_id 等必填项。
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "")
	t.Setenv("DM_OIDC_RT_ENC_KEY", "")
	// aegis alias 也清掉,避免 alias 兜底。
	t.Setenv("DM_OIDC_AEGIS_ISSUER", "")
	t.Setenv("DM_OIDC_AEGIS_CLIENT_ID", "")
	t.Setenv("DM_OIDC_AEGIS_CLIENT_SECRET", "")
	t.Setenv("DM_OIDC_AEGIS_REDIRECT_URI", "")
	s.ctx.GetConfig().Github.ClientID = ""
	s.ctx.GetConfig().Github.ClientSecret = ""
	s.ctx.GetConfig().Gitee.ClientID = ""
	s.ctx.GetConfig().Gitee.ClientSecret = ""

	require.NoError(t, s.db.upsert("login", "local_off", "1", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.False(t, s.LocalLoginOff(),
		"OIDC ENABLED 但配置残缺时,等同无可用第三方登录,必须回退避免锁死")
}

// PR #104 reviewer Jerry-Xin (P0): DM_OIDC_PROVIDER_ID 非法时,
// modules/oidc/config.go:loadProvider 会 fatal,api.go:119 把 cfg 置 nil,
// 整套 OIDC handler 被注册为 disabled (api.go:256)。此时 SSO 实际上不可用,
// 但本镜像如果只看 issuer/client_id/secret/redirect/rt_key 仍会返回 true,
// 配合 local_off=1 就锁死。镜像必须连 providerIDRe 一起守。
func TestSystemSettings_LocalLoginOff_AutoFalseWhenOIDCProviderIDInvalid(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	enableFullOIDCForTest(t)
	// 完整配置之上,把 provider ID 改成正则不允许的值(含 '/')。
	t.Setenv("DM_OIDC_PROVIDER_ID", "foo/bar")
	s.ctx.GetConfig().Github.ClientID = ""
	s.ctx.GetConfig().Github.ClientSecret = ""
	s.ctx.GetConfig().Gitee.ClientID = ""
	s.ctx.GetConfig().Gitee.ClientSecret = ""

	require.NoError(t, s.db.upsert("login", "local_off", "1", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.False(t, s.LocalLoginOff(),
		"非法 provider ID 让 OIDC handler 被禁用,等同无可用 SSO,必须回退")
}

// PR #104 reviewer yujiawei (P2): DM_OIDC_ENABLED 解析必须与
// modules/oidc/config.go:getBool 完全一致 —— 后者用 strconv.ParseBool,
// 接受 t/T/True/TRUE 等。镜像如果只识别 "true"/"1" 会出现 OIDC 实际在跑
// 但 isOIDCFullyConfigured 误判为关闭、safety override 错误打开本地登录。
func TestSystemSettings_LocalLoginOff_AcceptsParseBoolEnabledSpellings(t *testing.T) {
	for _, spelling := range []string{"t", "T", "True", "TRUE"} {
		t.Run(spelling, func(t *testing.T) {
			s := newTestSystemSettings(t, nil)
			enableFullOIDCForTest(t)
			t.Setenv("DM_OIDC_ENABLED", spelling)
			s.ctx.GetConfig().Github.ClientID = ""
			s.ctx.GetConfig().Github.ClientSecret = ""
			s.ctx.GetConfig().Gitee.ClientID = ""
			s.ctx.GetConfig().Gitee.ClientSecret = ""

			require.NoError(t, s.db.upsert("login", "local_off", "1", settingTypeBool, ""))
			require.NoError(t, s.Reload())
			assert.True(t, s.LocalLoginOff(),
				"DM_OIDC_ENABLED=%q 必须与 oidc/config.go 的 ParseBool 一致地识别为开启", spelling)
		})
	}
}

func TestSystemSettings_LocalLoginOff_TrueWhenGitHubConfigured(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	t.Setenv("DM_OIDC_ENABLED", "")
	s.ctx.GetConfig().Github.ClientID = "gh-client"
	s.ctx.GetConfig().Github.ClientSecret = "gh-secret"

	require.NoError(t, s.db.upsert("login", "local_off", "1", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.True(t, s.LocalLoginOff(), "GitHub OAuth 配置齐备 → 守卫生效")
}

func TestSystemSettings_LocalLoginOff_TrueWhenGiteeConfigured(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	t.Setenv("DM_OIDC_ENABLED", "")
	s.ctx.GetConfig().Gitee.ClientID = "gitee-client"
	s.ctx.GetConfig().Gitee.ClientSecret = "gitee-secret"

	require.NoError(t, s.db.upsert("login", "local_off", "1", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.True(t, s.LocalLoginOff(), "Gitee OAuth 配置齐备 → 守卫生效")
}

// SpaceDisableUserCreate 的 fallback 链：DB → env DM_SPACE_DISABLE_USER_CREATE → false。
// 默认无 DB 行且无 env 时,返回 false（保持历史行为：用户侧可创建空间）。
func TestSystemSettings_SpaceDisableUserCreate_DefaultsFalse(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	t.Setenv("DM_SPACE_DISABLE_USER_CREATE", "")
	assert.False(t, s.SpaceDisableUserCreate(),
		"DB 未配置且 env 未设置时必须保持开放（false）")
}

// DB 值优先于 env：admin 在管理台显式关闭时,即便 env 未设置也立即生效。
func TestSystemSettings_SpaceDisableUserCreate_DBTrueWins(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	t.Setenv("DM_SPACE_DISABLE_USER_CREATE", "")
	require.NoError(t, s.db.upsert("space", "disable_user_create", "1", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.True(t, s.SpaceDisableUserCreate(),
		"DB=1 必须关闭用户侧创建入口")
}

// DB 行存在但值为 0 时必须明确返回 false,即使 env 设置为 true 也不再"漏出去"。
// 这一条用例固化"DB 是单一真源"的语义：admin 在管理台 toggle 回 0 必须能压住
// 历史 env 配置；否则运维改了配置却看不到效果。
func TestSystemSettings_SpaceDisableUserCreate_DBFalseOverridesEnv(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	t.Setenv("DM_SPACE_DISABLE_USER_CREATE", "true")
	require.NoError(t, s.db.upsert("space", "disable_user_create", "0", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.False(t, s.SpaceDisableUserCreate(),
		"DB=0 必须覆盖 env=true（DB 是单一真源）")
}

// DB 行存在但 value 为空字符串时, lookup 视为"未配置"(与其它所有设置一致),
// 落回 env fallback —— 不是把 disable_user_create 强制 false。这条用例锁定
// "DB row 空值 == 未配置" 的语义,与其他 bool 设置(register/login/support)
// 共享同一回退规则,避免后续维护者误以为空值压制 env。
func TestSystemSettings_SpaceDisableUserCreate_DBEmptyValueFallsBackToEnv(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	t.Setenv("DM_SPACE_DISABLE_USER_CREATE", "true")
	require.NoError(t, s.db.upsert("space", "disable_user_create", "", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.True(t, s.SpaceDisableUserCreate(),
		"DB 值=\"\" 视为未配置, env=true 必须生效")
}

// 仅设置了 env 而无 DB 行时,getter 必须回退到 env 解析结果,保持对历史部署的
// 兼容（envDisableUserCreateSpace 是这个开关的原始入口）。
func TestSystemSettings_SpaceDisableUserCreate_EnvFallback(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	for _, spelling := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		t.Run(spelling, func(t *testing.T) {
			t.Setenv("DM_SPACE_DISABLE_USER_CREATE", spelling)
			assert.True(t, s.SpaceDisableUserCreate(),
				"env=%q 必须被识别为开启", spelling)
		})
	}
}

func TestSystemSettings_SpaceDisableUserCreate_EnvNegativeSpellings(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	for _, spelling := range []string{"0", "false", "FALSE", "no", "random"} {
		t.Run(spelling, func(t *testing.T) {
			t.Setenv("DM_SPACE_DISABLE_USER_CREATE", spelling)
			assert.False(t, s.SpaceDisableUserCreate(),
				"env=%q 不应被识别为开启", spelling)
		})
	}
}

func TestSystemSettings_StringFallsBackOnEmpty(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Support.EmailSmtp = "smtp.yaml.example:465"

	// No DB row -> yaml fallback.
	assert.Equal(t, "smtp.yaml.example:465", s.SupportEmailSmtp())

	// Empty DB value still triggers fallback (treated as "not configured").
	require.NoError(t, s.db.upsert("support", "email_smtp", "", settingTypeString, ""))
	require.NoError(t, s.Reload())
	assert.Equal(t, "smtp.yaml.example:465", s.SupportEmailSmtp())

	// Non-empty DB value wins.
	require.NoError(t, s.db.upsert("support", "email_smtp", "smtp.db.example:587", settingTypeString, ""))
	require.NoError(t, s.Reload())
	assert.Equal(t, "smtp.db.example:587", s.SupportEmailSmtp())
}

func TestSystemSettings_EncryptedRoundTrip(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Support.EmailPwd = "yaml-fallback"

	// Store encrypted; helper must decrypt on read.
	enc, err := encryptKey("real-smtp-password")
	require.NoError(t, err)
	require.NoError(t, s.db.upsert("support", "email_pwd", enc, settingTypeEncrypted, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, "real-smtp-password", s.SupportEmailPwd())
}

func TestSystemSettings_EncryptedDecryptFailureFallsBackToYaml(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Support.EmailPwd = "yaml-pwd"

	// Corrupted ciphertext (enc: prefix but invalid body).
	require.NoError(t, s.db.upsert("support", "email_pwd", "enc:not-real-base64", settingTypeEncrypted, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, "yaml-pwd", s.SupportEmailPwd(), "decryption failure must fall back to yaml, not panic")
}

func TestSystemSettings_ReloadRefreshesSnapshot(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Register.EmailOn = false

	require.NoError(t, s.db.upsert("register", "email_on", "1", settingTypeBool, ""))
	// Before reload, snapshot still empty -> yaml.
	assert.False(t, s.RegisterEmailOn())

	require.NoError(t, s.Reload())
	assert.True(t, s.RegisterEmailOn())
}

func TestSystemSettings_ConcurrentReadsAndReloads(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Register.EmailOn = false
	require.NoError(t, s.db.upsert("register", "email_on", "1", settingTypeBool, ""))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = s.RegisterEmailOn()
				_ = s.SupportEmailSmtp()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Reload()
			}
		}()
	}
	wg.Wait()
}
