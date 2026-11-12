package cliv2

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	sdk "codex.mlamp.cn/dmwork/octo-auth-sdk"
	"codex.mlamp.cn/dmwork/octo-auth-sdk/permission"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// Route is one explicitly authorized CLI v2 endpoint. Path is a Gin route path
// under /v2/cli; Action is a concrete, server-owned permission action.
type Route struct {
	Method      string
	Path        string
	Action      string
	Bot         wkhttp.HandlerFunc
	User        wkhttp.HandlerFunc
	UserOnly    bool
	Middlewares []wkhttp.HandlerFunc
}

type routeKey struct {
	method string
	path   string
}

type routeContribution struct {
	route   Route
	hasBot  bool
	hasUser bool
}

type sourceState struct {
	routes  map[routeKey]*routeContribution
	mounted bool
}

type targetState struct {
	auth           *sdk.Client
	uidRateLimiter wkhttp.HandlerFunc
	routes         map[routeKey]struct{}
}

var routeRegistry = struct {
	sync.Mutex
	sources map[*wkhttp.WKHttp]*sourceState
	targets map[*wkhttp.WKHttp]*targetState
}{
	sources: make(map[*wkhttp.WKHttp]*sourceState),
	targets: make(map[*wkhttp.WKHttp]*targetState),
}

// NewRouter creates an independent WKHttp router backed by the real Auth SDK.
// It never caches resolved identities or permissions.
func NewRouter(authBaseURL, serviceSecret string) (*wkhttp.WKHttp, error) {
	client, err := sdk.NewClient(authBaseURL, serviceSecret)
	if err != nil {
		return nil, fmt.Errorf("cliv2: create auth SDK client: %w", err)
	}

	router := wkhttp.New()
	router.SetErrorRenderer(errorRenderer{})
	router.Use(func(c *wkhttp.Context) {
		if c.FullPath() == "" {
			Fail(c, http.StatusNotFound, "NOT_FOUND", "Resource not found.")
			c.Abort()
			return
		}
		c.Next()
	})
	routeRegistry.Lock()
	routeRegistry.targets[router] = &targetState{
		auth:   client,
		routes: make(map[routeKey]struct{}),
	}
	routeRegistry.Unlock()
	return router, nil
}

// SetUIDRateLimiter installs the shared authenticated-user limiter on target.
// Mount refuses to expose routes until it has been configured.
func SetUIDRateLimiter(target *wkhttp.WKHttp, middleware wkhttp.HandlerFunc) error {
	if target == nil || middleware == nil {
		return errors.New("cliv2: target and UID rate limiter are required")
	}

	routeRegistry.Lock()
	defer routeRegistry.Unlock()
	state := routeRegistry.targets[target]
	if state == nil {
		return errors.New("cliv2: target was not created by NewRouter")
	}
	if len(state.routes) != 0 {
		return errors.New("cliv2: UID rate limiter must be configured before Mount")
	}
	if state.uidRateLimiter != nil {
		return errors.New("cliv2: UID rate limiter is already configured")
	}
	state.uidRateLimiter = middleware
	return nil
}

// Add contributes a module-bound handler to source's CLI v2 route. Modules may
// contribute User and Bot handlers separately during module setup. Conflicting
// declarations panic so an ambiguous authorization boundary prevents startup.
func Add(source *wkhttp.WKHttp, route Route) {
	if source == nil {
		panic("cliv2: route source is nil")
	}
	validateRoute(route)

	key := routeKey{method: route.Method, path: route.Path}
	routeRegistry.Lock()
	defer routeRegistry.Unlock()

	state := routeRegistry.sources[source]
	if state == nil {
		state = &sourceState{routes: make(map[routeKey]*routeContribution)}
		routeRegistry.sources[source] = state
	}
	if state.mounted {
		panic(fmt.Sprintf("cliv2: route %s %q contributed after Mount", route.Method, route.Path))
	}

	contribution := state.routes[key]
	if contribution == nil {
		contribution = &routeContribution{route: Route{
			Method:   route.Method,
			Path:     route.Path,
			Action:   route.Action,
			UserOnly: route.UserOnly,
		}}
		state.routes[key] = contribution
	} else if contribution.route.Action != route.Action || contribution.route.UserOnly != route.UserOnly {
		panic(fmt.Sprintf("cliv2: conflicting authorization declaration for %s %q", route.Method, route.Path))
	}

	if route.User != nil {
		if contribution.hasUser {
			panic(fmt.Sprintf("cliv2: duplicate User handler for %s %q", route.Method, route.Path))
		}
		contribution.route.User = route.User
		contribution.hasUser = true
	}
	if route.Bot != nil {
		if contribution.hasBot {
			panic(fmt.Sprintf("cliv2: duplicate Bot handler for %s %q", route.Method, route.Path))
		}
		contribution.route.Bot = route.Bot
		contribution.hasBot = true
	}
	contribution.route.Middlewares = append(contribution.route.Middlewares, route.Middlewares...)
}

