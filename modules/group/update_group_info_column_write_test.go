package group

import (
	"os"
	"strings"
	"testing"

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

	const groupNo = "g_column_write"
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, `version`, notice, forbidden, invite) "+
			"VALUES (?, 'before', 'u_c', 1, 1, 'notice text', 1, 1)",
		groupNo).Exec()
	require.NoError(t, err)

	// Somebody else disbands and locks the group down after the caller's snapshot
	// was taken. A full-row write-back would restore all three.
	_, err = ctx.DB().UpdateBySql(
		"UPDATE `group` SET status = ?, forbidden = 0, invite = 0 WHERE group_no = ?",
		GroupStatusDisband, groupNo).Exec()
	require.NoError(t, err)

	name := "after"
	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	require.NoError(t, g.db.UpdateNameNoticeTx(groupNo, &name, nil, 99, tx))
	require.NoError(t, tx.Commit())

	var rows []struct {
		Name      string `db:"name"`
		Version   int64  `db:"version"`
		Status    int    `db:"status"`
		Notice    string `db:"notice"`
		Forbidden int    `db:"forbidden"`
		Invite    int    `db:"invite"`
	}
	_, err = ctx.DB().SelectBySql(
		"SELECT name, `version`, status, notice, forbidden, invite FROM `group` WHERE group_no = ?",
		groupNo).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	require.Equal(t, "after", rows[0].Name)
	require.EqualValues(t, 99, rows[0].Version)
	require.Equal(t, GroupStatusDisband, rows[0].Status,
		"a rename must not resurrect a disbanded group")
	require.Equal(t, 0, rows[0].Forbidden, "a rename must not touch the mute flag")
	require.Equal(t, 0, rows[0].Invite, "a rename must not touch the invite switch")
	require.Equal(t, "notice text", rows[0].Notice,
		"and a name-only update must leave the notice alone")
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
}
