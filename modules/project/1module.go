package project

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

// init registers the Project module the standard way.
//
// Being registered means an init() or Route() failure takes down the WHOLE process,
// not just the Project endpoints — this module is blank-imported from
// internal/modules.go. That is not hypothetical: api_i18n.go's mustLookupSharedCode
// is designed to panic at init when a shared code is unregistered, so one missing
// registration turns into "no IM service at all". The external test package carries
// a boot smoke test for exactly that failure mode.
func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name: "project",
			SetupAPI: func() register.APIRouter {
				return New(ctx.(*config.Context))
			},
			SQLDir: register.NewSQLFS(sqlFS),
		}
	})
}
