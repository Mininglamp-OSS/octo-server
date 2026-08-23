package user

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateAdminUserEmail covers the endpoint that makes manager 2FA usable at
// all: without it a deployment whose accounts predate the feature could never
// satisfy the enable guard.
func TestUpdateAdminUserEmail(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "admin-email-caller"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+callerToken,
		"root-uid@root@"+string(wkhttp.SuperAdmin)))

	db := NewDB(ctx)
	const adminUID = "admin-email-target"
	require.NoError(t, db.Insert(&Model{
		UID: adminUID, Username: "admin-email-target", Name: "Admin",
		Role: string(wkhttp.Admin), Status: StatusEnable.Int(),
	}))
	const plainUID = "plain-email-user"
	require.NoError(t, db.Insert(&Model{
		UID: plainUID, Username: "plain-email-user", Name: "Plain",
		Email: "taken@example.com", Status: StatusEnable.Int(),
	}))

	put := func(body map[string]interface{}) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/v1/manager/user/admin/email",
			bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("token", callerToken)
		req.Header.Set("Content-Type", "application/json")
		route.ServeHTTP(w, req)
		return w
	}

	t.Run("sets the address", func(t *testing.T) {
		w := put(map[string]interface{}{"uid": adminUID, "email": " OPS@Example.com "})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		stored, err := db.QueryByUID(adminUID)
		require.NoError(t, err)
		assert.Equal(t, "ops@example.com", stored.Email, "address must be normalised before storage")
	})

	t.Run("rejects an address another account already uses", func(t *testing.T) {
		w := put(map[string]interface{}{"uid": adminUID, "email": "taken@example.com"})
		assert.Contains(t, w.Body.String(), "err.server.user.manager_email_taken")
	})

	t.Run("rejects a malformed address", func(t *testing.T) {
		w := put(map[string]interface{}{"uid": adminUID, "email": "not-an-email"})
		assert.Contains(t, w.Body.String(), "err.server.user.email_invalid")
	})

	t.Run("refuses non-console accounts", func(t *testing.T) {
		w := put(map[string]interface{}{"uid": plainUID, "email": "other@example.com"})
		assert.Contains(t, w.Body.String(), "err.server.user.manager_permission_required")
	})

	t.Run("clears the address", func(t *testing.T) {
		w := put(map[string]interface{}{"uid": adminUID, "email": ""})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		stored, err := db.QueryByUID(adminUID)
		require.NoError(t, err)
		assert.Empty(t, stored.Email, "a mistyped address must be removable")
	})
}

// TestUpdateAdminUserEmailRequiresSuperAdmin pins the gate: this endpoint decides
// where second-factor codes are delivered, so a plain admin must not be able to
// redirect them.
func TestUpdateAdminUserEmailRequiresSuperAdmin(t *testing.T) {
	route, ctx, _ := newManagerRouteOnly(t)

	const callerToken = "admin-email-plain-admin"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+callerToken,
		"admin-uid@admin@"+string(wkhttp.Admin)))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/manager/user/admin/email",
		bytes.NewReader([]byte(`{"uid":"whoever","email":"attacker@example.com"}`)))
	req.Header.Set("token", callerToken)
	route.ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
}

func TestSeedManagerEmail(t *testing.T) {
	t.Run("empty when unset", func(t *testing.T) {
		t.Setenv("DM_MANAGER_ADMIN_EMAIL", "")
		assert.Empty(t, seedManagerEmail())
	})
	t.Run("normalised when valid", func(t *testing.T) {
		t.Setenv("DM_MANAGER_ADMIN_EMAIL", "  Ops@Example.COM ")
		assert.Equal(t, "ops@example.com", seedManagerEmail())
	})
	t.Run("dropped when malformed", func(t *testing.T) {
		// A typo must not be the reason a fresh deployment ends up with no
		// administrator at all.
		t.Setenv("DM_MANAGER_ADMIN_EMAIL", "nonsense")
		assert.Empty(t, seedManagerEmail())
	})
}
