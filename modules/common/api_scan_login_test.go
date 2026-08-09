package common

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAppConfig_ScanLoginEnabledDefaultsFalse(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	cn := New(ctx)
	cleanAllTablesAndReloadSettings(t, ctx)
	require.NoError(t, cn.appConfigDB.insert(&appConfigModel{}))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/common/appconfig", nil)
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"scan_login_enabled":false`)
}

func TestGetAppConfig_ScanLoginDisabledInEveryResponseBranch(t *testing.T) {
	for _, path := range []string{
		"/v1/common/appconfig",
		"/v1/common/appconfig?version=99999999",
	} {
		t.Run(path, func(t *testing.T) {
			s, ctx := testutil.NewTestServer()
			cn := New(ctx)
			cleanAllTablesAndReloadSettings(t, ctx)
			require.NoError(t, cn.appConfigDB.insert(&appConfigModel{Version: 1}))

			settings := EnsureSystemSettings(ctx)
			require.NoError(t, settings.db.upsert("login", "scan_enabled", "0", settingTypeBool, ""))
			require.NoError(t, settings.Reload())

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, path, nil)
			s.GetRoute().ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), `"scan_login_enabled":false`)
		})
	}
}
