package messages_search

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/cardtrust"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/searchbackend"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

// Handler wires the four /v1/messages/_search* endpoints. New is invoked from
// 1module.go via the standard register.AddModule entry point.
type Handler struct {
	ctx *config.Context
	log.Log
	cfg            SearchConfig
	userService    user.IService
	groupService   group.IService
	messageService message.IService
	threadService  thread.IService
	// cardTrust 判定 type-17 命中的存储 sender 是否 bot/webhook 身份
	// （Decision 2 residual-risk 投影门，共享实现 modules/cardtrust，带 LRU：
	// 一页多条同 bot 命中只查一次 robot 表）。接口便于测试替换。
	cardTrust cardSenderTruster
	// visibility is the post-filter probe used by the /_search* hot path
	// (see visibility.go::filterVisible). Defined as an interface so tests
	// can stub the four signals directly without standing up a real
	// message.IService — message.IService exposes its responses through
	// types unexported from modules/message, which a test fake outside
	// that package cannot name.
	visibility visibilityProbe

	// spaceMembersFn is a test seam for the Space-member enumeration used by
	// enumerateDMPeers. Nil in production (falls through to the raw SQL
	// implementation on Handler.queryDMSpaceMemberUIDs); tests inject a stub
	// so the P0-2 union with the friend list can be exercised without a real
	// MySQL connection.
	spaceMembersFn func(spaceID, loginUID string) ([]string, error)
	// dmBotFilterFn is a test seam for the bot-in-Space filter tail of
	// enumerateDMPeers. Nil in production (falls through to spacepkg.GetBotUIDs
	// + spacepkg.CheckBotsInSpace on h.ctx.DB()); tests inject a pass-through
	// stub so enumerateDMPeers is exercisable without a real MySQL connection.
	dmBotFilterFn func(spaceID string, peers []string) ([]string, error)
	// threadEnumFn is a test seam for the thread enumeration inside
	// buildAllowlist. Nil in production (falls through to thread.NewDB(h.ctx)
	// .QueryNonDeletedShortIDsByGroupNos — archived threads are included per
	// the reject-deleted-only visibility contract; see RC on PR #553); tests
	// inject a stub so the thread coverage on the global feed is exercisable
	// without a real MySQL connection.
	//
	// Returns (map[groupNo][]shortID, err). The DB layer now bounds every
	// group to `NonDeletedByGroupNosPerGroupHardLimit` (=201) rows via a
	// UNION ALL of per-group `LIMIT` subqueries, so a single runaway group
	// can no longer starve other groups' thread coverage (RC 3 on PR #553).
	// The 1-row overshoot lets the caller's `len(shortIDs) > maxThreadsPerGroup`
	// cap branch still observe over-cap groups and WARN/downgrade them.
	threadEnumFn func(groupNos []string) (map[string][]string, error)
	// externalGroupFn is a test seam for the external-group lookup inside
	// buildAllowlist. Nil in production (falls through to group.NewDB(h.ctx)
	// .QueryExternalGroupNosForUser); tests inject a stub so buildAllowlist
	// is exercisable without a real MySQL connection.
	externalGroupFn func(loginUID string) (map[string]string, error)

	limiter *uidLimiter
	cache   *senderCache
	// oboCheck is the as-user(OBO) scope gate seam (YUJ-53 / #F). Nil until
	// the bot search route (#B) injects a bot_api-backed adapter via
	// SetOBOChecker; *bot_api.BotAPI.SearchOBOAllowed satisfies oboChecker
	// structurally so messages_search stays decoupled from bot_api. When an
	// obo principal reaches the gate with this nil, the gate fails closed
	// (never degrades to un-scoped grantor search). See obo.go.
	oboCheck oboChecker
	// mode is the resolved OCTO_SEARCH_BACKEND posture. When mode.ESServe is
	// false the backendGate middleware refuses every _search* request with
	// SEARCH_DISABLED — the OS path never serves under zinc/disabled.
	mode searchbackend.Mode
}

