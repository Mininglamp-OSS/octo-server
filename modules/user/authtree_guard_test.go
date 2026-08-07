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
		// Reports what the HANDLER observes through gin's query accessor — the same
		// call path u.get takes. Asserting on this rather than on what the middleware
		// computed is what makes the queryCache trap detectable (see enforceKeySpace).
		func(c *wkhttp.Context) {
			c.String(http.StatusOK, "detail:"+c.Param("uid")+" group_no="+c.Query(groupNoField))
		},
	)
	return r
}

func boundMemberGet(t *testing.T, r *wkhttp.WKHttp, uid string) *httptest.ResponseRecorder {
	t.Helper()
	return boundMemberGetRaw(t, r, "/v1/user/users/"+uid)
}

func boundMemberGetRaw(t *testing.T, r *wkhttp.WKHttp, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
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
	assert.Contains(t, w.Body.String(), "detail:u_peer")
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
	assert.Contains(t, self.Body.String(), "detail:"+boundGuardKeyUID)

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

// 🔴 PR #713 review round 4 P1-1 — /users/:uid has TWO caller-controlled tenant
// anchors, and pinning only :uid left the other one open for three rounds.
//
// u.get also reads group_no, and everything it reaches is unauthorized with respect
// to the caller: GetGroupMember takes no caller UID at all, and IsShowShortNo checks
// only the TARGET's membership. So a bound-to-A key naming a Space-B group gets that
// group's member block plus is_external / source_space_id / source_space_name /
// home_space_* — another Space's id and human-readable name — and vercode, which the
// friend-add flow consumes as invite_vercode (a capability, not an identifier).
//
// Nothing on this tree claims group-scoped user detail, so the guard strips the
// parameter instead of validating it: fail-closed by construction, and the human
// route keeps its behaviour. Asserting on what the handler observes also pins the
// gin queryCache constraint — a Del() done via c.Query would be invisible here.
func TestRequireBoundSpaceMemberStripsGroupNo(t *testing.T) {
	r := newBoundMemberRoute("sp_a", func(string, string) (bool, error) { return true, nil })

	w := boundMemberGetRaw(t, r, "/v1/user/users/u_peer?group_no=g_other_space")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "detail:u_peer", "the profile itself stays reachable")
	assert.Contains(t, w.Body.String(), "group_no=",
		"harness must report the observed group_no so an ineffective strip is visible")
	assert.NotContains(t, w.Body.String(), "g_other_space",
		"the handler must not observe a caller-supplied group_no")
}

// Stripping group_no must not disturb the rest of the query string — enforceKeySpace
// has already pinned space_id in there by the time this guard runs.
func TestRequireBoundSpaceMemberPreservesOtherQueryParams(t *testing.T) {
	r := newBoundMemberRoute("sp_a", func(string, string) (bool, error) { return true, nil })

	w := boundMemberGetRaw(t, r, "/v1/user/users/u_peer?space_id=sp_a&group_no=g_x")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "g_x")
}

// The self exemption short-circuits before the membership check, so it must not
// short-circuit the strip as well — otherwise the one fixture-free variant of the
// leak (owner asking about themselves in a Space-B group) stays open.
func TestRequireBoundSpaceMemberStripsGroupNoForExemptTargets(t *testing.T) {
	r := newBoundMemberRoute("sp_a", func(string, string) (bool, error) { return false, nil })

	w := boundMemberGetRaw(t, r, "/v1/user/users/"+boundGuardKeyUID+"?group_no=g_other_space")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "g_other_space",
		"the self exemption must skip the membership check, not the group_no strip")
}
