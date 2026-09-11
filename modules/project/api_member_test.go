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
			ownerTok, addMembersPayload(uids...))
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		var outcomes []memberOutcome
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
		for _, o := range outcomes {
			require.True(t, o.OK, "seeding member %s failed: %s", o.UID, o.Reason)
		}
	}
	return ownerTok, tokens, created
}
func addMembersPayload(uids ...string) map[string]any {
	members := make([]map[string]any, 0, len(uids))
	for _, uid := range uids {
		members = append(members, map[string]any{"uid": uid, "role": RoleCommon})
	}
	return map[string]any{"members": members}
}

func addMemberWithRolePayload(uid string, role int) map[string]any {
	return map[string]any{
		"members": []map[string]any{{"uid": uid, "role": role}},
	}
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
		ownerTok, addMembersPayload("nobody"))
	assertProjectErrorCode(t, w, "err.server.project.member_not_space_member")

	// A user whose Space seat was removed.
	seedUser(t, "exmember")
	seedSpaceMember(t, spaceA, "exmember", 0, 1)
	removeSpaceMember(t, spaceA, "exmember")
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, addMembersPayload("exmember"))
	assertProjectErrorCode(t, w, "err.server.project.member_not_space_member")
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
		ownerTok, addMembersPayload("bOnly"))
	assertProjectErrorCode(t, w, "err.server.project.member_not_space_member")
}

// TestAddRejectsWhenSpaceIsBanned covers the authorization side of the banned-Space
// axis. CheckMembership requires space.status=1, so admission into a banned Space's
// project is impossible — and it is impossible at BOTH layers:
//
//   - the middleware's Space gate refuses the request outright. Since the round-1 review
//     merge (Q3), the gate on /v1/projects/* reads the database on every request (space
//     membership and role answer the same predicate, so they are one read), so a ban takes
//     effect on the next request with no TTL window at all. The folded not-found response
//     is what that gate answers with.
//   - the transactional I1 check in the service layer refuses the TARGET regardless of any
//     cache state — pinned by TestI1CheckRunsInsideTheWriteTransaction, which asserts the
//     predicate fails for space.status=2, and by TestAddRejectsMemberOfAnotherSpace for the
//     errNotSpaceMember mapping.
//
// The middleware window this test used to exercise (a warm cache admitting the caller for
// up to one TTL while the transactional check refused the target) is gone by design: the
// gate got stricter, not looser. What P0 must guarantee is that no new project seat can be
// created in a banned Space, and both layers now guarantee it.
//
// Note this is the OPPOSITE of what cleanup does with a banned Space: cleanup uses
// CheckMembershipForCleanup and SKIPS, because the seat is still real. Authorization and
// cleanup deliberately answer differently here, and confusing the two would tear every
// project membership apart the moment a Space is banned.
func TestAddRejectsWhenSpaceIsBanned(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	seedUser(t, "later")
	seedSpaceMember(t, spaceA, "later", 0, 1)
	setSpaceStatus(t, spaceA, 2)

	// The middleware gate reads the database every request since the Q3 merge, so a ban
	// refuses the NEXT request outright with the folded not-found envelope.
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, addMembersPayload("later"))
	assertProjectErrorCode(t, w, "err.server.project.not_found")

	// The transactional layer independently refuses the TARGET — exercised directly, because
	// the middleware gate above now stops the request before the handler runs. This is the
	// guarantee that holds even if a future change reintroduces a cached caller gate.
	refs, refsErr := p.db.resolveSpaceSeatIDs(spaceA, []string{"later"})
	require.NoError(t, refsErr)
	tx, txErr := p.db.session.Begin()
	require.NoError(t, txErr)
	held, err := p.db.lockSpaceSeatsTx(tx, spaceA, []string{"later"}, refs)
	require.NoError(t, err)
	assert.False(t, held["later"],
		"the authorization predicate must fail for a banned Space, cache or no cache")
	require.NoError(t, tx.Rollback())
}

// ---------- member_epoch ----------

