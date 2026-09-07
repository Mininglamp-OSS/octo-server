package group

// D4's cancellation, enforced against a RUNNING cascade rather than only a
// queued one.
//
// PR #846's review found that `removalCancelled` is consulted once per job,
// before the step loop, and that the fan-out itself never looked at the seat
// again. Three comments in modules/project claim the worker re-checks "before
// EVERY batch"; there is one batch per job, so "before every batch" was once,
// before the loop. An admin who removed a member from a five-group project and
// immediately re-added them got a 200 while the worker went on to strip them
// from the remaining groups — active in the project, member of none of its
// groups, repairable only by re-adding each group by hand.
//
// The cases below drive the real registered step. The first is the interleaving
// itself, made deterministic with a row lock rather than a sleep; the second is
// the state that interleaving leaves, which is the shape Jerry-Xin's probe
// reproduced; the third is the control that keeps the other two from passing
// because the cascade stopped working altogether.

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/stretchr/testify/require"
)

// cascadeFixture builds a project with two groups, both owned by someone else,
// both containing the member whose seat is closing.
func cascadeFixture(t *testing.T, ctx *config.Context, uid string, removing int) (spaceID, projectID string, groups [2]string) {
	t.Helper()
	spaceID = "sp_" + util.GenerUUID()[:8]
	projectID = util.GenerUUID()

	seedSpaceSeat(t, ctx, spaceID, "cas_owner")
	seedSpaceSeat(t, ctx, spaceID, uid)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, "cas_owner", 0)
	seedProjectMember(t, ctx, projectID, spaceID, uid, removing)

	for i := range groups {
		groups[i] = util.GenerUUID()
		seedGroupRow(t, ctx, groups[i], spaceID, projectID)
		// The creator is someone else, so RemoveGroupMembers does not skip the
		// target for being the owner — the handover path has its own tests.
		seedGroupMemberRow(t, ctx, groups[i], "cas_owner", MemberRoleCreator)
		seedGroupMemberRow(t, ctx, groups[i], uid, MemberRoleCommon)
	}
	return spaceID, projectID, groups
}

func reopenSeat(t *testing.T, ctx *config.Context, projectID, uid string) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET removing = 0 WHERE project_id = ? AND uid = ?",
		projectID, uid).Exec()
	require.NoError(t, err)
}

// waitForCascadeToPark blocks until the cascade is parked on one of the row
// locks this test holds, so the interleaving below is a fact rather than a
// sleep.
//
// The signal is the server's own process list: a `group_member … FOR UPDATE`
// that has been executing for a second or more is the cascade's
// queryGroupCreatorTx waiting on the rows this test locked, and nothing else in
// this test runs such a statement. (innodb_trx was tried first and reported the
// waiter as RUNNING rather than LOCK WAIT, so it is not a usable signal here.)
//
// It also watches the cascade itself: a run that FINISHED instead of parking
// means the lock did not bite, and the test must say so rather than time out
// with a vague message.
func waitForCascadeToPark(t *testing.T, ctx *config.Context, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("the cascade finished without ever blocking on the held member rows "+
				"(err=%v): this test's premise is gone, so it would prove nothing", err)
		default:
		}
		var n int
		if err := ctx.DB().SelectBySql(
			"SELECT COUNT(*) FROM information_schema.processlist " +
				"WHERE command = 'Query' AND time >= 1 " +
				"  AND info LIKE '%group_member%' AND info LIKE '%FOR UPDATE%'").
			LoadOne(&n); err != nil {
			t.Fatalf("read processlist: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the cascade never parked on the held member rows within 30s")
}

