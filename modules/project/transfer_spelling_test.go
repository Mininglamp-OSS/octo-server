package project

// What this file pins:
//
//	An ACTIVE project must never end up with zero active owners, and identifier
//	drift must not be able to produce that state through any of the three doors
//	that can move the owner seat: leave, role change, and explicit transfer.
//
// # The defect it was written for
//
// Round 13 found the fifth instance of one class on this branch: an identifier whose
// COMPARISON was folded while the value that got WRITTEN stayed the caller's bytes.
// Here it is worse than a missed epoch bump — it leaves a project with zero active
// owners, which this module documents as unrecoverable in P0 (role change and disband
// are owner-only; a Space admin has read-only access), surfaced by
// project_ownerless_total and repaired by nothing.
//
// The original chain ran through a `transfer_to` argument on the leave and role-change
// doors: the last owner named a case variant of THEMSELVES, the folded seat check
// passed it, the byte-exact self-transfer guard did not catch it, the member row
// resolved case-insensitively onto the departing owner's own row, and the role UPDATE
// no-opped on its `role <> ?` predicate — active project, zero owners.
//
// # Why the tests changed shape
//
// main's #887 removed `transfer_to` from both of those doors: the last owner is now
// refused with errLastOwnerMustTransfer and has to call the explicit transfer endpoint
// first. That deletes the chain above by construction rather than by guarding it — so
// these cases assert the doors' refusals and the end state, not the old argument.
//
// The invariant is deliberately spelled as "an owner survives" rather than as a
// specific error code: a future change that returns a different error while still
// bricking ownership would pass an error-code assertion and fail this one.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// activeOwners counts owner seats that are actually going to remain.
//
// `removing = 0` is part of the predicate, not decoration. The leave door does not
// close the seat inline — it marks it removing and hands the close to the async
// cascade — so a count that ignored that flag would read 1 for a project whose only
// owner is already on the way out, and the leave-door case would look green while the
// zero-owner state was merely deferred.
func activeOwners(t *testing.T, projectID string) int {
	t.Helper()
	var n []int
	_, err := testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` "+
			"WHERE project_id = ? AND role = ? AND status = ? AND removing = 0",
		projectID, RoleOwner, MemberStatusActive).Load(&n)
	require.NoError(t, err)
	require.Len(t, n, 1)
	return n[0]
}

// roleOf reads one member's stored role, so a case can assert WHICH row moved rather
// than only how many owners are left.
func roleOf(t *testing.T, projectID, uid string) int {
	t.Helper()
	var role []int
	_, err := testCtx.DB().SelectBySql(
		"SELECT role FROM `octo_project_member` "+
			"WHERE project_id = ? AND uid = ? AND status = ?",
		projectID, uid, MemberStatusActive).Load(&role)
	require.NoError(t, err)
	require.Len(t, role, 1, "expected exactly one active seat for %s", uid)
	return role[0]
}

// TestLastOwnerCannotLeaveWithoutTransferring covers the leave door.
func TestLastOwnerCannotLeaveWithoutTransferring(t *testing.T) {
	srv, p := setup(t)

	const owner = "tfOwner"
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, owner)
	seedSpaceMember(t, spaceA, owner, 2, 1)

	proj := createProjectVia(t, srv, spaceA, ownerToken, "transfer-self-leave")
	require.Equal(t, 1, activeOwners(t, proj.ProjectID), "fixture must start with one owner")

	err := p.leaveProject(proj.ProjectID, spaceA, owner)
	assert.ErrorIs(t, err, errLastOwnerMustTransfer,
		"the last owner must not be able to walk out of the project: the door has no "+
			"successor argument any more, so the only safe answer is a refusal")
	assert.Equal(t, 1, activeOwners(t, proj.ProjectID),
		"the project must still have an active owner. Zero owners is the state this module "+
			"documents as unrecoverable — role change and disband are owner-only and a Space "+
			"admin has read-only access, so nothing in-product can repair it.")
}

// TestLastOwnerCannotBeDemoted covers the role-change door, which is the second way
// the only owner seat can stop being an owner seat.
func TestLastOwnerCannotBeDemoted(t *testing.T) {
	srv, p := setup(t)

	const owner = "tfRoleOwner"
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, owner)
	seedSpaceMember(t, spaceA, owner, 2, 1)

	proj := createProjectVia(t, srv, spaceA, ownerToken, "transfer-self-role")
	require.Equal(t, 1, activeOwners(t, proj.ProjectID))

	// The refusal comes from canActOnTargetRole rather than from the last-owner guard:
	// no ordinary member mutation may act on an Owner target at all, so the request is
	// turned away one step earlier. Asserted as "an error, and the owner survives"
	// rather than as a specific code on purpose — which of the two guards answers is an
	// implementation detail, while the end state is the invariant, and a change that
	// swapped the code while bricking ownership would pass a code assertion.
	_, err := p.changeMemberRole(proj.ProjectID, spaceA, owner, owner, RoleCommon)
	assert.Error(t, err,
		"demoting the last owner is the same end state as letting them leave, and must be "+
			"refused for the same reason")
	assert.Equal(t, 1, activeOwners(t, proj.ProjectID),
		"the project must still have an active owner")
	assert.Equal(t, RoleOwner, roleOf(t, proj.ProjectID, owner),
		"and it must still be the same person — a refusal that demoted the row anyway "+
			"would satisfy a count taken over a different project")
}

// TestSelfTransferIsRefusedWhenTheSuccessorSpellingDrifts is the surviving half of the
// round-13 defect, on the door that still takes a successor.
//
// The guard at the top of transferProjectOwnerOnce is a BYTE-EXACT `successorUID ==
// actorUID`, so a case variant of the actor walks past it. What refuses the request is
// that the member row resolves case-insensitively onto the actor's own seat, which is
// already an owner. That is a different barrier from the one round 13 fixed, and it is
// the one this case exists to keep: if it is ever relaxed, the transfer would promote
// the departing owner's own row, no-op on `role <> ?`, and demote them in the next
// statement — the zero-owner state, reached through the door built to prevent it.
func TestSelfTransferIsRefusedWhenTheSuccessorSpellingDrifts(t *testing.T) {
	srv, p := setup(t)

	const owner = "tfActorDrift"
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, owner)
	seedSpaceMember(t, spaceA, owner, 2, 1)

	proj := createProjectVia(t, srv, spaceA, ownerToken, "transfer-actor-drift")
	require.Equal(t, 1, activeOwners(t, proj.ProjectID))

	self := strings.ToUpper(owner)
	require.NotEqual(t, owner, self)

	err := p.transferProjectOwner(proj.ProjectID, spaceA, owner, self)
	assert.Error(t, err,
		"naming a case variant of YOURSELF is still naming yourself: the database resolves "+
			"both spellings onto one row, so the transfer has nowhere to go")
	assert.Equal(t, 1, activeOwners(t, proj.ProjectID),
		"the project must still have an active owner")
	assert.Equal(t, RoleOwner, roleOf(t, proj.ProjectID, owner),
		"and the actor must still hold it: the failure mode this guards is a promotion "+
			"that no-ops followed by a demotion that does not")
}

