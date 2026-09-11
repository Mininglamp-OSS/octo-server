package workspace

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		workspaceContext := ctx.(*config.Context)
		registerSpaceMemberRemovalCleanup()
		return register.Module{
			Name: "workspace",
			SetupAPI: func() register.APIRouter {
				return New(workspaceContext)
			},
			SQLDir: register.NewSQLFS(sqlFS),
		}
	})
}
