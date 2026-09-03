package oidc

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newInitialSpaceEnv boots a real context, seeds one active Space and points
// space.oidc_initial_space_id at it.
//
// The cleanup is not optional: SystemSettings is a process-wide singleton, so a
// setting left behind would make every later test in this package auto-join, and
// the failure would surface somewhere unrelated.
func newInitialSpaceEnv(t *testing.T, spaceID string, maxUsers int) *config.Context {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	_, err := ctx.DB().Exec(
		"INSERT INTO `space` (space_id, name, creator, max_users, join_mode, status) VALUES (?, ?, ?, ?, 0, 1)",
		spaceID, "初始空间", "owner-uid", maxUsers)
	require.NoError(t, err)
	_, err = ctx.DB().Exec(
		"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 2, 1)",
		spaceID, "owner-uid")
	require.NoError(t, err)

	setInitialSpaceSetting(t, ctx, spaceID)
	t.Cleanup(func() { setInitialSpaceSetting(t, ctx, "") })
	return ctx
}

func setInitialSpaceSetting(t *testing.T, ctx *config.Context, spaceID string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO system_setting (category, key_name, value, value_type) VALUES ('space','oidc_initial_space_id',?,'string') "+
			"ON DUPLICATE KEY UPDATE value=VALUES(value)", spaceID)
	require.NoError(t, err)
	require.NoError(t, commonmod.EnsureSystemSettings(ctx).Reload())
	require.Equal(t, spaceID, commonmod.EnsureSystemSettings(ctx).OIDCInitialSpaceID())
}

func memberRowCount(t *testing.T, ctx *config.Context, spaceID, uid string) int {
	t.Helper()
	var n int
	_, err := ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1", spaceID, uid).Load(&n)
	require.NoError(t, err)
	return n
}

