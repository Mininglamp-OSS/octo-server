package common

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerConsoleAccountsMissingEmail(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	insert := func(uid, username, role, email string, status, isDestroy int) {
		t.Helper()
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO `user` (uid, username, name, role, email, status, is_destroy) VALUES (?, ?, ?, ?, ?, ?, ?)",
			uid, username, username, role, email, status, isDestroy,
		).Exec()
		require.NoError(t, err)
	}

	insert("u-super", "superAdmin", string(wkhttp.SuperAdmin), "", 1, 0)
	insert("u-admin-ok", "admin-ok", string(wkhttp.Admin), "ops@example.com", 1, 0)
	insert("u-reader", "reader", auth.ManagerRoleDashboardReader, "", 1, 0)
	// Accounts that cannot sign in are none of the guard's business.
	insert("u-disabled", "disabled-admin", string(wkhttp.Admin), "", 0, 0)
	insert("u-destroyed", "destroyed-admin", string(wkhttp.Admin), "", 1, 2)
	insert("u-plain", "plain-user", "", "", 1, 0)

	missing, err := managerConsoleAccountsMissingEmail(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"superAdmin", "reader"}, missing,
		"only sign-in-capable console accounts without an address may be reported")
}

// TestManagerSystemSetting_Enable2FABlockedWithoutEmails pins the reason the
// guard exists: manager 2FA fails closed at sign-in, so letting the switch flip
// while addresses are missing would lock those administrators out of the very
// console needed to switch it back off.
func TestManagerSystemSetting_Enable2FABlockedWithoutEmails(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, username, name, role, email, status, is_destroy) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"u-super", "superAdmin", "superAdmin", string(wkhttp.SuperAdmin), "", 1, 0,
	).Exec()
	require.NoError(t, err)

	post := func(value string) *httptest.ResponseRecorder {
		t.Helper()
		body := []byte(`{"items":[{"category":"login","key":"manager_2fa_on","value":"` + value + `"}]}`)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	w := post("1")
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.common.manager_2fa_email_unconfigured")
	assert.Contains(t, w.Body.String(), "superAdmin", "the response must name the accounts to fix")

	// Turning it OFF is never blocked — that is the recovery path.
	assert.Equal(t, http.StatusOK, post("0").Code)

	// With an address on file the switch flips.
	_, err = ctx.DB().UpdateBySql("UPDATE `user` SET email='ops@example.com' WHERE uid='u-super'").Exec()
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, post("1").Code)
	require.NoError(t, EnsureSystemSettings(ctx).Reload())
	assert.True(t, EnsureSystemSettings(ctx).ManagerLogin2FAOn())
}
