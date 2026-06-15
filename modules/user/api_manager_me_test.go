package user

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManagerCapabilities pins the /v1/manager/me capability map: superAdmin-only
// features must be false for a plain admin, while admin∪superAdmin features are
// true for both. Pure function — no server / DB needed.
func TestManagerCapabilities(t *testing.T) {
	super := managerCapabilities(true)
	admin := managerCapabilities(false)

	superOnly := []string{
		"system_setting", "backup", "appversion.write", "dashboard.trigger", "space.destructive",
		"users.write", "users.manage_admin", "groups.write",
	}
	adminTier := []string{
		"appversion.read", "dashboard.read", "users.read", "groups.read", "space.read", "space.write",
	}

	for _, k := range superOnly {
		if !super[k] {
			t.Errorf("superAdmin must have capability %q", k)
		}
		if admin[k] {
			t.Errorf("admin must NOT have superAdmin-only capability %q", k)
		}
	}
	for _, k := range adminTier {
		if !super[k] || !admin[k] {
			t.Errorf("admin-tier capability %q must be true for both admin and superAdmin", k)
		}
	}

	// Guard against a key being silently dropped/renamed out of the contract.
	if got, want := len(super), len(superOnly)+len(adminTier); got != want {
		t.Errorf("capability map has %d keys, want %d (update this test if the contract changed)", got, want)
	}
}

// meResponse mirrors managerMeResp for decoding the HTTP body.
type meResponse struct {
	UID          string          `json:"uid"`
	Name         string          `json:"name"`
	Role         string          `json:"role"`
	Capabilities map[string]bool `json:"capabilities"`
}

func getManagerMe(h http.Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/me", nil)
	req.Header.Set("token", testutil.Token)
	h.ServeHTTP(w, req)
	return w
}

// loginAsRole logs testutil.UID in with the given system role via the token
// cache (uid@name[@role]). The test server does not wire a RoleResolver (only
// main.go does), so the token's baked role is authoritative here — matching the
// setPlainUserToken / setAdminToken helpers in the opanalytics tests. role == ""
// omits the role segment → no system role → CheckLoginRole rejects.
func loginAsRole(t *testing.T, ctx *config.Context, role string) {
	t.Helper()
	val := testutil.UID + "@test"
	if role != "" {
		val += "@" + role
	}
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token, val))
}

// TestManagerMe_RejectsPlainUser pins the gate at the HTTP layer: a logged-in but
// non-manager user (passes auth, fails CheckLoginRole) must NOT reach /v1/manager/me.
// Complements the pure-function TestManagerCapabilities, which cannot catch a
// regression in the handler's role check.
func TestManagerMe_RejectsPlainUser(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	loginAsRole(t, ctx, "") // no system role
	w := getManagerMe(s.GetRoute())
	// Assert the specific forbidden outcome, not merely "not 200": respondManagerForbidden
	// renders err.shared.auth.forbidden, pinned to wire 400 (D14) with the shared
	// permission message. This rejects a 500/panic, 404 (route regression) or 401
	// masquerading as a passing auth test.
	assert.Equal(t, http.StatusBadRequest, w.Code, "forbidden is pinned to wire 400 (D14)")
	assert.Contains(t, w.Body.String(), "permission",
		"plain user must be rejected for the role reason (forbidden), not some other failure")
}

// TestManagerMe_AdminGetsReadCapsNotWrite verifies an admin is admitted and the
// echoed capability map reflects the read/write split: read tiers true, the
// superAdmin-only write tiers false — so the console won't render write buttons
// that the backend would 403.
func TestManagerMe_AdminGetsReadCapsNotWrite(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	loginAsRole(t, ctx, string(wkhttp.Admin))
	w := getManagerMe(s.GetRoute())
	require.Equal(t, http.StatusOK, w.Code)

	var resp meResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, string(wkhttp.Admin), resp.Role)
	assert.True(t, resp.Capabilities["users.read"], "admin should have users.read")
	assert.True(t, resp.Capabilities["groups.read"], "admin should have groups.read")
	assert.True(t, resp.Capabilities["space.write"], "admin should have space.write (admin-allowed space writes)")
	assert.False(t, resp.Capabilities["users.write"], "admin must NOT have users.write")
	assert.False(t, resp.Capabilities["users.manage_admin"], "admin must NOT have users.manage_admin")
	assert.False(t, resp.Capabilities["groups.write"], "admin must NOT have groups.write")
	assert.False(t, resp.Capabilities["space.destructive"], "admin must NOT have space.destructive")
	assert.False(t, resp.Capabilities["system_setting"], "admin must NOT have system_setting")
}

// TestManagerMe_SuperAdminGetsWriteCaps verifies a superAdmin is admitted and the
// write/destructive tiers come back true.
func TestManagerMe_SuperAdminGetsWriteCaps(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	loginAsRole(t, ctx, string(wkhttp.SuperAdmin))
	w := getManagerMe(s.GetRoute())
	require.Equal(t, http.StatusOK, w.Code)

	var resp meResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, string(wkhttp.SuperAdmin), resp.Role)
	assert.True(t, resp.Capabilities["users.write"], "superAdmin should have users.write")
	assert.True(t, resp.Capabilities["users.manage_admin"], "superAdmin should have users.manage_admin")
	assert.True(t, resp.Capabilities["groups.write"], "superAdmin should have groups.write")
	assert.True(t, resp.Capabilities["system_setting"], "superAdmin should have system_setting")
	assert.True(t, resp.Capabilities["users.read"], "superAdmin should have users.read")
}