// TestMemberEpochStrictlyIncreasesOnEveryWrite covers add / remove / role change /
// leave / disband. The Space-cascade path is covered in the cascade test.
func TestMemberEpochStrictlyIncreasesOnEveryWrite(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "m1", "m2")

	// The atomic add during seeding moved the epoch once for the whole batch.
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
		ownerTok, addMembersPayload("m1"))
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
		Role: RoleCommon, InviteUID: "owner1", CreatedAt: now, JoinedAt: now, UpdatedAt: now,
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

	// Find the project under test by semantic ID; the list can contain any
	// number of explicitly created Projects.
	list := doJSON(t, srv, http.MethodGet, "/v1/space/"+spaceA+"/projects", ownerTok, nil)
	require.Equal(t, http.StatusOK, list.Code)
	var listResp []*Resp
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &listResp))
	var listed *Resp
	for _, item := range listResp {
		if item.ProjectID == created.ProjectID {
			listed = item
			break
		}
	}
	require.NotNil(t, listed)
	assert.Equal(t, resp.MemberEpoch, listed.MemberEpoch)
	assert.Equal(t, RoleOwner, listed.MyRole)
	assert.Equal(t, 2, listed.MemberCount)
	assert.Equal(t, 2, listed.HumanMemberCount, "both members are human here")
	assert.Zero(t, listed.AgentMemberCount)
}

// ---------- permission matrix ----------

// TestAdminMayManagePeerAdminsButNotOwner pins the transitive protection:
// admins manage every non-owner seat, including peer admins.
func TestAdminMayManagePeerAdminsButNotOwner(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "admin1", "admin2", "plain1")
	for _, uid := range []string{"admin1", "admin2"} {
		w := doJSON(t, srv, http.MethodPut,
			"/v1/projects/"+created.ProjectID+"/members/"+uid+"/role", ownerTok,
			map[string]any{"role": RoleAdmin})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	}

	// admin1 may remove a peer admin.
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		tokens["admin1"], map[string]any{"uids": []string{"admin2"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	require.Len(t, outcomes, 1)
	assert.True(t, outcomes[0].OK)

	// admin1 removing the owner is refused.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		tokens["admin1"], map[string]any{"uids": []string{"owner1"}})
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	assert.Equal(t, reasonPermissionDenied, outcomes[0].Reason)

	// Admins may change non-owner roles.
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/plain1/role", tokens["admin1"],
		map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// But admin1 removing an ordinary member is allowed.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		tokens["admin1"], map[string]any{"uids": []string{"plain1"}})
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
	assert.True(t, outcomes[0].OK, "reason: %s", outcomes[0].Reason)
}

// TestOwnerTransferIsDedicatedAndAtomic pins the ownership cutover contract.
func TestOwnerTransferIsDedicatedAndAtomic(t *testing.T) {
	srv, p := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "successor1")

	// Owner cannot leave or demote through the ordinary role paths.
	leaveErr := p.leaveProject(created.ProjectID, spaceA, "owner1")
	assert.ErrorIs(t, leaveErr, errLastOwnerMustTransfer,
		"the service guard must protect the sole Owner even if middleware is bypassed")
	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave", ownerTok, nil)
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")
	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/owner1/role", ownerTok,
		map[string]any{"role": RoleCommon})
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")

	w = doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID+"/owner", ownerTok,
		map[string]any{"uid": "successor1"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	successor, err := p.db.queryMember(created.ProjectID, "successor1")
	require.NoError(t, err)
	require.NotNil(t, successor)
	assert.Equal(t, RoleOwner, successor.Role)
	former, err := p.db.queryMember(created.ProjectID, "owner1")
	require.NoError(t, err)
	require.NotNil(t, former)
	assert.Equal(t, RoleAdmin, former.Role)

	// The former owner remains an active member and can leave as an admin.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave", tokens["owner1"], nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

// TestOwnerCannotRemoveThemselvesViaRemoveEndpoint pins that self-removal must be
// refused by the batch remove path. Owners must use the dedicated owner-transfer
// endpoint before leaving; allowing removal here would make the project unmanageable.
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

// TestRemovedMemberIsDeniedOnTheVeryNextRequest proves that a member removed
// from a Project is denied immediately. The response uses the same semantic 404
// envelope as an unknown Project, so revocation cannot leave a permission window
// or disclose membership history.
func TestRemovedMemberIsDeniedOnTheVeryNextRequest(t *testing.T) {
	srv, _ := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "m1")

	// Warm the read path before removing the member.
	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", tokens["m1"], nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Remove them, then immediately re-read. No sleep, no TTL expiry.
	w = doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		ownerTok, map[string]any{"uids": []string{"m1"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", tokens["m1"], nil)
	assertProjectErrorCode(t, w, "err.server.project.not_found")
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, http.StatusNotFound, decodeProjectEnvelope(t, w.Body.Bytes()).Error.HTTPStatus)
}

// TestNonMemberCannotReadListedProject covers the single Project-member read boundary.
func TestNonMemberCannotReadListedProject(t *testing.T) {
	srv, _ := setup(t)
	_, _, created := projectWithMembers(t, srv)
	strangerTok := seedUser(t, "stranger")
	seedSpaceMember(t, spaceA, "stranger", 0, 1)

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, strangerTok, nil)
	assertProjectErrorCode(t, w, "err.server.project.not_found")
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, http.StatusNotFound, decodeProjectEnvelope(t, w.Body.Bytes()).Error.HTTPStatus)

	w = doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", strangerTok, nil)
	assertProjectErrorCode(t, w, "err.server.project.not_found")
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, http.StatusNotFound, decodeProjectEnvelope(t, w.Body.Bytes()).Error.HTTPStatus)
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

