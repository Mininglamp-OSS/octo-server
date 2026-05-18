package user

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThirdAuthcodeAllowedByRegisterOff(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "register", "off", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/thirdlogin/authcode", nil)
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "authcode")
}

func TestThirdAuthStatusAllowedByRegisterOff(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSystemSettingForUserTest(t, ctx, "register", "off", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	authcode := "third-auth-status-off"
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(ThirdAuthcodePrefix+authcode, "1", time.Minute))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/user/thirdlogin/authstatus?authcode="+authcode, nil)
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"status":0`)
}
