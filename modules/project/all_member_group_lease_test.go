package project

// The lease protocol, tested as a protocol.
//
// PR #855's fourth review named the shape behind five blocking findings: they all
// came out of ensureAllMemberGroup / provisionAllMemberGroup and the lease
// primitives beside them, and the invariant test added in the round before did
// not catch the last of them because it exercises the rebuild's INPUTS. What was
// left unguarded is the protocol AROUND it — which of two concurrent attempts
// becomes authoritative, and what happens to the one that loses.
//
// So these assert the protocol's two properties directly:
//
//	P1. Exactly one group ever carries the project's pointer.
//	P2. The group that carries it is the one built from the LATER roster —
//	    an attempt that outlived its own lease must not overwrite a successor's
//	    work with its stale snapshot.
//
// P2 is the one that did not hold. releaseAllMemberGroupProvision was fenced on
// the claimed deadline in the round before; setAllMemberGroupNo never got the
// other half of that fence, so an expired claimant returning late still wrote its
// pointer, because the only predicate was "the pointer is still empty".

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// leaseTestProject creates a project with no all-member group, ready for a
// provisioning attempt to claim.
func leaseTestProject(t *testing.T, p *Project, name string) *Model {
	t.Helper()
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	// Prepare the exact seat identity before opening the write transaction.
	// Bypass createProjectOnce's post-commit provisioning hook: these protocol
	// cases need the project pointer empty before the first explicit claim.
	in := createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: name,
		Discoverability: DiscoverabilitySpaceListed,
	}
	refs, err := p.db.resolveSpaceSeatIDs(in.SpaceID, createSeatUIDs(in))
	require.NoError(t, err)
	tx, err := p.db.session.Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	model, err := p.createProjectTxWithSeatRefs(tx, in, refs)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Empty(t, model.AllMemberGroupNo)
	return model
}

// allMemberGroupColumnOf reads the raw column, not queryAllMemberGroupNo.
//
// These cases drive the CAS protocol directly and never run a provisioner, so no
// `group` row exists and the join-backed lookup would correctly answer "no
// group". What is under test is which value the column holds.
func allMemberGroupColumnOf(t *testing.T, projectID string) string {
	t.Helper()
	var stored []string
	_, err := testCtx.DB().SelectBySql(
		"SELECT all_member_group_no FROM `octo_project` WHERE project_id = ?", projectID,
	).Load(&stored)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	return stored[0]
}

// TestLeaseProtocolTheLaterRosterWins drives the exact interleaving the lease
// exists to arbitrate, at the primitive level, with no stand-in hooks in the way.
//
//	A claims (deadline Ta) → A's CreateGroup runs long → Ta expires →
//	B claims (deadline Tb) → B builds Gb from the CURRENT roster →
//	A returns and writes back Ga → B writes back Gb
//
// Before the fix A's write succeeded, because the pointer was still empty. The
// project then named Ga — built from A's older snapshot, missing everyone added
// in the window — while Gb, which has the complete roster, survived as an
// ordinary project group. Two groups named after the project, and the one the
// project points at is the stale one.
//
// The lease constant's own comment sizes 2 minutes as "an order of magnitude
// above a normal create" precisely because the IM channel call is the part that
// can take minutes, so an attempt exceeding it is the case the lease was sized
// for rather than an exotic one.
func TestLeaseProtocolTheLaterRosterWins(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_lease_protocol")
	model := leaseTestProject(t, p, "lease-protocol")

	now := time.Now().UTC()
	claimedA, leaseA, err := p.db.claimAllMemberGroupProvision(model.ProjectID, now)
	require.NoError(t, err)
	require.True(t, claimedA, "A claims first")

	// A runs past its lease. B takes over.
	afterExpiry := now.Add(allMemberGroupLease + time.Minute)
	claimedB, leaseB, err := p.db.claimAllMemberGroupProvision(model.ProjectID, afterExpiry)
	require.NoError(t, err)
	require.True(t, claimedB, "an expired lease must be re-claimable")
	require.NotEqual(t, leaseA, leaseB)

	// A returns LAST and tries to register the group it built from the older
	// roster. This is the ordering the previous comment did not consider: it
	// argued only the case where the successor writes first.
	okA, err := p.db.setAllMemberGroupNo(model.ProjectID, "grp_stale_from_A", leaseA)
	require.NoError(t, err)
	assert.False(t, okA,
		"an attempt that outlived its own lease must NOT register its group. Its roster "+
			"snapshot predates the successor's, so letting it win makes the older view "+
			"authoritative and strands every member added in the window outside the group")

	okB, err := p.db.setAllMemberGroupNo(model.ProjectID, "grp_fresh_from_B", leaseB)
	require.NoError(t, err)
	assert.True(t, okB, "the live claimant's write must land")

	assert.Equal(t, "grp_fresh_from_B", allMemberGroupColumnOf(t, model.ProjectID),
		"P2 — the pointer names the group built from the LATER roster")
}

// TestLeaseProtocolAnUncontestedExpiredClaimStillWins is the other direction, and
// it is the reason the fence is an equality on the deadline rather than a
// freshness check.
//
// A lease that has expired but that NOBODY took over still holds the claimant's
// own deadline. So A's late write still succeeds when there is no successor —
// which is what should happen: A built a group, nothing else did, and refusing it
// would leave the project with no group and a real group orphaned beside it.
func TestLeaseProtocolAnUncontestedExpiredClaimStillWins(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_lease_uncontested")
	model := leaseTestProject(t, p, "lease-uncontested")

	now := time.Now().UTC()
	claimed, lease, err := p.db.claimAllMemberGroupProvision(model.ProjectID, now)
	require.NoError(t, err)
	require.True(t, claimed)

	// Time passes well past the lease; nobody else claims.
	ok, err := p.db.setAllMemberGroupNo(model.ProjectID, "grp_late_but_alone", lease)
	require.NoError(t, err)
	assert.True(t, ok,
		"an expired lease nobody took over still carries this claimant's deadline, so its "+
			"write must land — refusing it would leave the project with no group and a real "+
			"one orphaned beside it")
	assert.Equal(t, "grp_late_but_alone", allMemberGroupColumnOf(t, model.ProjectID))
}

// TestLeaseProtocolExactlyOneGroupEverCarriesThePointer is property P1, driven
// through the SERVICE rather than the primitives, so it covers the branch that
// absorbs the loser.
func TestLeaseProtocolExactlyOneGroupEverCarriesThePointer(t *testing.T) {
	_, p := setup(t)
	stub := stubAllMemberGroup(t, "grp_lease_p1")
	model := leaseTestProject(t, p, "lease-p1")

	// Two provisioning attempts back to back. The second must find nothing to
	// claim, so exactly one group is built and exactly one pointer written.
	p.provisionAllMemberGroup(model.ProjectID, spaceA, "u_owner", "lease-p1", nil)
	p.provisionAllMemberGroup(model.ProjectID, spaceA, "u_owner", "lease-p1", nil)

	assert.Equal(t, 1, stub.provisionCalls,
		"the second attempt must find the claim taken (or the pointer set) and do nothing")
	assert.Equal(t, "grp_lease_p1", allMemberGroupNoOf(t, model.ProjectID))
}
