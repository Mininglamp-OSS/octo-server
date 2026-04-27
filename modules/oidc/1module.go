package oidc

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		x := ctx.(*config.Context)
		return register.Module{
			Name: "oidc",
			SetupAPI: func() register.APIRouter {
				return New(x)
			},
			SQLDir: register.NewSQLFS(sqlFS),
		}
	})
}
