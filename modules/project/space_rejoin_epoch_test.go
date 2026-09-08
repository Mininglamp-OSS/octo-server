package project

import (
	"testing"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Engine coverage for the REACTIVATION direction of the Space-member axis.
//
// Round 8 found this as the mirror image of the removal bump shipped in round 7,
// and the asymmetry was real: closing a seat moved member_epoch, reopening one did
// not. Since the Space conjunction landed, space_member.status IS part of the answer
// to "is this uid a member of this project" — in BOTH directions — so the invariant
// the writer guard states covers reopening too.
//
// The failure it produces is a stuck cached DENIAL, which the contract cares about
// because the peer caches the DECISION rather than only positive grants:
//
//  1. removal commits, epoch E -> E+1; the peer re-verifies and caches member:false at E+1
//  2. the member is re-added BEFORE the async cascade reaches them
//  3. the cascade then deliberately PRESERVES their project seats, seeing them back
//  4. `_verify` now answers member:true, but `epochs` still answers E+1 — so the peer's
//     staleness check AGREES with its own cached denial and a valid returning member
//     stays denied until some unrelated write in that project happens to bump.
//
// Unbounded in a quiet project. This is the same shape the ban/unban fix closed one
// table over (TestSpaceBanMovesTheEpochChannelToo).

// TestSpaceMemberRejoinMovesTheEpoch pins both reactivation entry points.
//
// They are separate cases because they are separate statements with different
// capacity semantics: the invite/add path reactivates directly, the join path goes
// through the capacity-checked variant.
func TestSpaceMemberRejoinMovesTheEpoch(t *testing.T) {
	cases := []struct {
		name     string
		maxUsers int
		rejoin   func(t *testing.T, spaceID, uid string, maxUsers int)
	}{
		{
			name: "reactivateMember",
			rejoin: func(t *testing.T, spaceID, uid string, _ int) {
				t.Helper()
				require.NoError(t, spacemod.ReactivateMemberForTest(testCtx, spaceID, uid, 0))
			},
		},
		{
			// maxUsers > 0 takes the COUNT + FOR UPDATE branch, which is the one that
			// holds locks while the step runs.
			name:     "atomicReactivateMemberIfNotFull",
			maxUsers: 10,
			rejoin: func(t *testing.T, spaceID, uid string, maxUsers int) {
				t.Helper()
				require.NoError(t, spacemod.ReactivateMemberIfNotFullForTest(
					testCtx, spaceID, uid, maxUsers))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, p := setup(t)
			p.registerSpaceMemberRemovalCleanup()

			seedSpace(t, spaceA, 1)
			ownerToken := seedUser(t, "rjOwner")
			seedSpaceMember(t, spaceA, "rjOwner", 2, 1)
			seedUser(t, "rjTarget")
			seedSpaceMember(t, spaceA, "rjTarget", 0, 1)

			inProject := createProjectVia(t, srv, spaceA, ownerToken, "rejoin-"+tc.name)
			untouched := createProjectVia(t, srv, spaceA, ownerToken, "rejoin-none-"+tc.name)
			admitted, err := p.addOneMember(inProject.ProjectID, spaceA, "rjOwner", "rjTarget")
			require.NoError(t, err)
			require.True(t, admitted)

			// Remove, through the real transaction, so the epoch moves the way it does in
			// production and the peer's cached denial is keyed on the post-removal value.
			removed, err := spacemod.RemoveMemberForTest(
				testCtx, spaceA, "rjTarget", 2, "rjOwner", spacemod.MemberRemoveReasonKicked)
			require.NoError(t, err)
			require.True(t, removed)

			afterRemoval := epochOf(t, inProject.ProjectID)
			untouchedBefore := epochOf(t, untouched.ProjectID)

			// The project seat must still be open: the cascade is asynchronous and has not
			// run. That is what makes the rejoin restore the answer WITHOUT any
			// project-side write — and therefore without any bump of its own.
			seat, err := testDB.queryMember(inProject.ProjectID, "rjTarget")
			require.NoError(t, err)
			require.NotNil(t, seat)
			require.Equal(t, MemberStatusActive, seat.Status,
				"the surviving seat IS the mechanism: with it closed, the rejoin would have "+
					"to go through a project-side add, which bumps on its own")

			tc.rejoin(t, spaceA, "rjTarget", tc.maxUsers)

			assert.Greater(t, epochOf(t, inProject.ProjectID), afterRemoval,
				"reopening a Space seat must move member_epoch in the same transaction, for "+
					"the same reason closing one does. The peer cached member:false at the "+
					"post-removal epoch; if the epoch does not move, its staleness check "+
					"agrees with that denial and a valid returning member stays locked out "+
					"until some unrelated write in this project happens to bump")
			assert.Equal(t, untouchedBefore, epochOf(t, untouched.ProjectID),
				"a project the member never held a seat in must not be churned")
		})
	}
}