// Mount validates the complete module contributions and installs them on target,
// which must be a NewRouter-created independent SDK-backed router. Call it after
// module setup and before serving requests.
func Mount(source, target *wkhttp.WKHttp) error {
	if source == nil || target == nil {
		return errors.New("cliv2: source and target routers are required")
	}
	if source == target {
		return errors.New("cliv2: target router must be independent from source")
	}

	routeRegistry.Lock()
	state := routeRegistry.sources[source]
	if state == nil {
		state = &sourceState{routes: make(map[routeKey]*routeContribution)}
		routeRegistry.sources[source] = state
	}
	if state.mounted {
		routeRegistry.Unlock()
		return errors.New("cliv2: source routes already mounted")
	}
	targetInfo := routeRegistry.targets[target]
	if targetInfo == nil || targetInfo.auth == nil {
		routeRegistry.Unlock()
		return errors.New("cliv2: target was not created by NewRouter")
	}
	if targetInfo.uidRateLimiter == nil {
		routeRegistry.Unlock()
		return errors.New("cliv2: UID rate limiter must be configured before Mount")
	}

	routes := make([]Route, 0, len(state.routes))
	for _, contribution := range state.routes {
		route := contribution.route
		if !contribution.hasUser {
			routeRegistry.Unlock()
			return fmt.Errorf("cliv2: User handler is required for %s %q", route.Method, route.Path)
		}
		if !route.UserOnly && !contribution.hasBot {
			routeRegistry.Unlock()
			return fmt.Errorf("cliv2: Bot handler is required for %s %q", route.Method, route.Path)
		}
		key := routeKey{method: route.Method, path: route.Path}
		if _, exists := targetInfo.routes[key]; exists {
			routeRegistry.Unlock()
			return fmt.Errorf("cliv2: duplicate mounted route %s %q", route.Method, route.Path)
		}
		routes = append(routes, route)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Method != routes[j].Method {
			return routes[i].Method < routes[j].Method
		}
		return routes[i].Path < routes[j].Path
	})
	state.mounted = true
	for _, route := range routes {
		targetInfo.routes[routeKey{method: route.Method, path: route.Path}] = struct{}{}
	}
	auth := targetInfo.auth
	uidRateLimiter := targetInfo.uidRateLimiter
	routeRegistry.Unlock()

	for _, route := range routes {
		if err := registerRoute(target, auth, uidRateLimiter, route); err != nil {
			return err
		}
	}
	return nil
}

func validateRoute(route Route) {
	switch route.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		panic(fmt.Sprintf("cliv2: unsupported method %q", route.Method))
	}
	if !strings.HasPrefix(route.Path, "/v2/cli/") || strings.ContainsAny(route.Path, "?#") {
		panic(fmt.Sprintf("cliv2: path must be an absolute /v2/cli route: %q", route.Path))
	}
	if _, _, ok := permission.SplitAction(route.Action, false); !ok {
		panic(fmt.Sprintf("cliv2: action must be a concrete permission action: %q", route.Action))
	}
	for _, middleware := range route.Middlewares {
		if middleware == nil {
			panic(fmt.Sprintf("cliv2: nil middleware for %s %q", route.Method, route.Path))
		}
	}
}

func registerRoute(target *wkhttp.WKHttp, auth *sdk.Client, uidRateLimiter wkhttp.HandlerFunc, route Route) error {
	handlers := make([]wkhttp.HandlerFunc, 0, len(route.Middlewares)+4)
	handlers = append(handlers, authenticate(auth, route.UserOnly))
	handlers = append(handlers, uidRateLimiter)
	handlers = append(handlers, authorizeAction(route.Action))
	handlers = append(handlers, route.Middlewares...)
	handlers = append(handlers, selectHandler(route))

	group := target.Group("")
	switch route.Method {
	case http.MethodGet:
		group.GET(route.Path, handlers...)
	case http.MethodPost:
		group.POST(route.Path, handlers...)
	case http.MethodPut:
		group.PUT(route.Path, handlers...)
	case http.MethodDelete:
		group.DELETE(route.Path, handlers...)
	case http.MethodPatch:
		ginHandlers := make([]gin.HandlerFunc, 0, len(handlers))
		for _, handler := range handlers {
			ginHandlers = append(ginHandlers, target.WKHttpHandler(handler))
		}
		group.RouterGroup.PATCH(route.Path, ginHandlers...)
	default:
		return fmt.Errorf("cliv2: unsupported method %q", route.Method)
	}
	return nil
}

func selectHandler(route Route) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		principal := PrincipalFrom(c)
		if route.UserOnly || principal.BotID == "" {
			route.User(c)
			return
		}
		route.Bot(c)
	}
}
