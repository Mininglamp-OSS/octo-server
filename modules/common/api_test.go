package common

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddAppVersion_RequiresSuperAdmin pins the issue #363 gate: POST
// /v1/common/appversion (sets the client download source — supply-chain
// sensitive) must reject a plain admin and only let superAdmin through. The
// denial goes through the generic forbidden envelope (anti-enumeration).
func TestAddAppVersion_RequiresSuperAdmin(t *testing.T) {
	// testutil.NewTestServer already registers every module's routes via
	// module.Setup, so the appversion handler is live — don't call cn.Route
	// (double registration panics). It also does not wire the i18n renderer, so
	// mirror main.go here to get a populated error.code in the envelope.
	s, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	cacheCfg := ctx.GetConfig().Cache
	body := util.ToJson(&appVersionReq{
		AppVersion:  "9.9",
		OS:          "android",
		DownloadURL: "http://example.com/x.apk",
		IsForce:     1,
		UpdateDesc:  "x",
	})

	// plain admin → rejected by the generic forbidden envelope
	adminTok := "common-appver-admin"
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+adminTok, "admin-uid@admin@"+string(wkhttp.Admin)))
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/common/appversion", bytes.NewReader([]byte(body)))
	req.Header.Set("token", adminTok)
	s.GetRoute().ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), "err.shared.auth.forbidden", "plain admin must be rejected: %s", w.Body.String())

	// superAdmin → passes the role gate (downstream insert outcome is irrelevant
	// to this assertion; we only prove the gate let it through)
	superTok := "common-appver-superadmin"
	require.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+superTok, "root-uid@root@"+string(wkhttp.SuperAdmin)))
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/common/appversion", bytes.NewReader([]byte(body)))
	req2.Header.Set("token", superTok)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.NotContains(t, w2.Body.String(), "err.shared.auth.forbidden", "superAdmin must pass the role gate: %s", w2.Body.String())
}

func cleanAllTablesAndReloadSettings(t *testing.T, ctx *config.Context) {
	t.Helper()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, EnsureSystemSettings(ctx).Reload())
}

func TestAddVersion(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())
	//清除数据
	cleanAllTablesAndReloadSettings(t, ctx)
	w := httptest.NewRecorder()
	model := &appVersionReq{
		AppVersion:  "1.0",
		OS:          "android",
		DownloadURL: "http://www.githubim.com/download/test.apk",
		IsForce:     1,
		UpdateDesc:  "发布新版本",
	}
	req, _ := http.NewRequest("POST", "/v1/common/appversion", bytes.NewReader([]byte(util.ToJson(model))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetNewVersion(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	//清除数据
	cleanAllTablesAndReloadSettings(t, ctx)
	_, err := f.db.insertAppVersion(&appVersionModel{
		AppVersion:  "1.0",
		OS:          "android",
		DownloadURL: "http://www.githubim.com",
		IsForce:     1,
		UpdateDesc:  "发布新版本",
	})
	assert.NoError(t, err)

	_, err = f.db.insertAppVersion(&appVersionModel{
		AppVersion:  "1.2",
		OS:          "android",
		DownloadURL: "http://www.githubim.com",
		IsForce:     1,
		UpdateDesc:  "发布新版本",
	})
	assert.NoError(t, err)

	f.Route(s.GetRoute())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appversion/android/1.2", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"app_version":1.0`))
}

func TestGetAppConfig(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	//清除数据
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{
		WelcomeMessage:                 "欢迎使用DMWork",
		NewUserJoinSystemGroup:         1,
		RegisterInviteOn:               1,
		InviteSystemAccountJoinGroupOn: 1,
		SendWelcomeMessageOn:           1,
	})
	assert.NoError(t, err)
	//f.Route(s.GetRoute())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"invite_system_account_join_group_on":1`))
	// YUJ-219 / GH#1283: system_bot_uids 必须出现在 appconfig 响应里，
	// 作为三端消除 SYSTEM_BOTS 硬编码漂移的单一真源。
	body := w.Body.String()
	assert.Contains(t, body, `"system_bot_uids":`)
	assert.Contains(t, body, `"botfather"`)
	assert.Contains(t, body, `"u_10000"`)
	assert.Contains(t, body, `"fileHelper"`)
}

// YUJ-219 / GH#1283: 即使客户端带上相同 version 触发短路分支，
// appconfig 也必须回吐 system_bot_uids，避免客户端升级后因 version 命中
// 缓存短路永远拿不到新字段（旧客户端只跟 Version 走）。
func TestGetAppConfig_SystemBotUIDsOnVersionShortCircuit(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	// 带一个极大 version 强制命中短路分支
	req, _ := http.NewRequest("GET", "/v1/common/appconfig?version=99999999", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"system_bot_uids":`)
	assert.Contains(t, body, `"botfather"`)
	assert.Contains(t, body, `"u_10000"`)
	assert.Contains(t, body, `"fileHelper"`)
}

// 消息搜索开关下发：search_enabled 与 messages_search_on 必须同源同值。
// messages_search_on 是收敛后的真源 key（octo-web ChannelSearch 读它，与
// octo-admin messages_search 命名一致），search_enabled 保留作向后兼容。
// OCTO_SEARCH_BACKEND=es → 两者都为 true。
func TestGetAppConfig_MessagesSearchOn_Enabled(t *testing.T) {
	t.Setenv("OCTO_SEARCH_BACKEND", "es")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"messages_search_on":true`)
	assert.Contains(t, body, `"search_enabled":true`)
}

// OCTO_SEARCH_BACKEND=disabled → messages_search_on 与 search_enabled 均为
// false，前端据此隐藏 ChannelSearch 入口。
func TestGetAppConfig_MessagesSearchOn_Disabled(t *testing.T) {
	t.Setenv("OCTO_SEARCH_BACKEND", "disabled")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"messages_search_on":false`)
	assert.Contains(t, body, `"search_enabled":false`)
}

// version 短路分支同样要下发 messages_search_on：老客户端命中版本短路也必须
// 拿到当前开关，否则被缓存住失去实时性（与 search_enabled / disable_user_create_space
// 同样的「与 app_config.version 解耦」约束）。
func TestGetAppConfig_MessagesSearchOn_OnVersionShortCircuit(t *testing.T) {
	t.Setenv("OCTO_SEARCH_BACKEND", "es")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig?version=99999999", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"messages_search_on":true`)
	assert.Contains(t, body, `"search_enabled":true`)
}

