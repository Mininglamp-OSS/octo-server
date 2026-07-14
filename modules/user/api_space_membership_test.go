package user

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authVerifySpaceMembership: POST /v1/auth/space-membership
//
// Service-to-service membership probe backing octo-docs-backend
// anyone_in_space share permissions (GH octo-docs #64). Cases:
//   member(role 0) -> true+role=0 / admin(role 1) -> true+role=1 /
//   owner(role 2) -> true+role=2 / non-member -> false(no role) /
//   removed(status=0) -> false / missing space_id -> 400 /
//   missing uid -> 400 / bot uid parity -> true.
// Plus a source guard pinning the "same protection as verify-bot"
// registration contract (verifyLimit, NOT session AuthMiddleware).

const (
	testSpaceMembershipSpace = "space_membership_space_a"
	testSpaceMembershipUID   = "space_membership_uid_001"
)

// seedSpaceMember inserts an active user + active space + one active
// space_member row with the given role. Idempotent on the user and space rows
// (INSERT IGNORE) so multiple members can share a space. The user row is
// required because the membership query INNER JOINs `user` ON u.status=1 —
// without it the happy-path cases would read as non-members.
func seedSpaceMember(t *testing.T, ctx *config.Context, spaceID, uid string, role int) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT IGNORE INTO `user` (uid, name, status, short_no) VALUES (?, ?, ?, ?)",
		uid, "member_"+uid, 1, "sn_"+uid,
	)
	require.NoError(t, err)
	_, err = ctx.DB().Exec(
		"INSERT IGNORE INTO space (space_id, name, creator, status) VALUES (?, ?, ?, ?)",
		spaceID, "Test "+spaceID, uid, 1,
	)
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, uid, role, 1).Exec()
	require.NoError(t, err)
}

func doVerifySpaceMembership(t *testing.T, s *server.Server, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doVerifySpaceMembershipWithKey(t, s, body, os.Getenv("AUTH_INTERNAL_KEY"))
}

// doVerifySpaceMembershipWithKey posts with an explicit X-Internal-Key value
// (empty string => header omitted) so the auth tests can exercise the
// missing/wrong-key rejection paths.
func doVerifySpaceMembershipWithKey(t *testing.T, s *server.Server, body interface{}, internalKey string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader([]byte(util.ToJson(body)))
	} else {
		reqBody = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/auth/space-membership", reqBody)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if internalKey != "" {
		req.Header.Set("X-Internal-Key", internalKey)
	}
	s.GetRoute().ServeHTTP(w, req)
	return w
}

type spaceMembershipResp struct {
	SpaceID  string `json:"space_id"`
	UID      string `json:"uid"`
	IsMember bool   `json:"is_member"`
	Role     *int   `json:"role"`
}

// Case 1: active member with base role -> 200 is_member=true, role=0.
// Guards that role=0 (member) is echoed as present, not swallowed.
func TestAuthVerifySpaceMembership_Member(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, testSpaceMembershipUID, 0)

	w := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	})

	require.Equal(t, http.StatusOK, w.Code)
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, testSpaceMembershipSpace, resp.SpaceID)
	assert.Equal(t, testSpaceMembershipUID, resp.UID)
	assert.True(t, resp.IsMember)
	require.NotNil(t, resp.Role)
	assert.Equal(t, 0, *resp.Role)
}

// Case 1b: admin (role 1) -> role echoed as 1.
func TestAuthVerifySpaceMembership_Admin(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, testSpaceMembershipUID, 1)

	w := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	})

	require.Equal(t, http.StatusOK, w.Code)
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.IsMember)
	require.NotNil(t, resp.Role)
	assert.Equal(t, 1, *resp.Role)
}

// Case 1c: owner (role 2) -> role echoed as 2.
func TestAuthVerifySpaceMembership_Owner(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, testSpaceMembershipUID, 2)

	w := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	})

	require.Equal(t, http.StatusOK, w.Code)
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.IsMember)
	require.NotNil(t, resp.Role)
	assert.Equal(t, 2, *resp.Role)
}

