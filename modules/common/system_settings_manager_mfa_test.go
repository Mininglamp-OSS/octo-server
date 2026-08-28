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

func TestEnsureManagerEmailMFASettingsDoesNotPersistPartialSMTPWhenPasswordEncryptionFails(t *testing.T) {
	settings := newTestSystemSettings(t, func(s *SystemSettings) {
		s.ctx.GetConfig().Support.Email = "mfa-default@example.com"
		s.ctx.GetConfig().Support.EmailSmtp = "smtp.example.com:587"
		s.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	})
	t.Setenv(masterKeyEnv, "invalid-master-key")

	err := settings.EnsureManagerEmailMFASettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encrypt default SMTP password")

	rows, err := settings.db.listAll()
	require.NoError(t, err)
	var mfaRow *systemSettingModel
	for _, row := range rows {
		if row.Category == "login" && row.KeyName == "manager_email_mfa_on" {
			mfaRow = row
		}
		if row.Category == "support" {
			t.Fatalf("SMTP bootstrap must remain atomic after password encryption failure: found %s.%s", row.Category, row.KeyName)
		}
	}
	require.NotNil(t, mfaRow, "the default-off MFA policy must be persisted even when SMTP encryption fails")
	assert.Equal(t, "0", mfaRow.Value)
	assert.Equal(t, settingTypeBool, mfaRow.ValueType)
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

func TestEnsureManagerEmailMFASettingsDoesNotCompletePartialSMTPFromYAML(t *testing.T) {
	settings := newTestSystemSettings(t, func(s *SystemSettings) {
		s.ctx.GetConfig().Support.Email = "yaml@example.com"
		s.ctx.GetConfig().Support.EmailSmtp = "yaml.example.com:587"
		s.ctx.GetConfig().Support.EmailPwd = "yaml-password"
	})
	password, err := encryptKey("database-password")
	require.NoError(t, err)
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, "legacy"))
	// This represents an old deployment that overrode only the password in the
	// database while the sender and endpoint came from YAML.
	require.NoError(t, settings.db.upsert("support", "email_pwd", password, settingTypeEncrypted, "legacy"))

	require.NoError(t, settings.EnsureManagerEmailMFASettings())
	require.NoError(t, settings.Load())

	provider := settings.ManagerEmailMFASMTPSettings()
	assert.Empty(t, provider.SupportEmail())
	assert.Empty(t, provider.SupportEmailSmtp())
	assert.Equal(t, "database-password", provider.SupportEmailPwd())
	assert.Equal(t, ManagerEmailMFAOn, settings.ManagerEmailMFAState())
	assert.Error(t, settings.ValidateManagerEmailMFASMTP(),
		"a partial legacy SMTP set must be reported as invalid instead of merged with YAML")

	rows, err := settings.db.listAll()
	require.NoError(t, err)
	for _, row := range rows {
		if row.Category == "support" && row.KeyName == "email" {
			t.Fatal("partial SMTP bootstrap must not create support.email from YAML")
		}
		if row.Category == "support" && row.KeyName == "email_smtp" {
			t.Fatal("partial SMTP bootstrap must not create support.email_smtp from YAML")
		}
	}
}

func TestManagerEmailMFASeedInsertDoesNotOverwriteExistingMFA(t *testing.T) {
	settings := newTestSystemSettings(t, nil)
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, "operator"))

	// Simulate a stale initialization plan that was built before the operator
	// enabled MFA. The database-level insert-if-absent must preserve the live
	// value; an application-level SELECT followed by upsert would reset it.
	require.NoError(t, settings.persistManagerEmailMFASeeds([]managerEmailMFASeed{{
		category: "login", key: "manager_email_mfa_on", value: "0",
		valueType: settingTypeBool, description: "default",
	}}))

	rows, err := settings.db.listAll()
	require.NoError(t, err)
	var got *systemSettingModel
	for _, row := range rows {
		if row.Category == "login" && row.KeyName == "manager_email_mfa_on" {
			got = row
			break
		}
	}
	require.NotNil(t, got)
	assert.Equal(t, "1", got.Value)
	assert.Equal(t, "operator", got.Description)
}

func TestEnsureManagerEmailMFASettingsPreservesEnabledMFA(t *testing.T) {
	settings := newTestSystemSettings(t, func(s *SystemSettings) {
		s.ctx.GetConfig().Support.Email = "yaml@example.com"
		s.ctx.GetConfig().Support.EmailSmtp = "yaml.example.com:587"
		s.ctx.GetConfig().Support.EmailPwd = "yaml-password"
	})
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, "operator"))

	// EnsureManagerEmailMFASettings is called on every startup. Its seed plan
	// may have been built while the row was absent, so the database-level
	// insert-if-absent must be what protects an already-enabled policy.
	require.NoError(t, settings.EnsureManagerEmailMFASettings())
	rows, err := settings.db.listAll()
	require.NoError(t, err)
	for _, row := range rows {
		if row.Category == "login" && row.KeyName == "manager_email_mfa_on" {
			assert.Equal(t, "1", row.Value)
			assert.Equal(t, "operator", row.Description)
			return
		}
	}
	t.Fatal("manager_email_mfa_on row was not found")
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