// appconfig 必须下发 disable_user_create_space：默认 0（缺 system_setting 行且
// env 未设置）。客户端据此显示/隐藏「创建空间」入口；缺省必须保持开放。
func TestGetAppConfig_DisableUserCreateSpace_DefaultsZero(t *testing.T) {
	t.Setenv("DM_SPACE_DISABLE_USER_CREATE", "")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"disable_user_create_space":0`)
}

// DB 写入 disable_user_create=1 时 appconfig 必须下发 1，admin 在管理台切换
// 后客户端下次拉配置即可看到入口隐藏 —— 系统级 KV + Reload 路径的实时性保证。
func TestGetAppConfig_DisableUserCreateSpace_DBOverride(t *testing.T) {
	t.Setenv("DM_SPACE_DISABLE_USER_CREATE", "")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)

	settings := EnsureSystemSettings(ctx)
	assert.NoError(t, settings.db.upsert("space", "disable_user_create", "1", settingTypeBool, ""))
	assert.NoError(t, settings.Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"disable_user_create_space":1`)
}

// version 短路分支同样要下发 disable_user_create_space：老客户端命中版本
// 短路也必须看到当前开关，否则被缓存住失去"实时调整"的能力（与 LocalLoginOff
// 同样的"system_setting 与 app_config.version 解耦"约束）。
func TestGetAppConfig_DisableUserCreateSpace_OnVersionShortCircuit(t *testing.T) {
	t.Setenv("DM_SPACE_DISABLE_USER_CREATE", "")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)

	settings := EnsureSystemSettings(ctx)
	assert.NoError(t, settings.db.upsert("space", "disable_user_create", "1", settingTypeBool, ""))
	assert.NoError(t, settings.Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig?version=99999999", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"disable_user_create_space":1`)
}

// appconfig 必须下发 local_login_off：默认 0（缺 system_setting 行）。
func TestGetAppConfig_LocalLoginOff_DefaultsZero(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"local_login_off":0`)
}

// system_setting login.local_off=1 + 第三方登录配置齐备时，appconfig 必须
// 下发 local_login_off=1。OIDC 启用满足"第三方登录已配置"前置条件 —— 没有
// 这一步 LocalLoginOff() 会触发安全回退强行返回 false,前端会继续渲染本地
// 登录卡片。这条用例对齐"admin 在配齐 SSO 后才开关本地登录"的运维路径。
func TestGetAppConfig_LocalLoginOff_DBOverride(t *testing.T) {
	enableFullOIDCForTest(t)
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)

	settings := EnsureSystemSettings(ctx)
	assert.NoError(t, settings.db.upsert("login", "local_off", "1", settingTypeBool, ""))
	assert.NoError(t, settings.Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"local_login_off":1`)
}

