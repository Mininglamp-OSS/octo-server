// Package authtree wires feature-module handlers into octo-server's non-session
// authentication trees, so a capability implemented once for browser sessions can
// also be reached by automation credentials.
//
// Two trees consume it:
//
//   - TreeUserKey  — /v1/user/*, User API Key (`uk_*`), mounted by modules/botfather
//   - TreeBotToken — /v1/bot/*, bot token (`bf_*` / `app_*`), mounted by modules/bot_api
//
// Why the indirection rather than a direct call. The handlers being reused are
// unexported methods on module structs (*file.File, *space.Space, *user.User,
// *message.Message), so whoever mounts them must hold the instance. The mounting
// module cannot obtain one:
//
//   - modules/botfather already imports file/space/user, so those modules cannot
//     import it back to register themselves.
//   - Constructing a second instance is not equivalent to the first. user.New
//     installs process-global providers (source.SetUserProvider) and message.New
//     registers event listeners, so a duplicate changes runtime behaviour rather
//     than just costing memory.
//
// Each module therefore hands its already-bound handler to this package from its
// own Route(), and the tree that owns the credential mounts it under its own
// authentication, rate-limit and tenant middleware. The tree keeps full control
// of authorization; a contribution is only ever a handler plus the path it
// answers on.
//
// Registration order does not matter. State is keyed by (tree, router): Mount
// installs whatever pended, and an Add arriving after Mount is installed
// immediately. Keying on the router — not on a process-global list — is what
// makes repeated module.Setup calls safe: every testutil.NewTestServer builds a
// fresh engine, and routes must never leak into a previous one.
package authtree

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
)

// Tree identifies one non-session authentication tree.
type Tree string

const (
	// TreeUserKey is the User API Key tree under /v1/user, mounted by modules/botfather.
	TreeUserKey Tree = "user_key"
	// TreeBotToken is the bot-token tree under /v1/bot, mounted by modules/bot_api.
	TreeBotToken Tree = "bot_token"
)

// CtxKeySpaceID is the context key the User API Key middleware writes the
// key-bound Space into. Declared here so a contributing module can read the
// verified tenant without importing modules/botfather.
const CtxKeySpaceID = "api_key_space_id"

// BoundSpaceID returns the Space frozen into the presented user API key, or ""
// when the caller did not arrive through that tree or the key predates Space
// binding.
func BoundSpaceID(c *wkhttp.Context) string {
	v, _ := c.Get(CtxKeySpaceID)
	s, _ := v.(string)
	return s
}

// BoundSpaceContext publishes the user-key tree's verified tenant as the
// request's Space, so a reused human handler that reads pkg/space's GetSpaceID
// isolates by Space exactly as it does for a browser session presenting
// X-Space-ID. Without it those handlers see an empty Space and take their
// backwards-compatibility path — which skips the isolation entirely.
//
// Only TreeUserKey has a tenant to publish: its credential freezes a Space at
// issue time and its mount re-checks the owner's membership in that Space before
// this runs. A bot token carries no Space and a bot has no space_member row, so
// TreeBotToken routes keep the empty-Space path; what stops them reaching
// another tenant's data is the handler's own relationship gate, not this.
//
// Add it per route rather than tree-wide: it is only correct for handlers whose
// Space semantics match "the key's Space", and a tree-wide c.Set would silently
// change the behaviour of every route mounted later.
func BoundSpaceContext() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if spaceID := BoundSpaceID(c); spaceID != "" {
			space.SetSpaceID(c, spaceID)
		}
		c.Next()
	}
}

// Route is one handler a module contributes to a tree.
type Route struct {
	// Method is an HTTP method constant. Only the verbs wkhttp.RouterGroup
	// exposes (GET/POST/PUT/DELETE) can be mounted.
	Method string
	// Path is relative to the tree's group, e.g. "/file/upload/presigned"
	// resolves to /v1/user/file/upload/presigned on TreeUserKey.
	Path string
	// Middlewares run after the tree's own middleware and before Handler. Use it
	// to carry route-local hardening the human route also applies (e.g. the
	// per-IP limiter on user search), which the tree cannot know about.
	Middlewares []wkhttp.HandlerFunc
	// Handler is the module's bound handler method.
	Handler wkhttp.HandlerFunc
}

// Mounter installs one Route into the tree's authenticated group. The tree
// prepends its own middleware; MountOn builds one for the common case.
type Mounter func(Route)

type key struct {
	tree   Tree
	router *wkhttp.WKHttp
}

type entry struct {
	mounter Mounter
	pending []Route
}

var (
	mu      sync.Mutex
	entries = map[key]*entry{}
)

// Add contributes a route to tree on router. Call it from the contributing
// module's own Route(r), passing the same r.
func Add(tree Tree, router *wkhttp.WKHttp, route Route) {
	mu.Lock()
	e := entryLocked(tree, router)
	if e.mounter == nil {
		e.pending = append(e.pending, route)
		mu.Unlock()
		return
	}
	mount := e.mounter
	mu.Unlock()
	mount(route)
}

// Mount registers the tree's mounter on router and installs every route already
// contributed for it. Call it from the owning module's Route(r).
func Mount(tree Tree, router *wkhttp.WKHttp, mounter Mounter) {
	mu.Lock()
	e := entryLocked(tree, router)
	e.mounter = mounter
	pending := e.pending
	e.pending = nil
	mu.Unlock()

	for _, route := range pending {
		mounter(route)
	}
}

func entryLocked(tree Tree, router *wkhttp.WKHttp) *entry {
	k := key{tree: tree, router: router}
	e := entries[k]
	if e == nil {
		e = &entry{}
		entries[k] = e
	}
	return e
}

// MountOn returns a Mounter that installs each route into group, prefixing the
// tree-wide middleware in before. Panics on a verb wkhttp.RouterGroup cannot
// register — a contribution with an unmountable method is a wiring bug that must
// surface at startup, not as a 404 in production.
func MountOn(group *wkhttp.RouterGroup, before ...wkhttp.HandlerFunc) Mounter {
	return func(route Route) {
		handlers := make([]wkhttp.HandlerFunc, 0, len(before)+len(route.Middlewares)+1)
		handlers = append(handlers, before...)
		handlers = append(handlers, route.Middlewares...)
		handlers = append(handlers, route.Handler)

		switch route.Method {
		case http.MethodGet:
			group.GET(route.Path, handlers...)
		case http.MethodPost:
			group.POST(route.Path, handlers...)
		case http.MethodPut:
			group.PUT(route.Path, handlers...)
		case http.MethodDelete:
			group.DELETE(route.Path, handlers...)
		default:
			panic(fmt.Sprintf("authtree: unsupported method %q for path %q", route.Method, route.Path))
		}
	}
}
