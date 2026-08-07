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
//
// # Tenant posture per route
//
// Every Route must declare a TenantScope (see Route.Tenant), so what confines a
// contribution is answerable without tracing into its handler.
//
// The census below lists, per route, EVERY request-derived input its handler reads
// and the confinement for each. Enumerating inputs rather than naming one anchor
// per route is deliberate: PR #713's review found four cross-tenant reads in a row
// by walking from a guarded input to an unguarded sibling on the same route or the
// same tree (/users/:uid was pinned on :uid while its group_no query stayed open
// for three rounds). A route whose inputs cannot be enumerated does not belong on a
// tenant-scoped tree. Keep this current when adding a route or when a reused
// handler starts reading a new input.
//
//	TreeUserKey (`uk_*`, tenant = the Space frozen into the key)
//	  GET /user/search                                  ScopeRouteGuard
//	      ?space_id   pinned to the bound Space by enforceKeySpace
//	      ?keyword    not tenant-bearing; handler returns only bound-Space members
//	  GET /space/:space_id/members                      ScopeRouteGuard
//	      :space_id   enforceKeySpace rejects a mismatch with the bound Space
//	      ?page/?limit  pagination only
//	  GET /space/my                                     ScopeRouteGuard
//	      (no input)  boundSpaceOnly reports only the bound Space
//	  GET /messages/person/:peer_uid/:message_id        ScopeBoundSpace
//	      :peer_uid   channel is derived from (peer, key owner); a third party's DM
//	                  is unreachable, and checkPersonDMAccess gates the rest
//	      :message_id per-message Space filter via personSpaceAllows on the bound Space
//	  GET /users/:uid                                   ScopeRouteGuard
//	      :uid        user.requireBoundSpaceMember — must be a bound-Space member
//	                  (exempt: self, system bots)
//	      ?group_no   STRIPPED by that guard. Unguarded it returns another Space's
//	                  group metadata (source_space_id/name, vercode) because
//	                  GetGroupMember/IsShowShortNo never check the caller.
//	  GET /groups/:group_no/messages/:message_id        ScopeRouteGuard
//	      :group_no   message.requireBoundSpaceGroup — group's effective Space must
//	                  equal the bound Space
//	      :message_id visibility handled by respondSingleMessage
//	  GET /groups/:group_no/threads/:short_id/messages/:message_id  ScopeRouteGuard
//	      :group_no   same guard (a thread's attribution is its parent group's)
//	      :short_id / :message_id  thread status + visibility, not tenant-bearing
//	  GET /file/upload/presigned                        ScopeRouteGuard
//	      ?path       REJECTED when non-empty by file.rejectCallerObjectKey, so
//	                  the object key is always server-minted — a caller cannot
//	                  name an existing object to obtain a PUT URL for it
//	      ?type       allow-listed by checkReq; becomes the key's prefix
//	      ?filename/?fileSize/?contentType  only the (allow-listed) extension
//	                  reaches the key; filename goes to Content-Disposition
//	  GET /file/download/url                            ScopeUnscoped
//	      ?path/?filename/?disposition  signs a GET for a caller-named object key;
//	                  deliberately NOT confined — see below
//
//	TreeBotToken (`bf_*` / `app_*`; a bot token freezes no Space and a bot has no
//	space_member row, so there is normally no tenant to enforce)
//	  GET /messages/person/:peer_uid/:message_id        ScopeRouteGuard
//	      :peer_uid   bot_api.appBotScopeGuard — a scope=space App Bot's peer must
//	                  still be a member of its bound Space (mirrors the send path);
//	                  `bf_*` and platform-scope App Bots are gated by
//	                  checkPersonDMAccess's friend + bilateral blacklist
//	  GET /groups/:group_no/messages/:message_id        ScopeRouteGuard
//	      :group_no   appBotScopeGuard rejects App Bots outright (DM-only, matching
//	                  every other bot group endpoint); for `bf_*` the gate is the
//	                  bot's own active group membership
//	  GET /groups/:group_no/threads/:short_id/messages/:message_id  ScopeRouteGuard
//	      :group_no   same
//
// Mediation covers path params, the query string and X-Space-ID — NOT request
// bodies. Every route above is a GET, so that is complete today; see
// enforceKeySpace's comment before adding a non-GET route.
//
// The two file routes are the only place where a uk key's reach is not defined by
// the Space frozen into it, because what they authorize is an object key rather
// than a tenant-owned row. The two sides are NOT symmetric, and this PR treats
// them differently on a maintainer-confirmed trade-off:
//
// Download stays ScopeUnscoped, deliberately. Buckets are created public-read
// (modules/file/service_minio.go applies readOnlyAnonymousPolicy — "allow
// anonymous download only"; service_oss.go creates with oss.ACLPublicRead), so
// knowing an object key is already enough to GET the object with no credential at
// all. Under that deployment model an object key IS the read capability, and the
// 30-minute signed URL supplies filename/disposition rather than confidentiality.
// Adding a Space check to this one signing route would therefore move no real
// boundary while implying one exists. Confidentiality has to be fixed a layer
// down (ownership table, capability token, bucket policy) and is tracked
// separately.
//
// Upload is ScopeRouteGuard, tightened here. Its risk is integrity, not
// confidentiality: signing a PUT for a caller-named key hands out write access to
// an existing object, and that boundary really does live on this route — nothing
// downstream re-checks it. file.rejectCallerObjectKey refuses a non-empty `path`
// outright, so the key is always server-minted and no known-key overwrite is
// reachable. Rejecting rather than validating is the point: the server has no
// fact source for "who owns this key", so removing the caller's ability to name a
// target is the only guard that cannot be written wrong. The bot tree already
// works this way (bot_api.botUploadPresigned takes only filename + fileSize).
// The human session routes and the shared handler are unchanged — a browser's
// custom `path` is a capability octo-web uses today.
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
// this runs. A key issued before Space binding carries no Space, so it keeps the
// empty-Space path exactly as a session without a verified Space does — a
// backwards-compatibility case, not the posture new keys should be issued in.
// A bot token likewise carries no Space and a bot has no space_member row, so
// TreeBotToken routes keep the empty-Space path; what stops them reaching
// another tenant's data is the handler's own relationship gate, not this.
//
// Add it per route rather than tree-wide: it is only correct for handlers whose
// Space semantics match "the key's Space", and a tree-wide c.Set would silently
// change the behaviour of every route mounted later.
//
// Follow-up (not this change): this is the only reason pkg/authtree depends on
// pkg/space and the only reason space.SetSpaceID is exported. Both narrow if
// pkg/space grows a middleware constructor taking a resolver func, letting the
// contributing module supply BoundSpaceID at the call site.
func BoundSpaceContext() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if spaceID := BoundSpaceID(c); spaceID != "" {
			space.SetSpaceID(c, spaceID)
		}
		c.Next()
	}
}

