package botfather

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserKeyProfileContactInfoSessionOnly(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	route, ctx := newUserAPITestServer(t)
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		require.NoError(t, testutil.CleanAllTables(ctx))
		require.NoError(t, settings.Reload())
	})
	// Both trees use the same per-UID bucket. Reset only this fixture's owner.
	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	defer rds.Close()
	require.NoError(t, rds.Del("ratelimit:uid:"+testutil.UID).Err())
	owner, peer := testutil.UID, "contact_peer_"+util.GenerUUID()[:8]
	spaceID := "contact_space_" + util.GenerUUID()[:8]
	for _, uid := range []string{owner, peer} {
		insertTestUser(t, ctx, uid, uid)
		insertTestSpace(t, ctx, spaceID, uid)
		_, err := ctx.DB().Update("user").Set("phone", "13800001234").Set("email", uid+"@example.com").
			Set("zone", "0086").Where("uid=?", uid).Exec()
		require.NoError(t, err)
	}
	_, err := ctx.DB().InsertBySql("INSERT INTO system_setting (category, key_name, value, value_type, description) VALUES ('profile', 'contact_info_on', '1', 'bool', '') ON DUPLICATE KEY UPDATE value='1'").Exec()
	require.NoError(t, err)
	require.NoError(t, settings.Reload())
	key := mintUserAPIKeyInSpace(t, ctx, owner, spaceID)

	readProfile := func(req *http.Request) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		route.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Contains(t, body, "category", "a successful full profile must still be returned")
		return body
	}
	// Positive control: same actor, same peer and relation, real session route.
	session := httptest.NewRequest(http.MethodGet, "/v1/users/"+peer, nil)
	session.Header.Set("token", testutil.Token)
	human := readProfile(session)
	assert.Equal(t, "13800001234", human["phone"])
	assert.Equal(t, peer+"@example.com", human["email"])
	// A key cannot sidestep the tree restriction by selecting the session URL.
	for _, header := range []string{"token", "Authorization"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/users/"+peer, nil)
		if header == "Authorization" {
			req.Header.Set(header, "Bearer "+key)
		} else {
			req.Header.Set(header, key)
		}
		w := httptest.NewRecorder()
		route.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	}

	for _, query := range []string{"", "?space_id=&api_key_space_id=", "?group_no=unused"} {
		t.Run("key peer"+query, func(t *testing.T) {
			body := readProfile(userAPIRequest(t, http.MethodGet, "/v1/user/users/"+peer+query, key, nil))
			for _, field := range []string{"phone", "email", "zone", "phone_country_code"} {
				assert.NotContains(t, body, field)
			}
		})
	}
	// The new projection is off for keys, including self. Historical self-only
	// phone/email/zone remain compatible; the new country-code field is absent.
	self := readProfile(userAPIRequest(t, http.MethodGet, "/v1/user/users/"+owner, key, nil))
	assert.Equal(t, "13800001234", self["phone"])
	assert.Equal(t, owner+"@example.com", self["email"])
	assert.NotContains(t, self, "phone_country_code")

	// An unbound legacy key must be refused upstream, never mistaken for a
	// session because BoundSpaceID is empty. A query cannot supply its binding.
	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, "/v1/user/users/"+peer+"?space_id="+spaceID,
		mintUserAPIKey(t, ctx, owner), nil))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
