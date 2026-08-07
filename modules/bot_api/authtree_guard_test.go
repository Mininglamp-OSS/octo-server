package bot_api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appBotGuardCredential is the bot-tree credential a case authenticates as.
type appBotGuardCredential struct {
	kind    string
	scope   string
	spaceID string
}

// newAppBotGuardRoute mounts the two contributed bot-tree shapes (a DM read with
// :peer_uid and a group read without one) behind appBotScopeGuard, with the
// space-membership query injected. spaceMemberQueryOverride exists for exactly this
// kind of DB-free App Bot permission test (see send_permission_observability_test).
func newAppBotGuardRoute(cred appBotGuardCredential, check func(uid, spaceID string) (bool, error)) *wkhttp.WKHttp {
	ba := &BotAPI{Log: log.NewTLog("app-bot-dm-guard-test"), spaceMemberQueryOverride: check}
	r := wkhttp.New()
	group := r.Group("/v1/bot", func(c *wkhttp.Context) {
		// Mirrors authBot: bot kind, and for a scope=space App Bot its bound Space,
		// land in the context before the contributed routes run.
		c.Set(CtxKeyBotKind, cred.kind)
		if cred.scope != "" {
			c.Set(CtxKeyAppBotScope, cred.scope)
		}
		if cred.spaceID != "" {
			c.Set(CtxKeyAppBotSpaceID, cred.spaceID)
		}
		c.Next()
	})
	mount := authtree.MountOn(group, ba.appBotScopeGuard())
	reached := func(c *wkhttp.Context) { c.String(http.StatusOK, "reached") }
	mount(authtree.Route{
		Method: http.MethodGet, Path: "/messages/person/:peer_uid/:message_id",
		Tenant: authtree.ScopeRouteGuard, Handler: reached,
	})
	mount(authtree.Route{
		Method: http.MethodGet, Path: "/groups/:group_no/messages/:message_id",
		Tenant: authtree.ScopeRouteGuard, Handler: reached,
	})
	return r
}

func appBotGuardGet(t *testing.T, r *wkhttp.WKHttp, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

const appBotDMPath = "/v1/bot/messages/person/u_peer/1"

var allowMembership = func(string, string) (bool, error) { return true, nil }

// 🔴 PR #713 review round 4 (Jerry-Xin) — the App Bot authorization bypass this
// guard exists for.
//
// getPersonMessage's own gate (checkPersonDMAccess) is "same Space OR friend" for a
// real-user peer, so a friendship row alone opens the read; it never looks at the
// App Bot's scope=space binding. The send path is stricter: checkSendPermission's
// BotKindApp rule 3 refuses when the target is no longer a member of the bot's
// Space. Without this guard a scope=space App Bot can therefore READ historical DM
// bodies for a user it can no longer SEND to.
func TestAppBotScopeGuardRefusesPeerOutsideBoundSpace(t *testing.T) {
	var gotUID, gotSpace string
	r := newAppBotGuardRoute(
		appBotGuardCredential{kind: BotKindApp, scope: "space", spaceID: "sp_bot"},
		func(uid, spaceID string) (bool, error) {
			gotUID, gotSpace = uid, spaceID
			return false, nil
		})

	w := appBotGuardGet(t, r, appBotDMPath)
	assert.NotEqual(t, http.StatusOK, w.Code,
		"a peer outside the App Bot's Space must not be readable: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "reached", "the handler must not run")
	assert.Equal(t, "u_peer", gotUID, "membership is checked for the DM peer")
	assert.Equal(t, "sp_bot", gotSpace, "against the App Bot's bound Space")
}

// The positive control: a peer still inside the bound Space reads normally, so the
// guard narrows the reachable set rather than disabling the route.
func TestAppBotScopeGuardAllowsPeerInBoundSpace(t *testing.T) {
	r := newAppBotGuardRoute(
		appBotGuardCredential{kind: BotKindApp, scope: "space", spaceID: "sp_bot"},
		allowMembership)

	w := appBotGuardGet(t, r, appBotDMPath)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "reached", w.Body.String())
}

// Credentials the rule does not apply to must pass through untouched: a User Bot
// (`bf_`) has no scope binding at all, and a platform-scope App Bot is deliberately
// not Space-confined. Neither may cost a membership lookup.
func TestAppBotScopeGuardIgnoresUnscopedCredentials(t *testing.T) {
	for _, cred := range []appBotGuardCredential{
		{kind: BotKindUser},
		{kind: BotKindApp, scope: "platform"},
	} {
		called := false
		r := newAppBotGuardRoute(cred, func(string, string) (bool, error) {
			called = true
			return false, nil
		})

		w := appBotGuardGet(t, r, appBotDMPath)
		require.Equal(t, http.StatusOK, w.Code, "kind=%s scope=%s: %s", cred.kind, cred.scope, w.Body.String())
		assert.False(t, called, "kind=%s scope=%s must not trigger a Space check", cred.kind, cred.scope)
	}
}

// A scope=space App Bot with no bound Space in context is a wiring fault, and the
// send path treats it as a hard failure rather than a pass. The read must too.
func TestAppBotScopeGuardFailsClosedOnMissingSpaceContext(t *testing.T) {
	r := newAppBotGuardRoute(
		appBotGuardCredential{kind: BotKindApp, scope: "space"},
		allowMembership)

	w := appBotGuardGet(t, r, appBotDMPath)
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "reached")
}

