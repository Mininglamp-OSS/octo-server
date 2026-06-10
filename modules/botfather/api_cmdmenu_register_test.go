package botfather

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/botfather/cmdmenu"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
)

// TestRegisterBotFatherCommands_WritesDeploymentDefault pins the #335 floor:
// the startup write renders the menu in OCTO_DEFAULT_LANGUAGE, and because it
// overwrites unconditionally on every boot, an existing deployment's stored
// blob self-heals after the default changes — no migration involved.
func TestRegisterBotFatherCommands_WritesDeploymentDefault(t *testing.T) {
	_, bf := setupTestBotFather(t)
	createTestRobot(t, bf, BotFatherUID, "", 1)

	readBlob := func() string {
		var blob string
		err := bf.db.session.Select("IFNULL(bot_commands,'')").From("robot").
			Where("robot_id=?", BotFatherUID).LoadOne(&blob)
		assert.NoError(t, err)
		return blob
	}

	t.Setenv(octoi18n.EnvDefaultLanguage, "en-US")
	bf.registerBotFatherCommands()
	assert.Equal(t, cmdmenu.JSON("en-US"), readBlob(),
		"startup write must render the deployment default language")

	t.Setenv(octoi18n.EnvDefaultLanguage, "zh-CN")
	bf.registerBotFatherCommands()
	assert.Equal(t, cmdmenu.JSON("zh-CN"), readBlob(),
		"unconditional overwrite must self-heal the stored blob on re-boot")
}
