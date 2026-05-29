package user

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRespondExecLoginError pins the shared execLogin error classifier: a
// disabled account is 403, a missing device info is 400, the phone-verification
// sentinel keeps its bespoke 110 response, and any other (genuine internal)
// error collapses to the shared 500 — instead of every login path reporting
// these client states as a server failure.
func TestRespondExecLoginError(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	u := New(ctx)
	byKind := map[string]error{
		"disabled": ErrUserDisabled,
		"device":   ErrUserDeviceInfoRequired,
		"verify":   ErrUserNeedVerification,
		"internal": errors.New("boom"),
	}
	s.GetRoute().GET("/_test/execloginerr", func(c *wkhttp.Context) {
		u.respondExecLoginError(c, byKind[c.Query("kind")], &Model{UID: "u1", Phone: "13800001234"})
	})

	cases := []struct{ kind, wantContains string }{
		{"disabled", `"code":"err.server.user.account_banned"`},
		{"device", `"code":"err.server.user.request_invalid"`},
		{"verify", `"status":110`},
		{"internal", `"code":"err.server.user.store_failed"`},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/_test/execloginerr?kind="+tc.kind, nil)
			s.GetRoute().ServeHTTP(w, req)
			assert.Contains(t, w.Body.String(), tc.wantContains, "body=%s", w.Body.String())
		})
	}
}

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