// Case 2: uid with no space_member row for the space -> 200 is_member=false,
// role omitted from the wire body entirely.
func TestAuthVerifySpaceMembership_NonMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	// seed a DIFFERENT member so the space exists but the queried uid is not in it
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, "some_other_member", 0)

	w := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	})

	require.Equal(t, http.StatusOK, w.Code)
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.IsMember)
	assert.Nil(t, resp.Role, "role must be omitted for a non-member")
	assert.NotContains(t, w.Body.String(), "\"role\"", "role key must not appear on the wire when is_member=false")
}

// Case 3: member row exists but status=0 (removed/left) -> 200 is_member=false.
// The status=1 filter is the whole point; a status!=1 check regression would
// flip this to true.
func TestAuthVerifySpaceMembership_RemovedMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, testSpaceMembershipUID, 0)
	_, err := ctx.DB().Update("space_member").
		Set("status", 0).
		Where("space_id=? AND uid=?", testSpaceMembershipSpace, testSpaceMembershipUID).
		Exec()
	require.NoError(t, err)

	w := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	})

	require.Equal(t, http.StatusOK, w.Code)
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.IsMember)
	assert.Nil(t, resp.Role)
}

// Case 3b: non-active status other than 0 (e.g. 2 "pending") -> false as well.
// Explicit coverage so a future `status != 0` rewrite is caught.
func TestAuthVerifySpaceMembership_NonActiveStatus(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, testSpaceMembershipUID, 0)
	_, err := ctx.DB().Update("space_member").
		Set("status", 2).
		Where("space_id=? AND uid=?", testSpaceMembershipSpace, testSpaceMembershipUID).
		Exec()
	require.NoError(t, err)

	w := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	})

	require.Equal(t, http.StatusOK, w.Code)
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.IsMember)
}

// Case 3c: space is disbanded/disabled (space.status=0) but the member row is
// left active (space_member.status=1) -> is_member=false. disbandSpace
// (modules/space/db.go) sets space.status=0 without cascading space_member, so
// a member-status-only query would wrongly report is_member=true and grant
// docs-backend share access inside a dead space. Mirrors the api-key sibling's
// TestAuthVerifyAPIKey_DisabledSpace_401. Requires the INNER JOIN space
// (s.status=1) guard.
func TestAuthVerifySpaceMembership_DisabledSpace(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	// Active user + active member row, but the space itself is disbanded.
	_, err := ctx.DB().Exec(
		"INSERT IGNORE INTO `user` (uid, name, status, short_no) VALUES (?, ?, ?, ?)",
		testSpaceMembershipUID, "member", 1, "sn_disabled_space",
	)
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(testSpaceMembershipSpace, "Disbanded", testSpaceMembershipUID, 0). // ← space disbanded
		Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(testSpaceMembershipSpace, testSpaceMembershipUID, 0, 1). // ← member left active
		Exec()
	require.NoError(t, err)

	w := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	})

	require.Equal(t, http.StatusOK, w.Code)
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.IsMember,
		"disbanded space (space.status=0) with a lingering active member must report is_member=false")
	assert.Nil(t, resp.Role)
}

// Case 3d: globally-banned user (user.status=0) with an active member row in an
// active space -> is_member=false. liftBanUser sets user.status=0 without
// touching space_member, matching the api-key path's TestAuthVerifyAPIKey_
// AccountBanned_401. Requires the INNER JOIN `user` (u.status=1) guard.
func TestAuthVerifySpaceMembership_BannedUser(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, testSpaceMembershipUID, 0)

	// Ban the user (mirrors liftBanUser: user.status=0, leave the member row
	// active to exercise the exact bypass the user-liveness join closes).
	_, err := ctx.DB().Update("user").
		Set("status", 0).
		Where("uid=?", testSpaceMembershipUID).
		Exec()
	require.NoError(t, err)

	w := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	})

	require.Equal(t, http.StatusOK, w.Code)
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.IsMember,
		"globally-banned user (user.status=0) must report is_member=false")
	assert.Nil(t, resp.Role)
}

