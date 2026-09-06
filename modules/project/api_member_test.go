package project

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectWithMembers seeds an active Space, an owner and the named ordinary members,
// creates a project through HTTP and admits each member. Returns the owner token, the
// per-uid tokens and the project.
func projectWithMembers(t *testing.T, srv *server.Server, uids ...string) (string, map[string]string, *Resp) {
	t.Helper()
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	tokens := map[string]string{"owner1": ownerTok}
	for _, uid := range uids {
		tokens[uid] = seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}
	created := createProjectVia(t, srv, spaceA, ownerTok, "p")
	if len(uids) > 0 {
		w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
			ownerTok, map[string]any{"uids": uids})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		var outcomes []memberOutcome
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
		for _, o := range outcomes {
			require.True(t, o.OK, "seeding member %s failed: %s", o.UID, o.Reason)
		}
	}
	return ownerTok, tokens, created
}

// ---------- invariant I1, synchronous half ----------

// TestAddRejectsNonSpaceMember covers I1 on the only P0 admission path. (The
// invite-accept path the brief also names is P2 along with the rest of the invite
// surface.)
func TestAddRejectsNonSpaceMember(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)

	// A user with no Space seat at all.
	seedUser(t, "nobody")
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"nobody"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	require.Len(t, outcomes, 1)
	assert.False(t, outcomes[0].OK)
	assert.Equal(t, reasonNotSpaceMember, outcomes[0].Reason)

	// A user whose Space seat was removed.
	seedUser(t, "exmember")
	seedSpaceMember(t, spaceA, "exmember", 0, 1)
	removeSpaceMember(t, spaceA, "exmember")
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"exmember"}})
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	assert.Equal(t, reasonNotSpaceMember, outcomes[0].Reason)
}

// TestAddRejectsMemberOfAnotherSpace covers the cross-Space half of I1: active in Space
// A does not admit you to a project in Space B.
func TestAddRejectsMemberOfAnotherSpace(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	seedSpace(t, spaceB, 1)
	seedUser(t, "bOnly")
	seedSpaceMember(t, spaceB, "bOnly", 0, 1)

	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"bOnly"}})
	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	require.Len(t, outcomes, 1)
	assert.Equal(t, reasonNotSpaceMember, outcomes[0].Reason)
}

// TestAddRejectsWhenSpaceIsBanned covers the authorization side of the banned-Space
// axis. CheckMembership requires space.status=1, so admission into a banned Space's
// project is impossible — and it is impossible at BOTH layers, which is what this pins,
// because the two layers fail on different schedules:
//
//   - the transactional I1 check in the handler refuses immediately, even while the
//     caller's own Space membership is still cached as valid;
//   - the middleware's Space gate refuses once its cache entry is cold.
//
// The middleware's staleness window is pre-existing and not introduced here: banning a
// Space does not invalidate space:member:*, so pkg/space's own SpaceMiddleware admits a
// member of a freshly banned Space for up to the same TTL. What P0 must guarantee is
// that no new project seat can be created in a banned Space, and the transactional check
// is what guarantees it regardless of cache state.
//
// Note this is the OPPOSITE of what cleanup does with a banned Space: cleanup uses
// CheckMembershipForCleanup and SKIPS, because the seat is still real. Authorization and
// cleanup deliberately answer differently here, and confusing the two would tear every
// project membership apart the moment a Space is banned.
func TestAddRejectsWhenSpaceIsBanned(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	seedUser(t, "later")
	seedSpaceMember(t, spaceA, "later", 0, 1)
	setSpaceStatus(t, spaceA, 2)

	// Warm cache: the caller still gets through the gate, but the transactional I1 check
	// refuses the admission.
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"later"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	require.Len(t, outcomes, 1)
	assert.False(t, outcomes[0].OK)
	assert.Equal(t, reasonNotSpaceMember, outcomes[0].Reason,
		"a banned Space must not admit anyone, whatever the caller's cache says")

	// Cold cache: the gate itself refuses, so the handler is never reached.
	flushProjectCache(t, testCtx)
	resetUIDRateLimit(t, testCtx)
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"later"}})
	require.NotEqual(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// No seat was created either way.
	member, err := testDB.queryMember(created.ProjectID, "later")
	require.NoError(t, err)
	assert.Nil(t, member)
}

// ---------- member_epoch ----------

