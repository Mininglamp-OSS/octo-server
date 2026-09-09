package botfather

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/require"
)

func TestAITeamProvisionerReceivesAuthoritativeCreationIdentity(t *testing.T) {
	aiTeamProvisionerMu.Lock()
	previous := aiTeamProvisioner
	aiTeamProvisionerMu.Unlock()
	t.Cleanup(func() { RegisterAITeamProvisioner(previous) })

	wantErr := errors.New("projection failed")
	var gotSpace, gotOwner, gotBot string
	RegisterAITeamProvisioner(func(_ *config.Context, spaceID, ownerUID, botID string) error {
		gotSpace, gotOwner, gotBot = spaceID, ownerUID, botID
		return wantErr
	})

	err := provisionAITeam(&config.Context{}, "space", "owner", "bot")
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, "space", gotSpace)
	require.Equal(t, "owner", gotOwner)
	require.Equal(t, "bot", gotBot)
}

func TestAITeamProvisionerSkipsIncompleteIdentity(t *testing.T) {
	aiTeamProvisionerMu.Lock()
	previous := aiTeamProvisioner
	aiTeamProvisionerMu.Unlock()
	t.Cleanup(func() { RegisterAITeamProvisioner(previous) })

	called := false
	RegisterAITeamProvisioner(func(_ *config.Context, _, _, _ string) error {
		called = true
		return nil
	})
	require.NoError(t, provisionAITeam(&config.Context{}, "", "owner", "bot"))
	require.False(t, called)
}

func TestEveryUserBotCreationPathTriggersAITeamProvisioning(t *testing.T) {
	for _, filename := range []string{"command.go", "api_user.go", "mint_obo.go"} {
		body, err := os.ReadFile(filename)
		require.NoError(t, err)
		require.Contains(t, strings.ReplaceAll(string(body), " ", ""), "provisionAITeam(",
			"%s must converge the Agent, pair group, and AI-team group after Space binding", filename)
	}
}
