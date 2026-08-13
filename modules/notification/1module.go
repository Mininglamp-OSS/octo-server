package notification

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
		api := New(ctx.(*config.Context))
		return register.Module{
			Name: "notification",
			SetupAPI: func() register.APIRouter {
				return api
			},
			Service: api,
			Swagger: swaggerContent,
			SQLDir:  register.NewSQLFS(sqlFS),
		}
	})
}
