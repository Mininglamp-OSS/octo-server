package user

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func enableScanLoginForUserTest(t *testing.T, ctx *config.Context) {
	t.Helper()
	setSystemSettingForUserTest(t, ctx, "login", "scan_enabled", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())
}

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
	enableScanLoginForUserTest(t, ctx)

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
	enableScanLoginForUserTest(t, ctx)

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
	enableScanLoginForUserTest(t, ctx)

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
	var loginAudit LoginLogModel
	err = ctx.DB().Select("*").From("login_log").
		Where("uid=? AND status=? AND login_type=?", testutil.UID, loginStatusSuccess, "scan_login").
		LoadOne(&loginAudit)
	require.NoError(t, err)
	assert.Equal(t, "9.9.8.41", loginAudit.LoginIP)

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost,
		"/v1/user/login_authcode/"+authCode+"?poll_secret="+pollSecret, nil)
	setPublicIPForUserTest(req, "9.9.8.42")
	s.GetRoute().ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), "err.server.user.auth_code_not_found")
}

func TestScanLoginAuthorization_DisabledAccountCannotRedeemLiveCode(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	enableScanLoginForUserTest(t, ctx)

	db := NewDB(ctx)
	model, err := db.QueryByUID(testutil.UID)
	require.NoError(t, err)
	if model == nil {
		require.NoError(t, db.Insert(&Model{
			UID:      testutil.UID,
			Name:     "disabled scan login user",
			Username: "disabled_scan_login_user",
			ShortNo:  "scan003",
			Status:   int(libcommon.UserAvailable),
		}))
	}

	authCode := util.GenerUUID()
	uuid := util.GenerUUID()
	authInfo, err := encodeScanLoginAuthorization(testutil.UID, uuid)
	require.NoError(t, err)
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(scanLoginReadyAuthorizationKey(authCode), authInfo, time.Minute))
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(scanLoginUUIDClaimKey(uuid), authCode, time.Minute))
	pollSecret, err := mintScanLoginPollSecret(ctx.GetRedisConn(), uuid)
	require.NoError(t, err)
	_, err = db.session.Update("user").Set("status", int(libcommon.UserDisable)).Where("uid=?", testutil.UID).Exec()
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/user/login_authcode/"+authCode+"?poll_secret="+pollSecret, nil)
	setPublicIPForUserTest(req, "9.9.8.43")
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.user.account_banned")
}

func TestScanLoginAuthorization_MultipleReplicasRedeemOnce(t *testing.T) {
	s1, ctx := testutil.NewTestServer()
	s2, _ := testutil.NewTestServer()
	wireI18nRendererForUserTest(s1)
	wireI18nRendererForUserTest(s2)
	require.NoError(t, testutil.CleanAllTables(ctx))
	enableScanLoginForUserTest(t, ctx)

	db := NewDB(ctx)
	model, err := db.QueryByUID(testutil.UID)
	require.NoError(t, err)
	if model == nil {
		require.NoError(t, db.Insert(&Model{
			UID:      testutil.UID,
			Name:     "multi replica scan login user",
			Username: "multi_replica_scan_login_user",
			ShortNo:  "scan002",
			Status:   1,
		}))
	}

	authCode := util.GenerUUID()
	uuid := util.GenerUUID()
	require.NoError(t, SavePendingScanLoginAuthorization(ctx.GetRedisConn(), authCode, testutil.UID, uuid))
	pollSecret, err := mintScanLoginPollSecret(ctx.GetRedisConn(), uuid)
	require.NoError(t, err)

	routes := []http.Handler{s1.GetRoute(), s2.GetRoute()}
	callBoth := func(method string, path string, token bool) []*httptest.ResponseRecorder {
		t.Helper()
		responses := make([]*httptest.ResponseRecorder, len(routes))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(len(routes))
		for i, route := range routes {
			go func(i int, route http.Handler) {
				defer wg.Done()
				<-start
				w := httptest.NewRecorder()
				req := httptest.NewRequest(method, path, nil)
				setPublicIPForUserTest(req, "9.9.9."+strconv.Itoa(i+1))
				if token {
					req.Header.Set("token", testutil.Token)
				}
				route.ServeHTTP(w, req)
				responses[i] = w
			}(i, route)
		}
		close(start)
		wg.Wait()
		return responses
	}

	confirmResponses := callBoth(http.MethodGet, "/v1/user/grant_login?auth_code="+authCode, true)
	confirmSuccesses := 0
	for _, response := range confirmResponses {
		if response.Code == http.StatusOK {
			confirmSuccesses++
			continue
		}
		assert.Contains(t, response.Body.String(), "err.server.user.auth_code_not_found")
	}
	require.Equal(t, 1, confirmSuccesses, "only one replica may promote the pending authorization")
	statusStarted := time.Now()
	statusResponses := callBoth(http.MethodGet,
		"/v1/user/loginstatus?uuid="+uuid+"&poll_secret="+pollSecret, false)
	assert.Less(t, time.Since(statusStarted), time.Second,
		"a replica that reads an already confirmed shared state must not enter the long poll")
	for _, response := range statusResponses {
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), authCode,
			"every replica must read the confirmed state from shared Redis")
	}

	redeemResponses := callBoth(http.MethodPost,
		"/v1/user/login_authcode/"+authCode+"?poll_secret="+pollSecret, false)
	redeemSuccesses := 0
	for _, response := range redeemResponses {
		if response.Code == http.StatusOK {
			redeemSuccesses++
			assert.Contains(t, response.Body.String(), `"token"`)
			continue
		}
		assert.Contains(t, response.Body.String(), "err.server.user.auth_code_not_found")
	}
	require.Equal(t, 1, redeemSuccesses, "only one replica may consume and issue a session")
}