func TestAddMembersPersistsRequestedRoles(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	for _, uid := range []string{"admin-add", "member-add"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}

	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"members": []map[string]any{
			{"uid": "admin-add", "role": RoleAdmin},
			{"uid": "member-add", "role": RoleCommon},
		}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), "body: %s", w.Body.String())
	require.Len(t, outcomes, 2)
	for _, outcome := range outcomes {
		assert.True(t, outcome.OK, "adding %s failed: %s", outcome.UID, outcome.Reason)
	}

	admin, err := p.db.queryMember(created.ProjectID, "admin-add")
	require.NoError(t, err)
	require.NotNil(t, admin)
	assert.Equal(t, RoleAdmin, admin.Role)

	member, err := p.db.queryMember(created.ProjectID, "member-add")
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Equal(t, RoleCommon, member.Role)
}

// TestAddMembersRejectsExistingActiveRoleConflictAtomically covers the service-level
// role conflict that request normalization cannot see. A conflicting active seat must
// reject the whole batch, leaving both the old role and every new target unchanged.
func TestAddMembersRejectsExistingActiveRoleConflictAtomically(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	for _, uid := range []string{"existing", "new"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}

	require.Equal(t, http.StatusOK, doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerTok,
		addMemberWithRolePayload("existing", RoleAdmin)).Code)

	w := doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerTok,
		map[string]any{"members": []map[string]any{
			// Put the new target first so an implementation that mutates while
			// walking the batch would leave observable partial state.
			{"uid": "new", "role": RoleCommon},
			{"uid": "existing", "role": RoleCommon},
		}})
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assertProjectErrorCode(t, w, "err.server.project.member_role_conflict")

	existing, err := p.db.queryMember(created.ProjectID, "existing")
	require.NoError(t, err)
	require.NotNil(t, existing)
	assert.Equal(t, RoleAdmin, existing.Role,
		"the conflicting active member must retain the original role")
	added, err := p.db.queryMember(created.ProjectID, "new")
	require.NoError(t, err)
	assert.Nil(t, added, "a new target in the rejected batch must not be persisted")
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
		{RoleOwner, RoleOwner, false},
		{RoleOwner, RoleAdmin, true},
		{RoleOwner, RoleCommon, true},
		{RoleAdmin, RoleOwner, false},
		{RoleAdmin, RoleAdmin, true},
		{RoleAdmin, RoleCommon, true},
		{RoleCommon, RoleCommon, false},
		{roleNonMember, RoleCommon, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, canActOnTargetRole(tc.actor, tc.target),
			"actor=%d target=%d", tc.actor, tc.target)
	}
}

func TestNormalizeMemberAddsDeduplicatesAndRejectsConflicts(t *testing.T) {
	got, err := normalizeMemberAdds([]memberAdd{
		{UID: " a ", Role: RoleCommon},
		{UID: "a", Role: RoleCommon},
		{UID: "b", Role: RoleAdmin},
	})
	require.NoError(t, err)
	assert.Equal(t, []memberAdd{{UID: "a", Role: RoleCommon}, {UID: "b", Role: RoleAdmin}}, got)

	_, err = normalizeMemberAdds([]memberAdd{
		{UID: "a", Role: RoleCommon},
		{UID: "a", Role: RoleAdmin},
	})
	assert.ErrorIs(t, err, errMemberRoleConflict)
	_, err = normalizeMemberAdds([]memberAdd{{UID: "owner", Role: RoleOwner}})
	assert.ErrorIs(t, err, errMemberRoleInvalid)
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

	// Two-phase removal (D4): drive the cascade before reading the end state.
	drainRemovalCascade(t, p)
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
		ownerTok, addMembersPayload("m1"))
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
		ownerTok, addMemberWithRolePayload("m1", RoleAdmin))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	stillAdmin, err := p.db.queryMember(created.ProjectID, "m1")
	require.NoError(t, err)
	assert.Equal(t, RoleAdmin, stillAdmin.Role,
		"re-adding an active admin must not silently demote them")
}

