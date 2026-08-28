package common

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerSystemSetting_RejectsClearingSMTPWhenManagerMFAIsOn(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := EnsureSystemSettings(ctx)
	originalConfig := *settings.ctx.GetConfig()
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		*settings.ctx.GetConfig() = originalConfig
		_ = settings.Reload()
	})
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))
	settings.ctx.GetConfig().Support.Email = "mfa-sender@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = "smtp.example.com:587"
	settings.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	password, err := encryptKey("smtp-password")
	require.NoError(t, err)
	require.NoError(t, settings.db.upsert("support", "email", "mfa-sender@example.com", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_smtp", "smtp.example.com:587", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_pwd", password, settingTypeEncrypted, ""))
	require.NoError(t, settings.Load())

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/common/system_setting", bytes.NewBufferString(
		`{"items":[{"category":"support","key":"email","value":""}]}`,
	))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(recorder, req)

	assert.NotEqual(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, "mfa-sender@example.com", settings.SupportEmail(),
		"rejected SMTP clearing must not alter the effective singleton")
}

func TestManagerSystemSetting_ValidatesMergedMFAAndSMTPConfiguration(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := EnsureSystemSettings(ctx)
	originalConfig := *settings.ctx.GetConfig()
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		*settings.ctx.GetConfig() = originalConfig
		_ = settings.Reload()
	})
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))
	settings.ctx.GetConfig().Support.Email = "mfa-sender@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = "smtp.example.com:587"
	settings.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	password, err := encryptKey("smtp-password")
	require.NoError(t, err)
	require.NoError(t, settings.db.upsert("support", "email", "mfa-sender@example.com", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_smtp", "smtp.example.com:587", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_pwd", password, settingTypeEncrypted, ""))
	require.NoError(t, settings.Load())

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/common/system_setting", bytes.NewBufferString(
		`{"items":[{"category":"login","key":"manager_email_mfa_on","value":"1"},{"category":"support","key":"email","value":"invalid"}]}`,
	))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(recorder, req)

	assert.NotEqual(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, ManagerEmailMFAOff, settings.ManagerEmailMFAState(),
		"failed final-config validation must not enable MFA")
}

func TestManagerSystemSettingSMTPTestUsesManagerMFASnapshot(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	route, settings := newManagerSystemSettingTest(t)
	originalConfig := *settings.ctx.GetConfig()
	t.Cleanup(func() {
		*settings.ctx.GetConfig() = originalConfig
		_ = settings.Reload()
	})

	probe := newManagerMFAProbeSMTP(t)
	settings.ctx.GetConfig().Support.Email = "yaml-sender@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = probe.address()
	settings.ctx.GetConfig().Support.EmailPwd = "yaml-password"

	// Only the sender is present in the database. The legacy SystemSettings
	// provider would silently combine this row with the YAML endpoint/password
	// and successfully send; the manager MFA provider must reject the same
	// partial snapshot before any SMTP command is issued.
	require.NoError(t, settings.db.upsert(
		"support", "email", "db-sender@example.com", settingTypeString, ""))
	require.NoError(t, settings.Load())

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/common/system_setting/test_email",
		bytes.NewBufferString(`{"to":"recipient@example.com"}`))
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(recorder, req)

	assert.NotEqual(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.False(t, probe.hasCommandPrefix("MAIL FROM"),
		"an incomplete manager MFA snapshot must fail before SMTP delivery")
}

func TestManagerSystemSetting_UnchangedSMTPDoesNotRunPreflight(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := EnsureSystemSettings(ctx)
	originalConfig := *settings.ctx.GetConfig()
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		*settings.ctx.GetConfig() = originalConfig
		_ = settings.Reload()
	})
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	smtpAddr := listener.Addr().String()
	require.NoError(t, listener.Close())

	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "0", settingTypeBool, ""))
	require.NoError(t, settings.db.upsert("support", "email", "relay@example.com", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_smtp", smtpAddr, settingTypeString, ""))
	// No password is required for this configuration; the closed port is only
	// here to prove that an unchanged SMTP payload never reaches preflight.
	require.NoError(t, settings.Load())

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/common/system_setting", bytes.NewBufferString(
		`{"items":[{"category":"support","key":"email","value":"relay@example.com"},{"category":"support","key":"email_smtp","value":"`+smtpAddr+`"}]}`,
	))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}

func TestManagerSystemSetting_FullPayloadWithEnabledMFADoesNotRunPreflight(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := EnsureSystemSettings(ctx)
	originalConfig := *settings.ctx.GetConfig()
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		*settings.ctx.GetConfig() = originalConfig
		_ = settings.Reload()
	})
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	smtpAddr := listener.Addr().String()
	require.NoError(t, listener.Close())

	password, err := encryptKey("smtp-password")
	require.NoError(t, err)
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	require.NoError(t, settings.db.upsert("support", "email", "mfa-sender@example.com", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_smtp", smtpAddr, settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_pwd", password, settingTypeEncrypted, ""))
	require.NoError(t, settings.Load())

	managerSMTP := settings.managerEmailMFASMTPSettings()
	items := make([]systemSettingItemReq, 0, len(systemSettingSchema))
	for _, def := range systemSettingSchema {
		value := ""
		switch {
		case def.Category == "login" && def.Key == "manager_email_mfa_on":
			value = "1"
		case def.Category == "support" && def.Key == "email":
			value = managerSMTP.from
		case def.Category == "support" && def.Key == "email_smtp":
			value = managerSMTP.address
		case def.Category == "support" && def.Key == "email_pwd":
			value = secretMask
		case def.Effective != nil:
			value = def.Effective(settings)
		}
		if def.Category == "register" && def.Key == "off" {
			if value == "1" {
				value = "0"
			} else {
				value = "1"
			}
		}
		items = append(items, systemSettingItemReq{
			Category: def.Category,
			Key:      def.Key,
			Value:    value,
		})
	}
	body, err := json.Marshal(systemSettingUpdateReq{Items: items})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/common/system_setting", bytes.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, ManagerEmailMFAOn, settings.ManagerEmailMFAState())
}