// TestTheCascadeStopsWhenTheSeatIsReopenedMidFanOut is the interleaving itself.
//
// The member's rows in BOTH groups are locked from another connection, so the
// cascade blocks inside its first group's removal — after that group's seat
// check and before the next one's. The seat is re-opened while it is parked
// there, the lock released, and the cascade then has exactly one more decision
// to make. Locking both groups is what makes this independent of the order
// queryProjectGroupNosWithActiveMember happens to return.
func TestTheCascadeStopsWhenTheSeatIsReopenedMidFanOut(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID, projectID, groups := cascadeFixture(t, ctx, "cas_member", 1)

	blocker, err := ctx.DB().Begin()
	require.NoError(t, err)
	// Released on every exit path. A blocker left open outlives the test and
	// makes the NEXT case fail on a 50s lock-wait timeout, which is how this was
	// first noticed.
	defer blocker.RollbackUnlessCommitted()
	var locked []string
	_, err = blocker.SelectBySql(
		"SELECT uid FROM group_member WHERE group_no IN ? AND uid = ? FOR UPDATE",
		[]string{groups[0], groups[1]}, "cas_member").Load(&locked)
	require.NoError(t, err)
	require.Len(t, locked, 2, "both member rows must be held before the cascade starts")

	done := make(chan error, 1)
	go func() {
		done <- f.detachMemberFromProjectGroups(ctx, projectmod.MemberRemoval{
			ProjectID:   projectID,
			UID:         "cas_member",
			SpaceID:     spaceID,
			OperatorUID: "cas_owner",
			Reason:      "kicked",
		})
	}()

	waitForCascadeToPark(t, ctx, done)
	// The re-admission the admin makes while the worker is mid fan-out.
	reopenSeat(t, ctx, projectID, "cas_member")
	require.NoError(t, blocker.Commit())

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("the cascade did not finish after the lock was released")
	}

	survivors := 0
	for _, g := range groups {
		if activeMemberExists(t, ctx, g, "cas_member") {
			survivors++
		}
	}
	require.Equal(t, 1, survivors,
		"the group already in flight is detached and the remaining one must be kept: "+
			"a re-admission that reports success must not be followed by more removals")
}

// TestTheCascadeDetachesNothingWhenTheSeatIsAlreadyReopened is the same rule
// with no timing at all: a seat that reads active is a cancelled cascade, and
// the fan-out must not touch a single group.
//
// This is the state a mid fan-out re-admission leaves behind, and the shape the
// review reproduced by driving the step directly: before the fix it removed the
// member from every group and returned nil.
func TestTheCascadeDetachesNothingWhenTheSeatIsAlreadyReopened(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID, projectID, groups := cascadeFixture(t, ctx, "cas_readded", 0)

	require.NoError(t, f.detachMemberFromProjectGroups(ctx, projectmod.MemberRemoval{
		ProjectID:   projectID,
		UID:         "cas_readded",
		SpaceID:     spaceID,
		OperatorUID: "cas_owner",
		Reason:      "kicked",
	}))

	for _, g := range groups {
		require.True(t, activeMemberExists(t, ctx, g, "cas_readded"),
			"an active project seat means the cascade was cancelled; group %s must keep "+
				"its member row", g)
	}
}

// TestTheCascadeDetachesEveryGroupWhileTheSeatIsClosing is the control. Without
// it, deleting the loop body would satisfy both cases above.
func TestTheCascadeDetachesEveryGroupWhileTheSeatIsClosing(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID, projectID, groups := cascadeFixture(t, ctx, "cas_leaving", 1)

	require.NoError(t, f.detachMemberFromProjectGroups(ctx, projectmod.MemberRemoval{
		ProjectID:   projectID,
		UID:         "cas_leaving",
		SpaceID:     spaceID,
		OperatorUID: "cas_owner",
		Reason:      "kicked",
	}))

	for _, g := range groups {
		require.False(t, activeMemberExists(t, ctx, g, "cas_leaving"),
			"a seat at removing = 1 must be cascaded out of every group; %s kept its row", g)
	}
}

// TestTheCascadeRefusesAJobWithNoSpace pins the defence-in-depth guard.
//
// queryProjectGroupNosWithActiveMember returns (nil, nil) for an empty spaceID,
// so without the guard the step reported DONE having detached nothing — and the
// worker would then close the seat with every group row intact, which is the I2
// violation the two-phase close exists to avoid. Not reachable through the API,
// which is why it has to be loud: a job that gets here with no Space is a bug
// upstream, and reporting success is how it would stay one.
func TestTheCascadeRefusesAJobWithNoSpace(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	_, projectID, groups := cascadeFixture(t, ctx, "cas_nospace", 1)

	err := f.detachMemberFromProjectGroups(ctx, projectmod.MemberRemoval{
		ProjectID:   projectID,
		UID:         "cas_nospace",
		SpaceID:     "",
		OperatorUID: "cas_owner",
		Reason:      "kicked",
	})
	require.Error(t, err, "a cascade job with no space_id must fail, not report success")
	require.Contains(t, err.Error(), "space_id")

	for _, g := range groups {
		require.True(t, activeMemberExists(t, ctx, g, "cas_nospace"),
			"and it must not have detached anything")
	}
}
