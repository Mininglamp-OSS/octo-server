package card_template_catalog

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		var api *API
		return register.Module{
			Name: "card_template_catalog",
			SetupAPI: func() register.APIRouter {
				api = New(ctx.(*config.Context))
				return api
			},
			Start: func() error {
				if api == nil {
					return nil
				}
				return api.Start()
			},
			Stop: func() error {
				if api == nil {
					return nil
				}
				return api.Stop()
			},
			SQLDir: register.NewSQLFS(sqlFS),
		}
	})
}