// Case 4: missing space_id -> 400.
func TestAuthVerifySpaceMembership_MissingSpaceID(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	w := doVerifySpaceMembership(t, s, map[string]string{"uid": testSpaceMembershipUID})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Case 5: missing uid -> 400.
func TestAuthVerifySpaceMembership_MissingUID(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	w := doVerifySpaceMembership(t, s, map[string]string{"space_id": testSpaceMembershipSpace})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Case 6: bot uid parity — a bot has a space_member row just like a human, so
// the same predicate resolves it to true with no special-casing. This is the
// human/bot parity guarantee docs-backend relies on.
func TestAuthVerifySpaceMembership_BotUIDParity(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	botUID := "space_membership_bot_001"
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, botUID, 0)

	w := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      botUID,
	})

	require.Equal(t, http.StatusOK, w.Code)
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.IsMember)
	require.NotNil(t, resp.Role)
	assert.Equal(t, 0, *resp.Role)
}

// TestAuthVerifySpaceMembership_ProtectionContract asserts the endpoint's
// protection by BEHAVIOR rather than by grepping the route-registration source
// line (which broke on reformatting and pinned the pre-hardening posture).
// Two properties matter and both are checked against live request/response:
//
//   - A non-internal caller (no X-Internal-Key) is rejected with 401 — the
//     in-code service-auth gate is actually in front of the handler.
//   - A caller presenting the valid X-Internal-Key reaches the handler and gets
//     a real membership answer WITHOUT any end-user session token — i.e. the
//     route is a service-to-service endpoint, deliberately NOT behind end-user
//     session AuthMiddleware (which would 401 a token-less request before the
//     handler ran).
func TestAuthVerifySpaceMembership_ProtectionContract(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, testSpaceMembershipUID, 0)

	// Non-internal caller (no X-Internal-Key) -> rejected.
	noKey := doVerifySpaceMembershipWithKey(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	}, "")
	assert.Equal(t, http.StatusUnauthorized, noKey.Code,
		"a caller without X-Internal-Key must be rejected")

	// Valid internal key, no session token -> handler runs and answers.
	withKey := doVerifySpaceMembership(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	})
	require.Equal(t, http.StatusOK, withKey.Code,
		"a service caller with a valid X-Internal-Key and no session token must reach the handler (route is not behind end-user AuthMiddleware)")
	var resp spaceMembershipResp
	require.NoError(t, json.Unmarshal(withKey.Body.Bytes(), &resp))
	assert.True(t, resp.IsMember)
}

// TestAuthVerifySpaceMembership_WrongInternalKey_401: a wrong X-Internal-Key is
// rejected (exercises the ConstantTimeCompare mismatch path).
func TestAuthVerifySpaceMembership_WrongInternalKey_401(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, testSpaceMembershipUID, 0)

	w := doVerifySpaceMembershipWithKey(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	}, "definitely-not-the-key")
	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"a wrong X-Internal-Key must be rejected")
}

// TestAuthVerifySpaceMembership_FailClosedWhenKeyUnset: when AUTH_INTERNAL_KEY
// is not configured the endpoint rejects EVERY request (even with a header),
// rather than fall back to network-level restriction alone. Mirrors notify's
// NOTIFY_INTERNAL_TOKEN fail-closed posture.
func TestAuthVerifySpaceMembership_FailClosedWhenKeyUnset(t *testing.T) {
	prev, had := os.LookupEnv("AUTH_INTERNAL_KEY")
	require.NoError(t, os.Unsetenv("AUTH_INTERNAL_KEY"))
	defer func() {
		if had {
			_ = os.Setenv("AUTH_INTERNAL_KEY", prev)
		}
	}()

	// Build the server AFTER unsetting so user.New reads an empty key.
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedSpaceMember(t, ctx, testSpaceMembershipSpace, testSpaceMembershipUID, 0)

	w := doVerifySpaceMembershipWithKey(t, s, map[string]string{
		"space_id": testSpaceMembershipSpace,
		"uid":      testSpaceMembershipUID,
	}, "anything")
	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"with AUTH_INTERNAL_KEY unset the endpoint must fail closed (reject all requests)")
}
