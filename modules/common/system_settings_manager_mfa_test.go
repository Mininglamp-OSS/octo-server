package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerEmailMFAStateIsTriStateAndDefaultsOff(t *testing.T) {
	settings := newTestSystemSettings(t, nil)

	// A freshly constructed settings object has no successful snapshot yet.
	unloaded := NewSystemSettings(settings.ctx, settings.db)
	assert.Equal(t, ManagerEmailMFAUnavailable, unloaded.ManagerEmailMFAState())

	// A successfully loaded database with no manager MFA row is the documented
	// default-off state, not the unavailable state.
	assert.Equal(t, ManagerEmailMFAOff, settings.ManagerEmailMFAState())

	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	require.NoError(t, settings.Load())
	assert.Equal(t, ManagerEmailMFAOn, settings.ManagerEmailMFAState())
}

func TestEnsureManagerEmailMFASettingsSeedsDatabaseDefaults(t *testing.T) {
	settings := newTestSystemSettings(t, func(s *SystemSettings) {
		s.ctx.GetConfig().Support.Email = "mfa-default@example.com"
		s.ctx.GetConfig().Support.EmailSmtp = "smtp.example.com:587"
		s.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	})

	require.NoError(t, settings.EnsureManagerEmailMFASettings())
	require.NoError(t, settings.Load())

	assert.Equal(t, ManagerEmailMFAOff, settings.ManagerEmailMFAState())
	provider := settings.ManagerEmailMFASMTPSettings()
	assert.Equal(t, "mfa-default@example.com", provider.SupportEmail())
	assert.Equal(t, "smtp.example.com:587", provider.SupportEmailSmtp())
	assert.Equal(t, "smtp-password", provider.SupportEmailPwd())

	rows, err := settings.db.listAll()
	require.NoError(t, err)
	stored := make(map[string]*systemSettingModel, len(rows))
	for _, row := range rows {
		stored[schemaKey(row.Category, row.KeyName)] = row
	}
	assert.Equal(t, "0", stored["login.manager_email_mfa_on"].Value)
	assert.Equal(t, settingTypeEncrypted, stored["support.email_pwd"].ValueType)
	assert.NotEqual(t, "smtp-password", stored["support.email_pwd"].Value)
}

func TestEnsureManagerEmailMFASettingsDoesNotOverwriteExplicitSMTPRows(t *testing.T) {
	settings := newTestSystemSettings(t, func(s *SystemSettings) {
		s.ctx.GetConfig().Support.Email = "yaml@example.com"
		s.ctx.GetConfig().Support.EmailSmtp = "yaml.example.com:587"
		s.ctx.GetConfig().Support.EmailPwd = "yaml-password"
	})
	require.NoError(t, settings.db.upsert("support", "email", "", settingTypeString, "explicitly cleared"))
	require.NoError(t, settings.EnsureManagerEmailMFASettings())
	require.NoError(t, settings.Load())

	provider := settings.ManagerEmailMFASMTPSettings()
	assert.Empty(t, provider.SupportEmail())
	assert.Empty(t, provider.SupportEmailSmtp())
	assert.Empty(t, provider.SupportEmailPwd())
}

func TestManagerEmailMFASMTPSettingsUseDatabaseInsteadOfYAML(t *testing.T) {
	settings := newTestSystemSettings(t, func(s *SystemSettings) {
		s.ctx.GetConfig().Support.Email = "yaml@example.com"
		s.ctx.GetConfig().Support.EmailSmtp = "yaml.example.com:587"
		s.ctx.GetConfig().Support.EmailPwd = "yaml-password"
	})
	require.NoError(t, settings.db.upsert("support", "email", "db@example.com", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_smtp", "db.example.com:465", settingTypeString, ""))
	password, err := encryptKey("db-password")
	require.NoError(t, err)
	require.NoError(t, settings.db.upsert("support", "email_pwd", password, settingTypeEncrypted, ""))
	require.NoError(t, settings.Load())

	provider := settings.ManagerEmailMFASMTPSettings()
	assert.Equal(t, "db@example.com", provider.SupportEmail())
	assert.Equal(t, "db.example.com:465", provider.SupportEmailSmtp())
	assert.Equal(t, "db-password", provider.SupportEmailPwd())
}

func TestManagerEmailMFASchemaDefaultsOffAndNamesConsoleScope(t *testing.T) {
	def := findSchemaDef("login", "manager_email_mfa_on")
	require.NotNil(t, def)
	assert.Equal(t, settingTypeBool, def.Type)
	assert.Contains(t, def.Description, "管理控制台")

	settings := newTestSystemSettings(t, nil)
	assert.False(t, settings.ManagerEmailMFAOn())
}
