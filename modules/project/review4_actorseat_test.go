package project

// yujiawei Q6 + round-1 review #3: the actor's own Space seat is never revalidated inside
// the write transaction, and updateProject/disbandProject never re-read the actor's project
// role under the lock at all. The middleware's Space gate runs before the transaction and
// through the shared 60s cache, so an actor whose Space seat is gone (cascade not caught
// up, or the two-failure cache branch) can still write. The target already gets an
// in-transaction check; the actor gets the same.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWritePathsRevalidateTheActorSpaceSeatInTx drives ALL SIX privileged writes directly at the
// service layer with an actor who holds a PROJECT seat but no SPACE seat — the exact state a
// Space removal leaves behind until its async cascade closes the project seat. Every path must
// refuse; before the fixes they all succeeded.
//
// The count in the name of the guarantee matters: round 1 wired five of the six and the
// omission was recorded nowhere, so keep this list exhaustive against the callers of
// requireActorSpaceSeatTx.
func TestWritePathsRevalidateTheActorSpaceSeatInTx(t *testing.T) {
	srv, p := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "admin9")
	_ = ownerTok

	// Give admin9 an admin project role, then remove their SPACE seat without running the
	// cascade: the project seat stays active, the Space seat is gone.
	w := doJSON(t, srv, "PUT", "/v1/projects/"+created.ProjectID+"/members/admin9/role",
		tokens["owner1"], map[string]any{"role": RoleAdmin})
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	removeSpaceMember(t, spaceA, "admin9")
	projSeat, err := p.db.queryMember(created.ProjectID, "admin9")
	require.NoError(t, err)
	require.Equal(t, MemberStatusActive, projSeat.Status, "precondition: project seat still active")

	spaceID := created.SpaceID

	// updateProject
	desc := "should not land"
	_, err = p.updateProject(created.ProjectID, "admin9", spaceID, updateReq{Description: &desc})
	assert.ErrorIs(t, err, errNotSpaceMember, "updateProject must refuse an actor without a Space seat")

	// disbandProject
	_, dErr := p.disbandProject(created.ProjectID, "admin9", spaceID)
	assert.ErrorIs(t, dErr, errNotSpaceMember,
		"disbandProject must refuse an actor without a Space seat")

	// removeMember (acting on another member)
	_, rErr := p.removeMember(created.ProjectID, spaceID, "admin9", "owner1")
	assert.ErrorIs(t, rErr, errNotSpaceMember,
		"removeMember must refuse an actor without a Space seat")

	// leaveProject
	_, lErr := p.leaveProject(created.ProjectID, spaceID, "admin9", "")
	assert.ErrorIs(t, lErr, errNotSpaceMember,
		"leaveProject must refuse an actor without a Space seat")

	// changeMemberRole (demote someone)
	_, _, cErr := p.changeMemberRole(created.ProjectID, spaceID, "admin9", "owner1", RoleCommon, "")
	assert.ErrorIs(t, cErr, errNotSpaceMember,
		"changeMemberRole must refuse an actor without a Space seat")

	// addMember — the sixth path, and the one this guard used to skip while claiming to
	// drive "each privileged write" (PR #841 round 2, yujiawei P1-1 / Jerry-Xin B-1). Its
	// exposure is the widest of the six: addMembers runs one transaction per target and
	// breaks the batch only on errPermissionDenied / errProjectGone, so an actor whose Space
	// seat closes mid-batch would otherwise have every remaining uid of a 200-uid batch
	// admitted and audited under them.
	seedUser(t, "fresh1")
	seedSpaceMember(t, spaceA, "fresh1", 0, 1)
	admitted, aErr := p.addOneMember(created.ProjectID, spaceID, "admin9", "fresh1")
	assert.ErrorIs(t, aErr, errNotSpaceMember,
		"addOneMember must refuse an actor without a Space seat")
	assert.False(t, admitted, "no seat may be admitted by an actor without a Space seat")
	freshSeat, fErr := p.db.queryMember(created.ProjectID, "fresh1")
	require.NoError(t, fErr)
	assert.Nil(t, freshSeat, "the target must not have gained a project seat")

	// Nothing may have changed.
	row, err := p.db.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, StatusNormal, row.Status, "no write may have landed")
	after, qErr := p.db.queryByProjectID(created.ProjectID)
	require.NoError(t, qErr)
	assert.NotEqual(t, "should not land", after.Description, "no profile write may have landed")
}
