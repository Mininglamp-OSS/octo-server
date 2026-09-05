package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerEmailMFALoadAndReloadDoNotPerformSMTPIO(t *testing.T) {
	settings := newTestSystemSettings(t, nil)
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	require.NoError(t, settings.db.upsert("support", "email", "mfa@example.com", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_smtp", "127.0.0.1:1", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_pwd", "password", settingTypeString, ""))

	// Load and Reload only publish database values. In particular, an SMTP
	// endpoint that would refuse a connection must not be contacted at startup
	// or during an ordinary settings refresh.
	require.NoError(t, settings.Load())
	require.NoError(t, settings.Reload())
	assert.Equal(t, ManagerEmailMFAOn, settings.ManagerEmailMFAState())
	assert.Equal(t, "127.0.0.1:1", settings.ManagerEmailMFASMTPSettings().SupportEmailSmtp())
}

func TestManagerEmailMFAMissingSMTPDoesNotFallbackToYAML(t *testing.T) {
	settings := newTestSystemSettings(t, func(s *SystemSettings) {
		s.ctx.GetConfig().Support.Email = "yaml@example.com"
		s.ctx.GetConfig().Support.EmailSmtp = "yaml.example.com:587"
		s.ctx.GetConfig().Support.EmailPwd = "yaml-password"
	})
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	require.NoError(t, settings.Load())

	provider := settings.ManagerEmailMFASMTPSettings()
	assert.Empty(t, provider.SupportEmail())
	assert.Empty(t, provider.SupportEmailSmtp())
	assert.Empty(t, provider.SupportEmailPwd())
	assert.Error(t, settings.ValidateManagerEmailMFASMTP(),
		"an MFA-on partial database SMTP set must be surfaced as an invalid upgrade state")
}
