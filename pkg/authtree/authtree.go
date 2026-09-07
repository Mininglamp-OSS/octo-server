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
// # Routes NOT on any tree, recorded deliberately
//
// modules/project's routes (/v1/space/:space_id/projects and /v1/projects/*) are
// session routes and are contributed to NO tree. That is a decision, not an
// omission, so it is written here rather than left as the absence of an Add call:
//
//   - `uk_*` User API Keys stay SPACE-scoped and are intentionally NOT
//     Project-scoped. A key freezes one Space at issue time; giving it a Project
//     dimension would mean a second tenant axis on a credential whose holder cannot
//     see or rotate that axis, and Project is explicitly not a read boundary — Space
//     remains the only security boundary. An automation credential therefore reaches
//     exactly the Space it was issued against, whatever Projects exist inside it.
//   - Bot tokens likewise gain nothing: a bot has no space_member row, so it has no
//     Project seat either, and every Project route's gate is the caller's own Space
//     membership.
//
// P1 added a Project dimension to two EXISTING session routes, and neither
// changes the picture above — recorded so the absence of a census entry is a
// decision rather than an oversight:
//
//   - POST /v1/group/create gained an optional body field `project_id`. It is on
//     no tree, and it is deliberately NOT Project-scoped in the tree sense: what
//     confines it is still the caller's Space membership, and project_id is
//     validated to belong to that same Space before the group is created. A
//     project id from another Space is answered with the same error as one that
//     does not exist, so the field cannot be used to learn which Space a project
//     lives in.
//   - POST /v1/auth/verify gained optional body fields `space_id` and
//     `project_ids[]`, answered only under ?include=context. It carries no
//     AuthMiddleware by design — the token IS the request — and the answers are
//     scoped to the token holder: a request can only ask about projects, and only
//     ever learns whether the TOKEN HOLDER is a member. Non-membership, absence
//     and cross-Space are one indistinguishable answer, so naming an arbitrary
//     project id reveals nothing the caller did not already supply.
//
// Both are BODY parameters, which the mediation note below explicitly does not
// cover. That remains true and is why neither route can be contributed to a tree
// as-is: enforceKeySpace mediates path params, the query string and X-Space-ID,
// so a tree-mounted non-GET route would need its body reconciled with the Space
// frozen into the credential first.
//
// If a Project route is ever contributed to a tree, it needs a census entry above
// with each of its request-derived inputs enumerated, exactly like the others.
//
// Mediation covers path params, the query string and X-Space-ID — NOT request
// bodies. Every route above is a GET, so that is complete today; see
// enforceKeySpace's comment before adding a non-GET route.
//
// The two file routes are the only place where a uk key's reach is not defined by
// the Space frozen into it, because what they authorize is an object key rather
// than a tenant-owned row. The two sides are NOT symmetric, and this change treats
// them differently:
//
// Download stays ScopeUnscoped — deferred, not solved. Be precise about why, since
// an earlier draft of this comment justified it with a property the code does not
// have. Object confidentiality is not enforced anywhere in this repository today.
// Of the six storage backends in modules/file, only two apply a public-read policy
// at all, and only on the path where they create the bucket themselves:
// service_minio.go's ensureBucket returns early when BucketExists reports true, so
// SetBucketPolicy never runs for an ops-provisioned bucket, and service_oss.go's
// CreateBucket(oss.ACLPublicRead) sits behind a `bucket == nil` check that
// client.Bucket does not satisfy. service_cos.go, service_s3.go, service_qiniu.go
// and service_seaweedfs.go apply none. service_s3.go's own DownloadURL comment
// states the opposite of the convenient reading — private buckets are "the AWS
// default with Block Public Access on", and /v1/file/download/url is named there as
// the presigned route that exists FOR them.
//
// So on a private-bucket deployment the signed URL is the read capability, and a
// Space-A key presenting a Space-B object key gets a working authenticated GET.
// What keeps that off this route's critical path is not a boundary here: keys are
// <type>/<ts>/<uuid>/<uuid><ext>, high-entropy and not enumerable, so it needs an
// out-of-band key leak; every message-read route on this tree IS Space-confined, so
// a Space-A key cannot harvest Space-B keys through them; and the exposure equals
// the human session route already in production. Closing it properly means an
// ownership table, a capability token or a bucket-policy change — a layer below this
// route, covering the human route too — and is tracked as separate work. Deferring
// it is a product decision; ScopeUnscoped is the honest declaration meanwhile.
//
// Upload is ScopeRouteGuard, tightened here. Its risk is integrity, not
// confidentiality: signing a PUT for a caller-named key hands out write access to
// an existing object, and unlike the read side that boundary really does live on
// this route — nothing downstream re-checks it. file.rejectCallerObjectKey refuses a
// non-empty `path` outright, so the key is always server-minted and no known-key
// overwrite is reachable. Rejecting rather than validating is the point: the server
// has no fact source for "who owns this key" — that is the ownership table above —
// so removing the caller's ability to name a target is the only guard here that
// cannot be written wrong. The bot tree already works this way
// (bot_api.botUploadPresigned takes only filename + fileSize). The human session
// routes and the shared handler are unchanged — a browser's custom `path` is a
// capability octo-web uses today.
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
	// specific to this route removes the caller's cross-tenant reach — either by
	// rejecting targets outside the credential's tenant, or by taking away the
	// ability to name a target at all when the server has no ownership fact to
	// check against (see file.rejectCallerObjectKey, which refuses a caller-named
	// object key so only server-minted ones exist; note such a route is not
	// *confined* to the bound Space, it simply has no cross-tenant target left).
	// The guard is either contributed here in Middlewares (see
	// user.requireBoundSpaceMember, message.requireBoundSpaceGroup) or installed by
	// the tree at its mount point when only the tree knows the credential's
	// semantics (see bot_api.appBotScopeGuard).
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
