package common

import (
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
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

var oidcTestEnvKeys = []string{
	"DM_OIDC_ENABLED",
	"DM_OIDC_PROVIDER_ID",
	"DM_OIDC_PROVIDER_NAME",
	"DM_OIDC_PROVIDER_ISSUER",
	"DM_OIDC_PROVIDER_CLIENT_ID",
	"DM_OIDC_PROVIDER_CLIENT_SECRET",
	"DM_OIDC_PROVIDER_REDIRECT_URI",
	"DM_OIDC_AEGIS_ISSUER",
	"DM_OIDC_AEGIS_CLIENT_ID",
	"DM_OIDC_AEGIS_CLIENT_SECRET",
	"DM_OIDC_AEGIS_REDIRECT_URI",
	"DM_OIDC_RT_ENC_KEY",
	"DM_OIDC_ACCOUNT_URL",
	"DM_OIDC_RESET_PASSWORD_URL",
}

func clearOIDCEnvForTest(t *testing.T) {
	t.Helper()
	for _, k := range oidcTestEnvKeys {
		t.Setenv(k, "")
	}
}

// enableFullOIDCForTest 调用 t.Setenv 把整张 fullOIDCTestEnv 写进当前测试
// 的环境,Setenv 在 t.Cleanup 自动复原。先清空全部 OIDC 相关 env,防止外部
// 或前置测试残留绑定到测试场景,影响断言。
func enableFullOIDCForTest(t *testing.T) {
	t.Helper()
	clearOIDCEnvForTest(t)
	for k, v := range fullOIDCTestEnv {
		t.Setenv(k, v)
	}
}

func clearExternalLoginConfig(cfg *config.Config) {
	cfg.Github.ClientID = ""
	cfg.Github.ClientSecret = ""
	cfg.Gitee.ClientID = ""
	cfg.Gitee.ClientSecret = ""
}

func disableThirdPartyLoginForTest(t *testing.T, ctxs ...*config.Context) {
	t.Helper()
	clearOIDCEnvForTest(t)
	for _, ctx := range ctxs {
		if ctx != nil {
			clearExternalLoginConfig(ctx.GetConfig())
		}
	}
	sharedMu.Lock()
	shared := sharedSystemSettings
	sharedMu.Unlock()
	if shared != nil {
		clearExternalLoginConfig(shared.ctx.GetConfig())
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
	cfg := ctx.GetConfig()
	origGithub := cfg.Github
	origGitee := cfg.Gitee
	t.Cleanup(func() {
		cfg.Github = origGithub
		cfg.Gitee = origGitee
	})
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

// ----- incomingwebhook settings (总开关 + 核心阈值) -----

func TestSystemSettings_IncomingWebhookEnabled_DefaultsTrue(t *testing.T) {
	t.Setenv(envIncomingWebhookEnabled, "") // 证明开关纯由 DB / 默认驱动
	s := newTestSystemSettings(t, nil)
	assert.True(t, s.IncomingWebhookEnabled(), "DB+env 缺失时默认开启")
}

func TestSystemSettings_IncomingWebhookEnabled_DBOverridesToFalse(t *testing.T) {
	t.Setenv(envIncomingWebhookEnabled, "")
	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("incomingwebhook", "enabled", "0", settingTypeBool, ""))
	require.NoError(t, s.Reload())
	assert.False(t, s.IncomingWebhookEnabled(), "DB 0 必须压制默认 true")
}

func TestSystemSettings_IncomingWebhookEnabled_EnvFallbackWhenDBUnset(t *testing.T) {
	t.Setenv(envIncomingWebhookEnabled, "false")
	s := newTestSystemSettings(t, nil)
	assert.False(t, s.IncomingWebhookEnabled(), "DB 未配置 → env=false 生效")
}

func TestSystemSettings_IncomingWebhookThresholds_DefaultsAndDBOverride(t *testing.T) {
	t.Setenv(envIncomingWebhookPerWebhookRPS, "")
	t.Setenv(envIncomingWebhookPerWebhookBurst, "")
	t.Setenv(envIncomingWebhookMaxPerGroup, "")
	s := newTestSystemSettings(t, nil)

	// DB+env 缺失 → code default。
	assert.Equal(t, defaultIncomingWebhookPerWebhookRPS, s.IncomingWebhookPerWebhookRPS())
	assert.Equal(t, defaultIncomingWebhookPerWebhookBurst, s.IncomingWebhookPerWebhookBurst())
	assert.Equal(t, defaultIncomingWebhookMaxPerGroup, s.IncomingWebhookMaxPerGroup())

	// DB 覆盖（含 float rps）。
	require.NoError(t, s.db.upsert("incomingwebhook", "per_webhook_rps", "8.5", settingTypeFloat, ""))
	require.NoError(t, s.db.upsert("incomingwebhook", "per_webhook_burst", "25", settingTypeInt, ""))
	require.NoError(t, s.db.upsert("incomingwebhook", "max_per_group", "3", settingTypeInt, ""))
	require.NoError(t, s.Reload())
	assert.Equal(t, 8.5, s.IncomingWebhookPerWebhookRPS())
	assert.Equal(t, 25, s.IncomingWebhookPerWebhookBurst())
	assert.Equal(t, 3, s.IncomingWebhookMaxPerGroup())
}

func TestSystemSettings_IncomingWebhookRPS_EnvFallbackWhenDBUnset(t *testing.T) {
	t.Setenv(envIncomingWebhookPerWebhookRPS, "7")
	s := newTestSystemSettings(t, nil)
	assert.Equal(t, 7.0, s.IncomingWebhookPerWebhookRPS(), "DB 未配置 → env 生效")
}

// TestSystemSettings_IncomingWebhook_ReadSideClamp_NoInfra pins the read-side
// defence (#292 review): a snapshot carrying NaN / ±Inf / ≤0 — which a direct DB
// edit could introduce even though the admin write path now rejects them — must
// clamp back to the env/code default so the rate limiter never sees a value that
// would silently disable it. Drives the snapshot directly, no infra.
func TestSystemSettings_IncomingWebhook_ReadSideClamp_NoInfra(t *testing.T) {
	t.Setenv(envIncomingWebhookPerWebhookRPS, "")   // → default 5
	t.Setenv(envIncomingWebhookPerWebhookBurst, "") // → default 10
	t.Setenv(envIncomingWebhookMaxPerGroup, "")     // → default 10

	for _, bad := range []string{"NaN", "+Inf", "-Inf", "0", "-1", "-3.5"} {
		s := &SystemSettings{}
		snap := map[string]string{
			"incomingwebhook.per_webhook_rps":   bad,
			"incomingwebhook.per_webhook_burst": bad,
			"incomingwebhook.max_per_group":     bad,
		}
		s.snapshot.Store(&snap)
		assert.Equalf(t, defaultIncomingWebhookPerWebhookRPS, s.IncomingWebhookPerWebhookRPS(), "rps=%q must clamp to default", bad)
		assert.Equalf(t, defaultIncomingWebhookPerWebhookBurst, s.IncomingWebhookPerWebhookBurst(), "burst=%q must clamp to default", bad)
		assert.Equalf(t, defaultIncomingWebhookMaxPerGroup, s.IncomingWebhookMaxPerGroup(), "max_per_group=%q must clamp to default", bad)
	}

	// env-derived fallback is sanitized too: DM_INCOMINGWEBHOOK_RPS=NaN (which
	// ParseRPSFromEnv passes through) with no DB row must NOT reach the getter as
	// NaN — it falls back to the code default (Jerry-Xin #292 review).
	t.Setenv(envIncomingWebhookPerWebhookRPS, "NaN")
	emptySnap := &SystemSettings{}
	em := map[string]string{}
	emptySnap.snapshot.Store(&em)
	assert.Equal(t, defaultIncomingWebhookPerWebhookRPS, emptySnap.IncomingWebhookPerWebhookRPS(),
		"env=NaN with no DB row must fall back to the code default, not NaN")
	t.Setenv(envIncomingWebhookPerWebhookRPS, "")

	// A valid positive value is served as-is (clamp only catches the bad cases).
	s := &SystemSettings{}
	snap := map[string]string{
		"incomingwebhook.per_webhook_rps":   "8.5",
		"incomingwebhook.per_webhook_burst": "25",
		"incomingwebhook.max_per_group":     "3",
	}
	s.snapshot.Store(&snap)
	assert.Equal(t, 8.5, s.IncomingWebhookPerWebhookRPS())
	assert.Equal(t, 25, s.IncomingWebhookPerWebhookBurst())
	assert.Equal(t, 3, s.IncomingWebhookMaxPerGroup())
}

// TestSystemSettings_IncomingWebhook_GetterChain_NoInfra exercises the full
// snapshot(DB) → env → code-default fallback for the incomingwebhook getters
// WITHOUT a test server: it drives the snapshot map directly. This lets the
// core getter logic be verified even where MySQL is unavailable; the DB-backed
// tests above additionally cover the real Load/Reload path in CI.
func TestSystemSettings_IncomingWebhook_GetterChain_NoInfra(t *testing.T) {
	// 1) 空快照 + 无 env → code default。
	t.Setenv(envIncomingWebhookEnabled, "")
	t.Setenv(envIncomingWebhookPerWebhookRPS, "")
	t.Setenv(envIncomingWebhookPerWebhookBurst, "")
	t.Setenv(envIncomingWebhookMaxPerGroup, "")
	s := &SystemSettings{}
	empty := map[string]string{}
	s.snapshot.Store(&empty)
	assert.True(t, s.IncomingWebhookEnabled())
	assert.Equal(t, defaultIncomingWebhookPerWebhookRPS, s.IncomingWebhookPerWebhookRPS())
	assert.Equal(t, defaultIncomingWebhookPerWebhookBurst, s.IncomingWebhookPerWebhookBurst())
	assert.Equal(t, defaultIncomingWebhookMaxPerGroup, s.IncomingWebhookMaxPerGroup())

	// 2) env override（snapshot 仍空）。
	t.Setenv(envIncomingWebhookEnabled, "off")
	t.Setenv(envIncomingWebhookPerWebhookRPS, "7")
	t.Setenv(envIncomingWebhookPerWebhookBurst, "3")
	t.Setenv(envIncomingWebhookMaxPerGroup, "2")
	assert.False(t, s.IncomingWebhookEnabled())
	assert.Equal(t, 7.0, s.IncomingWebhookPerWebhookRPS())
	assert.Equal(t, 3, s.IncomingWebhookPerWebhookBurst())
	assert.Equal(t, 2, s.IncomingWebhookMaxPerGroup())

	// 3) snapshot(DB) 压制 env。
	snap := map[string]string{
		"incomingwebhook.enabled":           "1",
		"incomingwebhook.per_webhook_rps":   "8.5",
		"incomingwebhook.per_webhook_burst": "25",
		"incomingwebhook.max_per_group":     "9",
	}
	s.snapshot.Store(&snap)
	assert.True(t, s.IncomingWebhookEnabled())
	assert.Equal(t, 8.5, s.IncomingWebhookPerWebhookRPS())
	assert.Equal(t, 25, s.IncomingWebhookPerWebhookBurst())
	assert.Equal(t, 9, s.IncomingWebhookMaxPerGroup())
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
	disableThirdPartyLoginForTest(t, s.ctx)

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
	disableThirdPartyLoginForTest(t, s.ctx)
	t.Setenv("DM_OIDC_ENABLED", "true")
	// 故意只开 ENABLED,不配 issuer / client_id 等必填项。
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", "")
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "")
	t.Setenv("DM_OIDC_RT_ENC_KEY", "")

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

// ---------------------------------------------------------------------------
// Sidebar recent-tab activity filter windows (issue #289)
// ---------------------------------------------------------------------------

// Defaults reproduce today's hard-coded behaviour: groups/threads = 3-day
// window, DMs = unfiltered (0).
func TestSystemSettings_SidebarRecentFilter_Defaults(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	assert.Equal(t, 3, s.SidebarRecentFilterGroupDays(), "group window default 3 天")
	assert.Equal(t, 3, s.SidebarRecentFilterThreadDays(), "thread window default 3 天")
	assert.Equal(t, 0, s.SidebarRecentFilterPersonDays(), "DM 默认 0 = 不过滤")
}

// A configured DB value overrides the code default, including the 0 sentinel
// that disables the filter for that channel type.
func TestSystemSettings_SidebarRecentFilter_DBOverrides(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("sidebar", "recent_filter_group_days", "0", settingTypeInt, ""))
	require.NoError(t, s.db.upsert("sidebar", "recent_filter_thread_days", "7", settingTypeInt, ""))
	require.NoError(t, s.db.upsert("sidebar", "recent_filter_person_days", "30", settingTypeInt, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, 0, s.SidebarRecentFilterGroupDays(), "DB 0 → 关闭群过滤（全量）")
	assert.Equal(t, 7, s.SidebarRecentFilterThreadDays(), "DB 覆盖话题窗口")
	assert.Equal(t, 30, s.SidebarRecentFilterPersonDays(), "DB 覆盖 DM 窗口")
}

// Out-of-range DB values (someone editing the table directly, bypassing the
// admin API's range check) clamp back to the code default — defence in depth.
func TestSystemSettings_SidebarRecentFilter_OutOfRangeClampsToDefault(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("sidebar", "recent_filter_group_days", "-5", settingTypeInt, ""))
	require.NoError(t, s.db.upsert("sidebar", "recent_filter_thread_days", "999999", settingTypeInt, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, 3, s.SidebarRecentFilterGroupDays(), "负值越界 → 回退默认 3")
	assert.Equal(t, 3, s.SidebarRecentFilterThreadDays(), "超上限越界 → 回退默认 3")
}

// Boundary values exactly on [settingIntMin, settingIntMax] are accepted as-is.
func TestSystemSettings_SidebarRecentFilter_BoundaryValuesAccepted(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("sidebar", "recent_filter_group_days", "0", settingTypeInt, ""))
	require.NoError(t, s.db.upsert("sidebar", "recent_filter_thread_days", "3650", settingTypeInt, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, 0, s.SidebarRecentFilterGroupDays(), "下界 0 接受")
	assert.Equal(t, 3650, s.SidebarRecentFilterThreadDays(), "上界 3650 接受")
}