// TestMemberEpochStrictlyIncreasesOnEveryWrite covers add / remove / role change /
// leave / disband. The Space-cascade path is covered in the cascade test.
func TestMemberEpochStrictlyIncreasesOnEveryWrite(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "m1", "m2")

	// The add during seeding already moved it twice (one per admitted member).
	afterAdd := epochOf(t, created.ProjectID)
	assert.Greater(t, afterAdd, int64(0), "admitting members must move the epoch")

	// role change
	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/role", ownerTok,
		map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	afterRole := epochOf(t, created.ProjectID)
	assert.Greater(t, afterRole, afterAdd)

	// remove
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/remove", ownerTok,
		map[string]any{"uids": []string{"m2"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	afterRemove := epochOf(t, created.ProjectID)
	assert.Greater(t, afterRemove, afterRole)

	// leave (m1 is an admin, not the last owner)
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave",
		tokens["m1"], nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	afterLeave := epochOf(t, created.ProjectID)
	assert.Greater(t, afterLeave, afterRemove)

	// disband
	w = doJSON(t, srv, http.MethodDelete, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Greater(t, epochOf(t, created.ProjectID), afterLeave)
}

// TestMemberEpochUnchangedOnNoOpWrites pins the property clients cache against: a write
// that changes nothing must not move the epoch, or every idempotent retry invalidates
// every consumer's cache.
func TestMemberEpochUnchangedOnNoOpWrites(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "m1")
	baseline := epochOf(t, created.ProjectID)

	// Re-adding an already-active member.
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"m1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, baseline, epochOf(t, created.ProjectID),
		"re-adding an active member is a no-op and must not move the epoch")

	// Setting the role a member already has.
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/role", ownerTok,
		map[string]any{"role": RoleCommon})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, baseline, epochOf(t, created.ProjectID),
		"setting the current role is a no-op and must not move the epoch")
}

// TestMemberEpochBumpIsInTheSameTransaction forces a rollback between the membership
// write and the commit and asserts BOTH the membership row and the epoch are unchanged.
// If the bump lived in its own transaction one of the two would survive.
func TestMemberEpochBumpIsInTheSameTransaction(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv)
	seedUser(t, "rollback1")
	seedSpaceMember(t, spaceA, "rollback1", 0, 1)
	before := epochOf(t, created.ProjectID)

	// Drive the real DAO inside a transaction that is then rolled back.
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	require.NoError(t, err)
	row, err := p.db.lockActiveProjectTx(tx, created.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, row)
	changed, err := p.db.admitMemberTx(tx, &MemberModel{
		ProjectID: created.ProjectID, UID: "rollback1", SpaceID: spaceA,
		Role: RoleCommon, InviteUID: "owner1", CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, p.db.bumpMemberEpochTx(tx, created.ProjectID, now))
	require.NoError(t, tx.Rollback())

	assert.Equal(t, before, epochOf(t, created.ProjectID),
		"a rolled-back membership write must leave the epoch untouched")
	member, err := p.db.queryMember(created.ProjectID, "rollback1")
	require.NoError(t, err)
	assert.Nil(t, member, "a rolled-back membership write must leave no member row")
}

// TestMemberEpochAndCapabilitiesAreInTheResponses covers D3: the epoch ships to
// first-party clients next to my_role and the capability bits, and nowhere else.
func TestMemberEpochAndCapabilitiesAreInTheResponses(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "m1")

	detail := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, detail.Code)
	resp := decodeResp(t, detail)
	assert.Equal(t, epochOf(t, created.ProjectID), resp.MemberEpoch)
	assert.Equal(t, RoleOwner, resp.MyRole)
	assert.True(t, resp.Capabilities.CanDisband)
	assert.True(t, resp.Capabilities.CanChangeRole)

	// An ordinary member gets the same epoch but different capabilities — the point of
	// emitting them rather than letting the client derive them from MyRole.
	memberDetail := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, tokens["m1"], nil)
	require.Equal(t, http.StatusOK, memberDetail.Code)
	mResp := decodeResp(t, memberDetail)
	assert.Equal(t, resp.MemberEpoch, mResp.MemberEpoch)
	assert.Equal(t, RoleCommon, mResp.MyRole)
	assert.False(t, mResp.Capabilities.CanDisband)
	assert.False(t, mResp.Capabilities.CanChangeRole)
	assert.False(t, mResp.Capabilities.CanManageMember)
	assert.True(t, mResp.Capabilities.CanLeave)

	// And it is present in the list.
	list := doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceA+"/projects", ownerTok, nil)
	require.Equal(t, http.StatusOK, list.Code)
	var listResp []*Resp
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &listResp))
	require.Len(t, listResp, 1)
	assert.Equal(t, resp.MemberEpoch, listResp[0].MemberEpoch)
	assert.Equal(t, RoleOwner, listResp[0].MyRole)
	assert.Equal(t, 2, listResp[0].MemberCount)
}

