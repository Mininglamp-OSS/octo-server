package notify

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		// Construct the single Notify instance here so SetupAPI and the
		// Start/Stop lifecycle hooks share it (mirrors modules/webhook).
		n := New(ctx.(*config.Context))
		return register.Module{
			Name: "notify",
			SetupAPI: func() register.APIRouter {
				return n
			},
			SQLDir: register.NewSQLFS(sqlFS),
			Start: func() error {
				return n.Start()
			},
			Stop: func() error {
				return n.Stop()
			},
		}
	})
}
