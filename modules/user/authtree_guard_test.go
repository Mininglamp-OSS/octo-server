package user

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boundGuardKeyUID is the key owner every case below authenticates as.
const boundGuardKeyUID = "u_key_owner"

// requireBoundSpaceMemberWithChecker touches no User field but Log, so the whole
// rule set runs against a bare engine with an injected membership checker — no
// MySQL, Redis or IM. Same harness shape as botfather's newSpaceGuardRouteWithChecker.
func newBoundMemberRoute(boundSpace string, check spacepkg.MembershipChecker) *wkhttp.WKHttp {
	u := &User{Log: log.NewTLog("uk-user-detail-guard-test")}
	r := wkhttp.New()
	group := r.Group("/v1/user", func(c *wkhttp.Context) {
		// Mirrors authUserAPIKey: the key's owner and its frozen Space both land in
		// the context before the route guard runs.
		c.Set("uid", boundGuardKeyUID)
		if boundSpace != "" {
			c.Set(authtree.CtxKeySpaceID, boundSpace)
		}
		c.Next()
	})
	group.GET("/users/:uid",
		func(c *wkhttp.Context) { u.requireBoundSpaceMemberWithChecker(c, check) },
		func(c *wkhttp.Context) { c.String(http.StatusOK, "detail:"+c.Param("uid")) },
	)
	return r
}

func boundMemberGet(t *testing.T, r *wkhttp.WKHttp, uid string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/user/users/"+uid, nil))
	return w
}

// 🔴 PR #713 review, Jerry-Xin blocker 1 / lml2468 P0 — the blocker this guard exists
// for. u.get resolves any known UID globally and never reads the request Space, so
// the tree's space_id injection is a no-op on this route: without the guard a key
// bound to Space A returns a Space-B-only user's full profile (short_no, presence,
// device state, bot metadata, verification). The reachable-profile set must be the
// bound Space's members.
func TestRequireBoundSpaceMemberRejectsTargetOutsideBoundSpace(t *testing.T) {
	var gotSpace, gotUID string
	r := newBoundMemberRoute("sp_a", func(spaceID, uid string) (bool, error) {
		gotSpace, gotUID = spaceID, uid
		return false, nil
	})

	w := boundMemberGet(t, r, "u_outsider")
	assert.NotEqual(t, http.StatusOK, w.Code,
		"a target outside the key's Space must not resolve: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "detail:", "the handler must not run")
	assert.Equal(t, "sp_a", gotSpace, "membership must be checked against the key's bound Space")
	assert.Equal(t, "u_outsider", gotUID, "membership must be checked for the TARGET, not the caller")
}

// A member of the bound Space resolves normally — the guard must not break the
// capability it protects.
func TestRequireBoundSpaceMemberAllowsMemberTarget(t *testing.T) {
	r := newBoundMemberRoute("sp_a", func(string, string) (bool, error) { return true, nil })

	w := boundMemberGet(t, r, "u_peer")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "detail:u_peer", w.Body.String())
}

// Self and system bots are the two exemptions: a caller's own profile is not a
// tenant-scoped resource, and botfather / u_10000 / fileHelper are visible in every
// Space (same posture as space_filter's SystemBots branch). Neither may cost a
// membership lookup that would 404 them.
func TestRequireBoundSpaceMemberExemptsSelfAndSystemBots(t *testing.T) {
	called := false
	deny := func(string, string) (bool, error) {
		called = true
		return false, nil
	}

	self := boundMemberGet(t, newBoundMemberRoute("sp_a", deny), boundGuardKeyUID)
	require.Equal(t, http.StatusOK, self.Code, self.Body.String())
	assert.Equal(t, "detail:"+boundGuardKeyUID, self.Body.String())

	for _, bot := range spacepkg.SystemBotList() {
		w := boundMemberGet(t, newBoundMemberRoute("sp_a", deny), bot)
		require.Equal(t, http.StatusOK, w.Code, "system bot %s must stay resolvable: %s", bot, w.Body.String())
	}

	assert.False(t, called, "exempt targets must be decided without a membership lookup")
}

// A key with no bound Space has no enforceable tenant, so this route cannot be
// constrained at all — refuse rather than trust that the tree's own middleware
// already rejected it. (It does: enforceKeySpace fail-closes on empty bindings.
// This is the second, local line.)
func TestRequireBoundSpaceMemberRefusesUnboundKey(t *testing.T) {
	called := false
	r := newBoundMemberRoute("", func(string, string) (bool, error) {
		called = true
		return true, nil
	})

	w := boundMemberGet(t, r, "u_peer")
	assert.NotEqual(t, http.StatusOK, w.Code,
		"an unbound key must not reach a globally-resolving handler: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "detail:")
	assert.False(t, called, "there is no Space to check membership against")
}

// A membership-check failure must fail closed and surface as a server error rather
// than being folded into "not a member" — a DB outage must not silently look like a
// clean negative decision.
func TestRequireBoundSpaceMemberFailsClosedOnCheckerError(t *testing.T) {
	r := newBoundMemberRoute("sp_a", func(string, string) (bool, error) {
		return false, errors.New("db down")
	})

	w := boundMemberGet(t, r, "u_peer")
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "detail:", "the handler must not run on an undecided check")
}
