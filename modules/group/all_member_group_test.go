package group

import (
	"os"
	"regexp"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/require"
)

// Group-side tests for the P2 all-member group.
//
// These cover the half modules/project cannot reach: the real provisioner and
// admitter, the owner-sync decision, and the D7 refusals — plus the one
// service-layer contract change P2 needed, which is that CreateGroup accepts a
// group whose only member is its creator.

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
		"INSERT INTO `octo_project_member` (project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 1, 0, ?, NOW(3), NOW(3)) "+
			"ON DUPLICATE KEY UPDATE role=VALUES(role), status=1, removing=0",
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

// TestIsAllMemberGroupRequiresBothHalvesOfThePredicate pins the reason
// pkg/project.IsAllMemberGroup checks the group's own project_id as well as the
// project's pointer.
//
// P1's cascade detaches a group to Space-direct when its creator leaves the
// project and nobody left can inherit it, and it does NOT clear the project's
// pointer (modules/group may not write octo_project). Asking only the project
// side would keep the D7 protection switched on for a group the project no
// longer owns — and since the protection blocks exit and disband, its members
// could never leave it and nobody could clean it up.
func TestIsAllMemberGroupRequiresBothHalvesOfThePredicate(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)

	const (
		projectID = "p_pred"
		spaceID   = "s_pred"
		groupNo   = "g_pred"
	)
	seedProjectForGroupTest(t, ctx, projectID, spaceID, "u_owner")
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) VALUES (?, ?, ?, 1, ?, ?)",
		groupNo, "all", "u_owner", spaceID, projectID,
	).Exec()
	require.NoError(t, err)
	setProjectAllMemberGroup(t, ctx, projectID, groupNo)

	ok, err := projectpkg.IsAllMemberGroup(ctx.DB(), projectID, groupNo)
	require.NoError(t, err)
	require.True(t, ok, "both halves agree, so this IS the all-member group")

	// Simulate P1's detach: the group reverts to Space-direct, the project's
	// pointer is left behind.
	_, err = ctx.DB().UpdateBySql("UPDATE `group` SET project_id = '' WHERE group_no = ?", groupNo).Exec()
	require.NoError(t, err)

	ok, err = projectpkg.IsAllMemberGroup(ctx.DB(), projectID, groupNo)
	require.NoError(t, err)
	require.False(t, ok,
		"a detached group must stop counting as the all-member group, or D7 would "+
			"protect a group the project no longer owns and its members could never leave")

	// A Space-direct group short-circuits with no query at all.
	ok, err = projectpkg.IsAllMemberGroup(ctx.DB(), "", groupNo)
	require.NoError(t, err)
	require.False(t, ok)
}

// TestPickActiveOwnerPrefersTheLongestStanding pins the successor rule the owner
// sync uses, and that an ownerless project is a normal answer rather than an
// error.
func TestPickActiveOwnerPrefersTheLongestStanding(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)

	const projectID, spaceID = "p_owner", "s_owner"
	seedProjectForGroupTest(t, ctx, projectID, spaceID, "u_first")
	// A second owner, written later.
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project_member` (project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, 2, 1, 0, ?, DATE_ADD(NOW(3), INTERVAL 1 SECOND), NOW(3))",
		projectID, "u_second", spaceID, "u_first",
	).Exec()
	require.NoError(t, err)

	got, err := projectpkg.PickActiveOwner(ctx.DB(), projectID)
	require.NoError(t, err)
	require.Equal(t, "u_first", got, "seniority decides, not who was promoted last")

	// An ownerless project answers "", which P0's Space cascade can genuinely
	// produce. The caller must leave the group's creator alone rather than error.
	_, err = ctx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET role = 0 WHERE project_id = ?", projectID).Exec()
	require.NoError(t, err)
	got, err = projectpkg.PickActiveOwner(ctx.DB(), projectID)
	require.NoError(t, err)
	require.Empty(t, got, "an ownerless project is a normal answer, not an error")
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

// TestAllMemberGroupHooksAreRegisteredByTheModule pins that this module wires all
// four all-member group hooks.
//
// Without them the feature fails OPEN in the quietest possible way: every project
// is created with no group, each failure logged as "provisioner_missing" and
// counted, and nothing looks broken from the outside until somebody opens a
// project and finds no group in it.
//
// Two assertions, because neither alone is enough. The source check is
// order-independent and names the exact line somebody would delete; the live
// check proves the call is actually on the path module.Setup runs, not merely
// present in a function nobody calls.
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
		"module.Setup must leave all four all-member group hooks registered")
}

// TestEveryAdmissionEntryConstantIsInTheGuardLists stops the two hard-coded
// entry lists from silently going stale.
//
// TestProjectGroupRefusesANonProjectMember and TestEveryAdmissionEntryLabelIsEmitted
// each enumerate the admission entries by hand, and those enumerations ARE the
// I2 guarantee: an entry missing from them is an admission path nobody asserts
// refuses a non-member, and a rejection label nobody checks is ever emitted.
// Both tests keep passing when an entry is added and not listed — the exact
// failure mode P1's D8 gives as the reason for preferring guards that enumerate
// over guards that match strings.
//
// So this reads the constants out of the source and compares. Adding an entry to
// admission.go without adding it to both lists fails here, naming it.
func TestEveryAdmissionEntryConstantIsInTheGuardLists(t *testing.T) {
	src, err := os.ReadFile("admission.go")
	require.NoError(t, err)

	// AdmissionEntryXxx = "..." — the declarations, not the uses.
	re := regexp.MustCompile(`(?m)^\s*(AdmissionEntry\w+)\s*=\s*"`)
	var declared []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		declared = append(declared, m[1])
	}
	require.GreaterOrEqual(t, len(declared), 11,
		"expected at least the eleven P1 entries plus P2's; found %d — the pattern "+
			"probably stopped matching, which would make this guard vacuous", len(declared))

	for _, file := range []string{"admission_i2_test.go", "project_cascade_guard_test.go"} {
		body, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, name := range declared {
			require.Contains(t, string(body), name,
				"%s does not list %s. Its enumeration IS the I2 guarantee: an entry "+
					"missing from it is an admission path nothing asserts refuses a "+
					"non-project-member.", file, name)
		}
	}
}

// TestGroupStatusDisbandMatchesPkgProject keeps the one literal that crosses the
// module boundary in step.
//
// pkg/project cannot import modules/group — modules/group imports IT — so the
// disbanded-group status is spelled out on that side. Both of its readers
// (IsAllMemberGroup here, and modules/project's I4 scan A through it) now compare
// against that single constant, and this is what stops the two definitions
// drifting: a change to GroupStatusDisband that did not reach pkg/project would
// make every "is this group disbanded" predicate outside modules/group answer
// about the wrong number, silently and in the permissive direction.
func TestGroupStatusDisbandMatchesPkgProject(t *testing.T) {
	require.Equal(t, GroupStatusDisband, projectpkg.GroupStatusDisband,
		"pkg/project.GroupStatusDisband mirrors this module's constant by hand; update it "+
			"in the same commit that changes this one")
}
