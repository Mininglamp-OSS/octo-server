package common

import (
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
	settings.RecordManagerEmailMFAPreflight(true)
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
