package botfather

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// AITeamProvisioner is invoked only after a User Bot and its Space membership
// are durable. The reverse registration keeps botfather independent from the
// AI-team module while giving every production creation path one hook.
type AITeamProvisioner func(ctx *config.Context, spaceID, ownerUID, botID string) error

var (
	aiTeamProvisionerMu sync.RWMutex
	aiTeamProvisioner   AITeamProvisioner
)

func RegisterAITeamProvisioner(fn AITeamProvisioner) {
	aiTeamProvisionerMu.Lock()
	defer aiTeamProvisionerMu.Unlock()
	aiTeamProvisioner = fn
}

func provisionAITeam(ctx *config.Context, spaceID, ownerUID, botID string) error {
	aiTeamProvisionerMu.RLock()
	fn := aiTeamProvisioner
	aiTeamProvisionerMu.RUnlock()
	if fn == nil || spaceID == "" || ownerUID == "" || botID == "" {
		return nil
	}
	return fn(ctx, spaceID, ownerUID, botID)
}
