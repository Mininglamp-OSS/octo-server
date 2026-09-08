package project

import (
	"testing"
	"time"

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

// TestSpaceMemberRejoinMovesTheEpoch pins EVERY reactivation entry point.
//
// FOUR of them, not two, and the first version of this test covered two — which is
// how round 9 found the axis still open after it had been declared closed. They are
// separate cases because they are separate statements with different semantics:
//
//	reactivateMember                  direct reactivation (invite/add-back)
//	atomicReactivateMemberIfNotFull   capacity-checked (join)
//	approveJoinApplyAtomic            the join-apply approval branch — the DESIGNED
//	                                  rejoin funnel (resetApprovedApplyForRejoin
//	                                  exists to route a removed member back through it)
//	upsertMembers                     admin bulk add, whose ON DUPLICATE branch
//	                                  reopens an existing removed row
//
// Enumerating from the SQL rather than from the names is what this table is for: two
// of the four do not have "reactivate" anywhere in their signature.
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
		{
			// The join-apply approval path, and it is not an afterthought: it is the
			// DESIGNED rejoin funnel. resetApprovedApplyForRejoin exists precisely so a
			// removed member can re-apply and be approved through this branch, which
			// reopens the existing row rather than inserting a new one. Three approval
			// entries (in-space, manager, H5 auth_code) all converge here.
			name:     "approveJoinApplyAtomic",
			maxUsers: 10,
			rejoin: func(t *testing.T, spaceID, uid string, maxUsers int) {
				t.Helper()
				applyID, err := spacemod.UpsertJoinApplyForTest(testCtx, spaceID, uid)
				require.NoError(t, err)
				require.Positive(t, applyID)
				_, err = spacemod.ApproveJoinApplyForTest(
					testCtx, applyID, "rjOwner", spaceID, maxUsers)
				require.NoError(t, err)
			},
		},
		{
			// The admin bulk add: `INSERT ... ON DUPLICATE KEY UPDATE status=1`, whose
			// DUPLICATE branch fires on any existing removed row. The function's own
			// name says "upsert" and its comment says "add/reactivate", so treating it
			// as insert-only was a misreading of its own bytes.
			name: "upsertMembers",
			rejoin: func(t *testing.T, spaceID, uid string, _ int) {
				t.Helper()
				require.NoError(t, spacemod.UpsertMembersForTest(testCtx, spaceID, []string{uid}))
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

// TestRepeatedUpsertOfAnActiveMemberDoesNotChurnTheEpoch pins the half of the
// reactivation fix that the obvious implementation gets wrong.
//
// The reviews suggested gating on `ON DUPLICATE KEY UPDATE`'s affected-rows
// convention (1 = insert, 2 = update, 0 = no change). Measured on MySQL 8.0.33
// against this statement, that convention cannot express the distinction needed
// here:
//
//	fresh insert                                   -> 1
//	removed row (status=0) reactivated             -> 2
//	ALREADY-ACTIVE member re-upserted, same second -> 0
//	ALREADY-ACTIVE member re-upserted, next second -> 2   <-- same as reactivation
//
// because `updated_at=NOW()` makes a cross-second repeat count as "changed". So
// affected==2 would bump the epoch on every repeated admin add of an existing
// member — and "a no-op write does not change the epoch" is a rule consumers cache
// against. Every needless bump costs every consumer of that project a re-verify.
//
// Hence the locking read. This test is what keeps it: it is timing-dependent in the
// direction that matters, so it sleeps past a second boundary to reach the case that
// the affected-rows shape gets wrong.
func TestRepeatedUpsertOfAnActiveMemberDoesNotChurnTheEpoch(t *testing.T) {
	srv, p := setup(t)
	p.registerSpaceMemberRemovalCleanup()

	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "chOwner")
	seedSpaceMember(t, spaceA, "chOwner", 2, 1)
	seedUser(t, "chTarget")
	seedSpaceMember(t, spaceA, "chTarget", 0, 1)

	inProject := createProjectVia(t, srv, spaceA, ownerToken, "upsert-churn")
	admitted, err := p.addOneMember(inProject.ProjectID, spaceA, "chOwner", "chTarget")
	require.NoError(t, err)
	require.True(t, admitted)

	before := epochOf(t, inProject.ProjectID)

	// The target is ALREADY an active Space member, so neither of these is a
	// reactivation and neither may move the epoch.
	require.NoError(t, spacemod.UpsertMembersForTest(testCtx, spaceA, []string{"chTarget"}))
	assert.Equal(t, before, epochOf(t, inProject.ProjectID),
		"re-adding an already-active member is a no-op for membership and must not bump")

	// Past a second boundary, which is where `updated_at=NOW()` starts reporting
	// affected==2 — indistinguishable from a real reactivation.
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, spacemod.UpsertMembersForTest(testCtx, spaceA, []string{"chTarget"}))
	assert.Equal(t, before, epochOf(t, inProject.ProjectID),
		"still a no-op across a second boundary. If this fails, the reactivation check is "+
			"keyed on affected-rows rather than on the row's actual prior status: NOW() makes "+
			"a cross-second repeat report affected==2, which is exactly what a real "+
			"reactivation reports, so every repeated admin add would churn the epoch and cost "+
			"every consumer of this project a re-verify")
}

// TestBotSeatCloseMovesTheEpoch covers the axis PR #855 built a path for.
//
// #855 replaced botfather's bare `UPDATE space_member SET status=0 WHERE uid=?`
// with modules/space.CloseAllSpaceSeats, which closes each seat in its own
// transaction and enqueues the cleanup outbox — the half its own header calls the
// authorization root. All three deletion entries (botfather command, botfather
// REST, manager REST) route through it.
//
// It enqueues the outbox but does NOT run the registered removal tx steps, so
// member_epoch does not move. That is the same defect shape this branch closed for
// the ordinary removal and rejoin paths, re-appearing on the axis both of us were
// treating as the disclosed one: `_verify` flips to member:false (the Space
// conjunction sees the closed seat) while `epochs` keeps answering the pre-deletion
// value, so a peer's cached positive grant never invalidates. And because the
// project seat is only closed by the async cascade, the window is a backoff at best
// and unbounded once the job is abandoned.
//
// Bots can hold project seats: admission applies no blanket bot filter (#855 added
// eligibility rules, not exclusion), and #855 itself seats the creator's agents.
func TestBotSeatCloseMovesTheEpoch(t *testing.T) {
	srv, p := setup(t)
	p.registerSpaceMemberRemovalCleanup()

	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "bscOwner")
	seedSpaceMember(t, spaceA, "bscOwner", 2, 1)
	seedUser(t, "bscBot")
	seedSpaceMember(t, spaceA, "bscBot", 0, 1)

	inProject := createProjectVia(t, srv, spaceA, ownerToken, "bot-seat-close")
	untouched := createProjectVia(t, srv, spaceA, ownerToken, "bot-seat-close-none")
	admitted, err := p.addOneMember(inProject.ProjectID, spaceA, "bscOwner", "bscBot")
	require.NoError(t, err)
	require.True(t, admitted)

	before := epochOf(t, inProject.ProjectID)
	untouchedBefore := epochOf(t, untouched.ProjectID)

	closed, err := spacemod.CloseAllSpaceSeats(testCtx, "bscBot", "bscOwner",
		spacemod.MemberRemoveReasonForceRemoved)
	require.NoError(t, err)
	require.Contains(t, closed, spaceA, "the fixture seat must actually have been closed")

	// The project seat is still open: closing it is the async cascade's job, which is
	// exactly the window the epoch has to cover.
	seat, err := testDB.queryMember(inProject.ProjectID, "bscBot")
	require.NoError(t, err)
	require.NotNil(t, seat)
	require.Equal(t, MemberStatusActive, seat.Status)

	assert.Greater(t, epochOf(t, inProject.ProjectID), before,
		"closing a Space seat through CloseAllSpaceSeats must move member_epoch in the same "+
			"transaction, exactly as the ordinary removal paths do. It enqueues the cleanup "+
			"outbox but the outbox is asynchronous and terminal after its retry cap — until it "+
			"runs, `_verify` already answers member:false while `epochs` answers the "+
			"pre-deletion value, so a peer re-reads the same number, its staleness check "+
			"AGREES, and the cached grant outlives the deletion")
	assert.Equal(t, untouchedBefore, epochOf(t, untouched.ProjectID),
		"a project the uid never held a seat in must not be churned")
}
