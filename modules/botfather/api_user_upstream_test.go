package botfather

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// Upstream capabilities reused on the User API Key tree (uk_*) — G1..G8.
//
// These assert three things the drive/CLI contract depends on:
//   1. the route exists on the uk tree and only `uk_*` opens it,
//   2. the actor is the real person behind the key, not a bot,
//   3. the key's frozen Space is the only tenant the request can reach (§3.1).
// ===========================================================================

// ukUpstreamPaths are the seven reused routes, with a request shape that reaches
// the handler (so a 404 means "not registered", never "bad input").
func ukUpstreamPaths(spaceID, groupNo string) []string {
	return []string{
		"/v1/user/file/upload/presigned?type=chat&filename=a.txt&fileSize=10",
		"/v1/user/file/download/url?path=chat/a.txt",
		"/v1/user/users/" + testutil.UID,
		"/v1/user/user/search?keyword=nobody-" + util.GenerUUID()[:8],
		"/v1/user/space/" + spaceID + "/members",
		"/v1/user/space/my",
		fmt.Sprintf("/v1/user/groups/%s/messages/1", groupNo),
	}
}

// insertTestSpace creates an active Space and makes uid an active member of it.
func insertTestSpace(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO space (space_id, name, status, creator) VALUES (?, ?, 1, ?)",
		spaceID, spaceID, uid,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 2, 1, NOW(), NOW())",
		spaceID, uid,
	).Exec()
	require.NoError(t, err)
}

// TestUserKeyUpstreamRoutesAreMountedAndAuthGated is the credential matrix from
// the handoff §9: every reused route must be reachable with a `uk_*` and closed
// to the other two credential kinds. A 404 here means the authtree contribution
// never got mounted.
func TestUserKeyUpstreamRoutesAreMountedAndAuthGated(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, uid)
	groupNo := "g" + util.GenerUUID()[:12]

	ukToken := mintUserAPIKeyInSpace(t, ctx, uid, spaceID)

	botID := "bot_" + util.GenerUUID()[:8]
	botToken := insertTestBot(t, ctx, botID, uid)

	for _, path := range ukUpstreamPaths(spaceID, groupNo) {
		t.Run("uk_accepted "+path, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, path, ukToken, nil))
			assert.NotEqual(t, http.StatusNotFound, w.Code,
				"route must be mounted on the uk tree: %s", w.Body.String())
			assert.NotEqual(t, http.StatusUnauthorized, w.Code,
				"a valid uk_ must pass authUserAPIKey: %s", w.Body.String())
		})

		// A bot token and a browser session token must both bounce off
		// authUserAPIKey — the uk tree is not a second door for them.
		t.Run("bf_rejected "+path, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, path, botToken, nil))
			assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		})
		t.Run("session_rejected "+path, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, path, testutil.Token, nil))
			assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		})
	}
}

// TestUserKeyActorIsTheRealPerson proves §4's premise: the reused handler reads
// its actor from the same context key the uk middleware writes, so the person —
// not a bot — is the subject. listMembers is the probe because the member list it
// returns must contain the key's owner, which only holds if actor == uid.
func TestUserKeyActorIsTheRealPerson(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	member := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, member, "member")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, member)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		"/v1/user/space/"+spaceID+"/members", mintUserAPIKeyInSpace(t, ctx, member, spaceID), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var members []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &members))
	uids := make([]string, 0, len(members))
	for _, m := range members {
		uids = append(uids, m["uid"].(string))
	}
	assert.Contains(t, uids, member, "actor must resolve to the person bound to the key")

	// An outsider's key resolves to the outsider, so the tenant middleware's
	// membership gate (F1) refuses it before listMembers runs — the key alone never
	// buys access to the Space it is bound to.
	outsider := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, outsider, "outsider")
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		"/v1/user/space/"+spaceID+"/members", mintUserAPIKeyInSpace(t, ctx, outsider, spaceID), nil))
	assert.Equal(t, http.StatusForbidden, w.Code,
		"a non-member's key must not read the member list: %s", w.Body.String())
}

// TestUserKeySpaceRuleSameSpaceAllowed covers §3.1 rule 2.
func TestUserKeySpaceRuleSameSpaceAllowed(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, uid)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		"/v1/user/space/"+spaceID+"/members", mintUserAPIKeyInSpace(t, ctx, uid, spaceID), nil))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestUserKeySpaceRuleMismatchIsForbidden covers §3.1 rule 3 — the important one.
// The caller is a legitimate member of BOTH Spaces, so nothing but the tenant rule
// stands between the key and the other Space's member list.
func TestUserKeySpaceRuleMismatchIsForbidden(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	boundSpace := "sp_" + util.GenerUUID()[:8]
	otherSpace := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, boundSpace, uid)
	insertTestSpace(t, ctx, otherSpace, uid)

	token := mintUserAPIKeyInSpace(t, ctx, uid, boundSpace)

	// Path-parameter Space.
	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		"/v1/user/space/"+otherSpace+"/members", token, nil))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var env errEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.forbidden", env.Error.Code)

	// Query-parameter Space, same rule.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		"/v1/user/user/search?keyword=x&space_id="+otherSpace, token, nil))
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