func TestReactivationWhileRemovalIsPendingUsesRequestedRole(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv, "pending-role")

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/pending-role/role", ownerTok,
		map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/remove", ownerTok,
		map[string]any{"uids": []string{"pending-role"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	pending, err := p.db.queryMember(created.ProjectID, "pending-role")
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, MemberStatusActive, pending.Status)
	require.Equal(t, 1, pending.Removing)

	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerTok,
		map[string]any{"members": []map[string]any{
			{"uid": "pending-role", "role": RoleCommon},
		}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	rejoined, err := p.db.queryMember(created.ProjectID, "pending-role")
	require.NoError(t, err)
	require.NotNil(t, rejoined)
	assert.Equal(t, MemberStatusActive, rejoined.Status)
	assert.Zero(t, rejoined.Removing)
	assert.Equal(t, RoleCommon, rejoined.Role,
		"re-admission of a closing seat must apply the requested role")
}

func TestReactivationWhileRemovalIsPendingCountsAgainstMemberQuota(t *testing.T) {
	srv, p := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "pending-quota")
	_ = tokens

	_, err := testCtx.DB().UpdateBySql(
		"UPDATE octo_project SET max_members = 2 WHERE project_id = ?", created.ProjectID,
	).Exec()
	require.NoError(t, err)

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/pending-quota/role", ownerTok,
		map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/remove", ownerTok,
		map[string]any{"uids": []string{"pending-quota"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	seedUser(t, "quota-new")
	seedSpaceMember(t, spaceA, "quota-new", 0, 1)
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerTok,
		addMembersPayload("quota-new"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerTok,
		addMembersPayload("pending-quota"))
	assertProjectErrorCode(t, w, "err.server.project.quota_members")

	pending, err := p.db.queryMember(created.ProjectID, "pending-quota")
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, MemberStatusActive, pending.Status)
	assert.Equal(t, 1, pending.Removing,
		"quota refusal must leave the pending removal intact")
	assert.Equal(t, RoleAdmin, pending.Role,
		"quota refusal must not change the pending member role")
	active, err := p.db.countActiveMembers(created.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, 2, active,
		"closing seats that would be reactivated count toward the cap")
}

// TestLegacyMemberWriterCanInsertAndRejoinWithNullableJoinedAt exercises the
// expand half of the rolling contract against the real MySQL table. An old
// binary omits joined_at from its explicit INSERT list; the new reader must
// expose created_at while the row is still legacy-shaped, and a later
// re-admission through the new writer must persist a real joined_at value.
func TestLegacyMemberWriterCanInsertAndRejoinWithNullableJoinedAt(t *testing.T) {
	srv, p := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv)
	legacyUID := "legacy-writer"
	legacyCreatedAt := time.Date(2022, 2, 3, 4, 5, 6, 7000000, time.UTC)
	seedUser(t, legacyUID)
	seedSpaceMember(t, spaceA, legacyUID, 0, 1)

	// This is the merge-base writer shape: removing has a schema default, and
	// joined_at does not exist in the INSERT column list.
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO octo_project_member "+
			"(project_id, uid, space_id, role, status, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		created.ProjectID, legacyUID, spaceA, RoleCommon, MemberStatusActive,
		"owner1", legacyCreatedAt, legacyCreatedAt,
	).Exec()
	require.NoError(t, err, "an old binary must still insert while joined_at is nullable")

	w := doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/members/"+legacyUID, ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "legacy row must be readable: %s", w.Body.String())
	var legacyResp MemberResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &legacyResp))
	assert.Equal(t, formatTime(legacyCreatedAt), legacyResp.JoinedAt,
		"new readers must fall back to created_at for a legacy NULL joined_at")

	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{legacyUID}})
	require.Equal(t, http.StatusOK, w.Code, "remove legacy row: %s", w.Body.String())
	drainRemovalCascade(t, p)

	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		addMembersPayload(legacyUID))
	require.Equal(t, http.StatusOK, w.Code, "re-admit legacy row: %s", w.Body.String())

	var stored struct {
		CreatedAt time.Time `db:"created_at"`
		JoinedAt  time.Time `db:"joined_at"`
	}
	_, err = testCtx.DB().SelectBySql(
		"SELECT created_at, joined_at FROM octo_project_member WHERE project_id = ? AND uid = ?",
		created.ProjectID, legacyUID,
	).Load(&stored)
	require.NoError(t, err)
	assert.Equal(t, legacyCreatedAt, stored.CreatedAt,
		"re-admission must preserve the first-ever created_at")
	assert.True(t, stored.JoinedAt.After(legacyCreatedAt),
		"the new writer must replace the legacy NULL with the current round timestamp")
}

