package user

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Project half of POST /v1/auth/verify?include=context (D11).
//
// This endpoint runs on EVERY request of every subsystem that fronts
// octo-server, its per-IP limiter is shared by all gateway pods, and the three
// verify routes carry no AuthMiddleware. So the cases below are as much about
// what the response does NOT contain as about what it does.

type verifyProjectResp struct {
	UID             string `json:"uid"`
	Role            string `json:"role"`
	ContextIncluded bool   `json:"context_included"`
	ContextError    bool   `json:"context_error"`
	SpacesTruncated bool   `json:"spaces_truncated"`
	Projects        []struct {
		ProjectID    string   `json:"project_id"`
		Member       bool     `json:"member"`
		Role         *int     `json:"role"`
		Capabilities []string `json:"capabilities"`
		MemberEpoch  *int64   `json:"member_epoch"`
	} `json:"projects"`
}

func seedVerifyProject(t *testing.T, ctx *config.Context, projectID, spaceID string) {
	t.Helper()
	// The name must be unique per Space: P0's uk_octo_project_space_active_name
	// is a real constraint, and deriving it from the id keeps this fixture from
	// tripping it when a case seeds several projects in one Space.
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project` (project_id, space_id, name, creator, status, member_epoch, created_at, updated_at) "+
			"VALUES (?, ?, ?, '', 1, 7, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		projectID, spaceID, "p-"+projectID[:8]).Exec()
	require.NoError(t, err)
}

func seedVerifyProjectMember(t *testing.T, ctx *config.Context, projectID, spaceID, uid string, role int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project_member` (project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 1, 0, '', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		projectID, uid, spaceID, role).Exec()
	require.NoError(t, err)
}

// TestVerifyAnswersOnlyTheProjectsAsked pins the point-query contract: N ids in,
// exactly N answers out, and never a project the caller did not name.
//
// That is what keeps the response O(asked) instead of O(the user's projects) —
// and with it, truncation stops being a case that has to be designed correctly.
func TestVerifyAnswersOnlyTheProjectsAsked(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	spaceID := "sp_verify_1"
	member := util.GenerUUID()
	other := util.GenerUUID()   // the caller is in it, but does not ask about it
	unknown := util.GenerUUID() // does not exist
	seedVerifyProject(t, ctx, member, spaceID)
	seedVerifyProject(t, ctx, other, spaceID)
	seedVerifyProjectMember(t, ctx, member, spaceID, testutil.UID, 1)
	seedVerifyProjectMember(t, ctx, other, spaceID, testutil.UID, 0)

	w := doVerifyToken(t, s, map[string]interface{}{
		"token":       testutil.Token,
		"space_id":    spaceID,
		"project_ids": []string{member, unknown},
	}, true)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp verifyProjectResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Projects, 2, "exactly the ids asked about, no more")
	assert.Equal(t, member, resp.Projects[0].ProjectID)
	assert.Equal(t, unknown, resp.Projects[1].ProjectID)
	assert.NotContains(t, w.Body.String(), other,
		"a project the caller belongs to but did not ask about must not appear")
}

// TestVerifyNonMemberAnswersCarryNothingElse.
//
// "not a member", "no such project" and "a project in another Space" must be
// byte-identical on the wire. Handing a non-member the epoch would leak that the
// project exists and how often its membership changes; distinguishing the three
// would let a caller probe which Space a project lives in.
func TestVerifyNonMemberAnswersCarryNothingElse(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	spaceID := "sp_verify_2"
	otherSpace := "sp_verify_2_other"
	notMember := util.GenerUUID()
	elsewhere := util.GenerUUID()
	missing := util.GenerUUID()
	seedVerifyProject(t, ctx, notMember, spaceID)    // exists, caller not in it
	seedVerifyProject(t, ctx, elsewhere, otherSpace) // exists, another Space
	seedVerifyProjectMember(t, ctx, elsewhere, otherSpace, testutil.UID, 2)

	w := doVerifyToken(t, s, map[string]interface{}{
		"token":       testutil.Token,
		"space_id":    spaceID,
		"project_ids": []string{notMember, elsewhere, missing},
	}, true)
	require.Equal(t, http.StatusOK, w.Code)

	var resp verifyProjectResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Projects, 3)
	for _, p := range resp.Projects {
		assert.False(t, p.Member, "%s must be answered not-a-member", p.ProjectID)
		assert.Nil(t, p.Role, "a non-member answer must carry no role")
		assert.Nil(t, p.MemberEpoch, "a non-member answer must carry no epoch")
		assert.Empty(t, p.Capabilities, "a non-member answer must carry no capabilities")
	}

	// And the three are indistinguishable: their JSON objects are identical
	// apart from the id the caller already supplied.
	var raw struct {
		Projects []map[string]interface{} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	for _, obj := range raw.Projects {
		require.Len(t, obj, 2, "a non-member item must carry exactly project_id and member: %v", obj)
	}
}