// TenantScope declares what confines a contributed route to the credential's
// tenant. It is required on every Route.
//
// Why a declaration rather than a convention. The tree prepends a tenant
// middleware that validates and pins the request's space_id, which makes every
// route mounted under it *look* protected. But injection only isolates a handler
// that reads the injected value: a handler keyed on a bare :uid or :group_no
// ignores it, and the route silently answers across tenants (PR #713 review —
// three of the four blockers were exactly this shape). Naming the anchor forces
// the contributor to answer "what actually confines this?" at the Add site, and
// makes the answer reviewable in one place instead of requiring a trace into each
// handler.
type TenantScope string

const (
	// ScopeBoundSpace: the handler isolates on pkg/space's GetSpaceID, so
	// publishing the credential's bound Space as the request Space is the whole
	// anchor. MountOn injects BoundSpaceContext for these routes — declaring the
	// scope is what installs it, so the declaration cannot drift from the wiring.
	ScopeBoundSpace TenantScope = "bound_space"
	// ScopeRouteGuard: the handler does not read the request Space, so a guard
	// specific to this route rejects targets outside the credential's tenant —
	// either contributed here in Middlewares (see user.requireBoundSpaceMember,
	// message.requireBoundSpaceGroup) or installed by the tree at its mount point
	// when only the tree knows the credential's semantics (see
	// bot_api.appBotScopeGuard).
	ScopeRouteGuard TenantScope = "route_guard"
	// ScopeUnscoped: the route is deliberately NOT confined to a bound Space, and
	// the handler's own gates (friendship, group membership, bilateral blacklist)
	// are the entire boundary. Every TreeBotToken route is unscoped by
	// construction — a bot token freezes no Space and a bot has no space_member
	// row. Use it on TreeUserKey only with the reason written at the Add site.
	ScopeUnscoped TenantScope = "unscoped"
)

// Route is one handler a module contributes to a tree.
type Route struct {
	// Method is an HTTP method constant. Only the verbs wkhttp.RouterGroup
	// exposes (GET/POST/PUT/DELETE) can be mounted.
	Method string
	// Path is relative to the tree's group, e.g. "/file/upload/presigned"
	// resolves to /v1/user/file/upload/presigned on TreeUserKey.
	Path string
	// Tenant declares what confines this route to the credential's tenant.
	// Required: Add panics on the zero value, so a route cannot reach production
	// without someone having answered the question.
	Tenant TenantScope
	// Middlewares run after the tree's own middleware and before Handler. Use it
	// to carry route-local hardening the human route also applies (e.g. the
	// per-IP limiter on user search), which the tree cannot know about, and the
	// route's own tenant guard when Tenant is ScopeRouteGuard.
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
//
// Panics on a route with no Tenant declaration: an undeclared route is a wiring
// bug that must surface at startup, not as a cross-tenant read in production.
func Add(tree Tree, router *wkhttp.WKHttp, route Route) {
	if route.Tenant == "" {
		panic(fmt.Sprintf("authtree: route %s %q has no Tenant declaration", route.Method, route.Path))
	}
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
//
// A ScopeBoundSpace route gets BoundSpaceContext installed here, after the tree's
// own middleware (which is what validates the binding) and before the route's
// own. Injecting from the declaration rather than asking each contributor to
// remember the middleware is what keeps the two from drifting apart. It is a
// no-op on a tree whose credential carries no Space, so this is safe for every
// tree.
func MountOn(group *wkhttp.RouterGroup, before ...wkhttp.HandlerFunc) Mounter {
	return func(route Route) {
		handlers := make([]wkhttp.HandlerFunc, 0, len(before)+len(route.Middlewares)+2)
		handlers = append(handlers, before...)
		if route.Tenant == ScopeBoundSpace {
			handlers = append(handlers, BoundSpaceContext())
		}
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
