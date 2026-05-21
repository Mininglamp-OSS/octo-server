package voice_adapter

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
		cfg := NewAdapterConfigFromEnv()

		if cfg.SpeechServiceURL == "" {
			log.Warn("SPEECH_SERVICE_URL is not set; voice adapter requests will fail")
		}

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
