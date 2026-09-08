package group

import (
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// TestUpdateGroupInfoWritesColumnsNotTheWholeRow pins that a rename touches the
// name and the version and NOTHING ELSE.
//
// UpdateGroupInfo used to read the row on a pooled connection and write the whole
// snapshot back — status, forbidden, invite, notice and creator included. Anything
// another writer committed inside that window was reverted, and the expensive case
// is `status`: a rename could undo a disband. The repo already had this lesson
// twice; UpdateInviteTx and UpdateStatusTx both carry the comment.
//
// What made it worth fixing now is D8: before P2 this window only opened when a
// person clicked "rename group", and now every project rename drives it
// automatically. PR #855s fifth review, Q9.
//
// The window itself is not what this test asserts — reproducing it needs a hook
// between the service's read and its write. It asserts the property that closes
// it: the statement names the columns it changes.
func TestUpdateGroupInfoWritesColumnsNotTheWholeRow(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	g := New(ctx)

	// Case 1 — the group was disbanded and locked down after the caller's snapshot.
	const disbanded = "g_column_write"
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, `version`, notice, forbidden, invite) "+
			"VALUES (?, 'before', 'u_c', 1, 1, 'notice text', 1, 1)",
		disbanded).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().UpdateBySql(
		"UPDATE `group` SET status = ?, forbidden = 0, invite = 0 WHERE group_no = ?",
		GroupStatusDisband, disbanded).Exec()
	require.NoError(t, err)

	name := "after"
	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	affected, err := g.db.UpdateNameNoticeTx(disbanded, &name, nil, 99, tx)
	require.NoError(t, err)
	require.Zero(t, affected,
		"the write must report that it changed nothing, so the caller can skip the "+
			"push-cache invalidation, the GroupUpdate broadcast and the channel refresh — "+
			"otherwise the database is clean and the clients are told a group that is gone "+
			"was renamed")
	require.NoError(t, tx.Commit())

	row := readGroupColumns(t, ctx, disbanded)
	require.Equal(t, GroupStatusDisband, row.Status,
		"a rename must not resurrect a disbanded group")
	require.Equal(t, 0, row.Forbidden, "a rename must not touch the mute flag")
	require.Equal(t, 0, row.Invite, "a rename must not touch the invite switch")
	require.Equal(t, "notice text", row.Notice,
		"and a name-only update must leave the notice alone")
	require.Equal(t, "before", row.Name,
		"the rename must not LAND either: the service checked status on a pooled read, "+
			"and a disband committing in that window would otherwise still get a new name, "+
			"a bumped version and an update notification pushed to a group that is gone")
	require.EqualValues(t, 1, row.Version, "and no version bump for a write that did nothing")

	// Case 2 — a live group, so the status predicate cannot pass by refusing everything.
	const live = "g_column_write_live"
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, `version`, notice, forbidden, invite) "+
			"VALUES (?, 'before', 'u_c', 1, 1, 'notice text', 1, 1)",
		live).Exec()
	require.NoError(t, err)

	tx, err = ctx.DB().Begin()
	require.NoError(t, err)
	affected, err = g.db.UpdateNameNoticeTx(live, &name, nil, 42, tx)
	require.NoError(t, err)
	require.EqualValues(t, 1, affected, "a live group's rename lands, so the notifications go out")
	require.NoError(t, tx.Commit())

	row = readGroupColumns(t, ctx, live)
	require.Equal(t, "after", row.Name, "a live group still gets renamed")
	require.EqualValues(t, 42, row.Version)
	require.Equal(t, "notice text", row.Notice, "name-only leaves the notice")
	require.Equal(t, 1, row.Forbidden, "and leaves the mute flag")
	require.Equal(t, 1, row.Invite, "and the invite switch")
}

type groupColumns struct {
	Name      string `db:"name"`
	Version   int64  `db:"version"`
	Status    int    `db:"status"`
	Notice    string `db:"notice"`
	Forbidden int    `db:"forbidden"`
	Invite    int    `db:"invite"`
}

func readGroupColumns(t *testing.T, tctx *config.Context, groupNo string) groupColumns {
	t.Helper()
	var rows []groupColumns
	_, err := tctx.DB().SelectBySql(
		"SELECT name, `version`, status, notice, forbidden, invite FROM `group` WHERE group_no = ?",
		groupNo).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0]
}

// TestUpdateGroupInfoDoesNotWriteTheWholeRow is the half the case above cannot
// reach.
//
// The behavioural test drives UpdateNameNoticeTx directly, so it stays green if
// somebody points UpdateGroupInfo back at UpdateTx — the revert would restore the
// window with every assertion still passing. Reproducing the window itself needs a
// hook between the service's read and its write, which does not exist and is not
// worth inventing for one call site; a source guard costs nothing and fails on the
// exact edit.
func TestUpdateGroupInfoDoesNotWriteTheWholeRow(t *testing.T) {
	body, err := os.ReadFile("service.go")
	require.NoError(t, err)
	src := string(body)

	start := strings.Index(src, "func (s *Service) UpdateGroupInfo(")
	require.Positive(t, start, "UpdateGroupInfo must exist")
	end := strings.Index(src[start:], "\n}\n")
	require.Positive(t, end, "could not find the end of UpdateGroupInfo")
	fn := src[start : start+end]

	// Asserted on a boolean rather than with Contains/NotContains so a failure
	// prints the sentence and not the whole function body.
	require.True(t, strings.Contains(fn, "UpdateNameNoticeTx("),
		"UpdateGroupInfo must write the columns it changes")
	require.False(t, strings.Contains(fn, "UpdateTx("),
		"UpdateGroupInfo must NOT write the whole row back: the snapshot it read is "+
			"stale for status, forbidden, invite and notice, and D8 now drives this path "+
			"on every project rename")

	// And the caller half of the same window: when the status predicate refuses the
	// write, the three publish steps must not run. Asserted at the source because
	// reproducing it needs a hook between the service's pooled status read and its
	// write, which does not exist. PR #855s seventh review, P2-1.
	zero := strings.Index(fn, "if affected == 0")
	require.Positive(t, zero,
		"UpdateGroupInfo must inspect the rows affected: 0 means the group was "+
			"disbanded after its snapshot, and the database is then clean while the "+
			"clients are not")
	for _, publish := range []string{"InvalidateGroupName(", "SendGroupUpdate(", "SendChannelUpdateToGroup("} {
		at := strings.Index(fn, publish)
		require.Positive(t, at, "%s must still be part of the happy path", publish)
		require.Less(t, zero, at,
			"the affected == 0 return must come BEFORE %s — otherwise a rename that "+
				"wrote nothing still tells every client the group was renamed, and the "+
				"API reports success", publish)
	}
}
