package user

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	appauth "github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The enable guard lives in modules/common (modules/user imports it, so the
// reverse edge would be an import cycle) but reads the `user` table, and only
// this package's test binary has the full migration set that creates it. So the
// guard is exercised here, through the settings endpoint it protects.

func seedConsoleAccountForGuard(t *testing.T, ctx *config.Context, uid, username, role, email string, status, isDestroy int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, username, name, short_no, role, manager_two_factor_email, status, is_destroy) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		uid, username, username, uid, role, email, status, isDestroy,
	).Exec()
	require.NoError(t, err)
}

// TestManager2FAEnableGuard pins the reason the guard exists: manager 2FA fails
// closed at sign-in, so letting the switch flip while addresses are missing
// would lock those administrators out of the very console needed to switch it
// back off.
//
// The seeded accounts also pin the guard's filter: every role the console admits
// counts, and accounts that cannot sign in anyway do not.
func TestManager2FAEnableGuard(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))
	t.Cleanup(func() {
		if _, err := ctx.DB().DeleteFrom("system_setting").
			Where("category='login' AND key_name='manager_2fa_on'").Exec(); err != nil {
			t.Logf("cleanup: delete manager_2fa_on failed: %v", err)
		}
		if err := commonsettings.EnsureSystemSettings(ctx).Reload(); err != nil {
			t.Logf("cleanup: reload settings failed: %v", err)
		}
	})

	seedConsoleAccountForGuard(t, ctx, "guard-super", "guard-superAdmin", string(wkhttp.SuperAdmin), "", 1, 0)
	seedConsoleAccountForGuard(t, ctx, "guard-reader", "guard-reader", appauth.ManagerRoleDashboardReader, "", 1, 0)
	seedConsoleAccountForGuard(t, ctx, "guard-ok", "guard-admin-ok", string(wkhttp.Admin), "ops@example.com", 1, 0)
	// None of these can sign in, so none of them can be locked out.
	seedConsoleAccountForGuard(t, ctx, "guard-disabled", "guard-disabled-admin", string(wkhttp.Admin), "", 0, 0)
	seedConsoleAccountForGuard(t, ctx, "guard-destroyed", "guard-destroyed-admin", string(wkhttp.Admin), "", 1, 2)
	seedConsoleAccountForGuard(t, ctx, "guard-plain", "guard-plain-user", "", "", 1, 0)

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
	body := w.Body.String()
	assert.Contains(t, body, "err.server.common.manager_2fa_email_unconfigured")
	assert.Contains(t, body, "guard-superAdmin", "the response must name the accounts to fix")
	assert.Contains(t, body, "guard-reader", "every console-capable role counts, not just admin∪superAdmin")
	assert.NotContains(t, body, "guard-admin-ok", "an account that already has an address is not a blocker")
	assert.NotContains(t, body, "guard-disabled-admin", "a disabled account cannot be locked out")
	assert.NotContains(t, body, "guard-destroyed-admin", "a destroyed account cannot be locked out")
	assert.NotContains(t, body, "guard-plain-user", "a non-console account is none of this switch's business")

	// Turning it OFF is never blocked — that is the recovery path for a
	// deployment that has locked itself out.
	off := post("0")
	require.Equal(t, http.StatusOK, off.Code, off.Body.String())

	// With every console account addressable, the switch flips.
	_, err := ctx.DB().UpdateBySql(
		"UPDATE `user` SET manager_two_factor_email=CONCAT(uid,'@example.com') WHERE manager_two_factor_email=''").Exec()
	require.NoError(t, err)
	enabled := post("1")
	require.Equal(t, http.StatusOK, enabled.Code, enabled.Body.String())
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())
	assert.True(t, commonsettings.EnsureSystemSettings(ctx).ManagerLogin2FAOn())
}