// 版本号短路分支同样要下发 local_login_off：system_setting 与 app_config.version
// 解耦，老客户端命中版本短路也必须看到当前开关，避免被缓存住。
func TestGetAppConfig_LocalLoginOff_OnVersionShortCircuit(t *testing.T) {
	enableFullOIDCForTest(t)
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)

	settings := EnsureSystemSettings(ctx)
	assert.NoError(t, settings.db.upsert("login", "local_off", "1", settingTypeBool, ""))
	assert.NoError(t, settings.Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig?version=99999999", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"local_login_off":1`)
}

// PR #104 reviewer Jerry-Xin (P0 on commit 72e67c83): DM_OIDC_ENABLED 在
// appconfig 三个 OIDC 出口(oidc_providers / oidc_account_url /
// oidc_reset_password_url)上的解析必须与 LocalLoginOff() 安全回退完全一致,
// 否则会出现"local_login_off=1 但 oidc_providers 为空"的前端死锁组合:
// 前端按 local_login_off 隐藏本地登录卡片,又因 oidc_providers omitempty
// 拿不到 SSO 入口,用户无路可走。
//
// 触发场景:DM_OIDC_ENABLED=T(或 True / TRUE 等 ParseBool 合法但非 "true"/
// "1" 字面量) + 完整 OIDC + local_off=1。
func TestGetAppConfig_OIDCProvidersHonoursParseBoolSpellings(t *testing.T) {
	for _, spelling := range []string{"t", "T", "True", "TRUE"} {
		t.Run(spelling, func(t *testing.T) {
			enableFullOIDCForTest(t)
			t.Setenv("DM_OIDC_ENABLED", spelling)

			s, ctx := testutil.NewTestServer()
			f := New(ctx)
			cleanAllTablesAndReloadSettings(t, ctx)
			err := f.appConfigDB.insert(&appConfigModel{})
			assert.NoError(t, err)

			settings := EnsureSystemSettings(ctx)
			assert.NoError(t, settings.db.upsert("login", "local_off", "1", settingTypeBool, ""))
			assert.NoError(t, settings.Reload())

			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
			req.Header.Set("token", testutil.Token)
			s.GetRoute().ServeHTTP(w, req)
			body := w.Body.String()
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, body, `"local_login_off":1`,
				"DM_OIDC_ENABLED=%q + full OIDC + local_off=1 → 应下发 local_login_off=1", spelling)
			assert.Contains(t, body, `"oidc_providers"`,
				"同一个 DM_OIDC_ENABLED 值在 oidcEnabled() 与 isOIDCFullyConfigured() 必须解析一致,否则前端拿不到 SSO 入口")
		})
	}
}

