package project

// What this file pins:
//
//	An ownership transfer must resolve `transfer_to` to the spelling the seat set
//	returned, and must refuse a successor who IS the departing owner under the same
//	collation the database compares with.
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
// The chain, with the last owner stored as `tfOwner` and the request naming `TFOWNER`:
//
//  1. lockSeatsTx always add()s the actor, so heldSeats carries the leaver's own stored
//     Space seat — leaving a PROJECT does not touch the SPACE seat.
//  2. FoldedHas(heldSeats, "TFOWNER") folds the needle onto that key and passes.
//  3. promoteSuccessorTx's self-transfer guard was byte-exact, so "TFOWNER" != "tfOwner"
//     slipped through it.
//  4. queryMemberTx resolves under octo_project_member's utf8mb4_general_ci — which is
//     case-insensitive — onto the departing owner's OWN active row.
//  5. updateMemberRoleTx no-ops on its `role <> ?` predicate, because that row is
//     already the owner. The caller discarded the affected-rows result, so the no-op
//     was invisible.
//  6. The leave then closes that seat and commits: active project, zero owners.
//
// This is a regression THIS branch introduced: at the merge base the guard was an
// exact-match `heldSeats[transferTo]`, which refused the request cleanly. Widening it
// to FoldedHas fixed a real fail-CLOSED bug (a legitimate successor whose stored
// spelling differed in case was refused) and opened this fail-OPEN one, because the
// widening stopped at the comparison and never reached the write.
//
// # Why the assertion is "an owner survives" rather than a specific error
//
// The refusal code is worth pinning too, but the invariant that matters is the end
// state. A future change that returns a different error while still bricking ownership
// would pass an error-code assertion and fail this one.

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

// TestLastOwnerCannotTransferToACaseVariantOfThemselves covers the leave door.
func TestLastOwnerCannotTransferToACaseVariantOfThemselves(t *testing.T) {
	srv, p := setup(t)

	const owner = "tfOwner"
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, owner)
	seedSpaceMember(t, spaceA, owner, 2, 1)

	proj := createProjectVia(t, srv, spaceA, ownerToken, "transfer-self-leave")
	require.Equal(t, 1, activeOwners(t, proj.ProjectID), "fixture must start with one owner")

	self := strings.ToUpper(owner)
	require.NotEqual(t, owner, self)

	_, err := p.leaveProject(proj.ProjectID, spaceA, owner, self)
	assert.ErrorIs(t, err, errLastOwnerMustTransfer,
		"naming a case variant of YOURSELF is still naming yourself: the successor check "+
			"must compare under the same collation the database resolves the member row with, "+
			"or the promotion targets the departing owner's own row and no-ops")
	assert.Equal(t, 1, activeOwners(t, proj.ProjectID),
		"the project must still have an active owner. Zero owners is the state this module "+
			"documents as unrecoverable — role change and disband are owner-only and a Space "+
			"admin has read-only access, so nothing in-product can repair it.")
}

// TestSelfTransferIsRefusedWhenTHEACTORsSpellingDrifts is what makes the folded
// self-transfer guard load-bearing.
//
// Written after mutation testing showed it was not: reverting that guard to a
// byte-exact compare left the two cases above GREEN, because they pass the actor's
// stored spelling, so the resolved successor and the departing uid are byte-equal
// anyway. The guard only earns its fold when the DEPARTING side is the drifted one —
// the uid arrives from the auth token, which is not the same source as the seat row
// and need not agree with it byte for byte.
//
// Recorded because a guard whose mutation passes is a guard that is not being tested,
// and this branch has shipped three of those.
func TestSelfTransferIsRefusedWhenTHEACTORsSpellingDrifts(t *testing.T) {
	srv, p := setup(t)

	const owner = "tfActorDrift"
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, owner)
	seedSpaceMember(t, spaceA, owner, 2, 1)

	proj := createProjectVia(t, srv, spaceA, ownerToken, "transfer-actor-drift")
	require.Equal(t, 1, activeOwners(t, proj.ProjectID))

	// The actor names themselves with the STORED spelling while their own uid arrives
	// drifted: still a self-transfer, and the compare has to see that.
	_, err := p.leaveProject(proj.ProjectID, spaceA, strings.ToUpper(owner), owner)
	assert.ErrorIs(t, err, errLastOwnerMustTransfer,
		"a self-transfer is a self-transfer whichever side carries the drift — the compare "+
			"must fold both, because the database resolves both onto one row")
	assert.Equal(t, 1, activeOwners(t, proj.ProjectID),
		"the project must still have an active owner")
}

// TestDemotingTheLastOwnerViaACaseVariantIsRefused covers the role-change door, which
// is the same widening applied to a second call site.
func TestDemotingTheLastOwnerViaACaseVariantIsRefused(t *testing.T) {
	srv, p := setup(t)

	const owner = "tfRoleOwner"
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, owner)
	seedSpaceMember(t, spaceA, owner, 2, 1)

	proj := createProjectVia(t, srv, spaceA, ownerToken, "transfer-self-role")
	require.Equal(t, 1, activeOwners(t, proj.ProjectID))

	self := strings.ToUpper(owner)
	_, _, err := p.changeMemberRole(proj.ProjectID, spaceA, owner, owner, RoleCommon, self)
	assert.ErrorIs(t, err, errLastOwnerMustTransfer,
		"demoting the last owner while naming a case variant of them as the successor is a "+
			"self-transfer, and must be refused for the same reason the leave door refuses it")
	assert.Equal(t, 1, activeOwners(t, proj.ProjectID),
		"the project must still have an active owner")
}

// TestTransferToACaseVariantOfARealSuccessorPromotesTheStoredRow is the other half:
// the fold must keep WORKING for a genuine successor, and the promotion must land on
// the stored row rather than creating or missing one.
//
// Without this the P1 could be "fixed" by reverting to the exact match, which
// reintroduces the fail-CLOSED bug the fold was widened to solve — a legitimate
// successor refused because the caller typed their uid in a different case.
func TestTransferToACaseVariantOfARealSuccessorPromotesTheStoredRow(t *testing.T) {
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

	promoted, err := p.leaveProject(proj.ProjectID, spaceA, owner, strings.ToUpper(successor))
	require.NoError(t, err, "a genuine successor named in a different case must still be "+
		"accepted — that is what widening the guard to a folded lookup was for")
	assert.Equal(t, successor, promoted,
		"the reported successor must be the STORED spelling: it is written to the audit "+
			"record and used as the member-cache invalidation key, and a key built from the "+
			"caller's bytes invalidates an entry nobody reads")

	var role []int
	_, err = testCtx.DB().SelectBySql(
		"SELECT role FROM `octo_project_member` WHERE project_id = ? AND uid = ? AND status = ?",
		proj.ProjectID, successor, MemberStatusActive).Load(&role)
	require.NoError(t, err)
	require.Len(t, role, 1)
	assert.Equal(t, RoleOwner, role[0], "the stored successor row must actually be owner now")
	assert.Equal(t, 1, activeOwners(t, proj.ProjectID))
}
