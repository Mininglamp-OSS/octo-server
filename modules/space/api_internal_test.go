package space

// Tests for the internal single-subject Space role lookup
// (GET /v1/internal/spaces/:space_id/members/:uid/role, see api_internal.go)
// and for ActiveAdminUIDs (admin_targets.go), the cross-module read that
// modules/notify's role-targeted delivery consumes.
//
// Shape follows modules/internal_resolve/api_test.go: a hermetic wkhttp router
// mounting ONLY auth + handler, with the token injected directly onto a Space
// struct literal. Injecting the field is deliberate — this package shares one
// process-wide test server (api_test.go TestMain), so mutating
// OCTO_MARKETPLACE_INTERNAL_TOKEN in the environment would leak across
// unrelated tests.
//
// The PRODUCTION middleware chain (strict per-IP limiter mounted ahead of auth
// on the concrete GET) needs live Redis, so it is pinned at the source level by
// route_wiring_test.go, which reads api.go off disk. An earlier revision of
// this file "verified" the order by building its own router and registering
// handlers in the order it then asserted — a test that could not fail, and that
// would have stayed green if production moved auth onto the group. That test is
// gone; do not reintroduce that shape.
//
// DB rows come from the seeding helpers in api_member_search_test.go /
// db_member_name_fallback_test.go so the fixtures stay in one place.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testMarketplaceToken is 32 bytes so it clears minMarketplaceInternalTokenBytes.
const testMarketplaceToken = "0123456789abcdef0123456789abcdef"

type memberRoleEnvelopeForTest struct {
	Data struct {
		Role *int `json:"role"`
	} `json:"data"`
}

type internalErrorEnvelopeForTest struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// newInternalRoleSpace builds a Space wired to the shared test DB with the
// internal token injected (no env mutation).
func newInternalRoleSpace(token string) *Space {
	return &Space{
		ctx:                      testCtx,
		Log:                      log.NewTLog("SpaceInternalRoleTest"),
		db:                       testSpaceDB,
		marketplaceInternalToken: token,
	}
}

// newInternalRoleRouter mounts auth + handler only. The error renderer must be
// set or the dual envelope comes back with an empty error body.
func newInternalRoleRouter(s *Space) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/v1/internal/spaces/:space_id/members/:uid/role",
		s.marketplaceInternalTokenMiddleware(), s.getSpaceMemberRole)
	return r
}

func doRoleRequest(t *testing.T, r *wkhttp.WKHttp, token, spaceID, uid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/internal/spaces/"+spaceID+"/members/"+uid+"/role", nil)
	if token != "" {
		req.Header.Set(marketplaceInternalTokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeRoleEnvelope(t *testing.T, w *httptest.ResponseRecorder) memberRoleEnvelopeForTest {
	t.Helper()
	var env memberRoleEnvelopeForTest
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), w.Body.String())
	return env
}

func decodeInternalError(t *testing.T, w *httptest.ResponseRecorder) internalErrorEnvelopeForTest {
	t.Helper()
	var env internalErrorEnvelopeForTest
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), w.Body.String())
	return env
}

// ============================================================================
// Auth (X-Internal-Token)
// ============================================================================

// The fail-closed shapes must be indistinguishable on the wire: no header, a
// wrong value, a prefix of the real value, and a server with no token.
func TestInternalMemberRole_AuthFailures(t *testing.T) {
	cases := []struct {
		name         string
		serverToken  string
		requestToken string
	}{
		{"missing header", testMarketplaceToken, ""},
		{"wrong token", testMarketplaceToken, "wrong-token-wrong-token-wrongtok"},
		{"prefix of the real token", testMarketplaceToken, testMarketplaceToken[:16]},
		{"server token unset", "", testMarketplaceToken},
		{"server token unset and no header", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newInternalRoleRouter(newInternalRoleSpace(tc.serverToken))
			w := doRoleRequest(t, r, tc.requestToken, "sp-auth", "u-auth")
			require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, "err.shared.auth.token_invalid", decodeInternalError(t, w).Error.Code)
			assert.NotContains(t, w.Body.String(), testMarketplaceToken,
				"the 401 body must never echo the configured token")
		})
	}
}

// ============================================================================
// Parameter validation
// ============================================================================