// ---------- permission matrix ----------

// TestAdminCannotRemoveOrDemoteAdminOrOwner pins the transitive protection. Without it
// "admin" is effectively "owner": one admin demotes every peer and the owner.
func TestAdminCannotRemoveOrDemoteAdminOrOwner(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "admin1", "admin2", "plain1")
	for _, uid := range []string{"admin1", "admin2"} {
		w := doJSON(t, srv, http.MethodPut,
			"/v1/projects/"+created.ProjectID+"/members/"+uid+"/role", ownerTok,
			map[string]any{"role": RoleAdmin})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	}

	// admin1 removing admin2 is refused.
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		tokens["admin1"], map[string]any{"uids": []string{"admin2"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	require.Len(t, outcomes, 1)
	assert.False(t, outcomes[0].OK)
	assert.Equal(t, reasonPermissionDenied, outcomes[0].Reason)

	// admin1 removing the owner is refused.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		tokens["admin1"], map[string]any{"uids": []string{"owner1"}})
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	assert.Equal(t, reasonPermissionDenied, outcomes[0].Reason)

	// admin1 changing anyone's role is refused outright: role change is owner-only.
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/plain1/role", tokens["admin1"],
		map[string]any{"role": RoleAdmin})
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")

	// But admin1 removing an ordinary member is allowed.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		tokens["admin1"], map[string]any{"uids": []string{"plain1"}})
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	assert.True(t, outcomes[0].OK, "reason: %s", outcomes[0].Reason)
}

// TestLastOwnerMustTransferBeforeLeavingOrBeingDemoted pins that a project cannot be
// left ownerless, and that the transfer is atomic.
func TestLastOwnerMustTransferBeforeLeavingOrBeingDemoted(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "successor1")

	// Leaving without a successor is refused.
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave", ownerTok, nil)
	assertProjectErrorCode(t, w, "err.server.project.last_owner_must_transfer")

	// Self-demotion without a successor is refused too.
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/owner1/role", ownerTok,
		map[string]any{"role": RoleCommon})
	assertProjectErrorCode(t, w, "err.server.project.last_owner_must_transfer")

	// With a successor both the promotion and the departure land — one transaction, so
	// there is never a window with two owners or none.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave", ownerTok,
		map[string]any{"transfer_to": "successor1"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	successor, err := p.db.queryMember(created.ProjectID, "successor1")
	require.NoError(t, err)
	require.NotNil(t, successor)
	assert.Equal(t, RoleOwner, successor.Role)
	assert.Equal(t, MemberStatusActive, successor.Status)

	former, err := p.db.queryMember(created.ProjectID, "owner1")
	require.NoError(t, err)
	require.NotNil(t, former)
	assert.Equal(t, MemberStatusRemoved, former.Status)
}

// TestOwnerCannotRemoveThemselvesViaRemoveEndpoint pins that self-removal must go
// through leave, which carries the transfer rule. Allowing it here would let the last
// owner delete their own seat and leave the project unmanageable.
func TestOwnerCannotRemoveThemselvesViaRemoveEndpoint(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "m1")
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"owner1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	assert.False(t, outcomes[0].OK)
	assert.Equal(t, reasonPermissionDenied, outcomes[0].Reason)
}

// TestRemovedMemberIsDeniedOnTheVeryNextRequest proves the membership cache is
// invalidated SYNCHRONOUSLY, in the request that removed them. Deferring it to a worker
// would leave the removed member authorized for up to a full 60s TTL.
func TestRemovedMemberIsDeniedOnTheVeryNextRequest(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "m1")

	// Warm the cache: the member reads the roster, which populates project:member:*.
	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", tokens["m1"], nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Remove them, then immediately re-read. No sleep, no TTL expiry.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"m1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", tokens["m1"], nil)
	assertProjectErrorCode(t, w, "err.server.project.not_member")
}

