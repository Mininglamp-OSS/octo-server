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