// A membership-lookup failure must not be folded into "allowed".
func TestAppBotScopeGuardFailsClosedOnCheckError(t *testing.T) {
	r := newAppBotGuardRoute(
		appBotGuardCredential{kind: BotKindApp, scope: "space", spaceID: "sp_bot"},
		func(string, string) (bool, error) { return false, errors.New("space member store down") })

	w := appBotGuardGet(t, r, appBotDMPath)
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "reached")
}

// PR #713 review round 4 P2-1 — the group shape. Every other bot group/thread
// endpoint rejects App Bots outright ("App Bot is DM-only", validateBotGroupAccess
// and the inline equivalents in groups.go). These two contributed routes happen to
// 404 for `app_*` anyway, because requireGroupMember needs an active group_member
// row and App Bots have none — but that is luck standing in for the policy the rest
// of the module states explicitly, so state it here too.
func TestAppBotScopeGuardRejectsAppBotOnGroupRoutes(t *testing.T) {
	called := false
	r := newAppBotGuardRoute(
		appBotGuardCredential{kind: BotKindApp, scope: "space", spaceID: "sp_bot"},
		func(string, string) (bool, error) {
			called = true
			return true, nil
		})

	w := appBotGuardGet(t, r, "/v1/bot/groups/g_1/messages/1")
	assert.NotEqual(t, http.StatusOK, w.Code,
		"App Bot is DM-only; group reads must be refused: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "reached", "the handler must not run")
	assert.False(t, called, "a group route has no DM peer to check membership for")

	// A platform-scope App Bot is refused on the same grounds — the DM-only policy is
	// about the credential kind, not its scope.
	platform := appBotGuardGet(t,
		newAppBotGuardRoute(appBotGuardCredential{kind: BotKindApp, scope: "platform"}, allowMembership),
		"/v1/bot/groups/g_1/messages/1")
	assert.NotEqual(t, http.StatusOK, platform.Code, platform.Body.String())
}

// A User Bot (`bf_*`) must keep reaching the group route — its gate is its own active
// group membership, and the App Bot rejection must not cost it that capability.
func TestAppBotScopeGuardAllowsUserBotOnGroupRoutes(t *testing.T) {
	called := false
	r := newAppBotGuardRoute(appBotGuardCredential{kind: BotKindUser}, func(string, string) (bool, error) {
		called = true
		return false, nil
	})

	w := appBotGuardGet(t, r, "/v1/bot/groups/g_1/messages/1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "reached", w.Body.String())
	assert.False(t, called, "a group route must not trigger a DM-peer Space check")
}
