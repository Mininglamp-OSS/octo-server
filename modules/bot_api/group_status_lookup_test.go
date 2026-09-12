package bot_api

// The group-status lookup's missing-row contract must stay distinct from the
// test override seam: a real absent row is dbr.ErrNotFound.

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroupStatusLookupReportsAMissingRowAsNotFound(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	ba := NewBotAPI(ctx)
	require.Nil(t, ba.groupStatusQueryOverride,
		"this case must drive the real query — the stub seam is what hid the regression")

	_, err := ba.queryGroupStatus("no_such_group")
	require.Error(t, err, "a group that does not exist is not 'status 0, not disbanded'")
	assert.ErrorIs(t, err, dbr.ErrNotFound,
		"callers classify on errors.Is(err, dbr.ErrNotFound): send.go maps it to "+
			"errBotSendPermGroupNotFound and the not_found send-permission metric, which "+
			"exists to separate a bot probing channel ids that do not exist from a query "+
			"that failed. Returning nil here makes both unreachable and removes the only "+
			"existence check the OBO send path has")

	disbanded, err := ba.isGroupDisbanded("no_such_group")
	require.Error(t, err, "isGroupDisbanded must propagate it unchanged")
	assert.ErrorIs(t, err, dbr.ErrNotFound)
	assert.False(t, disbanded)
}

func TestGroupStatusLookupReadsStatusForLiveGroups(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	exec := func(sql string, args ...interface{}) {
		t.Helper()
		_, err := ctx.DB().InsertBySql(sql, args...).Exec()
		require.NoError(t, err)
	}
	exec("INSERT INTO `group` (group_no, name, creator, status, project_id) VALUES (?, ?, ?, 1, ?)",
		"gs_project_group", "g", "u", "gs_project")
	exec("INSERT INTO `group` (group_no, name, creator, status, project_id) VALUES (?, ?, ?, 2, '')",
		"gs_space_group", "g", "u")

	ba := NewBotAPI(ctx)

	status, err := ba.queryGroupStatus("gs_project_group")
	require.NoError(t, err)
	assert.Equal(t, 1, status)

	status, err = ba.queryGroupStatus("gs_space_group")
	require.NoError(t, err)
	assert.Equal(t, 2, status, "a disbanded group still reports its status")
}
