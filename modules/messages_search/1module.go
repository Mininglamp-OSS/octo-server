package messages_search

import (
	_ "embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed swagger/api.yaml
var swaggerContent string

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name: "messages_search",
			SetupAPI: func() register.APIRouter {
				// Shared 单例：web / bot / uk 三条路由树共用同一 Handler（YUJ-49），
				// 共享限流桶与 sender 缓存。bot_api / botfather 也经 Shared 取同一实例。
				return Shared(ctx.(*config.Context))
			},
			Swagger: swaggerContent,
		}
	})
}
