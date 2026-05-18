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

func TestUsernameRegisterBlockedByRegisterOff(t *testing.T) {
	s, ctx := testutil.NewTestServer()
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

	assert.Contains(t, w.Body.String(), "注册通道暂不开放")
}
