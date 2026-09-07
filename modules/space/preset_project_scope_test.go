package space

// The preset-group project check answers about THIS Space only.
//
// PR #844's review found that firstProjectGroupAmongPresets queried
// `group_no IN ? AND project_id <> ''` with no space_id predicate, so a Space
// admin who guessed a group_no belonging to another Space learned from the
// presence of the preset_group_ids validation error whether it is a project
// group. PR #846's review then measured that the fix was pinned by nothing:
// un-scoping it again kept modules/space green.
//
// The rule the pair of cases below encodes: a project group in the caller's own
// Space is refused, and a group in someone else's Space is not answered about at
// all — it simply is not a preset group of this Space, which is also true.
// joinPresetGroups re-checks the attribution at execution time, so nothing is
// weakened for a group this Space can actually preset.

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/require"
)

func seedProjectGroupForPresetTest(t *testing.T, spaceID, projectID, groupNo string) {
	t.Helper()
	// Only the columns modules/space's own binary has. Its `group` table is the
	// stub the Space migrations create, not the full one modules/group ships —
	// which is the same fact the project_id migration header is about.
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) "+
			"VALUES (?, 'preset', '', 1, ?, ?)", groupNo, spaceID, projectID).Exec()
	require.NoError(t, err)
}

func presetIDsJSON(t *testing.T, groupNos ...string) string {
	t.Helper()
	raw, err := json.Marshal(groupNos)
	require.NoError(t, err)
	return string(raw)
}

func TestPresetProjectCheckRefusesAProjectGroupInThisSpace(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	mine := util.GenerUUID()
	seedProjectGroupForPresetTest(t, "sp_preset_mine", util.GenerUUID(), mine)

	bad, err := f.firstProjectGroupAmongPresets("sp_preset_mine", presetIDsJSON(t, mine))
	require.NoError(t, err)
	require.Equal(t, mine, bad,
		"a project group of this Space may not be a preset group: every new member would "+
			"violate I2 on the way in")
}

func TestPresetProjectCheckDoesNotAnswerAboutAnotherSpace(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	theirs := util.GenerUUID()
	seedProjectGroupForPresetTest(t, "sp_preset_theirs", util.GenerUUID(), theirs)

	bad, err := f.firstProjectGroupAmongPresets("sp_preset_mine", presetIDsJSON(t, theirs))
	require.NoError(t, err)
	require.Empty(t, bad,
		"a group_no in another Space must not produce this Space's validation error: the "+
			"error's presence would tell a caller whether a group they cannot see belongs "+
			"to a project")
}

// TestPresetProjectCheckIgnoresASpaceDirectGroup keeps the other two from
// passing because the query started matching nothing.
func TestPresetProjectCheckIgnoresASpaceDirectGroup(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	plain := util.GenerUUID()
	seedProjectGroupForPresetTest(t, "sp_preset_mine", "", plain)

	bad, err := f.firstProjectGroupAmongPresets("sp_preset_mine", presetIDsJSON(t, plain))
	require.NoError(t, err)
	require.Empty(t, bad, "a Space-direct group is a perfectly good preset group")
}