func TestInternalMemberRole_MalformedParams(t *testing.T) {
	r := newInternalRoleRouter(newInternalRoleSpace(testMarketplaceToken))

	cases := []struct {
		name    string
		spaceID string
		uid     string
	}{
		// space_id / uid are VARCHAR(40); anything longer cannot identify a row.
		{"space_id too long", strings.Repeat("x", maxSpaceIDBytes+1), "u-1"},
		{"uid too long", "sp-1", strings.Repeat("y", maxMemberUIDBytes+1)},
		// %20 reaches the handler as a single space → trimmed to empty.
		{"space_id whitespace only", "%20", "u-1"},
		{"uid whitespace only", "sp-1", "%20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRoleRequest(t, r, testMarketplaceToken, tc.spaceID, tc.uid)
			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, "err.shared.param.invalid", decodeInternalError(t, w).Error.Code)
		})
	}
}

// ============================================================================
// Role resolution
// ============================================================================

// Every active role must round-trip with its native space_member encoding.
// role=0 in particular must come back as 0 and never be confused with absence —
// that is the whole reason the wire field is a pointer.
func TestInternalMemberRole_ActiveRolesRoundTrip(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sp-internal-role"
	seedMemberSearchSpace(t, spaceID, "owner-uid") // creator seeded as role=2, status=1
	seedMemberSearchMember(t, spaceID, "admin-uid", 1, 1)
	seedMemberSearchMember(t, spaceID, "member-uid", 0, 1)

	r := newInternalRoleRouter(newInternalRoleSpace(testMarketplaceToken))

	cases := []struct {
		uid  string
		want int
	}{
		{"owner-uid", 2},
		{"admin-uid", 1},
		{"member-uid", 0},
	}
	for _, tc := range cases {
		t.Run(tc.uid, func(t *testing.T) {
			w := doRoleRequest(t, r, testMarketplaceToken, spaceID, tc.uid)
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			got := decodeRoleEnvelope(t, w).Data.Role
			require.NotNil(t, got, "an active member must never answer null")
			assert.Equal(t, tc.want, *got)
		})
	}

	// role=0 must serialize as the number 0, never be omitted: a consumer that
	// distinguishes "plain member" from "not a member" needs both answers to be
	// present and different.
	w := doRoleRequest(t, r, testMarketplaceToken, spaceID, "member-uid")
	assert.JSONEq(t, `{"data":{"role":0}}`, w.Body.String())
}

// The absence contract: non-member, unknown Space, and disbanded Space must all
// produce the SAME bytes. Any difference turns one shared service token into a
// cross-tenant Space-existence oracle.
func TestInternalMemberRole_AbsenceIsIndistinguishable(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	const liveSpace = "sp-internal-role-live"
	seedMemberSearchSpace(t, liveSpace, "live-owner")
	// A removed admin: role>=1 but status=0. Must read as absent.
	seedMemberSearchMember(t, liveSpace, "removed-admin", 1, 0)

	// A disbanded space whose space_member rows are still status=1 — disband
	// only flips space.status.
	const deadSpace = "sp-internal-role-dead"
	require.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: deadSpace, Name: deadSpace, Creator: "dead-owner",
		Status: SpaceStatusDisbanded,
	}))
	require.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: deadSpace, UID: "dead-owner", Role: 2, Status: 1,
	}))

	// An owner of a DIFFERENT space — cross-space leakage guard.
	const otherSpace = "sp-internal-role-other"
	seedMemberSearchSpace(t, otherSpace, "other-owner")

	r := newInternalRoleRouter(newInternalRoleSpace(testMarketplaceToken))

	cases := []struct {
		name    string
		spaceID string
		uid     string
	}{
		{"not a member", liveSpace, "stranger-uid"},
		{"removed member", liveSpace, "removed-admin"},
		{"disbanded space, real owner", deadSpace, "dead-owner"},
		{"space does not exist", "sp-nope", "live-owner"},
		{"uid does not exist", liveSpace, "nobody-at-all"},
		{"owner of another space", liveSpace, "other-owner"},
	}

	bodies := make([]string, 0, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRoleRequest(t, r, testMarketplaceToken, tc.spaceID, tc.uid)
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.Nil(t, decodeRoleEnvelope(t, w).Data.Role)
			bodies = append(bodies, w.Body.String())
		})
	}

	require.Len(t, bodies, len(cases))
	for i := 1; i < len(bodies); i++ {
		assert.Equal(t, bodies[0], bodies[i],
			"every absence case must be byte-identical (%s vs %s); a difference "+
				"lets a token holder distinguish 'no such space' from 'not a member'",
			cases[0].name, cases[i].name)
	}
	assert.Contains(t, bodies[0], `"role":null`,
		"absence must be an explicit null, not an omitted key")
}