// driveCallback runs authorize → callback for one subject and returns the
// callback recorder.
func driveCallback(t *testing.T, r http.Handler, mp *MockProvider, sub, authcode, code string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/auth/oidc/aegis/authorize?authcode="+authcode+"&return_to=/home", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusFound, w.Code)

	authURL, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	mp.PrepCode(code, sub, authURL.Query().Get("nonce"))

	req2 := httptest.NewRequest("GET",
		"/v1/auth/oidc/aegis/callback?state="+authURL.Query().Get("state")+"&code="+code, nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	return w2
}

// TestAPI_Callback_NewAccountJoinsInitialSpace covers acceptance 1 end to end:
// a browser OIDC callback that creates an account leaves that account holding an
// active membership of the configured Space by the time the response is written.
//
// The timing is the point of asserting it here rather than only on the join
// function. A client typically calls GET /v1/integrations/oidc/spaces as soon as
// it holds the session, and that endpoint filters on an active member row — so a
// join deferred into a goroutine would show an empty first screen. This passes
// only while the write stays on the request path.
func TestAPI_Callback_NewAccountJoinsInitialSpace(t *testing.T) {
	const spaceID = "sp-cb-join"
	ctx := newInitialSpaceEnv(t, spaceID, 0)

	mp := NewMockProvider(t)
	mp.PrepUser("sub-join", map[string]interface{}{
		"email": "join@example.com", "email_verified": true, "name": "Joiner",
	})
	users := &fakeUserLookup{loginResp: &IssueSessionResp{
		UID: "u-cb-new", IsNewUser: true, LoginRespJSON: `{"token":"t","uid":"u-cb-new"}`,
	}}
	o := newTestOIDC(t, mp, users, newFakeIdentityStore())
	o.ctx = ctx
	o.authcode = newFakeAuthcode()

	w := driveCallback(t, newTestRouter(o), mp, "sub-join", "ac-join", "code-join")
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	require.Equal(t, "/home", w.Header().Get("Location"), "login must succeed")

	assert.Equal(t, 1, memberRowCount(t, ctx, spaceID, "u-cb-new"),
		"the account created by this callback must be a member before the response returns")
}

// TestAPI_Callback_ExistingAccountDoesNotJoin covers acceptance 3 and, with it,
// acceptance 5.
//
// The hook fires on account creation, never on login. That single distinction is
// what guarantees a user an administrator removed from the Space is not silently
// added back on their next SSO login — so it is asserted on the callback, where
// the distinction actually lives, and on the counter rather than only on the
// absence of a row (a row could be absent for the wrong reason).
func TestAPI_Callback_ExistingAccountDoesNotJoin(t *testing.T) {
	const spaceID = "sp-cb-existing"
	ctx := newInitialSpaceEnv(t, spaceID, 0)

	mp := NewMockProvider(t)
	mp.PrepUser("sub-old", map[string]interface{}{
		"email": "old@example.com", "email_verified": true,
	})
	users := &fakeUserLookup{loginResp: &IssueSessionResp{
		UID: "u-cb-old", LoginRespJSON: `{"token":"t","uid":"u-cb-old"}`,
	}}
	store := newFakeIdentityStore()
	require.NoError(t, store.Insert(&IdentityModel{
		UID: "u-cb-old", Issuer: mp.Issuer, Subject: "sub-old",
	}))
	o := newTestOIDC(t, mp, users, store)
	o.ctx = ctx
	o.authcode = newFakeAuthcode()

	before := totalInitialSpaceJoinSamples()
	w := driveCallback(t, newTestRouter(o), mp, "sub-old", "ac-old", "code-old")
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	require.Equal(t, "/home", w.Header().Get("Location"), "login must still succeed")

	assert.Equal(t, before, totalInitialSpaceJoinSamples(),
		"a returning user must not reach the join path at all")
	assert.Equal(t, 0, memberRowCount(t, ctx, spaceID, "u-cb-old"))
}

// TestAPI_Callback_IdentityRaceGhostDoesNotTakeASeat covers acceptance 7.
//
// Two first logins for one subject each create a user; the unique key on
// (issuer, subject) lets only one identity row through, and the loser's account
// is a ghost nobody can ever log into — its session is re-signed onto the winner.
// Joining that ghost would consume a seat under max_users, so a Space sized for
// the workforce could refuse a real hire because of an account that exists only
// in the audit trail.
//
// max_users is set to 2 (owner + one) so the assertion is not merely "no row"
// but "the one remaining seat is still free".
func TestAPI_Callback_IdentityRaceGhostDoesNotTakeASeat(t *testing.T) {
	const spaceID = "sp-cb-ghost"
	ctx := newInitialSpaceEnv(t, spaceID, 3) // owner + 2 seats

	// Both callbacks are for the same subject — that is what makes them a race.
	mp := NewMockProvider(t)
	mp.PrepUser("sub-race", map[string]interface{}{
		"email": "race@example.com", "email_verified": true,
	})

	// --- The winner's callback. Ordinary first login: creates u-winner, writes
	// the identity row, joins. Driving it is what makes the assertions below
	// discriminating: without it, an implementation that suppressed the join for
	// *both* accounts would satisfy "the ghost has no row".
	winnerStore := newFakeIdentityStore()
	winnerUsers := &fakeUserLookup{loginResp: &IssueSessionResp{
		UID: "u-winner", IsNewUser: true, LoginRespJSON: `{"token":"t","uid":"u-winner"}`,
	}}
	winner := newTestOIDC(t, mp, winnerUsers, winnerStore)
	winner.ctx = ctx
	winner.authcode = newFakeAuthcode()

	w1 := driveCallback(t, newTestRouter(winner), mp, "sub-race", "ac-win", "code-win")
	require.Equal(t, http.StatusFound, w1.Code, w1.Body.String())
	require.Equal(t, 1, memberRowCount(t, ctx, spaceID, "u-winner"),
		"the winner is an ordinary first login and must join")

	// --- The loser's callback, interleaved as it really happens: the winner's
	// identity row commits between this one's lookup and its insert. The first
	// Get misses (so this callback creates u-ghost), the insert hits 1062, and
	// the recovery lookup finds u-winner and re-signs the session onto it.
	loserStore := newFakeIdentityStore()
	loserStore.bindings[mp.Issuer+"|sub-race"] = &IdentityModel{
		UID: "u-winner", Issuer: mp.Issuer, Subject: "sub-race",
	}
	loserStore.winnerAppearsAfterFirstGet = true
	loserStore.failInsertWithDuplicate = true
	loserUsers := &fakeUserLookup{
		// The create call carries an empty UID and yields the ghost; the recovery
		// re-issue carries the winner's uid and must yield the winner's session.
		loginResp: &IssueSessionResp{
			UID: "u-ghost", IsNewUser: true, LoginRespJSON: `{"token":"t","uid":"u-ghost"}`,
		},
		loginRespByUID: map[string]*IssueSessionResp{
			"u-winner": {UID: "u-winner", LoginRespJSON: `{"token":"t","uid":"u-winner"}`},
		},
	}
	loser := newTestOIDC(t, mp, loserUsers, loserStore)
	loser.ctx = ctx
	loser.authcode = newFakeAuthcode()

	w2 := driveCallback(t, newTestRouter(loser), mp, "sub-race", "ac-lose", "code-lose")
	require.Equal(t, http.StatusFound, w2.Code, w2.Body.String())

	// Only the winner. Both halves are asserted against a run where the winner
	// genuinely did join, so "nobody joined" cannot pass.
	assert.Equal(t, 1, memberRowCount(t, ctx, spaceID, "u-winner"),
		"the winner keeps exactly the one membership it created; the loser adds no second one")
	assert.Equal(t, 0, memberRowCount(t, ctx, spaceID, "u-ghost"),
		"the race loser is a ghost account nobody can log into and must not occupy a seat")

	var active int
	_, err := ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND status=1", spaceID).Load(&active)
	require.NoError(t, err)
	assert.Equal(t, 2, active,
		"owner + winner; the third seat stays free for a real user rather than going to a ghost")
}

// TestAPI_Callback_FeatureOffWritesNoMembership covers acceptance 14 on the path
// that matters most: with the setting empty, an account-creating callback behaves
// exactly as it did before this feature — no membership, and no counter movement
// (not even under the error label, which is how a "skip" could otherwise hide).
func TestAPI_Callback_FeatureOffWritesNoMembership(t *testing.T) {
	const spaceID = "sp-cb-off"
	ctx := newInitialSpaceEnv(t, spaceID, 0)
	setInitialSpaceSetting(t, ctx, "") // turn it off after seeding the Space

	mp := NewMockProvider(t)
	mp.PrepUser("sub-off", map[string]interface{}{
		"email": "off@example.com", "email_verified": true,
	})
	users := &fakeUserLookup{loginResp: &IssueSessionResp{
		UID: "u-cb-off", IsNewUser: true, LoginRespJSON: `{"token":"t","uid":"u-cb-off"}`,
	}}
	o := newTestOIDC(t, mp, users, newFakeIdentityStore())
	o.ctx = ctx
	o.authcode = newFakeAuthcode()

	before := totalInitialSpaceJoinSamples()
	w := driveCallback(t, newTestRouter(o), mp, "sub-off", "ac-off", "code-off")
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	require.Equal(t, "/home", w.Header().Get("Location"))

	assert.Equal(t, 0, memberRowCount(t, ctx, spaceID, "u-cb-off"))
	assert.Equal(t, before, totalInitialSpaceJoinSamples(),
		"an unconfigured deployment must not even record a skip")
}

// TestAPI_Callback_FullSpaceStillLogsIn covers acceptance 10 at the boundary the
// user actually feels: the Space cannot take them, and the login still succeeds.
//
// This is the property the whole design hangs on — the join is best-effort and
// sits after the session is issued, so no Space-side condition can cost someone
// their login. Asserting it only on the join function would prove the function
// returns an outcome, not that the handler ignores it.
func TestAPI_Callback_FullSpaceStillLogsIn(t *testing.T) {
	const spaceID = "sp-cb-full"
	ctx := newInitialSpaceEnv(t, spaceID, 1) // the owner already fills it

	mp := NewMockProvider(t)
	mp.PrepUser("sub-full", map[string]interface{}{
		"email": "full@example.com", "email_verified": true,
	})
	users := &fakeUserLookup{loginResp: &IssueSessionResp{
		UID: "u-cb-full", IsNewUser: true, LoginRespJSON: `{"token":"t","uid":"u-cb-full"}`,
	}}
	o := newTestOIDC(t, mp, users, newFakeIdentityStore())
	o.ctx = ctx
	fakeAC := newFakeAuthcode()
	o.authcode = fakeAC

	w := driveCallback(t, newTestRouter(o), mp, "sub-full", "ac-full", "code-full")
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	assert.Equal(t, "/home", w.Header().Get("Location"),
		"a full Space must not turn into a failed login")
	assert.Contains(t, fakeAC.get("ac-full"), `"token":"t"`,
		"the session must still reach the client")
	assert.Equal(t, 0, memberRowCount(t, ctx, spaceID, "u-cb-full"))
}

// TestAPI_BindCreate_NewAccountJoinsInitialSpace covers acceptance 2.
//
// /bind/create is the second of the two account-creating entry points, and the
// only one a client reaches after the callback handed it off to the bind page.
// A user arriving through it is exactly as stranded as one from the callback if
// nothing joins them, so it carries the same hook — and needs the same proof,
// since nothing about the callback tests would notice the line going missing here.
func TestAPI_BindCreate_NewAccountJoinsInitialSpace(t *testing.T) {
	const spaceID = "sp-bind-join"
	ctx := newInitialSpaceEnv(t, spaceID, 0)

	o, jti, _, _, _, _, users, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
	o.ctx = ctx
	users.resp = &IssueSessionResp{UID: "u-bind-new", LoginRespJSON: `{"token":"t-bind"}`}

	body, _ := json.Marshal(map[string]string{"token": jti})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	newTestBindRouter(o).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, 1, memberRowCount(t, ctx, spaceID, "u-bind-new"),
		"an account created through /bind/create must join the initial Space too")
}

