package group

import (
	"os"
	"regexp"
	"strings"
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

// TestAllMemberAdmissionPairsThreadSubscriptionWithTheParent is a source guard on
// the pairing every admission path in this module observes.
//
// WuKongIM gives each thread its own channel (`groupNo____shortId`) and checks
// send permission against that channel's own subscriber table, so subscribing to
// the parent group does not carry into its threads. Every other admission path
// pairs IMAddSubscriber with addUsersToGroupThreads — memberAdd, groupScanJoin,
// un-blacklist, Service.AddGroupMembers, event.go — and removal is symmetric
// (RemoveGroupMembers → removeUserFromGroupThreads).
//
// The all-member admitter shipped without the second half, so a member added to a
// project could not post in the all-member group's threads and did not receive
// them — permanently, since the admitter runs once per join, and invisibly, since
// I4 scan B reads group_member rows rather than thread subscriptions.
//
// A source guard rather than a behavioural test because asserting the effect needs
// the broker's subscriber table read back, and nothing in this repository can do
// that — the same limitation recorded in open_verification for the group's own
// subscriber set. What CAN be checked is that the two calls stay together.
func TestAllMemberAdmissionPairsThreadSubscriptionWithTheParent(t *testing.T) {
	raw, err := os.ReadFile("all_member_group.go")
	require.NoError(t, err)
	body := string(raw)

	i := strings.Index(body, "func (g *Group) admitToAllMemberGroup(")
	require.Positive(t, i, "admitToAllMemberGroup not found")
	end := strings.Index(body[i:], "\n}\n")
	require.Positive(t, end, "could not delimit admitToAllMemberGroup")

	// Comments stripped, and the assertions match CALLS rather than bare tokens.
	//
	// The first version of this guard did neither, and the doc comment two lines
	// above the call names the function — so deleting the CALL left this test green.
	// PR #855s sixth review executed exactly that mutation. A guard whose subject
	// also appears in the prose beside its subject is not a guard.
	fn := stripLineComments(body[i : i+end])

	require.Contains(t, fn, "ctx.IMAddSubscriber(",
		"precondition: the admitter subscribes to the parent channel")
	require.Contains(t, fn, "g.addUsersToGroupThreads(",
		"the admitter must ALSO subscribe the new member to the group's threads. A thread "+
			"is its own WuKongIM channel and its own subscriber list, so the parent "+
			"subscription does not carry into it: without this the member cannot post in "+
			"any of the all-member group's threads and does not receive them, permanently "+
			"(the admitter runs once per join) and invisibly (I4 scan B reads group_member "+
			"rows, not subscriptions). Every other admission path in this module pairs the "+
			"two, and removal is symmetric.")

	require.Less(t,
		strings.Index(fn, "ctx.IMAddSubscriber("), strings.Index(fn, "g.addUsersToGroupThreads("),
		"the parent subscription comes first, matching every sibling path")
}

// stripLineComments blanks out // comments so a source guard matches code rather
// than prose about the code.
//
// Over-stripping is the safe direction for a guard: removing text can only make an
// assertion harder to satisfy, never easier. (It would also cut a // inside a
// string literal, which the functions guarded here do not contain.)
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if at := strings.Index(line, "//"); at >= 0 {
			lines[i] = line[:at]
		}
	}
	return strings.Join(lines, "\n")
}

// seedGroupMemberRole writes one group_member row with an explicit role and age.
//
// Age matters: the owner sync now orders its creator rows by (created_at, uid),
// so a case about "which of two creators is treated as the sitting one" has to
// control that order rather than inherit the storage engine's.
func seedGroupMemberRole(
	t *testing.T, tctx *config.Context, groupNo, uid string, role, ageSeconds int,
) {
	t.Helper()
	_, err := tctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, remark, role, `version`, status, vercode, "+
			"is_deleted, invite_uid, robot, forbidden_expir_time, is_external, source_space_id, created_at) "+
			"VALUES (?, ?, '', ?, 1, 1, ?, 0, '', 0, 0, 0, '', DATE_SUB(NOW(3), INTERVAL ? SECOND))",
		groupNo, uid, role, groupNo+"-"+uid, ageSeconds).Exec()
	require.NoError(t, err)
}

