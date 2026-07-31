package bot_mention

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name: "bot_mention",
			SetupAPI: func() register.APIRouter {
				return New(ctx.(*config.Context))
			},
		}
	})
}
