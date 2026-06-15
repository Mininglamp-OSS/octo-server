package group

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGroupNameWidenMigration genuinely exercises the VARCHAR(40)->(50) in-place widen
// against PRE-EXISTING data, rather than just asserting a 40-rune string fits a 50-wide
// column.
//
// The harness migrates the column to VARCHAR(50) up front, so to reproduce the pre-widen
// state we shrink it back to VARCHAR(40), write a 40-rune row as legacy data, then run the
// migration's Up DDL (the widen) and assert: the legacy row survives byte-for-byte with no
// truncation, the column is now 50, and a new 50-rune name (the MaxGroupNameLen ceiling)
// fits. A defer restores the column to 50 so a mid-test failure can't leave the shared test
// schema narrowed for later tests.
func TestGroupNameWidenMigration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	require.Equal(t, 50, MaxGroupNameLen, "MaxGroupNameLen and the column width must move together")

	colLen := func() int {
		var n int
		err := ctx.DB().SelectBySql(
			"SELECT CHARACTER_MAXIMUM_LENGTH FROM information_schema.columns " +
				"WHERE table_schema=DATABASE() AND table_name='group' AND column_name='name'",
		).LoadOne(&n)
		require.NoError(t, err)
		return n
	}
	modifyNameWidth := func(width int) {
		// DDL column length can't be a bound placeholder; width is a controlled int constant.
		_, err := ctx.DB().UpdateBySql(
			fmt.Sprintf("ALTER TABLE `group` MODIFY `name` VARCHAR(%d) NOT NULL DEFAULT ''", width),
		).Exec()
		require.NoError(t, err)
	}

	// Migration applied → column is already VARCHAR(50).
	require.Equal(t, MaxGroupNameLen, colLen(), "group.name must be widened to VARCHAR(50) by the migration")
	defer modifyNameWidth(MaxGroupNameLen) // never leave the shared schema narrowed

	// Reproduce the pre-widen world: a VARCHAR(40) column holding a 40-rune name.
	modifyNameWidth(40)
	require.Equal(t, 40, colLen())
	name40 := strings.Repeat("名", 40)
	legacyNo := "g40_" + util.GenerUUID()[:8]
	_, err := ctx.DB().InsertBySql("INSERT INTO `group` (group_no, name) VALUES (?, ?)", legacyNo, name40).Exec()
	require.NoError(t, err)

	// Run the widen (the migration's Up DDL) over the existing data.
	modifyNameWidth(MaxGroupNameLen)
	require.Equal(t, MaxGroupNameLen, colLen(), "widen must take effect")

	// Existing 40-rune data survives the in-place widen untouched.
	var got string
	require.NoError(t, ctx.DB().SelectBySql("SELECT name FROM `group` WHERE group_no=?", legacyNo).LoadOne(&got))
	assert.Equal(t, name40, got, "pre-existing name must survive the widen with no truncation")
	assert.Equal(t, 40, len([]rune(got)))

	// A new 50-rune name (the new ceiling) now fits and round-trips in full.
	name50 := strings.Repeat("字", 50)
	newNo := "g50_" + util.GenerUUID()[:8]
	_, err = ctx.DB().InsertBySql("INSERT INTO `group` (group_no, name) VALUES (?, ?)", newNo, name50).Exec()
	require.NoError(t, err, "a 50-rune name must fit the widened column")
	require.NoError(t, ctx.DB().SelectBySql("SELECT name FROM `group` WHERE group_no=?", newNo).LoadOne(&got))
	assert.Equal(t, name50, got)
	assert.Equal(t, 50, len([]rune(got)))
}
