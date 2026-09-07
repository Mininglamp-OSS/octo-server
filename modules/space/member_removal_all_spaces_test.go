package space

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// D14 — closing a uid's seats in every Space must go through the transactional
// outbox, not a bare UPDATE.
//
// The reason this matters is one layer away from this package: space_member is
// the root of the project layer's authorization chain
// (space_member.status=1 → octo_project_member.status=1 → may stay in a project
// group). botfather's bot deletion used to flip the column with a single
// statement and enqueue nothing, so a deleted bot kept its project seat forever —
// an I1 violation with no path to repair. CloseAllSpaceSeats is what closes that.

func seatCount(t *testing.T, ctx *config.Context, uid string, status int) int {
	t.Helper()
	var n int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE uid = ? AND status = ?", uid, status,
	).LoadOne(&n))
	return n
}

func pendingCleanupCount(t *testing.T, ctx *config.Context, uid string) int {
	t.Helper()
	var n int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member_removal_cleanup WHERE uid = ? AND status = 0", uid,
	).LoadOne(&n))
	return n
}

func seedSpaceForClose(t *testing.T, ctx *config.Context, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, "creator",
	).Exec()
	require.NoError(t, err)
}

func seedSeat(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid,
	).Exec()
	require.NoError(t, err)
}

// TestCloseAllSpaceSeatsEnqueuesOneCleanupPerSpace is the core D14 assertion: the
// seats close AND every one of them leaves a cleanup job behind, which is what
// drives the project-side cascade.
func TestCloseAllSpaceSeatsEnqueuesOneCleanupPerSpace(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	seedSpaceForClose(t, ctx, "sp_a")
	seedSpaceForClose(t, ctx, "sp_b")
	seedSeat(t, ctx, "sp_a", "bot_x")
	seedSeat(t, ctx, "sp_b", "bot_x")
	// A seat belonging to somebody else must be untouched.
	seedSeat(t, ctx, "sp_a", "human_y")

	closed, err := CloseAllSpaceSeats(ctx, "bot_x", "bot_x", MemberRemoveReasonBotDeleted)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"sp_a", "sp_b"}, closed)

	require.Zero(t, seatCount(t, ctx, "bot_x", 1), "every seat must be closed")
	require.Equal(t, 2, pendingCleanupCount(t, ctx, "bot_x"),
		"one cleanup job per Space — the job is what drives the project-side cascade, "+
			"and a bare UPDATE that skipped it is the defect D14 exists to fix")
	require.Equal(t, 1, seatCount(t, ctx, "human_y", 1), "another uid's seat must be untouched")
}

// TestCloseAllSpaceSeatsIsIdempotent pins that a re-run enqueues nothing.
//
// A job per call would mean a retried bot deletion fans out duplicate cleanup
// work — and the step contract says a job is re-driven on failure, so duplicates
// multiply rather than merely accumulate.
func TestCloseAllSpaceSeatsIsIdempotent(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	seedSpaceForClose(t, ctx, "sp_idem")
	seedSeat(t, ctx, "sp_idem", "bot_i")

	_, err := CloseAllSpaceSeats(ctx, "bot_i", "bot_i", MemberRemoveReasonBotDeleted)
	require.NoError(t, err)
	require.Equal(t, 1, pendingCleanupCount(t, ctx, "bot_i"))

	closed, err := CloseAllSpaceSeats(ctx, "bot_i", "bot_i", MemberRemoveReasonBotDeleted)
	require.NoError(t, err)
	require.Empty(t, closed, "nothing left to close")
	require.Equal(t, 1, pendingCleanupCount(t, ctx, "bot_i"),
		"a re-run must not enqueue a second job for a seat that was already closed")
}

// TestCloseAllSpaceSeatsRejectsAnUnknownReason pins the enum guard.
//
// The reason reaches the group cascade, which branches on it to decide whether to
// post "X was removed by Y" into every group. An unvalidated string would fall
// through that switch and start emitting that sentence for a deleted bot, which
// is exactly what MemberRemoveReasonBotDeleted exists to prevent.
func TestCloseAllSpaceSeatsRejectsAnUnknownReason(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	_, err := CloseAllSpaceSeats(ctx, "bot_z", "bot_z", "not_a_real_reason")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown member removal reason")
}

// TestBotDeletedIsARegisteredRemovalReason pins that the new reason is in the
// validation set, since enqueueMemberRemovalCleanupTx is fail-closed on it: an
// unregistered reason would make every bot deletion fail to close its seats.
func TestBotDeletedIsARegisteredRemovalReason(t *testing.T) {
	require.True(t, IsMemberRemoveReason(MemberRemoveReasonBotDeleted))
}
