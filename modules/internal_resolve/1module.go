// Package internal_resolve exposes octo-server's identity graph over an
// authenticated internal HTTP surface for consumers that lack a user token
// (e.g. octo-drive's docs-auto-mount polling loop).
//
// Only endpoints strictly needed by internal services live here. Each endpoint
// is gated by a distinct X-Internal-Token so credentials never span services;
// see internalAuthMiddleware for the fail-closed + constant-time compare check.
package internal_resolve

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name: "internal_resolve",
			SetupAPI: func() register.APIRouter {
				return New(ctx.(*config.Context))
			},
		}
	})
}
