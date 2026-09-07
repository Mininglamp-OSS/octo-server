// Package internal_membership exposes octo-server's Project membership facts
// over an authenticated service-to-service surface, for consumers that hold no
// end-user token.
//
// # Why a separate module from modules/project
//
// modules/project owns the user-facing surface: every route there is mounted
// behind AuthMiddleware plus the Space/Project middleware, and its handlers
// answer a logged-in human. The endpoints here answer a PEER SERVICE holding a
// deployment-issued token, and they must not inherit any of that: no
// AuthMiddleware, no uid, no per-uid rate limiter. Mixing the two trust domains
// in one module means one wrong middleware line grants a service token a
// user-scoped route, or leaves a service route depending on a uid that is never
// present.
//
// The split also keeps the error contract right. modules/project pins its wire
// status to 400 (ResponseErrorL) for D14 compatibility with shipped clients;
// these endpoints have no such clients and use ResponseErrorLWithStatus, so a
// peer service reads a real 401/400/500 like it does from every other
// /v1/internal endpoint in this repository.
//
// Data access goes through pkg/project, never modules/project — importing the
// module would recreate the cycle that pkg/project exists to avoid, and
// TestPkgProjectDoesNotImportModulesProject pins that direction.
//
// # Consumers
//
// The Loop/Fleet control plane, which needs two facts octo-server alone can
// answer: whether a NAMED OTHER user still holds a seat in a project (the
// end-user verify endpoint can only speak for its own token holder), and
// whether a project's membership has changed since an authorization snapshot
// was taken. Every endpoint is gated by its own X-Internal-Token so a leaked
// credential grants exactly one capability.
package internal_membership

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name: "internal_membership",
			SetupAPI: func() register.APIRouter {
				return New(ctx.(*config.Context))
			},
		}
	})
}
