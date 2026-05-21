package voice_adapter

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
		cfg := NewAdapterConfigFromEnv()
		adapter := NewVoiceAdapter(x, cfg)

		return register.Module{
			Name: "voice_adapter",
			SQLDir: register.NewSQLFS(sqlFS),
			SetupAPI: func() register.APIRouter {
				return adapter
			},
		}
	})
}
