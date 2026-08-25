package common

import (
	"bytes"
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

func TestManagerSystemSetting_AllowsPasswordlessSMTPToReachPreflight(t *testing.T) {
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

	// Reserve and close a local port so the real preflight fails quickly after
	// configuration validation. A 503 proves the empty password was accepted
	// as a possible relay configuration; the old password-required branch
	// returned the configuration-invalid 400 before attempting preflight.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	smtpAddr := listener.Addr().String()
	require.NoError(t, listener.Close())

	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "0", settingTypeBool, ""))
	require.NoError(t, settings.db.upsert("support", "email", "relay@example.com", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_smtp", smtpAddr, settingTypeString, ""))
	// Deliberately do not create support.email_pwd.
	require.NoError(t, settings.Load())

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/common/system_setting", bytes.NewBufferString(
		`{"items":[{"category":"login","key":"manager_email_mfa_on","value":"1"}]}`,
	))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	assert.Equal(t, ManagerEmailMFAOff, settings.ManagerEmailMFAState(),
		"failed passwordless SMTP preflight must not enable MFA")
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