// TestUserKeySpaceRuleInjectsBoundSpace covers §3.1 rule 1 and is the end-to-end
// lock on the query-injection mechanism: without the injection, user.search falls
// through to its "any shared Space" branch and would resolve a user the key's
// tenant has no business seeing.
func TestUserKeySpaceRuleInjectsBoundSpace(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	caller := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, caller, "caller")
	boundSpace := "sp_" + util.GenerUUID()[:8]
	otherSpace := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, boundSpace, caller)
	insertTestSpace(t, ctx, otherSpace, caller)

	// Two searchable peers, one per Space. username is what QueryByKeyword matches.
	inBound := "peer_in_" + util.GenerUUID()[:8]
	inOther := "peer_out_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, inBound, "peer-in")
	insertTestUser(t, ctx, inOther, "peer-out")
	insertTestSpace(t, ctx, boundSpace, inBound)
	insertTestSpace(t, ctx, otherSpace, inOther)

	token := mintUserAPIKeyInSpace(t, ctx, caller, boundSpace)

	search := func(keyword string) map[string]any {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
			"/v1/user/user/search?keyword="+keyword, token, nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		return out
	}

	assert.EqualValues(t, 1, search(inBound)["exist"],
		"a peer inside the bound Space must resolve")
	assert.EqualValues(t, 0, search(inOther)["exist"],
		"the bound Space must be injected, so a peer only reachable via another shared Space stays hidden")
}

// TestUserKeySpaceMyReturnsOnlyBoundSpace covers §3.1 rule 4 (§5.1.1 plan A) and
// pins the human endpoint's zero-regression requirement in the same test, so the
// two can never drift apart unnoticed.
func TestUserKeySpaceMyReturnsOnlyBoundSpace(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	boundSpace := "sp_" + util.GenerUUID()[:8]
	otherSpace := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, boundSpace, uid)
	insertTestSpace(t, ctx, otherSpace, uid)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, "/v1/user/space/my",
		mintUserAPIKeyInSpace(t, ctx, uid, boundSpace), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var spaces []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &spaces))
	require.Len(t, spaces, 1, "uk must see only its bound Space: %s", w.Body.String())
	assert.Equal(t, boundSpace, spaces[0]["space_id"])
	// Response shape parity with mySpaces — clients reuse the same parser.
	for _, field := range []string{"space_id", "name", "logo", "creator", "status", "role", "member_count", "join_mode", "created_at", "updated_at"} {
		assert.Contains(t, spaces[0], field, "spaceResp field %q must be present", field)
	}

	// Zero regression: the human endpoint still reports every Space.
	humanReq := httptest.NewRequest(http.MethodGet, "/v1/space/my", nil)
	humanReq.Header.Set("token", testutil.Token)
	w = httptest.NewRecorder()
	route.ServeHTTP(w, humanReq)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestHumanSpaceMyStillReturnsAllSpaces is the explicit non-regression assertion
// for /v1/space/my: mySpaces must keep reporting every Space the session user
// belongs to. The session user is testutil.UID, whose token the test server preloads.
func TestHumanSpaceMyStillReturnsAllSpaces(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	insertTestUser(t, ctx, testutil.UID, "session-user")
	first := "sp_" + util.GenerUUID()[:8]
	second := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, first, testutil.UID)
	insertTestSpace(t, ctx, second, testutil.UID)

	req := httptest.NewRequest(http.MethodGet, "/v1/space/my", nil)
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var spaces []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &spaces))
	ids := map[string]bool{}
	for _, sp := range spaces {
		ids[sp["space_id"].(string)] = true
	}
	assert.True(t, ids[first] && ids[second],
		"human /v1/space/my must still return every Space, got %s", w.Body.String())
}

// TestUserKeySpaceMyRefusesRevokedBinding pins what a dead binding does on the uk
// tree: enforceKeySpace's membership gate refuses it (403) rather than letting
// boundSpaceOnly report an empty scope. A disabled Space and a revoked membership
// are the same thing to CheckMembership, which joins space.status=1.
//
// This is a deliberate change from the pre-F1 behaviour (200 + `[]`): a key whose
// owner has no live membership has no verified tenant, so every uk upstream route
// — /space/my included — fails closed uniformly instead of one route reporting
// "empty" while the others reject.
func TestUserKeySpaceMyRefusesRevokedBinding(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, uid)
	token := mintUserAPIKeyInSpace(t, ctx, uid, spaceID)

	_, err := ctx.DB().UpdateBySql("UPDATE space SET status=0 WHERE space_id=?", spaceID).Exec()
	require.NoError(t, err)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, "/v1/user/space/my", token, nil))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var env errEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.forbidden", env.Error.Code)
}

