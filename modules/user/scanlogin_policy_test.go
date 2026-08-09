package user

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	authInfo, err := encodeScanLoginAuthorization(testutil.UID, "disabled-existing-uuid")
	require.NoError(t, err)
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(
		scanLoginPendingAuthorizationKey("code"), authInfo, time.Minute))
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(
		scanLoginReadyAuthorizationKey("code"), authInfo, time.Minute))
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
			setPublicIPForUserTest(req, "9.9.8."+strconv.Itoa(i+1))
			if tc.token {
				req.Header.Set("token", testutil.Token)
			}
			s.GetRoute().ServeHTTP(w, req)

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "err.server.user.scan_login_disabled")
		})
	}
	pending, err := ctx.GetRedisConn().GetString(scanLoginPendingAuthorizationKey("code"))
	require.NoError(t, err)
	ready, err := ctx.GetRedisConn().GetString(scanLoginReadyAuthorizationKey("code"))
	require.NoError(t, err)
	assert.Equal(t, authInfo, pending, "disabled confirmation must not promote or delete an existing pending record")
	assert.Equal(t, authInfo, ready, "disabled redemption must not consume an existing ready record")
}

func TestScanLoginStatusReturnsDisabledStateImmediately(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "login", "scan_enabled", "0", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/user/loginstatus?uuid=existing-or-new", nil)
	setPublicIPForUserTest(req, "9.9.8.20")
	started := time.Now()
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"status":"`+scanLoginStatusDisabled+`"}`, w.Body.String())
	assert.Less(t, time.Since(started), time.Second, "disabled status must bypass the 10-second long poll")
}

func TestScanLoginRateLimitsRejectNonFiniteRPSConfig(t *testing.T) {
	t.Setenv("DM_API_SCANLOGIN_UUID_RATELIMIT_RPS", "NaN")
	t.Setenv("DM_API_SCANLOGIN_UUID_RATELIMIT_BURST", "1")
	t.Setenv("DM_API_SCANLOGIN_STATUS_RATELIMIT_RPS", "+Inf")
	t.Setenv("DM_API_SCANLOGIN_STATUS_RATELIMIT_BURST", "1")

	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	const ip = "9.9.8.21"
	for _, key := range []string{
		"ratelimit:strict:scanlogin_uuid:" + ip,
		"ratelimit:strict:scanlogin_status:" + ip,
	} {
		require.NoError(t, ctx.GetRedisConn().Del(key))
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "uuid NaN", path: "/v1/user/loginuuid"},
		{name: "status positive infinity", path: "/v1/user/loginstatus?uuid=missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for attempt := 1; attempt <= 2; attempt++ {
				w := httptest.NewRecorder()
				req, _ := http.NewRequest(http.MethodGet, tc.path, nil)
				setPublicIPForUserTest(req, ip)
				s.GetRoute().ServeHTTP(w, req)

				if attempt == 1 {
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					continue
				}
				require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), "err.shared.rate.limited")
			}
		})
	}
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

func TestScanLoginAuthorization_RequiresConfirmationAndRedeemsOnce(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	db := NewDB(ctx)
	model, err := db.QueryByUID(testutil.UID)
	require.NoError(t, err)
	if model == nil {
		require.NoError(t, db.Insert(&Model{
			UID:      testutil.UID,
			Name:     "scan login user",
			Username: "scan_login_user",
			ShortNo:  "scan001",
			Status:   1,
		}))
	}

	authCode := util.GenerUUID()
	uuid := util.GenerUUID()
	require.NoError(t, SavePendingScanLoginAuthorization(ctx.GetRedisConn(), authCode, testutil.UID, uuid))
	pollSecret, err := mintScanLoginPollSecret(ctx.GetRedisConn(), uuid)
	require.NoError(t, err)

	// Scanning alone creates no ready authorization, even for the browser that
	// owns the correct poll secret.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost,
		"/v1/user/login_authcode/"+authCode+"?poll_secret="+pollSecret, nil)
	setPublicIPForUserTest(req, "9.9.8.40")
	s.GetRoute().ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), "err.server.user.auth_code_not_found")
	pendingBeforeConfirm, err := ctx.GetRedisConn().GetString(scanLoginPendingAuthorizationKey(authCode))
	require.NoError(t, err)
	assert.NotEmpty(t, pendingBeforeConfirm)

	// The authenticated scanner explicitly confirms, atomically promoting the
	// pending record and publishing authed state to the browser.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/v1/user/grant_login?auth_code="+authCode, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	pendingAfterConfirm, err := ctx.GetRedisConn().GetString(scanLoginPendingAuthorizationKey(authCode))
	require.NoError(t, err)
	ready, err := ctx.GetRedisConn().GetString(scanLoginReadyAuthorizationKey(authCode))
	require.NoError(t, err)
	assert.Empty(t, pendingAfterConfirm)
	assert.Equal(t, pendingBeforeConfirm, ready)

	qrcodeRaw, err := ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", libcommon.QRCodeCachePrefix, uuid))
	require.NoError(t, err)
	var qrcodeState libcommon.QRCodeModel
	require.NoError(t, json.Unmarshal([]byte(qrcodeRaw), &qrcodeState))
	assert.Equal(t, string(libcommon.ScanLoginStatusAuthed), fmt.Sprint(qrcodeState.Data["status"]))

	// Exactly one redemption can issue a session. The second request observes
	// the atomic consume and cannot replay the same authorization.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost,
		"/v1/user/login_authcode/"+authCode+"?poll_secret="+pollSecret, nil)
	setPublicIPForUserTest(req, "9.9.8.41")
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"token"`)

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost,
		"/v1/user/login_authcode/"+authCode+"?poll_secret="+pollSecret, nil)
	setPublicIPForUserTest(req, "9.9.8.42")
	s.GetRoute().ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), "err.server.user.auth_code_not_found")
}
