package common

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func TestAddVersion(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	f.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	_, err = f.db.insertAppVersion(&appVersionModel{
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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

func TestGetAppConfig_OIDCURLsExplicit(t *testing.T) {
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_ACCOUNT_URL", "https://accounts.example.com/")
	t.Setenv("DM_OIDC_RESET_PASSWORD_URL", "https://accounts.example.com/reset")
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = f.appConfigDB.insert(&appConfigModel{})
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
