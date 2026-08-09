package user

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScanLoginDisabledBlocksUserEntryPoints(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "login", "scan_enabled", "0", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	cases := []struct {
		name   string
		method string
		path   string
		token  bool
	}{
		{name: "create uuid", method: http.MethodGet, path: "/v1/user/loginuuid"},
		{name: "confirm", method: http.MethodGet, path: "/v1/user/grant_login?auth_code=code", token: true},
		{name: "redeem", method: http.MethodPost, path: "/v1/user/login_authcode/code?poll_secret=secret"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(tc.method, tc.path, nil)
			setPublicIPForUserTest(req, "9.9.8."+string(rune('1'+i)))
			if tc.token {
				req.Header.Set("token", testutil.Token)
			}
			s.GetRoute().ServeHTTP(w, req)

			assert.Contains(t, w.Body.String(), "err.server.user.scan_login_disabled")
		})
	}
}

func TestScanLoginStatusReturnsDisabledStateImmediately(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "login", "scan_enabled", "0", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/user/loginstatus?uuid=existing-or-new", nil)
	setPublicIPForUserTest(req, "9.9.8.20")
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"status":"disabled"}`, w.Body.String())
}

func TestLoginWithAuthCode_WrongPollSecretDoesNotConsumeAuthorization(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	authCode := util.GenerUUID()
	uuid := util.GenerUUID()
	authInfo, err := encodeScanLoginAuthorization(testutil.UID, uuid)
	require.NoError(t, err)
	authKey := fmt.Sprintf("%s%s", libcommon.AuthCodeCachePrefix, authCode)
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(authKey, authInfo, time.Minute))
	_, err = mintScanLoginPollSecret(ctx.GetRedisConn(), uuid)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost,
		"/v1/user/login_authcode/"+authCode+"?poll_secret=wrong", nil)
	setPublicIPForUserTest(req, "9.9.8.30")
	s.GetRoute().ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "err.server.user.auth_code_not_found")
	stillReady, err := ctx.GetRedisConn().GetString(authKey)
	require.NoError(t, err)
	assert.Equal(t, authInfo, stillReady, "wrong browser secret must not burn a valid confirmation")
}