// New constructs the Handler. ES client setup is deferred to first request so
// that a missing OS dependency does not prevent the rest of the server from
// booting (the request layer will surface UPSTREAM_UNAVAILABLE instead).
func New(ctx *config.Context) *Handler {
	cfg := loadConfig()
	msgSvc := message.NewService(ctx)
	h := &Handler{
		ctx:               ctx,
		Log:               log.NewTLog("messages_search"),
		cfg:               cfg,
		userService:       user.NewService(ctx),
		groupService:      group.NewService(ctx),
		messageService:    msgSvc,
		threadService:     thread.NewService(ctx),
		cardTrust:         cardtrust.New(ctx),
		visibility:        newMessageVisibilityProbe(msgSvc),
		limiter:           newUIDLimiter(cfg.RateLimit.QPS, cfg.RateLimit.Burst),
		cache:             newSenderCache(senderCacheCapacity, senderCacheTTL),
		mode:              searchbackend.Resolve(ctx.GetConfig().ZincSearch.SearchOn),
	}
	if cfg.CursorHMAC == "" {
		// The fallback key in cursor.go is a published constant, so cursors
		// are forgeable. Tolerable (the cursor carries no authorization data
		// and access is gated server-side) but every real deployment should
		// set its own key — make the misconfiguration loud instead of silent.
		h.Warn("OCTO_SEARCH_CURSOR_HMAC is not set; falling back to the " +
			"built-in default cursor signing key. Set a per-deployment " +
			"secret in production.")
	}
	if cfg.OSInsecureSkipVerify {
		h.Warn("OCTO_SEARCH_OS_INSECURE_SKIP_VERIFY=true; OpenSearch TLS " +
			"certificate verification is DISABLED. Use only in dev/test " +
			"environments with self-signed certificates.")
	}
	return h
}

// Route mounts the four endpoints under /v1/messages with the standard
// auth/space/uid-limit chain plus the per-user search rate limiter and the
// audit middleware (PRM-02). Individual handlers are wired in their own
// search_*.go files via the registerHandler helper.
//
// The backendGate middleware runs INSIDE the chain (after auth + rate limit +
// audit) so a disabled / zinc deployment still meters and audits the refusal
// — an attacker cannot enumerate channels through an unmetered "search off"
// reply (V9). When the backend is not `es` every _search* endpoint returns
// SEARCH_DISABLED uniformly.
func (h *Handler) Route(r *wkhttp.WKHttp) {
	g := r.Group("/v1/messages",
		h.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, h.ctx),
		spacepkg.SpaceMiddleware(h.ctx),
		h.searchRateLimiter(),
		h.auditMiddleware(),
		h.backendGate(),
	)
	for _, mount := range routeMounters {
		mount(h, g)
	}
	// _search_file_types must NOT sit under the /_search* chain (§7.5): the
	// backendGate would refuse it in disabled/zinc deployments and the Space
	// middleware would 403 clients that haven't picked a Space, even though
	// the enum is a static dictionary with no backend dependency. Mounted
	// separately with only AuthMiddleware + the shared UID rate limiter.
	h.mountFileTypesRoute(r)
}

// backendGate refuses every _search* request with SEARCH_DISABLED unless the
// resolved backend is `es`. It sits after the rate-limiter + audit middleware
// so the refusal is still counted and logged (V9: failure paths must not be a
// free enumeration oracle).
func (h *Handler) backendGate() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if !h.mode.ESServe {
			respondSearchDisabled(c)
			c.Abort()
			return
		}
		c.Next()
	}
}

// routeMounters is populated by each search_*.go file's init() so handlers
// can be added in independent commits without churning api.go.
var routeMounters []func(*Handler, *wkhttp.RouterGroup)

func registerRoute(mount func(*Handler, *wkhttp.RouterGroup)) {
	routeMounters = append(routeMounters, mount)
}
