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
	assert.Contains(t, body, `"configured"`, "GET must report whether each setting is explicitly configured")
	assert.Contains(t, body, `"effective_value"`, "GET must report the effective (DB→yaml→default) value")
	assert.NotContains(t, body, "super-secret", "encrypted values must NEVER be returned in cleartext")
	assert.Contains(t, body, "****", "encrypted columns must be masked")
}

// GET must return:
//   - configured=true + value=DB value for explicitly configured rows
//   - configured=false + value="" for unconfigured rows
//   - effective_value reflecting DB → yaml → code-default for every row
//   - encrypted plaintext (from yaml or DB) is masked, never leaked
func TestManagerSystemSetting_GetReturnsEffectiveValues(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	// Mutate via the singleton's captured ctx — see the BoolEmptyValueResetsToYaml
	// test for the test-infra rationale.
	settings := EnsureSystemSettings(ctx)
	cfg := settings.ctx.GetConfig()
	origRegEmailOn := cfg.Register.EmailOn
	origRegOff := cfg.Register.Off
	origSupportEmail := cfg.Support.Email
	origSupportPwd := cfg.Support.EmailPwd
	t.Cleanup(func() {
		cfg.Register.EmailOn = origRegEmailOn
		cfg.Register.Off = origRegOff
		cfg.Support.Email = origSupportEmail
		cfg.Support.EmailPwd = origSupportPwd
	})
	// yaml provides defaults the DB will not override.
	cfg.Register.EmailOn = true
	cfg.Register.Off = false
	cfg.Support.Email = "yaml-default@example.com"
	cfg.Support.EmailPwd = "yaml-fallback-secret"

	// DB explicitly overrides one bool and one string.
	db := newSystemSettingDB(ctx)
	require.NoError(t, db.upsert("register", "email_on", "0", settingTypeBool, ""))
	require.NoError(t, db.upsert("support", "email_smtp", "smtp.db:587", settingTypeString, ""))
	require.NoError(t, settings.Reload())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/common/system_setting", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Items []struct {
			Category       string `json:"category"`
			Key            string `json:"key"`
			Configured     bool   `json:"configured"`
			Value          string `json:"value"`
			EffectiveValue string `json:"effective_value"`
			ValueType      string `json:"value_type"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	byKey := map[string]struct {
		Configured     bool
		Value          string
		EffectiveValue string
	}{}
	for _, it := range resp.Items {
		byKey[it.Category+"."+it.Key] = struct {
			Configured     bool
			Value          string
			EffectiveValue string
		}{it.Configured, it.Value, it.EffectiveValue}
	}

	// DB-overridden bool: configured + DB value wins.
	got := byKey["register.email_on"]
	assert.True(t, got.Configured, "DB row present → configured=true")
	assert.Equal(t, "0", got.Value)
	assert.Equal(t, "0", got.EffectiveValue, "DB override 0 must be the effective value")

	// Unconfigured bool: falls back to yaml.
	got = byKey["register.off"]
	assert.False(t, got.Configured)
	assert.Equal(t, "", got.Value)
	assert.Equal(t, "0", got.EffectiveValue, "yaml false → effective_value=\"0\"")

	// Unconfigured string: yaml default surfaces in effective_value.
	got = byKey["support.email"]
	assert.False(t, got.Configured)
	assert.Equal(t, "", got.Value)
	assert.Equal(t, "yaml-default@example.com", got.EffectiveValue)

	// DB-overridden string.
	got = byKey["support.email_smtp"]
	assert.True(t, got.Configured)
	assert.Equal(t, "smtp.db:587", got.Value)
	assert.Equal(t, "smtp.db:587", got.EffectiveValue)

	// Encrypted with only a yaml fallback: effective_value must be masked,
	// plaintext must never appear in the body.
	got = byKey["support.email_pwd"]
	assert.False(t, got.Configured, "no DB row → configured=false")
	assert.Equal(t, "", got.Value, "no DB row → value empty (not masked)")
	assert.Equal(t, "****", got.EffectiveValue, "yaml-backed secret must surface as mask")
	assert.NotContains(t, w.Body.String(), "yaml-fallback-secret",
		"encrypted yaml plaintext must NEVER leak through GET")
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

// --- int range validation (issue #289) -------------------------------------

func TestManagerSystemSetting_UpdateRejectsOutOfRangeInt(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	for _, v := range []string{"-1", "3651", "100000"} {
		body := []byte(`{"items":[{"category":"sidebar","key":"recent_filter_group_days","value":"` + v + `"}]}`)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusOK, w.Code, "越界整数 %q 必须被拒绝", v)
	}
}

func TestManagerSystemSetting_UpdateRejectsNonInt(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	body := []byte(`{"items":[{"category":"sidebar","key":"recent_filter_group_days","value":"abc"}]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestManagerSystemSetting_UpdateAcceptsInRangeIntBoundaries(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	body := []byte(`{"items":[` +
		`{"category":"sidebar","key":"recent_filter_group_days","value":"0"},` +
		`{"category":"sidebar","key":"recent_filter_thread_days","value":"3650"},` +
		`{"category":"sidebar","key":"recent_filter_person_days","value":"7"}` +
		`]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	db := newSystemSettingDB(ctx)
	rows, err := db.listAll()
	require.NoError(t, err)
	got := map[string]string{}
	for _, r := range rows {
		got[schemaKey(r.Category, r.KeyName)] = r.Value
	}
	assert.Equal(t, "0", got["sidebar.recent_filter_group_days"])
	assert.Equal(t, "3650", got["sidebar.recent_filter_thread_days"])
	assert.Equal(t, "7", got["sidebar.recent_filter_person_days"])
}

// TestManagerSystemSetting_UpdateRejectsInvalidIncomingWebhookNumerics pins the
// #292-review hardening: the rate-limit / quota knobs are Positive keys, so the
// write path must reject 0 / negative / NaN / ±Inf / non-numeric — values that
// would otherwise silently disable the per-webhook limiter or the quota.
func TestManagerSystemSetting_UpdateRejectsInvalidIncomingWebhookNumerics(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	cases := []struct{ key, value string }{
		{"per_webhook_rps", "0"}, {"per_webhook_rps", "-1.5"}, {"per_webhook_rps", "NaN"},
		{"per_webhook_rps", "+Inf"}, {"per_webhook_rps", "-Inf"}, {"per_webhook_rps", "abc"},
		{"per_webhook_burst", "0"}, {"per_webhook_burst", "-1"},
		{"max_per_group", "0"}, {"max_per_group", "-5"},
	}
	for _, tc := range cases {
		body := []byte(`{"items":[{"category":"incomingwebhook","key":"` + tc.key + `","value":"` + tc.value + `"}]}`)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		assert.NotEqualf(t, http.StatusOK, w.Code, "incomingwebhook.%s=%q must be rejected", tc.key, tc.value)
	}
}

// TestManagerSystemSetting_UpdateAcceptsPositiveIncomingWebhookNumerics pins that
// valid positive values are accepted, including a large burst — Positive keys opt
// out of the shared [0,3650] day-window cap, so there is no artificial upper bound
// (matching the env semantics they fall back to).
func TestManagerSystemSetting_UpdateAcceptsPositiveIncomingWebhookNumerics(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	body := []byte(`{"items":[` +
		`{"category":"incomingwebhook","key":"per_webhook_rps","value":"8.5"},` +
		`{"category":"incomingwebhook","key":"per_webhook_burst","value":"100000"},` +
		`{"category":"incomingwebhook","key":"max_per_group","value":"50"}` +
		`]}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	db := newSystemSettingDB(ctx)
	rows, err := db.listAll()
	require.NoError(t, err)
	got := map[string]string{}
	for _, r := range rows {
		got[schemaKey(r.Category, r.KeyName)] = r.Value
	}
	assert.Equal(t, "8.5", got["incomingwebhook.per_webhook_rps"])
	assert.Equal(t, "100000", got["incomingwebhook.per_webhook_burst"], "no artificial upper bound for Positive keys")
	assert.Equal(t, "50", got["incomingwebhook.max_per_group"])
}
