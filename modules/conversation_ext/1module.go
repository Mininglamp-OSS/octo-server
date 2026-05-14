package conversation_ext

import (
	"embed"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

//go:embed sql
var sqlFS embed.FS

//go:embed swagger/api.yaml
var swaggerContent string

// ---------------------------------------------------------------------------
// Global singleton — same pattern as modules/user/db_pinned.go
// ---------------------------------------------------------------------------

var (
	globalConvExtService     *Service
	globalConvExtServiceOnce sync.Once
)

// InitGlobalConvExtService initialises the package-level *Service singleton.
// It is idempotent: repeated calls after the first are no-ops (sync.Once).
// Called from the module factory below so the singleton is ready before any
// handler or cascade-cleanup hook uses it.
func InitGlobalConvExtService(ctx *config.Context) {
	globalConvExtServiceOnce.Do(func() {
		globalConvExtService = NewService(ctx)
	})
}

// GetGlobalConvExtService returns the singleton *Service, or nil if
// InitGlobalConvExtService has not been called yet.
// External modules (group, thread, …) that inject cascade-cleanup hooks
// should call this to reach the service without importing anything else.
func GetGlobalConvExtService() *Service {
	return globalConvExtService
}

// ---------------------------------------------------------------------------
// Module registration
// ---------------------------------------------------------------------------

// followRouter is a thin wrapper that implements register.APIRouter so the
// Follow handlers are wired into the global route table via SetupAPI.
type followRouter struct {
	ctx *config.Context
	f   *Follow
}

// Route registers all 7 Follow endpoints under /v1/follow with auth and space
// middleware — mirrors the pattern in modules/user/api.go (pinned group).
func (fr *followRouter) Route(r *wkhttp.WKHttp) {
	grp := r.Group("/v1/follow",
		fr.ctx.AuthMiddleware(r),
		spacepkg.SpaceMiddleware(fr.ctx),
	)
	grp.POST("/dm", fr.f.FollowDM)
	grp.DELETE("/dm", fr.f.UnfollowDM)
	grp.POST("/channel/unfollow", fr.f.UnfollowChannel)
	grp.POST("/channel/refollow", fr.f.FollowChannel)
	grp.POST("/thread", fr.f.FollowThread)
	grp.DELETE("/thread", fr.f.UnfollowThread)
	grp.PUT("/sort", fr.f.UpdateSort)
}

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		appCtx := ctx.(*config.Context)

		// Initialise both singletons so they are available to other modules
		// (group, thread, user) for cascade-cleanup before any HTTP request is served.
		InitGlobalConvExtService(appCtx)
		InitGlobalConvExtDB(appCtx)

		return register.Module{
			Name:    "conversation_ext",
			SQLDir:  register.NewSQLFS(sqlFS),
			Service: GetGlobalConvExtService(),
		}
	})

	// Register Follow API routes as a separate module entry so the HTTP
	// handlers are wired without interfering with the migration/service
	// registration above.
	register.AddModule(func(ctx interface{}) register.Module {
		appCtx := ctx.(*config.Context)
		svc := GetGlobalConvExtService()
		db := NewDB(appCtx)
		f := NewFollow(svc, db)

		return register.Module{
			Name: "conversation_ext_follow",
			SetupAPI: func() register.APIRouter {
				return &followRouter{ctx: appCtx, f: f}
			},
			Swagger: swaggerContent,
		}
	})
}