// TestVerifyEmitsCapabilitiesAndDistinguishesRoleTypes.
//
// capabilities is emitted explicitly so no consumer needs to map role numbers to
// permissions — a consumer that did would re-implement this repository's
// permission matrix and drift from it the first time it changed.
//
// The response also carries TWO fields called "role": the platform role at the
// top level (a string) and the project role inside each answer (an int). Same
// name, different type, different meaning; pinned here because a consumer that
// conflates them gets a silent authorization bug.
func TestVerifyEmitsCapabilitiesAndDistinguishesRoleTypes(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	spaceID := "sp_verify_3"
	owned := util.GenerUUID()
	seedVerifyProject(t, ctx, owned, spaceID)
	seedVerifyProjectMember(t, ctx, owned, spaceID, testutil.UID, 2) // owner

	w := doVerifyToken(t, s, map[string]interface{}{
		"token":       testutil.Token,
		"space_id":    spaceID,
		"project_ids": []string{owned},
	}, true)
	require.Equal(t, http.StatusOK, w.Code)

	var resp verifyProjectResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Projects, 1)
	p := resp.Projects[0]
	require.True(t, p.Member)
	require.NotNil(t, p.Role)
	assert.Equal(t, 2, *p.Role)
	assert.Contains(t, p.Capabilities, "project.disband", "an owner's capabilities must be explicit")
	assert.Contains(t, p.Capabilities, "project.member.manage")
	require.NotNil(t, p.MemberEpoch)
	assert.Equal(t, int64(7), *p.MemberEpoch, "member_epoch must be the column, not a derived number")

	// The two "role" fields are different types and must decode independently.
	var typed struct {
		Role     string `json:"role"`
		Projects []struct {
			Role int `json:"role"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &typed),
		"top-level role is a string and projects[].role is an int; both must decode")
	assert.Equal(t, 2, typed.Projects[0].Role)
}

// TestVerifyRejectsTooManyProjectIDs pins the structural bound (50). A
// well-formed but pathological payload must not turn one gateway request into an
// unbounded lookup.
func TestVerifyRejectsTooManyProjectIDs(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)
	// testutil.NewTestServer installs no ErrorRenderer (main.go does), and
	// without one the route falls back to the legacy {msg,status} body with no
	// error.details — so an assertion on the offending field would silently
	// compare against nothing.
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	ids := make([]string, 0, maxVerifyProjectIDs+1)
	for i := 0; i <= maxVerifyProjectIDs; i++ {
		ids = append(ids, util.GenerUUID())
	}
	w := doVerifyToken(t, s, map[string]interface{}{
		"token":       testutil.Token,
		"space_id":    "sp_verify_4",
		"project_ids": ids,
	}, true)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "project_ids",
		"the refusal must name the offending field: %s", w.Body.String())
}

// TestVerifyDefaultShapeIsUnchanged is the C9 golden assertion.
//
// A caller that does NOT pass ?include=context must see byte-identical output to
// what it saw before P1 — IM clients and admin tools depend on this schema.
// Every field added by this change is omitempty for that reason, which makes
// this test the thing that would catch a future addition that is not.
func TestVerifyDefaultShapeIsUnchanged(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, false)
	require.Equal(t, http.StatusOK, w.Code)

	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	for _, added := range []string{"projects", "spaces_truncated", "context_error", "context_included", "spaces", "owned_bots_by_space"} {
		assert.NotContains(t, got, added,
			"the default response must not grow a field: %s", w.Body.String())
	}
	// And it still carries exactly what it always did.
	for _, kept := range []string{"uid", "name", "role", "owned_bots"} {
		assert.Contains(t, got, kept)
	}
}

// TestVerifyDistinguishesAFailedLookupFromAnEmptyOne.
//
// context_error is a SEPARATE field from context_included on purpose.
// context_included means "this server speaks the v2 contract", and a consumer
// reading false falls back to trusting the client-supplied X-Space-Id header —
// so a transient database error must not clear it. The failure is reported
// alongside instead, which is what makes "no spaces" and "lookup failed"
// distinguishable without opening that trust.
func TestVerifyDistinguishesAFailedLookupFromAnEmptyOne(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	// Healthy: no error flag.
	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code)
	var ok verifyProjectResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ok))
	assert.True(t, ok.ContextIncluded)
	assert.False(t, ok.ContextError, "a healthy lookup must not claim an error")

	// Break the bots query the way the existing fail-secure case does.
	_, err := ctx.DB().Exec("RENAME TABLE robot TO robot_tmp_p1_project")
	require.NoError(t, err)
	defer func() { _, _ = ctx.DB().Exec("RENAME TABLE robot_tmp_p1_project TO robot") }()

	w = doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code, "a context failure must not 500")
	var failed verifyProjectResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &failed))
	assert.True(t, failed.ContextIncluded,
		"the contract flag must stay true: clearing it downgrades the consumer to trusting X-Space-Id")
	assert.True(t, failed.ContextError, "and the failure must be visible on the wire")
}

// TestVerifySpacesTruncationIsVisibleOnTheWire.
//
// The cap has always existed and the over-fetch has always computed this fact;
// it went to a server-side Warn and was then dropped. A middleware doing an
// X-Space-Id membership check against a silently truncated list denies a
// legitimate member — an outage that looks like a permissions bug, for exactly
// the power users most likely to notice.
func TestVerifySpacesTruncationIsVisibleOnTheWire(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	seedVerifyTokenFixtures(t, ctx)

	// One past the cap: the smallest input that must flip the flag.
	const over = userSpacesLimit + 1
	for i := 0; i < over; i++ {
		sid := fmt.Sprintf("sp_trunc_%03d", i)
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO `space` (space_id, name, status, created_at, updated_at) "+
				"VALUES (?, ?, 1, NOW(), NOW())", sid, sid).Exec()
		require.NoError(t, err)
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) "+
				"VALUES (?, ?, 0, 1, NOW(), NOW())", sid, testutil.UID).Exec()
		require.NoError(t, err)
	}

	w := doVerifyToken(t, s, map[string]string{"token": testutil.Token}, true)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Spaces          []string `json:"spaces"`
		SpacesTruncated bool     `json:"spaces_truncated"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Spaces, userSpacesLimit, "the list is still capped")
	assert.True(t, resp.SpacesTruncated, "and the caller can now tell that it was capped")
}
