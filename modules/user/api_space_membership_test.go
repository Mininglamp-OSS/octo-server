package user

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
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

// seedSpaceMember inserts a space + one active space_member row with the given
// role. Idempotent on the space row so multiple members can share a space.
func seedSpaceMember(t *testing.T, ctx *config.Context, spaceID, uid string, role int) {
	t.Helper()
	_, err := ctx.DB().Exec(
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

// TestAuthVerifySpaceMembership_ProtectionContract pins the "same protection as
// verify-bot" registration requirement via source grep. The endpoint's
// caller-authorization (401/403 for a non-internal caller) is enforced at the
// network layer (nginx internal-IP allowlist / X-Internal-Key) shared by the
// whole verify group, exactly like verify / verify-bot / verify-api-key — it is
// deliberately NOT behind end-user session AuthMiddleware. This guard fails if
// a future edit drops the verifyLimit protection primitive or moves the route
// behind AuthMiddleware, either of which would break the service-to-service
// contract.
func TestAuthVerifySpaceMembership_ProtectionContract(t *testing.T) {
	data, err := os.ReadFile("api.go")
	require.NoError(t, err)
	source := string(data)

	line := "v.POST(\"/auth/space-membership\", verifyLimit, u.authVerifySpaceMembership)"
	assert.Contains(t, source, line,
		"space-membership must register in the verify group with verifyLimit, matching verify-bot")

	// Must sit in the same verify group block as verify-bot (between the group
	// header comment and the third-party auth block), not in an AuthMiddleware
	// group.
	groupStart := strings.Index(source, "Token / Bot 认证验证（供 Gateway 调用）")
	require.NotEqual(t, -1, groupStart, "verify group header not found")
	routeIdx := strings.Index(source, line)
	require.NotEqual(t, -1, routeIdx)
	assert.Greater(t, routeIdx, groupStart, "space-membership must be inside the Gateway verify group")

	// The route registration must NOT carry an AuthMiddleware wrapper.
	assert.Regexp(t,
		regexp.MustCompile(`v\.POST\(\s*"/auth/space-membership",\s*verifyLimit,\s*u\.authVerifySpaceMembership\s*\)`),
		source,
		"space-membership must use verifyLimit only, not session AuthMiddleware")
}