// TestMemberJoinedAtTracksMembershipRounds pins the distinction between the
// first-ever row timestamp and the current membership round timestamp.
//
// A new seat starts both clocks together. Role changes and idempotent admission
// do not start a new round. A removed or closing seat rejoining does, while its
// created_at remains the first-ever timestamp.
func TestMemberJoinedAtTracksMembershipRounds(t *testing.T) {
	srv, p := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv, "round-member")

	member, err := p.db.queryMember(created.ProjectID, "round-member")
	require.NoError(t, err)
	require.NotNil(t, member)
	require.False(t, member.CreatedAt.IsZero())
	require.False(t, member.JoinedAt.IsZero())
	assert.Equal(t, member.CreatedAt, member.JoinedAt,
		"a first admission starts created_at and joined_at together")
	firstCreatedAt := member.CreatedAt
	firstJoinedAt := member.JoinedAt

	// A role adjustment is not a new membership round.
	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/round-member/role",
		ownerToken, map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	member, err = p.db.queryMember(created.ProjectID, "round-member")
	require.NoError(t, err)
	assert.Equal(t, firstJoinedAt, member.JoinedAt,
		"changing role must not change the membership-round timestamp")

	// Re-adding an already-active member is idempotent, including its timestamp.
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		addMemberWithRolePayload("round-member", RoleAdmin))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	member, err = p.db.queryMember(created.ProjectID, "round-member")
	require.NoError(t, err)
	assert.Equal(t, firstJoinedAt, member.JoinedAt,
		"idempotent admission must not change the membership-round timestamp")

	// A completed removal followed by admission starts a fresh membership round.
	// Backdate the old round so DATETIME(3) precision cannot make two fast writes
	// appear equal.
	backdatedJoinedAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET joined_at = ? WHERE project_id = ? AND uid = ?",
		backdatedJoinedAt, created.ProjectID, "round-member").Exec()
	require.NoError(t, err)
	firstJoinedAt = backdatedJoinedAt
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{"round-member"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	drainRemovalCascade(t, p)
	removed, err := p.db.queryMember(created.ProjectID, "round-member")
	require.NoError(t, err)
	require.NotNil(t, removed)
	assert.Equal(t, MemberStatusRemoved, removed.Status)
	assert.Equal(t, backdatedJoinedAt, removed.JoinedAt,
		"removing a member must not overwrite the previous membership-round timestamp")
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		addMembersPayload("round-member"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	member, err = p.db.queryMember(created.ProjectID, "round-member")
	require.NoError(t, err)
	assert.Equal(t, firstCreatedAt, member.CreatedAt,
		"rejoining must preserve the first-ever row timestamp")
	assert.True(t, member.JoinedAt.After(firstJoinedAt),
		"rejoining after removal must refresh joined_at: old=%s new=%s",
		firstJoinedAt, member.JoinedAt)

	// A re-admission that cancels an in-flight removal is also a new round.
	createdBeforePending := member.CreatedAt
	backdatedPendingJoinedAt := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET joined_at = ? WHERE project_id = ? AND uid = ?",
		backdatedPendingJoinedAt, created.ProjectID, "round-member").Exec()
	require.NoError(t, err)
	joinedBeforePending := backdatedPendingJoinedAt

	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{"round-member"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	pending, err := p.db.queryMember(created.ProjectID, "round-member")
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, MemberStatusActive, pending.Status)
	assert.Equal(t, backdatedPendingJoinedAt, pending.JoinedAt,
		"a closing seat still belongs to its previous membership round until re-admission")
	require.Equal(t, 1, pending.Removing)
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		addMembersPayload("round-member"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	member, err = p.db.queryMember(created.ProjectID, "round-member")
	require.NoError(t, err)
	assert.Equal(t, createdBeforePending, member.CreatedAt)
	assert.True(t, member.JoinedAt.After(joinedBeforePending),
		"rejoining a closing seat must refresh joined_at: old=%s new=%s",
		joinedBeforePending, member.JoinedAt)
	assert.Zero(t, member.Removing)
}