// creatorsOf reads the group's live creator rows, ordered.
func creatorsOf(t *testing.T, tctx *config.Context, groupNo string) []string {
	t.Helper()
	var uids []string
	_, err := tctx.DB().SelectBySql(
		"SELECT uid FROM group_member WHERE group_no=? AND role=? AND is_deleted=0 ORDER BY uid",
		groupNo, MemberRoleCreator).Load(&uids)
	require.NoError(t, err)
	return uids
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

// TestOwnerSyncConvergesAGroupWithTwoCreators pins that the sync treats a second
// creator row as state to REPAIR, not as a row to ignore.
//
// The file's own comment says two role=creator rows is the reachable state the
// FOR UPDATE exists to prevent — two concurrent syncs, or a sync racing P1's
// cascade handover, each promoting a successor. It is reachable, so the sync also
// has to converge it.
//
// It did not. The read had no ORDER BY and only creators[0] was inspected: if the
// row the engine happened to return first was still an active project owner, the
// function returned at its first check and both creators stayed. Nothing else can
// fix it either — D7 refuses transfer, exit and disband on an all-member group, so
// no group-face path reaches this state at all. PR #855s fifth review, Q2.
func TestOwnerSyncConvergesAGroupWithTwoCreators(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	g := New(ctx)

	const projectID, spaceID, groupNo = "p_conv", "s_conv", "grp_conv"
	seedProjectForGroupTest(t, ctx, projectID, spaceID, "u_owner")
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, "u_owner")

	// The sitting creator IS the active project owner, so the early return fires —
	// which is exactly why the second creator used to survive.
	seedGroupMemberRole(t, ctx, groupNo, "u_owner", MemberRoleCreator, 10)
	seedGroupMemberRole(t, ctx, groupNo, "u_extra", MemberRoleCreator, 5)

	require.NoError(t, g.ensureAllMemberGroupOwner(ctx, projectID, groupNo))

	require.Equal(t, []string{"u_owner"}, creatorsOf(t, ctx, groupNo),
		"the sync must leave exactly one creator, and it must be the project owner")

	// The extra creator is DEMOTED, not removed: they are still a project member,
	// and the all-member group's member set equals the project's.
	var roles []int
	_, err := ctx.DB().SelectBySql(
		"SELECT role FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		groupNo, "u_extra").Load(&roles)
	require.NoError(t, err)
	require.Equal(t, []int{MemberRoleCommon}, roles)
}

// TestOwnerSyncKeepsTheOwnerWhoIsAlreadyACreator covers the other half of the same
// state: the project owner is one of the two creator rows, but not the senior one.
//
// The naive convergence — promote the successor, then demote everyone else — turns
// this into a hard error: promoting a row that is already a creator updates no row,
// and the promote path treats "affected no live row" as a bug and rolls back. A
// state the sync exists to repair would then fail permanently on every attempt.
func TestOwnerSyncKeepsTheOwnerWhoIsAlreadyACreator(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	g := New(ctx)

	const projectID, spaceID, groupNo = "p_conv2", "s_conv2", "grp_conv2"
	seedProjectForGroupTest(t, ctx, projectID, spaceID, "u_owner")
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, "u_stale")

	// u_stale is senior and is NOT a project member at all — a former owner whose
	// seat is gone. u_owner is the active project owner and already holds a creator
	// row.
	seedGroupMemberRole(t, ctx, groupNo, "u_stale", MemberRoleCreator, 10)
	seedGroupMemberRole(t, ctx, groupNo, "u_owner", MemberRoleCreator, 5)

	require.NoError(t, g.ensureAllMemberGroupOwner(ctx, projectID, groupNo))

	require.Equal(t, []string{"u_owner"}, creatorsOf(t, ctx, groupNo),
		"the active project owner keeps the role; the stale creator is demoted")
}

// TestOwnerSyncAsksTheProjectInsideItsOwnTransaction is a source guard on the two
// project-side reads the owner sync makes while holding locks.
//
// They used to run on ctx.DB() — a pooled connection, i.e. a different snapshot
// from the one the transaction's writes land in. The window is real: the group's
// creator rows are locked FOR UPDATE, the project role can change inside it, and
// this sync only re-fires when a project owner changes, so a wrong answer here is
// not retried until the next owner change. Carried as "deferred" for three rounds
// before it was closed by widening the two helpers to dbr.SessionRunner.
// PR #855s fifth review, Q11.
//
// A source guard rather than a behavioural one: what is being pinned is which
// connection the read uses, and both connections give the same answer unless a
// concurrent writer commits inside the window — a race a test would have to win
// on purpose to observe.
func TestOwnerSyncAsksTheProjectInsideItsOwnTransaction(t *testing.T) {
	body, err := os.ReadFile("all_member_group.go")
	require.NoError(t, err)
	src := string(body)

	start := strings.Index(src, "func (g *Group) ensureAllMemberGroupOwner(")
	require.Positive(t, start, "ensureAllMemberGroupOwner must exist")
	end := strings.Index(src[start:], "\n}\n")
	require.Positive(t, end, "could not find the end of ensureAllMemberGroupOwner")
	fn := src[start : start+end]

	require.Contains(t, fn, "projectpkg.MemberRole(tx,",
		"the sitting creator's project role must be read on the transaction, not on a "+
			"pooled connection: the transaction already holds the group_member locks this "+
			"answer is about")
	require.Contains(t, fn, "projectpkg.PickActiveOwner(tx,",
		"and so must the successor pick — the two answers have to come from the snapshot "+
			"the writes land in")
	require.NotContains(t, fn, "ctx.DB()."+"Select",
		"no pooled read may reappear inside the sync's transaction")
}

