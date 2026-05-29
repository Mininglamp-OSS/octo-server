package user

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsernameLoginBlockedByLocalLoginOff(t *testing.T) {
	// 必须先把 OIDC 切到完整可用状态,否则 LocalLoginOff() 的安全回退会把
	// local_off=1 视为"无 SSO 兜底的危险状态"强行返回 false,守卫就不会触发。
	// 这条用例只是验证守卫语义,不是验证安全回退本身 —— 后者归 modules/common
	// 的 TestSystemSettings_LocalLoginOff_AutoFalse* 系列。
	enableFullOIDCForUserTest(t)
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "login", "local_off", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/usernamelogin", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": "someuser12345",
		"password": "1234567",
	}))))
	setPublicIPForUserTest(req, "9.9.9.10")
	s.GetRoute().ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "本地登录已关闭")
}

func TestUsernameRegisterBlockedByRegisterOff(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "register", "off", "1", "bool")
	setSystemSettingForUserTest(t, ctx, "register", "username_on", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/user/usernameregister", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": "blockeduser",
		"password": "1234567",
		"name":     "blocked",
	}))))
	setPublicIPForUserTest(req, "9.9.9.9")
	s.GetRoute().ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "err.server.user.registration_closed")
}