// The response must carry NOTHING but the role. Shipping identity alongside the
// role is the defect that retired the roster endpoint: it joined user /
// user_verification and handed verified legal names to any token holder.
func TestInternalMemberRole_ResponseCarriesNoPII(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sp-internal-role-pii"
	seedMemberSearchSpace(t, spaceID, "pii-owner")
	seedMemberSearchUser(t, "pii-owner", "Display Name", "gated-username", "a@b.c", "13800000000")
	seedFallbackVerification(t, "pii-owner", "Legal Name")

	r := newInternalRoleRouter(newInternalRoleSpace(testMarketplaceToken))
	w := doRoleRequest(t, r, testMarketplaceToken, spaceID, "pii-owner")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	body := w.Body.String()
	for _, leak := range []string{
		"Display Name", "Legal Name", "gated-username", "a@b.c", "13800000000",
		"pii-owner", // even echoing the uid back is unnecessary — the caller supplied it
	} {
		assert.NotContains(t, body, leak,
			"the role endpoint must return the role and nothing else; %q leaked", leak)
	}
	assert.JSONEq(t, `{"data":{"role":2}}`, body)
}

// ============================================================================
// Token config resolution
// ============================================================================

func TestResolveMarketplaceInternalTokenRejectsUnset(t *testing.T) {
	if _, err := resolveMarketplaceInternalToken(func(string) string { return "" }); err == nil {
		t.Fatalf("expected error when %s is unset", MarketplaceInternalTokenEnv)
	}
}

func TestResolveMarketplaceInternalTokenRejectsNilGetenv(t *testing.T) {
	if _, err := resolveMarketplaceInternalToken(nil); err == nil {
		t.Fatal("expected error for a nil getenv (capability must fail closed)")
	}
}

// The length floor is checked BEFORE the collision checks: a short token is
// unusable regardless of collisions, and the error must not tell the operator
// which sibling it clashes with. Here the short value ALSO collides with every
// sibling; the error must still be the length one.
func TestResolveMarketplaceInternalTokenChecksLengthBeforeCollision(t *testing.T) {
	shortTok := strings.Repeat("x", minMarketplaceInternalTokenBytes-1)
	getenv := func(string) string { return shortTok } // every env returns the same short value
	_, err := resolveMarketplaceInternalToken(getenv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 32 bytes",
		"a too-short token must be reported as too short, never as colliding with a sibling")
	assert.NotContains(t, err.Error(), "must differ from")
	assert.NotContains(t, err.Error(), shortTok, "error messages must never contain token values")
}

func TestResolveMarketplaceInternalTokenRejectsSiblingCollision(t *testing.T) {
	sharedSecret := strings.Repeat("s", minMarketplaceInternalTokenBytes)
	cases := []struct {
		name    string
		sibling string
	}{
		{"notify", notifyInternalTokenEnv},
		{"docs-notify", docsNotifyInternalTokenEnv},
		{"bot-mention", botMentionInternalTokenEnv},
		{"drive", driveInternalTokenEnvForExclusion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string {
				if k == MarketplaceInternalTokenEnv || k == tc.sibling {
					return sharedSecret
				}
				return ""
			}
			_, err := resolveMarketplaceInternalToken(getenv)
			require.Error(t, err, "expected error when %s == %s",
				MarketplaceInternalTokenEnv, tc.sibling)
			assert.NotContains(t, err.Error(), sharedSecret,
				"error messages must never contain token values")
		})
	}
}

func TestResolveMarketplaceInternalTokenAcceptsUnique(t *testing.T) {
	mine := strings.Repeat("m", minMarketplaceInternalTokenBytes)
	other := strings.Repeat("o", minMarketplaceInternalTokenBytes)
	getenv := func(k string) string {
		if k == MarketplaceInternalTokenEnv {
			return mine
		}
		return other
	}
	got, err := resolveMarketplaceInternalToken(getenv)
	require.NoError(t, err)
	assert.Equal(t, mine, got)
}