// TestUserKeyUpstreamRefusesKeyWithNoBinding pins the fail-closed posture for a key
// with no bound Space at all (historical rows with an empty space_id): every uk
// upstream route refuses it (PR #713 review, Jerry-Xin blocker 3 / lml2468 P0).
//
// There is no tenant to enforce, so passing through would let the caller pick its
// own tenant via space_id — fail-open tenant selection on a credential whose stated
// guarantee is tenant confinement. These routes did not exist before this change,
// so no unbound key loses access it previously had; /v1/user/bots* does not pass
// through enforceKeySpace and is covered separately below.
func TestUserKeyUpstreamRefusesKeyWithNoBinding(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, uid)
	groupNo := "g" + util.GenerUUID()[:12]

	unboundToken := mintUserAPIKeyInSpace(t, ctx, uid, "")

	for _, path := range ukUpstreamPaths(spaceID, groupNo) {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, path, unboundToken, nil))
			require.Equal(t, http.StatusForbidden, w.Code,
				"an unbound key has no enforceable tenant and must be refused: %s", w.Body.String())
			var env errEnvelope
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
			assert.Equal(t, "err.shared.auth.forbidden", env.Error.Code)
		})
	}
}

// TestUserKeyBotsRoutesUnaffectedByTenantMiddleware guards the reason the tenant
// middleware is mounted per-route instead of on the whole /v1/user group: the
// pre-existing /v1/user/bots* contract must be untouched, including the
// space_id-in-body path that enforceKeySpace knows nothing about.
func TestUserKeyBotsRoutesUnaffectedByTenantMiddleware(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	boundSpace := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, boundSpace, uid)
	token := mintUserAPIKeyInSpace(t, ctx, uid, boundSpace)

	// A stray space_id query param on /v1/user/bots must not turn into a 403 —
	// that handler resolves its Space from the key, not from the request.
	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		"/v1/user/bots?space_id=sp_totally_unrelated", token, nil))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestUserKeyPresignedUploadRefusesCallerObjectKey exercises the guard through the
// real uk tree — its own auth and tenant middleware in front — which is the one
// thing modules/file cannot assert DB-free: that the refusal survives contact with
// the tree, and that tightening the route did not turn it into a second door for
// the other two credential kinds.
//
// 🔴 PR #713 review (lml2468 P1 / yujiawei S-1) — the integrity hole it closes.
// getUploadCredentials signs a 30-minute PUT for whatever object key the caller
// puts in `path`, and nothing below it checks who owns that key. A `uk_*` is a
// long-lived non-interactive bearer token on a CLI/drive client, so without this
// guard knowing any object key buys write access to it and the object can be
// overwritten. The guard refuses a caller-named key outright, leaving only
// server-minted ones — see modules/file/authtree_guard.go for why rejecting beats
// validating here, and modules/file/authtree_guard_test.go for the route-level
// behaviour (whitespace path, server-minted key shape, human route untouched).
//
// Download is deliberately NOT guarded the same way; that asymmetry, and the fact
// that it is a deferral rather than a solved problem, is recorded in pkg/authtree's
// census.
func TestUserKeyPresignedUploadRefusesCallerObjectKey(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, uid)
	ukToken := mintUserAPIKeyInSpace(t, ctx, uid, spaceID)

	botID := "bot_" + util.GenerUUID()[:8]
	botToken := insertTestBot(t, ctx, botID, uid)

	const withPath = "/v1/user/file/upload/presigned" +
		"?type=common&path=/common/victim/secret.pdf&filename=a.pdf&fileSize=10"
	const withoutPath = "/v1/user/file/upload/presigned" +
		"?type=common&filename=a.pdf&fileSize=10"

	// The guard's own refusal, matched on its message so an unrelated 400
	// (bad fileSize, blocked extension) cannot stand in for it.
	const guardRefusal = "自定义上传路径"

	t.Run("uk with a caller-named path is refused", func(t *testing.T) {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, withPath, ukToken, nil))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), guardRefusal,
			"the guard must be mounted on this route, not just defined")
	})

	// The positive control: the shape octo-drive actually sends must still pass the
	// guard. Asserted as "the guard did not fire" rather than 200, because whether
	// the signer succeeds depends on the object-storage backend configured for the
	// test run, and that is not what this test is about.
	t.Run("uk without a path passes the guard", func(t *testing.T) {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, withoutPath, ukToken, nil))
		assert.NotContains(t, w.Body.String(), guardRefusal,
			"omitting path is the supported shape and must not be refused: %s", w.Body.String())
	})

	// The wrong credentials still bounce off authUserAPIKey, ahead of the guard —
	// the tightening must not have turned the route into a second door.
	for name, token := range map[string]string{"bf": botToken, "session": testutil.Token} {
		t.Run(name+" is still rejected", func(t *testing.T) {
			for _, path := range []string{withPath, withoutPath} {
				w := httptest.NewRecorder()
				route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, path, token, nil))
				assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			}
		})
	}
}
