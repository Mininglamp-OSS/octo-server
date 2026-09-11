package group

import (
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/stretchr/testify/require"
)

// Group-side tests for the P2 all-member group.
//
// These cover initial provisioning and metadata behaviour. Project member/role
// changes do not synchronize the native group after creation.

// seedProjectForGroupTest writes a project and its owner seat directly.
//
// Directly rather than through modules/project's API because modules/group must
// keep working without a Project HTTP surface mounted, and because these tests
// are about the GROUP side's behaviour given a project that exists — how it came
// to exist is modules/project's business.
func seedProjectForGroupTest(t *testing.T, tctx *config.Context, projectID, spaceID, ownerUID string) {
	t.Helper()
	_, err := tctx.DB().InsertBySql(
		"INSERT INTO `octo_project` (project_id, space_id, name, creator, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 1, NOW(3), NOW(3))",
		projectID, spaceID, "proj-"+projectID, ownerUID,
	).Exec()
	require.NoError(t, err)
	seedProjectSeat(t, tctx, projectID, spaceID, ownerUID, 2)
}

func seedProjectSeat(t *testing.T, tctx *config.Context, projectID, spaceID, uid string, role int) {
	t.Helper()
	_, err := tctx.DB().InsertBySql(
		"INSERT INTO `octo_project_member` (project_id, uid, space_id, role, status, removing, invite_uid, created_at, joined_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 1, 0, ?, NOW(3), NOW(3), NOW(3)) "+
			"ON DUPLICATE KEY UPDATE role=VALUES(role), joined_at=IF(status=0 OR removing=1, VALUES(joined_at), joined_at), status=1, removing=0",
		projectID, uid, spaceID, role, uid,
	).Exec()
	require.NoError(t, err)
}

func setProjectAllMemberGroup(t *testing.T, tctx *config.Context, projectID, groupNo string) {
	t.Helper()
	_, err := tctx.DB().UpdateBySql(
		"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
		groupNo, projectID,
	).Exec()
	require.NoError(t, err)
}

// seedAllMemberGroupRow writes the group row and points the project at it.
func seedAllMemberGroupRow(t *testing.T, tctx *config.Context, groupNo, projectID, spaceID, creator string) {
	t.Helper()
	_, err := tctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) VALUES (?, ?, ?, 1, ?, ?)",
		groupNo, "all-"+projectID, creator, spaceID, projectID).Exec()
	require.NoError(t, err)
	setProjectAllMemberGroup(t, tctx, projectID, groupNo)
}

// TestCreateGroupAcceptsACreatorOnlyGroup pins the service-contract change P2
// required.
//
// A project created with no agents picked needs an all-member group whose only
// initial member is its owner. Before P2 the service refused that outright, so
// the most common create in the product would have produced a project with no
// group.
//
// The HTTP handler's own check is unchanged and is covered by the existing
// tests: a person filling in the form still cannot create a memberless group.
func TestCreateGroupAcceptsACreatorOnlyGroup(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)

	g := New(ctx)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", "u_solo", "solo", "u_solo",
	).Exec()
	require.NoError(t, err)

	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: "u_solo",
		Name:    "just me",
	})
	// The IM channel call fails without a broker, and that failure rolls the group
	// back — so a broker-less environment cannot assert the happy path. What it CAN
	// assert is that the refusal is no longer the members check: that one returned
	// "members is required" before any transaction opened.
	if err != nil {
		require.NotContains(t, err.Error(), "members is required",
			"CreateGroup must no longer refuse a creator-only group at the service layer")
		return
	}
	require.NotEmpty(t, resp.GroupNo)
}

// TestAllMemberGroupHooksAreRegisteredByTheModule pins the two supported hooks.
//
// Provisioning is required so every project can receive its initial group;
// rename keeps the independent native group's metadata aligned with the project.
func TestAllMemberGroupHooksAreRegisteredByTheModule(t *testing.T) {
	// newTestServer runs module.Setup, which is what invokes the registration.
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)

	body, err := os.ReadFile("1module.go")
	require.NoError(t, err)
	require.Contains(t, string(body), "registerAllMemberGroupHooks()",
		"1module.go must call registerAllMemberGroupHooks at module construction, "+
			"beside registerProjectCascadeSteps and registerPresetGroupAdmitter")

	require.True(t, projectmod.AllMemberGroupHooksRegisteredForTest(),
		"module.Setup must leave provisioning and rename hooks registered")
}