// TestTransferToARealSuccessorLeavesExactlyOneOwner is the other half: the door must
// keep WORKING, and it must land the owner seat on the successor and nowhere else.
//
// Without it the case above could be "fixed" by refusing every transfer, which would
// make the last owner unable to leave at all.
func TestTransferToARealSuccessorLeavesExactlyOneOwner(t *testing.T) {
	srv, p := setup(t)

	const (
		owner     = "tfLeaver"
		successor = "tfHeirMixed"
	)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, owner)
	seedSpaceMember(t, spaceA, owner, 2, 1)
	seedUser(t, successor)
	seedSpaceMember(t, spaceA, successor, 0, 1)

	proj := createProjectVia(t, srv, spaceA, ownerToken, "transfer-real-heir")
	admitted, err := p.addOneMember(proj.ProjectID, spaceA, owner, successor)
	require.NoError(t, err)
	require.True(t, admitted)

	require.NoError(t, p.transferProjectOwner(proj.ProjectID, spaceA, owner, successor))

	assert.Equal(t, RoleOwner, roleOf(t, proj.ProjectID, successor),
		"the successor's stored row must actually be owner now")
	assert.Equal(t, RoleAdmin, roleOf(t, proj.ProjectID, owner),
		"and the former owner must be demoted to admin rather than left as a second owner")
	assert.Equal(t, 1, activeOwners(t, proj.ProjectID),
		"exactly one owner: two would make the last-owner guard unreachable on the next leave")
}