// TestAPI_BindConfirm_ExistingAccountDoesNotJoin is the mirror property on the
// same endpoint family: /bind/confirm links an OIDC identity to an account that
// already exists, so it creates nobody and must not join anybody.
//
// Worth pinning separately from the callback's returning-user case, because the
// two paths decide "was an account created here" by different means — the
// callback reads ResolveOrLink's IsNew, while the bind flow relies on the hook
// sitting only in the create handler. A hook added to the wrong bind handler
// would silently re-add users administrators had removed.
func TestAPI_BindConfirm_ExistingAccountDoesNotJoin(t *testing.T) {
	const spaceID = "sp-bind-confirm"
	ctx := newInitialSpaceEnv(t, spaceID, 0)

	o, jti, auth, loc, _, _, users, _ := newTestOIDCWithBindFull(t, defaultBindCfg(), sampleClaims(), false)
	o.ctx = ctx
	auth.verifyPasswordResp.matched = true
	loc.byUsername["alice"] = "u-alice"
	users.resp = &IssueSessionResp{UID: "u-alice", LoginRespJSON: `{"token":"t-alice"}`}
	r := newTestBindRouter(o)

	before := totalInitialSpaceJoinSamples()

	body, _ := json.Marshal(map[string]string{"token": jti, "identifier": "alice", "password": "Pwd@1"})
	req := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/verify/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	body2, _ := json.Marshal(map[string]string{"token": jti})
	req2 := httptest.NewRequest("POST", "/v1/auth/oidc/aegis/bind/confirm", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())

	assert.Equal(t, before, totalInitialSpaceJoinSamples(),
		"binding an existing account creates nobody and must not reach the join path")
	assert.Equal(t, 0, memberRowCount(t, ctx, spaceID, "u-alice"))
}

