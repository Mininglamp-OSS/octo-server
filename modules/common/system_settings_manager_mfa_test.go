package common

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerEmailMFAStateIsTriStateAndFailClosed(t *testing.T) {
	settings := newTestSystemSettings(t, nil)

	// A freshly constructed settings object has no successful snapshot yet.
	unloaded := NewSystemSettings(settings.ctx, settings.db)
	assert.Equal(t, ManagerEmailMFAUnavailable, unloaded.ManagerEmailMFAState())

	// A successfully loaded database with no manager MFA row is the documented
	// default-off state, not the unavailable state.
	assert.Equal(t, ManagerEmailMFAOff, settings.ManagerEmailMFAState())

	settings.ctx.GetConfig().Support.Email = "mfa-sender@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = "smtp.example.com:587"
	settings.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	require.NoError(t, settings.Load())

	assert.Equal(t, ManagerEmailMFAOn, settings.ManagerEmailMFAState())
	assert.False(t, settings.ManagerEmailMFAReady(), "MFA must remain fail-closed before a real preflight")

	// The test publishes the result of a successful preflight without sending
	// an external message; the handler tests cover the actual SMTP path.
	settings.RecordManagerEmailMFAPreflight(settings.ManagerEmailMFAProbeGeneration(), true)
	assert.True(t, settings.ManagerEmailMFAReady())
}

func TestManagerEmailMFAPreflightDoesNotPublishStaleGeneration(t *testing.T) {
	settings := newTestSystemSettings(t, nil)
	settings.ctx.GetConfig().Support.Email = "mfa-preflight-generation@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = ""
	settings.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	require.NoError(t, settings.Load())

	// Simulate a settings replacement while the startup probe is in flight.
	// The probe result must not open the gate for the newer generation.
	settings.managerMFAProbeMu.Lock()
	settings.managerMFAProbeGeneration++
	settings.managerMFAProbeMu.Unlock()
	assert.Error(t, settings.PreflightManagerEmailMFA(context.Background()))
	assert.False(t, settings.ManagerEmailMFAReady())
}

func TestManagerEmailMFAPreflightRecordRejectsStaleGeneration(t *testing.T) {
	settings := newTestSystemSettings(t, nil)
	settings.ctx.GetConfig().Support.Email = "mfa-record-generation@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = "smtp.example.com:587"
	settings.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	require.NoError(t, settings.Load())

	generation := settings.ManagerEmailMFAProbeGeneration()
	settings.managerMFAProbeMu.Lock()
	settings.managerMFAProbeGeneration++
	settings.managerMFAProbeMu.Unlock()
	assert.False(t, settings.RecordManagerEmailMFAPreflight(generation, true))
	assert.False(t, settings.ManagerEmailMFAReady())
}

func TestManagerEmailMFAPreflightRecordRejectsUnmatchedSMTPSnapshot(t *testing.T) {
	settings := newTestSystemSettings(t, nil)
	settings.ctx.GetConfig().Support.Email = "mfa-record-match@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = "smtp.example.com:587"
	settings.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	require.NoError(t, settings.Load())

	probed := settings.managerEmailMFASMTPSettings()
	// Simulate a concurrent partial update that changes the loaded SMTP
	// combination after the original prospective values were probed.
	require.NoError(t, settings.db.upsert("support", "email_smtp", "other.example.com:587", settingTypeString, ""))
	require.NoError(t, settings.Load())
	generation := settings.ManagerEmailMFAProbeGeneration()
	assert.False(t, settings.RecordManagerEmailMFAPreflightIfMatches(generation, probed))
	assert.False(t, settings.ManagerEmailMFAReady())

	assert.True(t, settings.RecordManagerEmailMFAPreflightIfMatches(
		generation, settings.managerEmailMFASMTPSettings(),
	))
	assert.True(t, settings.ManagerEmailMFAReady())
}

func TestManagerEmailMFASchemaDefaultsOffAndNamesConsoleScope(t *testing.T) {
	def := findSchemaDef("login", "manager_email_mfa_on")
	require.NotNil(t, def)
	assert.Equal(t, settingTypeBool, def.Type)
	assert.Contains(t, def.Description, "管理控制台")

	settings := newTestSystemSettings(t, nil)
	assert.False(t, settings.ManagerEmailMFAOn())
}
