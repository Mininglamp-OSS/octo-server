package group

// A10 (preset-group auto-join) exercised through its own function, not the funnel.
//
// PR #846's review noted that six of the eleven entry points are covered only by
// calling admitOrRestoreMembersTx directly — which proves the funnel refuses, not
// that the path reaches the funnel with the group's real attribution. A10 is the
// cheapest of the six to pin at path level: it is a plain function that
// modules/space calls through a registered hook, with no HTTP request and no
// Space-join flow needed.
//
// What this catches that the funnel tests cannot: admitToPresetGroup reading the
// project_id from the group row rather than passing "". Passing "" would be
// correct today (modules/space refuses to preset a project group) and would be a
// silent fail-OPEN the moment that check moved — which is precisely the argument
// the function's own doc comment makes.

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/require"
)

func TestPresetGroupAdmissionRefusesANonProjectMember(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	groupNo := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "a10_outsider")
	seedProject(t, ctx, projectID, spaceID)
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)
	// Deliberately no octo_project_member row.

	err := f.admitToPresetGroup(ctx, spaceID, groupNo, "a10_outsider")
	require.ErrorIs(t, err, ErrAdmissionRefused,
		"a Space member who is not a project member must not be auto-joined into a "+
			"project group, however the group came to be listed as a preset")
	require.False(t, activeMemberExists(t, ctx, groupNo, "a10_outsider"),
		"I2 has no read-path filter: the row IS the access")
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

// TestPresetGroupAdmissionAdmitsIntoASpaceDirectGroup keeps the refusal above
// honest: without a project the same call must sail through.
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
