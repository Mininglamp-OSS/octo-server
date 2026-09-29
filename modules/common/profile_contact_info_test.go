package common

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProfileContactInfoSetting(t *testing.T) {
	s := &SystemSettings{}
	require.False(t, s.ProfileContactInfoOn(), "unavailable snapshot must fail closed")
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", false}, {"0", false}, {"false", false}, {"invalid", false}, {"1", true}, {"true", true}} {
		t.Run(tc.value, func(t *testing.T) {
			snapshot := map[string]string{"profile.contact_info_on": tc.value}
			s.snapshot.Store(&snapshot)
			assert.Equal(t, tc.want, s.ProfileContactInfoOn())
		})
	}
	snapshot := map[string]string{}
	s.snapshot.Store(&snapshot)
	assert.False(t, s.ProfileContactInfoOn(), "missing setting must default off")
}

func TestProfileContactInfoAppConfigAndManagerReload(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	cleanAllTablesAndReloadSettings(t, ctx)
	settings := EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		require.NoError(t, testutil.CleanAllTables(ctx))
		require.NoError(t, settings.Reload())
	})
	require.NoError(t, New(ctx).appConfigDB.insert(&appConfigModel{Version: 1}))
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin)))

	checkConfig := func(want bool) {
		t.Helper()
		for _, path := range []string{"/v1/common/appconfig", "/v1/common/appconfig?version=99999999"} {
			w := httptest.NewRecorder()
			s.GetRoute().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Contains(t, body, "profile_contact_info_on")
			var enabled bool
			require.NoError(t, json.Unmarshal(body["profile_contact_info_on"], &enabled))
			assert.Equal(t, want, enabled, path)
		}
	}
	checkConfig(false)
	for _, value := range []string{"1", "0", "invalid"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/manager/common/system_setting",
			bytes.NewBufferString(`{"items":[{"category":"profile","key":"contact_info_on","value":"`+value+`"}]}`))
		req.Header.Set("token", testutil.Token)
		req.Header.Set("Content-Type", "application/json")
		s.GetRoute().ServeHTTP(w, req)
		if value == "invalid" {
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		} else {
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		}
		checkConfig(value == "1")
	}
}