// The endpoint is un-user-authed, so its rate limit is a load-bearing control
// (.octospec/rules/rate-limit.md). wkhttp.ParseRPSFromEnv accepts NaN/+Inf
// (ParseFloat lets both through and the helper only rejects n <= 0) and those
// values silently DISABLE the limiter inside the Redis Lua script, so Route()
// must sanitize. This pins the exact composition Route() uses.
func TestSpaceMemberRoleRateLimitEnvSanitized(t *testing.T) {
	for _, raw := range []string{"NaN", "+Inf", "0", "-3", "not-a-number"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(envSpaceMemberRoleIPRPS, raw)
			t.Setenv(envSpaceMemberRoleIPBurst, raw)
			assert.Equal(t, defSpaceMemberRoleIPRPS, sanitizedSpaceMemberRoleRPS(),
				"a pathological RPS env must fall back to the compiled default, not disable the limiter")
			assert.Equal(t, defSpaceMemberRoleIPBurst, sanitizedSpaceMemberRoleBurst(),
				"a pathological burst env must fall back to the compiled default")
		})
	}

	t.Run("legitimate override passes through", func(t *testing.T) {
		t.Setenv(envSpaceMemberRoleIPRPS, "10")
		t.Setenv(envSpaceMemberRoleIPBurst, "50")
		assert.Equal(t, 10.0, sanitizedSpaceMemberRoleRPS())
		assert.Equal(t, 50, sanitizedSpaceMemberRoleBurst())
	})
}

// ============================================================================
// ActiveAdminUIDs — the delivery-targeting read consumed by modules/notify
// ============================================================================

// The predicate set is an authorization boundary: whoever lands in this slice
// receives an approval card, and the consumer subsequently treats that uid as
// an authorized approver.
func TestActiveAdminUIDs_PredicateSet(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sp-admin-targets"
	seedMemberSearchSpace(t, spaceID, "t-owner") // role=2, status=1
	seedMemberSearchMember(t, spaceID, "t-admin", 1, 1)
	seedMemberSearchMember(t, spaceID, "t-member", 0, 1)        // excluded: role 0
	seedMemberSearchMember(t, spaceID, "t-removed-admin", 1, 0) // excluded: status 0
	seedMemberSearchRobot(t, "t-bot-admin", "Bot Admin")
	seedMemberSearchMember(t, spaceID, "t-bot-admin", 1, 1) // excluded: robot
	// An owner of a DIFFERENT space — cross-space leakage guard.
	seedMemberSearchSpace(t, "sp-admin-targets-other", "t-other")

	uids, err := ActiveAdminUIDs(testCtx.DB(), spaceID, 200)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"t-owner", "t-admin"}, uids,
		"only human members with status=1 AND role>=1 of THIS space may be targeted")

	// Ordering is role DESC, so the owner comes first — deterministic output is
	// part of the contract (ORDER BY + LIMIT).
	require.Len(t, uids, 2)
	assert.Equal(t, "t-owner", uids[0])
}

// A disbanded Space must resolve to zero recipients even though its
// space_member rows stay status=1.
func TestActiveAdminUIDs_DisbandedSpaceResolvesEmpty(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sp-admin-targets-dead"
	require.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceID, Name: spaceID, Creator: "dead-admin",
		Status: SpaceStatusDisbanded,
	}))
	require.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: "dead-admin", Role: 2, Status: 1,
	}))

	uids, err := ActiveAdminUIDs(testCtx.DB(), spaceID, 200)
	require.NoError(t, err)
	assert.Empty(t, uids)
}

// Unknown Space, empty space_id, nil session and a zero limit must all be quiet
// no-ops rather than errors or unbounded queries.
func TestActiveAdminUIDs_DegenerateInputs(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	uids, err := ActiveAdminUIDs(testCtx.DB(), "sp-does-not-exist", 200)
	require.NoError(t, err)
	assert.Empty(t, uids)

	uids, err = ActiveAdminUIDs(testCtx.DB(), "", 200)
	require.NoError(t, err)
	assert.Empty(t, uids)

	uids, err = ActiveAdminUIDs(nil, "sp-x", 200)
	require.NoError(t, err)
	assert.Empty(t, uids)

	uids, err = ActiveAdminUIDs(testCtx.DB(), "sp-x", 0)
	require.NoError(t, err)
	assert.Empty(t, uids)
}

// The limit really bounds the query — the caller's truncation detection
// (over-fetch by one, then truncate + WARN) depends on it.
func TestActiveAdminUIDs_RespectsLimit(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	const spaceID = "sp-admin-targets-limit"
	seedMemberSearchSpace(t, spaceID, "l-owner")
	seedMemberSearchMember(t, spaceID, "l-admin-1", 1, 1)
	seedMemberSearchMember(t, spaceID, "l-admin-2", 1, 1)

	uids, err := ActiveAdminUIDs(testCtx.DB(), spaceID, 2)
	require.NoError(t, err)
	assert.Len(t, uids, 2)
}
