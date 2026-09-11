package group

// A10 (preset-group auto-join) is exercised through its own function, not the
// invitation HTTP flow. Preset eligibility remains a Space concern, while
// native membership is independent from Project membership.

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/require"
)

func TestPresetGroupAdmissionAdmitsANonProjectMember(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	groupNo := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "a10_outsider")
	seedProject(t, ctx, projectID, spaceID)
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)

	require.NoError(t, f.admitToPresetGroup(ctx, spaceID, groupNo, "a10_outsider"))
	require.True(t, activeMemberExists(t, ctx, groupNo, "a10_outsider"),
		"native preset admission must not require a Project seat")
}

func TestPresetGroupAdmissionAdmitsAProjectMember(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	groupNo := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "a10_member")
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, "a10_member", 0)
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)

	require.NoError(t, f.admitToPresetGroup(ctx, spaceID, groupNo, "a10_member"))
	require.True(t, activeMemberExists(t, ctx, groupNo, "a10_member"),
		"the gate must not have become a blanket refusal")
}

func TestPresetGroupAdmissionSkipsDedicatedAllMemberGroup(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	groupNo := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "a10_preset_outsider")
	seedProject(t, ctx, projectID, spaceID)
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)
	_, err := ctx.DB().Update("octo_project").
		Set("all_member_group_no", groupNo).
		Where("project_id=?", projectID).Exec()
	require.NoError(t, err)

	require.NoError(t, f.admitToPresetGroup(
		ctx, spaceID, groupNo, "a10_preset_outsider",
	))
	require.False(t, activeMemberExists(t, ctx, groupNo, "a10_preset_outsider"),
		"preset admission must not bypass the dedicated Project group projection")
}

// TestPresetGroupAdmissionAdmitsIntoASpaceDirectGroup covers the same native
// policy for a Space-direct group.
func TestPresetGroupAdmissionAdmitsIntoASpaceDirectGroup(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	groupNo := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "a10_plain")
	seedGroupRow(t, ctx, groupNo, spaceID, "")

	require.NoError(t, f.admitToPresetGroup(ctx, spaceID, groupNo, "a10_plain"))
	require.True(t, activeMemberExists(t, ctx, groupNo, "a10_plain"))
}
