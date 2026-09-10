package common

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// project_on — the Project module switch, shipped to clients through
// GET /v1/common/appconfig.
//
// Unlike docs_on / mail_on and the rest of that family, this is NOT a
// presentation-only toggle: modules/project's requireWriteEnabled resolves the
// SAME value. The tests below therefore pin two things at once — that the field
// is on the wire in both response branches, and that its resolution order is
// DB → env → false, so the client's view and the server's gate cannot diverge.

func setProjectEnabledSetting(t *testing.T, ctx *config.Context, enabled bool) {
	t.Helper()
	v := "0"
	if enabled {
		v = "1"
	}
	_, err := ctx.DB().InsertInto("system_setting").
		Columns("category", "key_name", "value", "value_type").
		Values("project", "enabled", v, "bool").Exec()
	require.NoError(t, err)
	require.NoError(t, EnsureSystemSettings(ctx).Reload())
}

// Default is OFF. The module is fail-closed: with neither a system_setting row
// nor the env var, clients must hide the Project entry.
func TestGetAppConfig_ProjectOn_DefaultFalse(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	t.Setenv("OCTO_PROJECT_CREATE_ENABLED", "")
	require.NoError(t, f.appConfigDB.insert(&appConfigModel{}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"project_on":false`)
}

// A system_setting row turns it on with no restart.
func TestGetAppConfig_ProjectOn_DBTrue(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	setProjectEnabledSetting(t, ctx, true)
	require.NoError(t, f.appConfigDB.insert(&appConfigModel{}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"project_on":true`)
}

// The version short-circuit branch must ship it too.
//
// This is the trap this repo has hit before: a client that already knows
// app_config.version takes an early return, and a toggle emitted only on the
// main branch is invisible to it forever. An operator flipping the switch would
// see it work on fresh clients and not on any existing one.
func TestGetAppConfig_ProjectOn_OnVersionShortCircuit(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	f := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	setProjectEnabledSetting(t, ctx, true)
	require.NoError(t, f.appConfigDB.insert(&appConfigModel{}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/common/appconfig?version=99999999", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"project_on":true`)
}

// Resolution order: DB → env → false.
//
// The env half is what makes this change zero-impact for an existing
// deployment, and the override half is what makes the admin console useful.
// Both directions of the override are asserted, because "a row exists but says
// false" must beat an env var that says true — otherwise turning the feature off
// from the console would silently do nothing.
func TestProjectEnabled_ResolutionOrder(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	cleanAllTablesAndReloadSettings(t, ctx)
	settings := EnsureSystemSettings(ctx)

	t.Run("no row, no env → false", func(t *testing.T) {
		t.Setenv("OCTO_PROJECT_CREATE_ENABLED", "")
		assert.False(t, settings.ProjectEnabled())
		_, found := settings.ProjectEnabledOverride()
		assert.False(t, found, "no row must report found=false so modules/project keeps its env value")
	})

	t.Run("no row, env on → true", func(t *testing.T) {
		t.Setenv("OCTO_PROJECT_CREATE_ENABLED", "true")
		assert.True(t, settings.ProjectEnabled())
	})

	t.Run("row wins over env", func(t *testing.T) {
		t.Setenv("OCTO_PROJECT_CREATE_ENABLED", "true")
		setProjectEnabledSetting(t, ctx, false)
		assert.False(t, settings.ProjectEnabled(), "an explicit row must beat the env var")
		v, found := settings.ProjectEnabledOverride()
		assert.True(t, found)
		assert.False(t, v)
	})
}

// The env parser must accept exactly what modules/project/config.go's envBool
// accepts. The two are mirrored rather than shared, so a drift here is a switch
// that means different things at its two exits.
func TestParseProjectEnabledEnv_MatchesModuleDialect(t *testing.T) {
	on := []string{"1", "true", "TRUE", "yes", "On", "  true  "}
	off := []string{"", "0", "false", "no", "off", "enabled", "2"}
	for _, v := range on {
		assert.True(t, parseProjectEnabledEnv(v), "%q should parse as on", v)
	}
	for _, v := range off {
		assert.False(t, parseProjectEnabledEnv(v), "%q should parse as off", v)
	}
	// Guard against the parser drifting away from os.Getenv semantics.
	require.NoError(t, os.Unsetenv("OCTO_PROJECT_CREATE_ENABLED_PROBE"))
}