// TestOwnerSyncKeepsTheOwnerInTheGroupWhenTheSeniorOwnerIsMissing covers the one
// arm of the convergence that misbehaved: the pool-picked successor cannot be
// promoted, and another creator IS still a project owner.
//
// PickActiveOwner returns the SENIOR active owner and only that one. When that
// person is not in the group — the D12 admission gap I4 scan B exists to report —
// the promotion falls through, and the previous version kept creators[0] and then
// demoted everyone else. If a junior owner held one of those creator rows, this
// sync DEMOTED the only valid owner the group had and left a non-owner in charge:
// unrepairable from the group side (D7 refuses transfer, exit and disband), not
// retried (the sync only re-fires on a project owner change), and invisible (no
// scan asks whether a group's creator is a project owner).
//
// Worse than the state it replaced, which was "two creators, one of them valid".
// PR #855s sixth review traced it line by line.
func TestOwnerSyncKeepsTheOwnerInTheGroupWhenTheSeniorOwnerIsMissing(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)
	g := New(ctx)

	const projectID, spaceID, groupNo = "p_conv3", "s_conv3", "grp_conv3"
	// u_a is the senior project owner and is deliberately NOT in the group.
	seedProjectForGroupTest(t, ctx, projectID, spaceID, "u_a")
	// u_b is a junior project owner, written a second later so seniority is not a tie.
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project_member` (project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, 2, 1, 0, ?, DATE_ADD(NOW(3), INTERVAL 1 SECOND), NOW(3))",
		projectID, "u_b", spaceID, "u_a").Exec()
	require.NoError(t, err)

	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, "u_stale")
	// The senior creator is not a project member at all; the junior one is the
	// project owner who is actually in the group.
	seedGroupMemberRole(t, ctx, groupNo, "u_stale", MemberRoleCreator, 10)
	seedGroupMemberRole(t, ctx, groupNo, "u_b", MemberRoleCreator, 5)

	require.Equal(t, "u_a", mustPickActiveOwner(t, ctx, projectID),
		"precondition: the pool pick is the senior owner, who is NOT in the group")

	require.NoError(t, g.ensureAllMemberGroupOwner(ctx, projectID, groupNo))

	require.Equal(t, []string{"u_b"}, creatorsOf(t, ctx, groupNo),
		"the convergence must keep the creator who is still a project owner. Demoting "+
			"them because the SENIOR owner could not be promoted leaves the group owned by "+
			"a non-owner, with no group-face path to repair it and no scan reporting it")
}

func mustPickActiveOwner(t *testing.T, tctx *config.Context, projectID string) string {
	t.Helper()
	got, err := projectpkg.PickActiveOwner(tctx.DB(), projectID)
	require.NoError(t, err)
	return got
}

// TestAdmissionMarksAPostCommitSubscribeFailureAsItself is the group half of the
// pin: the admitter must WRAP pkg/project.ErrAdmittedButNotSubscribed when the
// transaction committed and the broker subscribe then failed.
//
// Without the wrap, modules/project counts the failure as a generic admit failure
// and logs that the member is not in the group and scan B will report it — and
// both halves of that sentence are false for this path. The row is committed, so
// scan B is blind to it; what is missing is a broker subscription, which nothing
// here can read back.
//
// A source guard because the behaviour needs a broker that accepts the group
// creation and then refuses the subscribe, mid-transaction. PR #855's ninth review
// noted the sentinel had nothing pinning it at all: unwrapping it left every test
// in both modules green.
func TestAdmissionMarksAPostCommitSubscribeFailureAsItself(t *testing.T) {
	raw, err := os.ReadFile("all_member_group.go")
	require.NoError(t, err)
	body := string(raw)

	i := strings.Index(body, "func (g *Group) admitToAllMemberGroup(")
	require.Positive(t, i, "admitToAllMemberGroup not found")
	end := strings.Index(body[i:], "\n}\n")
	require.Positive(t, end, "could not delimit admitToAllMemberGroup")
	fn := stripLineComments(body[i : i+end])

	commit := strings.Index(fn, "tx.Commit()")
	require.Positive(t, commit, "precondition: the admission commits before it subscribes")
	subscribe := strings.Index(fn, "ctx.IMAddSubscriber(")
	require.Less(t, commit, subscribe, "precondition: the subscribe follows the commit")

	require.True(t, strings.Contains(fn[subscribe:], "projectpkg.ErrAdmittedButNotSubscribed"),
		"the IM-subscribe failure AFTER the commit must be wrapped in "+
			"pkg/project.ErrAdmittedButNotSubscribed. Returning a plain error makes the "+
			"project side count it as a generic admit failure and point on-call at I4 "+
			"scan B — which cannot see this state, because the group_member row is "+
			"committed and only the broker subscription is missing")
}