// TestRosterIsMembersOnlyEvenForListedProjects pins that discoverability governs
// metadata, not the roster: a Space member who has not joined can see a space_listed
// project's metadata but not who is in it.
func TestRosterIsMembersOnlyEvenForListedProjects(t *testing.T) {
	srv, _ := setup(t)
	_, _, created := projectWithMembers(t, srv)
	strangerTok := seedUser(t, "stranger")
	seedSpaceMember(t, spaceA, "stranger", 0, 1)

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, strangerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "a space_listed project's metadata is visible")

	w = doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", strangerTok, nil)
	assertProjectErrorCode(t, w, "err.server.project.not_member")
}

// TestRoleValidationRejectsUnknownRole covers the enum guard.
func TestRoleValidationRejectsUnknownRole(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "m1")
	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/m1/role", ownerTok,
		map[string]any{"role": 9})
	assertProjectErrorCode(t, w, "err.server.project.role_invalid")
}

// TestSanitizeUIDsDeduplicates pins that the same uid twice in one batch does not take
// the project row lock twice and report two outcomes for one seat.
func TestSanitizeUIDsDeduplicates(t *testing.T) {
	got := sanitizeUIDs([]string{" a ", "a", "", "b", "a", "   "})
	assert.Equal(t, []string{"a", "b"}, got)
}

// TestCanActOnTargetRole is the permission matrix as a table, so the rule is readable
// without reconstructing it from HTTP cases.
func TestCanActOnTargetRole(t *testing.T) {
	cases := []struct {
		actor, target int
		want          bool
	}{
		{RoleOwner, RoleOwner, true},
		{RoleOwner, RoleAdmin, true},
		{RoleOwner, RoleCommon, true},
		{RoleAdmin, RoleOwner, false},
		{RoleAdmin, RoleAdmin, false},
		{RoleAdmin, RoleCommon, true},
		{RoleCommon, RoleCommon, false},
		{roleNonMember, RoleCommon, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, canActOnTargetRole(tc.actor, tc.target),
			"actor=%d target=%d", tc.actor, tc.target)
	}
}

// TestReactivationResetsRoleAndTimestamps pins the ON DUPLICATE KEY UPDATE assignment
// order in admitMemberTx.
//
// MySQL evaluates those assignments left to right, and a column read on the right-hand
// side sees the value written by a preceding assignment. So anything testing the OLD status
// must come before `status = 1`. An earlier draft had
// `updated_at = IF(status = 0, ...)` after it, which made the condition permanently false:
// a re-admitted member kept the updated_at from when they were removed, so "when did this
// seat come back" was silently unanswerable. Verified against MySQL 8.0.33 both ways.
//
// The same case also pins the deliberate asymmetry: an admin who is re-added while STILL
// active keeps their role, while a removed member comes back as an ordinary member.
func TestReactivationResetsRoleAndTimestamps(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "m1")

	// Promote, then remove.
	w := doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID+"/members/m1/role",
		ownerTok, map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"m1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	removed, err := p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)
	require.NotNil(t, removed)
	require.Equal(t, MemberStatusRemoved, removed.Status)
	removedAt := removed.UpdatedAt

	// Backdate the removal so a stalled updated_at is unmistakable rather than a
	// sub-millisecond difference.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET updated_at = ? WHERE project_id = ? AND uid = ?",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), created.ProjectID, "m1").Exec()
	require.NoError(t, err)

	// Re-add.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"m1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	back, err := p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)
	require.NotNil(t, back)
	assert.Equal(t, MemberStatusActive, back.Status)
	assert.Equal(t, RoleCommon, back.Role, "a removed member comes back as an ordinary member")
	assert.True(t, back.UpdatedAt.After(time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)),
		"reactivation must move updated_at; got %s (the ON DUPLICATE KEY UPDATE clause "+
			"testing the old status must precede `status = 1`)", back.UpdatedAt)
	_ = removedAt

	// And re-adding a STILL-ACTIVE admin must not demote them.
	w = doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID+"/members/m1/role",
		ownerTok, map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"m1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	stillAdmin, err := p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)
	assert.Equal(t, RoleAdmin, stillAdmin.Role,
		"re-adding an active admin must not silently demote them")
}
