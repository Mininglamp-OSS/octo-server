package robot

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

//go:embed swagger/api.yaml
var swaggerContent string

func init() {

	register.AddModule(func(ctx interface{}) register.Module {

		return register.Module{
			Name: "robot",
			SetupAPI: func() register.APIRouter {
				return New(ctx.(*config.Context))
			},
			SQLDir:  register.NewSQLFS(sqlFS),
			Swagger: swaggerContent,
		}
	})

	// Manager routes are a separate API surface, as in the group/message
	// modules. Keep one instance so its durable Avatar cleanup worker has the
	// same lifecycle as the registered handlers.
	register.AddModule(func(ctx interface{}) register.Module {
		manager := NewManager(ctx.(*config.Context))
		return register.Module{
			Name:     "robot_manager",
			SetupAPI: func() register.APIRouter { return manager },
			Start:    manager.StartAvatarCleanupWorker,
			Stop:     manager.StopAvatarCleanupWorker,
		}
	})
}