// 安全回退在 appconfig 层同样要体现:DB 写了 local_off=1 但部署没配置任何
// SSO 时,下发的 local_login_off 必须为 0,否则前端会隐藏本地登录卡片导致
// 所有人都进不来。这条用例锁定"appconfig 返回的是 effective 值而不是 DB
// 原始值"这个语义,与 LocalLoginOff() getter 的安全回退一致。
func TestGetAppConfig_LocalLoginOff_SafetyOverrideWithoutSSO(t *testing.T) {
	clearOIDCEnvForTest(t)
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	disableThirdPartyLoginForTest(t, ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)

	settings := EnsureSystemSettings(ctx)
	assert.NoError(t, settings.db.upsert("login", "local_off", "1", settingTypeBool, ""))
	assert.NoError(t, settings.Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"local_login_off":0`,
		"无第三方登录配置 → 自动回退,前端继续显示本地登录")
}

func TestGetAppConfig_OIDCURLsExplicit(t *testing.T) {
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_ACCOUNT_URL", "https://accounts.example.com/")
	t.Setenv("DM_OIDC_RESET_PASSWORD_URL", "https://accounts.example.com/reset")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, body, `"oidc_account_url":"https://accounts.example.com/"`)
	assert.Contains(t, body, `"oidc_reset_password_url":"https://accounts.example.com/reset"`)
}

// 未显式配置 DM_OIDC_ACCOUNT_URL 时，回退到 issuer，避免重复维护两份 URL。
func TestGetAppConfig_OIDCAccountURLFallsBackToIssuer(t *testing.T) {
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_ACCOUNT_URL", "")
	t.Setenv("DM_OIDC_AEGIS_ISSUER", "https://accounts.imocto.cn")
	t.Setenv("DM_OIDC_RESET_PASSWORD_URL", "")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, body, `"oidc_account_url":"https://accounts.imocto.cn"`)
	assert.NotContains(t, body, "oidc_reset_password_url")
}

// 单 OIDC provider 元数据下发: provider id/name/authorize_path 让前端不再硬编码,
// 接入新 IdP（Aegis/Google/...）时只改部署 env 即可,前端无需改代码。
func TestGetAppConfig_OIDCProvidersWithCustomID(t *testing.T) {
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ID", "google")
	t.Setenv("DM_OIDC_PROVIDER_NAME", "Google")
	t.Setenv("DM_OIDC_ACCOUNT_URL", "https://accounts.google.com/")
	t.Setenv("DM_OIDC_RESET_PASSWORD_URL", "https://accounts.google.com/signin/recovery")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, body, `"oidc_providers":[`)
	assert.Contains(t, body, `"id":"google"`)
	assert.Contains(t, body, `"name":"Google"`)
	assert.Contains(t, body, `"authorize_path":"/v1/auth/oidc/google/authorize"`)
	assert.Contains(t, body, `"account_url":"https://accounts.google.com/"`)
	assert.Contains(t, body, `"reset_password_url":"https://accounts.google.com/signin/recovery"`)
}

// 未配置 PROVIDER_ID/NAME 时 provider 元数据回退到默认值,保证基础部署即可工作。
func TestGetAppConfig_OIDCProvidersDefaults(t *testing.T) {
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ID", "")
	t.Setenv("DM_OIDC_PROVIDER_NAME", "")
	t.Setenv("DM_OIDC_AEGIS_ISSUER", "https://accounts.imocto.cn")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, body, `"id":"oidc"`)
	assert.Contains(t, body, `"name":"SSO"`)
	assert.Contains(t, body, `"authorize_path":"/v1/auth/oidc/oidc/authorize"`)
}

// account_url 仅配 PROVIDER_ISSUER (新 key,无 ACCOUNT_URL/AEGIS_ISSUER) 时也要回退,
// 防止迁移到新 env 名后 account_url 变空。
func TestGetAppConfig_OIDCAccountURLFallsBackToProviderIssuer(t *testing.T) {
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_ACCOUNT_URL", "")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "https://accounts.example.com")
	t.Setenv("DM_OIDC_AEGIS_ISSUER", "")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, body, `"oidc_account_url":"https://accounts.example.com"`)
	assert.Contains(t, body, `"account_url":"https://accounts.example.com"`)
}

// 畸形 PROVIDER_ID 不应进 authorize_path,common 模块独立校验确保即便
// oidc 模块 LoadConfig 失败/未运行,appconfig 也不会下发坏值。
func TestGetAppConfig_OIDCProvidersInvalidIDFallsBackToDefault(t *testing.T) {
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ID", "bad/id")
	t.Setenv("DM_OIDC_PROVIDER_NAME", "Bad")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, body, `"id":"oidc"`)
	assert.NotContains(t, body, "bad/id")
	assert.Contains(t, body, `"authorize_path":"/v1/auth/oidc/oidc/authorize"`)
}

// OIDC 关闭时 oidc_providers 整个不下发,与已有 oidc_account_url/reset 保持一致行为。
func TestGetAppConfig_OIDCProvidersDisabledOmitted(t *testing.T) {
	t.Setenv("DM_OIDC_ENABLED", "false")
	t.Setenv("DM_OIDC_PROVIDER_ID", "google")
	t.Setenv("DM_OIDC_ACCOUNT_URL", "https://accounts.google.com/")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, body, "oidc_providers")
}

// OIDC 未启用时，即使 issuer/url 已配置也不下发，避免误导前端。
func TestGetAppConfig_OIDCDisabledOmitsAll(t *testing.T) {
	t.Setenv("DM_OIDC_ENABLED", "false")
	t.Setenv("DM_OIDC_ACCOUNT_URL", "https://accounts.example.com/")
	t.Setenv("DM_OIDC_AEGIS_ISSUER", "https://accounts.imocto.cn")
	t.Setenv("DM_OIDC_RESET_PASSWORD_URL", "https://accounts.example.com/reset")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, body, "oidc_account_url")
	assert.NotContains(t, body, "oidc_reset_password_url")
}

// YUJ-219-A / GH#1283 (analysis-report.md §4.2)：
// appconfig 下发 system_bot_uids，三端以后端 pkg/space.SystemBots 为单一真源，
// 替代各端硬编码（Android 只有 "botfather"、iOS 只有 botfatherUID）。
func TestGetAppConfig_SystemBotUIDsDownstreamed(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	err := f.appConfigDB.insert(&appConfigModel{})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	// 单一真源：后端 pkg/space.SystemBots 里的三个 UID 必须全部出现。
	assert.Contains(t, body, `"system_bot_uids":`)
	assert.Contains(t, body, `"botfather"`)
	assert.Contains(t, body, `"fileHelper"`)
	assert.Contains(t, body, `"u_10000"`)
}
