package docs_proxy

// docs_proxy: 反代 /v1/docs/proxy/*path 到 doc 服务(octo-docs-html)，转发前把当前
// 登录用户的 octo token 注入成 X-Octo-Token（对端 FEAT-1 身份桥消费）。
//
// 未配置 OCTO_DOCS_UPSTREAM 时整个 module 跳过（Route 不挂 endpoint），避免误反代 nil upstream。

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"go.uber.org/zap"
)

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name: "docs_proxy",
			SetupAPI: func() register.APIRouter {
				cctx := ctx.(*config.Context)
				h := New(cctx)
				if h == nil {
					// 未配置 upstream：返回一个 no-op router；Route 里不挂任何路径。
					log.NewTLog("docs_proxy").Warn(
						"OCTO_DOCS_UPSTREAM 未配置，docs_proxy 路由未启用",
						zap.String("module", "docs_proxy"),
					)
					return &Proxy{disabled: true}
				}
				return h
			},
		}
	})
}
