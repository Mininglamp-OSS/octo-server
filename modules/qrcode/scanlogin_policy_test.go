package qrcode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func enableScanLoginForQRCodeTest(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertInto("system_setting").
		Columns("category", "key_name", "value", "value_type").
		Values("login", "scan_enabled", "1", "bool").Exec()
	require.NoError(t, err)
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())
}

func setScanLoginEnabledForQRCodeTest(t *testing.T, enabled bool) (*httptest.ResponseRecorder, string) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	require.NoError(t, testutil.CleanAllTables(ctx))
	value := "0"
	if enabled {
		value = "1"
	}
	_, err := ctx.DB().InsertInto("system_setting").
		Columns("category", "key_name", "value", "value_type").
		Values("login", "scan_enabled", value, "bool").Exec()
	require.NoError(t, err)
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	code := util.GenerUUID()
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", libcommon.QRCodeCachePrefix, code),
		util.ToJson(libcommon.NewQRCodeModel(libcommon.QRCodeTypeScanLogin, map[string]interface{}{
			"app_id": "wukongchat",
			"status": libcommon.ScanLoginStatusWaitScan,
		})),
		time.Minute,
	))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/qrcode/"+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	return w, code
}

func TestScanLoginQRCodeRejectedWhenDisabled(t *testing.T) {
	w, _ := setScanLoginEnabledForQRCodeTest(t, false)
	assert.Contains(t, w.Body.String(), "err.server.user.scan_login_disabled")
}

func TestScanLoginQRCodeCreatesPendingStateOnly(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	enableScanLoginForQRCodeTest(t, ctx)

	code := util.GenerUUID()
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", libcommon.QRCodeCachePrefix, code),
		util.ToJson(libcommon.NewQRCodeModel(libcommon.QRCodeTypeScanLogin, map[string]interface{}{
			"app_id": "wukongchat",
			"status": libcommon.ScanLoginStatusWaitScan,
		})),
		time.Minute,
	))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/qrcode/"+code, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Data map[string]interface{} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	authCode, _ := body.Data["auth_code"].(string)
	require.NotEmpty(t, authCode)

	ready, err := ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", libcommon.AuthCodeCachePrefix, authCode))
	require.NoError(t, err)
	assert.Empty(t, ready, "scanning must not create a redeemable authorization")

	stored, err := ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", libcommon.QRCodeCachePrefix, code))
	require.NoError(t, err)
	var state libcommon.QRCodeModel
	require.NoError(t, json.Unmarshal([]byte(stored), &state))
	assert.Equal(t, string(libcommon.ScanLoginStatusScanned), fmt.Sprint(state.Data["status"]))
	assert.NotContains(t, state.Data, "uid")
	assert.NotContains(t, state.Data, "auth_code")
}

func TestScanLoginQRCodeRejectsRescanAfterStateLeavesWaitScan(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	require.NoError(t, testutil.CleanAllTables(ctx))
	enableScanLoginForQRCodeTest(t, ctx)

	code := util.GenerUUID()
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", libcommon.QRCodeCachePrefix, code),
		util.ToJson(libcommon.NewQRCodeModel(libcommon.QRCodeTypeScanLogin, map[string]interface{}{
			"app_id": "wukongchat",
			"status": libcommon.ScanLoginStatusWaitScan,
		})),
		time.Minute,
	))

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/v1/qrcode/"+code, nil)
	firstReq.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(first, firstReq)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodGet, "/v1/qrcode/"+code, nil)
	secondReq.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(second, secondReq)
	assert.NotEqual(t, http.StatusOK, second.Code, second.Body.String())
	assert.Contains(t, second.Body.String(), "err.server.qrcode.not_found")
}

func TestScanLoginQRCodePendingFlowsThroughConfirmationAndRedemption(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	require.NoError(t, testutil.CleanAllTables(ctx))
	enableScanLoginForQRCodeTest(t, ctx)

	db := user.NewDB(ctx)
	model, err := db.QueryByUID(testutil.UID)
	require.NoError(t, err)
	if model == nil {
		require.NoError(t, db.Insert(&user.Model{
			UID:      testutil.UID,
			Name:     "scan login cross module user",
			Username: "scan_login_cross_module",
			ShortNo:  "scan003",
			Status:   1,
		}))
	}

	create := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodGet, "/v1/user/loginuuid", nil)
	createReq.Header.Set("X-Forwarded-For", "9.9.10.1")
	createReq.RemoteAddr = "9.9.10.1:12345"
	s.GetRoute().ServeHTTP(create, createReq)
	require.Equal(t, http.StatusOK, create.Code, create.Body.String())
	var created struct {
		UUID       string `json:"uuid"`
		PollSecret string `json:"poll_secret"`
	}
	require.NoError(t, json.Unmarshal(create.Body.Bytes(), &created))
	require.NotEmpty(t, created.UUID)
	require.NotEmpty(t, created.PollSecret)

	scan := httptest.NewRecorder()
	scanReq := httptest.NewRequest(http.MethodGet, "/v1/qrcode/"+created.UUID, nil)
	scanReq.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(scan, scanReq)
	require.Equal(t, http.StatusOK, scan.Code, scan.Body.String())
	var scanned struct {
		Data map[string]interface{} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(scan.Body.Bytes(), &scanned))
	authCode, _ := scanned.Data["auth_code"].(string)
	require.NotEmpty(t, authCode)

	confirm := httptest.NewRecorder()
	confirmReq := httptest.NewRequest(http.MethodGet, "/v1/user/grant_login?auth_code="+authCode, nil)
	confirmReq.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(confirm, confirmReq)
	require.Equal(t, http.StatusOK, confirm.Code, confirm.Body.String())

	redeem := httptest.NewRecorder()
	redeemReq := httptest.NewRequest(http.MethodPost,
		"/v1/user/login_authcode/"+authCode+"?poll_secret="+created.PollSecret, nil)
	redeemReq.Header.Set("X-Forwarded-For", "9.9.10.2")
	redeemReq.RemoteAddr = "9.9.10.2:12345"
	s.GetRoute().ServeHTTP(redeem, redeemReq)
	require.Equal(t, http.StatusOK, redeem.Code, redeem.Body.String())
	assert.Contains(t, redeem.Body.String(), `"token"`)
}

func TestNonLoginQRCodeRemainsAvailableWhenScanLoginDisabled(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, user.NewDB(ctx).Insert(&user.Model{
		UID:      testutil.UID,
		Name:     "QR user",
		Username: "qr_user",
		ShortNo:  "qr001",
		Status:   1,
	}))
	_, err := ctx.DB().InsertInto("system_setting").
		Columns("category", "key_name", "value", "value_type").
		Values("login", "scan_enabled", "0", "bool").Exec()
	require.NoError(t, err)
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/qrcode/user_"+testutil.UID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "err.server.user.scan_login_disabled")
}
