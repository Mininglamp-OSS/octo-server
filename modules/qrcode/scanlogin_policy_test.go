package qrcode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestNonLoginQRCodeRemainsAvailableWhenScanLoginDisabled(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	_, err := ctx.DB().InsertInto("system_setting").
		Columns("category", "key_name", "value", "value_type").
		Values("login", "scan_enabled", "0", "bool").Exec()
	require.NoError(t, err)
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/qrcode/user_"+testutil.UID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.NotContains(t, w.Body.String(), "err.server.user.scan_login_disabled")
}