func TestScanLoginAuthorization_MultipleAuthCodesForOneUUIDRedeemOnce(t *testing.T) {
	s1, ctx := testutil.NewTestServer()
	s2, _ := testutil.NewTestServer()
	wireI18nRendererForUserTest(s1)
	wireI18nRendererForUserTest(s2)
	require.NoError(t, testutil.CleanAllTables(ctx))
	enableScanLoginForUserTest(t, ctx)

	db := NewDB(ctx)
	model, err := db.QueryByUID(testutil.UID)
	require.NoError(t, err)
	if model == nil {
		require.NoError(t, db.Insert(&Model{
			UID:      testutil.UID,
			Name:     "single QR claim user",
			Username: "single_qr_claim_user",
			ShortNo:  "scan004",
			Status:   1,
		}))
	}

	uuid := util.GenerUUID()
	authCodes := []string{util.GenerUUID(), util.GenerUUID()}
	for _, authCode := range authCodes {
		require.NoError(t, SavePendingScanLoginAuthorization(ctx.GetRedisConn(), authCode, testutil.UID, uuid))
	}
	pollSecret, err := mintScanLoginPollSecret(ctx.GetRedisConn(), uuid)
	require.NoError(t, err)

	routes := []http.Handler{s1.GetRoute(), s2.GetRoute()}
	callBoth := func(method string, paths []string, token bool) []*httptest.ResponseRecorder {
		t.Helper()
		responses := make([]*httptest.ResponseRecorder, len(routes))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(len(routes))
		for i, route := range routes {
			go func(i int, route http.Handler) {
				defer wg.Done()
				<-start
				w := httptest.NewRecorder()
				req := httptest.NewRequest(method, paths[i], nil)
				setPublicIPForUserTest(req, "9.9.11."+strconv.Itoa(i+1))
				if token {
					req.Header.Set("token", testutil.Token)
				}
				route.ServeHTTP(w, req)
				responses[i] = w
			}(i, route)
		}
		close(start)
		wg.Wait()
		return responses
	}

	confirmPaths := []string{
		"/v1/user/grant_login?auth_code=" + authCodes[0],
		"/v1/user/grant_login?auth_code=" + authCodes[1],
	}
	confirmResponses := callBoth(http.MethodGet, confirmPaths, true)
	confirmSuccesses := 0
	for _, response := range confirmResponses {
		if response.Code == http.StatusOK {
			confirmSuccesses++
			continue
		}
		assert.Contains(t, response.Body.String(), "err.server.user.auth_code_not_found")
	}
	require.Equal(t, 1, confirmSuccesses,
		"two auth codes for one QR session must not both become redeemable")

	redeemPaths := []string{
		"/v1/user/login_authcode/" + authCodes[0] + "?poll_secret=" + pollSecret,
		"/v1/user/login_authcode/" + authCodes[1] + "?poll_secret=" + pollSecret,
	}
	redeemResponses := callBoth(http.MethodPost, redeemPaths, false)
	redeemSuccesses := 0
	for _, response := range redeemResponses {
		if response.Code == http.StatusOK {
			redeemSuccesses++
			assert.Contains(t, response.Body.String(), `"token"`)
			continue
		}
		assert.Contains(t, response.Body.String(), "err.server.user.auth_code_not_found")
	}
	require.Equal(t, 1, redeemSuccesses,
		"one displayed QR session must issue at most one login session")
}
