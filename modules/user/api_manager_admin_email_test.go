package user

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateAdminUserEmail covers the endpoint that makes manager 2FA usable at
// all: without it a deployment whose accounts predate the feature could never
// satisfy the enable guard.
func TestUpdateAdminTwoFactorEmail(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "admin-email-caller"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))

	db := NewDB(ctx)
	const adminUID = "admin-email-target"
	require.NoError(t, db.Insert(&Model{
		UID: adminUID, Username: "admin-email-target", Name: "Admin",
		// short_no carries a unique index; two rows without one collide.
		ShortNo: "admin-email-1",
		Role:    string(wkhttp.Admin), Status: StatusEnable.Int(),
	}))
	const plainUID = "plain-email-user"
	require.NoError(t, db.Insert(&Model{
		UID: plainUID, Username: "plain-email-user", Name: "Plain",
		ShortNo: "admin-email-2",
		Email:   "taken@example.com", Status: StatusEnable.Int(),
	}))

	put := func(body map[string]interface{}) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/v1/manager/user/admin/two_factor_email",
			bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("token", callerToken)
		req.Header.Set("Content-Type", "application/json")
		route.ServeHTTP(w, req)
		return w
	}

	t.Run("sets the address", func(t *testing.T) {
		w := put(map[string]interface{}{"uid": adminUID, "two_factor_email": " OPS@Example.com "})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		stored, err := db.QueryByUID(adminUID)
		require.NoError(t, err)
		assert.Equal(t, "ops@example.com", stored.ManagerTwoFactorEmail, "address must be normalised before storage")
	})

	t.Run("allows an address another account already uses", func(t *testing.T) {
		// Deliberate: nothing resolves an account by manager_two_factor_email, so
		// several administrators pointing at one shared ops mailbox is a supported
		// setup rather than a clash. (It would NOT be safe on user.email, which is
		// a login identity — which is exactly why the two are separate columns.)
		w := put(map[string]interface{}{"uid": adminUID, "two_factor_email": "taken@example.com"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		stored, err := db.QueryByUID(adminUID)
		require.NoError(t, err)
		assert.Equal(t, "taken@example.com", stored.ManagerTwoFactorEmail)
	})

	t.Run("rejects a malformed address", func(t *testing.T) {
		w := put(map[string]interface{}{"uid": adminUID, "two_factor_email": "not-an-email"})
		assert.Contains(t, w.Body.String(), "err.server.user.email_invalid")
	})

	t.Run("refuses non-console accounts", func(t *testing.T) {
		w := put(map[string]interface{}{"uid": plainUID, "two_factor_email": "other@example.com"})
		assert.Contains(t, w.Body.String(), "err.server.user.manager_permission_required")
	})

	t.Run("clears the address", func(t *testing.T) {
		w := put(map[string]interface{}{"uid": adminUID, "two_factor_email": ""})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		stored, err := db.QueryByUID(adminUID)
		require.NoError(t, err)
		assert.Empty(t, stored.ManagerTwoFactorEmail, "a mistyped address must be removable")
	})
}

// TestUpdateAdminUserEmailRequiresSuperAdmin pins the gate: this endpoint decides
// where second-factor codes are delivered, so a plain admin must not be able to
// redirect them.
func TestUpdateAdminTwoFactorEmailRequiresSuperAdmin(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "admin-email-plain-admin"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+callerToken,
		"admin-uid@admin@"+string(wkhttp.Admin)))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/manager/user/admin/two_factor_email",
		bytes.NewReader([]byte(`{"uid":"whoever","two_factor_email":"attacker@example.com"}`)))
	req.Header.Set("token", callerToken)
	route.ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
}

func TestSeedManagerTwoFactorEmail(t *testing.T) {
	t.Run("empty when unset", func(t *testing.T) {
		t.Setenv("DM_MANAGER_2FA_EMAIL", "")
		assert.Empty(t, seedManagerTwoFactorEmail())
	})
	t.Run("normalised when valid", func(t *testing.T) {
		t.Setenv("DM_MANAGER_2FA_EMAIL", "  Ops@Example.COM ")
		assert.Equal(t, "ops@example.com", seedManagerTwoFactorEmail())
	})
	t.Run("dropped when malformed", func(t *testing.T) {
		// A typo must not be the reason a fresh deployment ends up with no
		// administrator at all.
		t.Setenv("DM_MANAGER_2FA_EMAIL", "nonsense")
		assert.Empty(t, seedManagerTwoFactorEmail())
	})
}

// TestManagerTwoFactorWritePathsCannotStrandAnAccount pins the half of the
// lockout guard the enable-time check cannot cover.
//
// managerConsoleAccountsMissingEmail only runs on the OFF→ON transition, so
// without these two checks a SuperAdmin could clear an address — or create a new
// console account without one — while the second factor is live, and that
// account would fail closed at every sign-in from then on. If it were the last
// SuperAdmin, nobody could reach the console to turn the switch back off.
func TestManagerTwoFactorWritePathsCannotStrandAnAccount(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)
	setSystemSettingForUserTest(t, ctx, "login", "manager_2fa_on", "1", "bool")
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	const callerToken = "strand-guard-caller"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))

	const adminUID = "strand-guard-target"
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: adminUID, Username: "strand-guard-target", Name: "Admin",
		ShortNo: "strand-guard-1", Role: string(wkhttp.Admin),
		Status: StatusEnable.Int(), ManagerTwoFactorEmail: "ops@example.com",
	}))

	t.Run("clearing an address is refused while the factor is live", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/v1/manager/user/admin/two_factor_email",
			bytes.NewReader([]byte(util.ToJson(map[string]interface{}{"uid": adminUID, "two_factor_email": ""}))))
		req.Header.Set("token", callerToken)
		route.ServeHTTP(w, req)
		assert.Contains(t, w.Body.String(), "err.server.user.manager_2fa_email_missing")

		stored, err := NewDB(ctx).QueryByUID(adminUID)
		require.NoError(t, err)
		assert.Equal(t, "ops@example.com", stored.ManagerTwoFactorEmail, "the address must be left intact")
	})

	t.Run("creating an addressless console account is refused", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/manager/user/admin",
			bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
				"login_name": "strand-guard-new", "name": "New Admin", "password": "Adm1n-Passw0rd",
			}))))
		req.Header.Set("token", callerToken)
		route.ServeHTTP(w, req)
		assert.Contains(t, w.Body.String(), "err.server.user.manager_2fa_email_missing")
	})
}