// TestExchange_NewAccountJoinsInitialSpace covers the two token-exchange
// endpoints added by #829, which land on this branch through the merge.
//
// `/exchange` and `/exchange-jwt` both create accounts (completeExchange issues
// with CreateUser=res.IsNew and writes the identity row), so they are the third
// and fourth account-creating entry points, not just new authenticators. An
// account created there and left out of the initial Space is stranded in exactly
// the way this feature exists to prevent — and, being a merge interaction, it is
// precisely the kind of gap no pre-merge test on either side would catch.
//
// Both endpoints funnel through completeExchange, so one hook covers both and
// this test drives the /exchange half.
func TestExchange_NewAccountJoinsInitialSpace(t *testing.T) {
	const spaceID = "sp-exchange-join"
	ctx := newInitialSpaceEnv(t, spaceID, 0)

	users := newExchangeUserFake()
	users.loginResp = &IssueSessionResp{
		UID:           "u-exchange-new",
		IsNewUser:     true,
		LoginRespJSON: `{"token":"sess","uid":"u-exchange-new"}`,
	}
	o := newTestOIDCForExchange(t, defaultExchangeProvider(), users, newFakeIdentityStore())
	o.ctx = ctx

	w := postExchange(o, `{"access_token":"good"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, 1, memberRowCount(t, ctx, spaceID, "u-exchange-new"),
		"an account created through /exchange must join the initial Space too")
}

// TestExchange_ExistingAccountDoesNotJoin is the mirror case on the same
// endpoint: a caller exchanging a credential for an account that already exists
// creates nobody, so the join path must not be entered at all.
func TestExchange_ExistingAccountDoesNotJoin(t *testing.T) {
	const spaceID = "sp-exchange-existing"
	ctx := newInitialSpaceEnv(t, spaceID, 0)

	prov := defaultExchangeProvider()
	users := newExchangeUserFake()
	users.loginResp = &IssueSessionResp{
		UID:           "u-exchange-old",
		LoginRespJSON: `{"token":"sess","uid":"u-exchange-old"}`,
	}
	store := newFakeIdentityStore()
	require.NoError(t, store.Insert(&IdentityModel{
		UID: "u-exchange-old", Issuer: prov.issuer, Subject: "default-sub",
	}))
	o := newTestOIDCForExchange(t, prov, users, store)
	o.ctx = ctx

	before := totalInitialSpaceJoinSamples()
	w := postExchange(o, `{"access_token":"good"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, before, totalInitialSpaceJoinSamples(),
		"exchanging a credential for an existing account must not reach the join path")
	assert.Equal(t, 0, memberRowCount(t, ctx, spaceID, "u-exchange-old"))
}
