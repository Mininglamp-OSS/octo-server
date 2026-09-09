package ai_team

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	botfather.RegisterAITeamProvisioner(ProvisionOwnedBot)
	aiteampkg.RegisterLifecycleRosterMarker(markLifecycleRosterRemovalTx)
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name: "ai_team",
			SetupAPI: func() register.APIRouter {
				return New(ctx.(*config.Context))
			},
			SQLDir: register.NewSQLFS(sqlFS),
		}
	})
}
