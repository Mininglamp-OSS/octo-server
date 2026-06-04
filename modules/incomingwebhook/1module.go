package incomingwebhook

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
		// datasource 只需读 incoming_webhook 表，用独立轻量 db session 即可，
		// 不必提前构造完整 API（避免在注册阶段建 redis 等重资源）。
		d := newDB(x)
		return register.Module{
			Name: "incomingwebhook",
			SetupAPI: func() register.APIRouter {
				return New(x)
			},
			SQLDir: register.NewSQLFS(sqlFS),
			// 让 webhook 合成身份（iwh_）能被 /v1/channels/:id/:type 与 /v1/users/:uid
			// 解析为发送者名/头像，客户端无需适配即可渲染。
			BussDataSource: register.BussDataSource{
				ChannelGet: newChannelGetDatasource(d),
			},
		}
	})
}
