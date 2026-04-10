package voice

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		x := ctx.(*config.Context)
		cfg := NewVoiceConfigFromEnv()
		if err := cfg.Validate(); err != nil {
			lg := log.NewTLog("Voice")
			lg.Warn("voice module disabled: " + err.Error())
		}
		api := New(x, cfg)
		return register.Module{
			Name: "voice",
			SetupAPI: func() register.APIRouter {
				return api
			},
			SQLDir: register.NewSQLFS(sqlFS),
		}
	})
}
