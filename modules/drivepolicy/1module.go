// Package drivepolicy exposes the effective Drive upload policy over an
// authenticated internal HTTP surface for the octo-drive backend (②).
//
// octo-server is the configuration single source of truth for the Drive upload
// limit and the chunked-upload parameters; octo-drive polls
// GET /v1/internal/drive/upload-policy (≤60s cache) to learn the effective
// policy so its server-side gate stays in lockstep with what the clients were
// told via appconfig. This module owns only that one endpoint and its
// enablement gate; the byte-level upload enforcement lives entirely in ②.
//
// The endpoint reuses OCTO_DRIVE_INTERNAL_TOKEN (the same peer already presents
// it inbound on modules/internal_resolve) and is gated fail-closed + constant-
// time via internalAuthMiddleware; see Route for the middleware-order contract.
package drivepolicy

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name: "drivepolicy",
			SetupAPI: func() register.APIRouter {
				return New(ctx.(*config.Context))
			},
		}
	})
}
