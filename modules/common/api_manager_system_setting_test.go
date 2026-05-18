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

func TestManagerSystemSetting_GetReturnsSchemaAndMaskedSecrets(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	db := newSystemSettingDB(ctx)
	require.NoError(t, db.upsert("register", "email_on", "1", settingTypeBool, ""))

	enc, err := encryptKey("super-secret")
	require.NoError(t, err)
	require.NoError(t, db.upsert("support", "email_pwd", enc, settingTypeEncrypted, ""))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/common/system_setting", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, `"schema":`, "must surface schema for UI to render form")
	assert.Contains(t, body, `"register"`)
	assert.Contains(t, body, `"email_on"`)
	assert.Contains(t, body, `"items":`)
	assert.NotContains(t, body, "super-secret", "encrypted values must NEVER be returned in cleartext")
	assert.Contains(t, body, "****", "encrypted columns must be masked")
}

func TestManagerSystemSetting_UpdateRequiresSuperAdmin(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, _ := testutil.NewTestServer()

	body := []byte(`{"items":[{"category":"register","key":"email_on","value":"1"}]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token) // plain user token — should be rejected
	s.GetRoute().ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusOK, w.Code, "non-superAdmin must not be able to write")
}

func TestManagerSystemSetting_UpdateRejectsUnknownKey(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	body := []byte(`{"items":[{"category":"register","key":"bogus_key","value":"1"}]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "bogus_key")
}

func TestManagerSystemSetting_UpdateRejectsInvalidBool(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	body := []byte(`{"items":[{"category":"register","key":"email_on","value":"yes"}]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestManagerSystemSetting_UpdateRejectsMixedCaseBool(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	body := []byte(`{"items":[{"category":"register","key":"email_on","value":"True"}]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestManagerSystemSetting_EncryptedEmptyDoesNotOverwrite(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	db := newSystemSettingDB(ctx)
	enc, err := encryptKey("original")
	require.NoError(t, err)
	require.NoError(t, db.upsert("support", "email_pwd", enc, settingTypeEncrypted, ""))

	body := []byte(`{"items":[{"category":"support","key":"email_pwd","value":""}]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	rows, err := db.listAll()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	plaintext, err := decryptKey(rows[0].Value)
	require.NoError(t, err)
	assert.Equal(t, "original", plaintext, "empty payload must preserve existing secret")
}

func TestManagerSystemSetting_EncryptedMaskDoesNotOverwrite(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	db := newSystemSettingDB(ctx)
	enc, err := encryptKey("original")
	require.NoError(t, err)
	require.NoError(t, db.upsert("support", "email_pwd", enc, settingTypeEncrypted, ""))

	body := []byte(`{"items":[{"category":"support","key":"email_pwd","value":"****"}]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	rows, err := db.listAll()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	plaintext, err := decryptKey(rows[0].Value)
	require.NoError(t, err)
	assert.Equal(t, "original", plaintext, "mask payload must preserve existing secret")
}

// Writing "" for a bool resets the setting to "fall back to yaml". This
// round-trip is the only way to revert an explicit DB override from the
// admin UI; cover it explicitly so the contract does not regress.
//
// Test infra quirk: octo-lib's register.GetModules caches the moduleList
// with sync.Once, so the Manager + Singleton are bound to the FIRST ctx
// passed across the test binary. To mutate the yaml fallback that the
// Manager sees, write through settings.ctx (the singleton's captured ctx)
// rather than the per-test ctx returned by NewTestServer.
func TestManagerSystemSetting_BoolEmptyValueResetsToYaml(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	// The Manager handler reads the yaml fallback through its singleton's
	// ctx, which may differ from this test's ctx (see comment above).
	settings := EnsureSystemSettings(ctx)
	originalEmailOn := settings.ctx.GetConfig().Register.EmailOn
	t.Cleanup(func() { settings.ctx.GetConfig().Register.EmailOn = originalEmailOn })
	settings.ctx.GetConfig().Register.EmailOn = true // yaml says enabled

	// DB explicitly says "0" (off).
	require.NoError(t, newSystemSettingDB(ctx).upsert(
		"register", "email_on", "0", settingTypeBool, "",
	))
	require.NoError(t, settings.Reload())
	require.False(t, settings.RegisterEmailOn(), "DB override 0 must win over yaml true")

	// Admin clears the override by POSTing an empty value.
	body := []byte(`{"items":[{"category":"register","key":"email_on","value":""}]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	rows, err := newSystemSettingDB(ctx).listAll()
	require.NoError(t, err)
	require.Len(t, rows, 1, "row should still exist with empty value")
	assert.Equal(t, "", rows[0].Value, "value column should be empty after reset POST")

	assert.True(t, settings.RegisterEmailOn(), `"" must clear the override and restore yaml default`)
}

func TestManagerSystemSetting_UpdatePersistsAndReloads(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	settings := EnsureSystemSettings(ctx)
	// Mutate via settings.ctx because octo-lib caches the moduleList with
	// sync.Once — see comment on TestManagerSystemSetting_BoolEmptyValueResetsToYaml
	// for why the per-test ctx may differ from the Manager's captured ctx.
	originalEmailOn := settings.ctx.GetConfig().Register.EmailOn
	t.Cleanup(func() { settings.ctx.GetConfig().Register.EmailOn = originalEmailOn })
	settings.ctx.GetConfig().Register.EmailOn = false
	require.NoError(t, settings.Reload())
	require.False(t, settings.RegisterEmailOn())

	payload := map[string]interface{}{
		"items": []map[string]string{
			{"category": "register", "key": "email_on", "value": "1"},
			{"category": "support", "key": "email_smtp", "value": "smtp.test:587"},
		},
	}
	raw, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(raw))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// The handler must call Reload — the test caller sees the new snapshot
	// without explicitly invoking Reload itself.
	assert.True(t, settings.RegisterEmailOn(), "Reload should run inside the update handler")
	assert.Equal(t, "smtp.test:587", settings.SupportEmailSmtp())
}
