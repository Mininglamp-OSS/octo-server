package workspacecapability

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		api := New(ctx.(*config.Context))
		return register.Module{
			Name:     "workspace_capability",
			SetupAPI: func() register.APIRouter { return api },
			SQLDir:   register.NewSQLFS(sqlFS),
		}
	})
}
