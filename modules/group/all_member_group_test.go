package group

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/require"
)

// Group-side tests for the P2 all-member group.
//
// These cover initial provisioning, the pointer-scoped live admission hook,
// and metadata behaviour. Ordinary Project-associated groups remain native
// snapshots and never enter the dedicated-group hooks.

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

// TestDedicatedAdmissionAndHTTPProtection exercises the dedicated pointer
// against a real MySQL fixture. The same Project also owns an ordinary
// associated group; that group must remain outside the live admission hook.
func TestDedicatedAdmissionAndHTTPProtection(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	defer testutil.CleanAllTables(ctx)

	f := New(ctx)
	owner := testutil.UID
	target := "all-member-target-" + testutil.Token
	suffix := util.GenerUUID()[:12]
	projectID := "project-" + suffix
	spaceID := "space-" + suffix
	dedicatedNo := "dedicated-" + suffix
	ordinaryNo := "ordinary-" + suffix

	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: owner, Name: "all-member owner", ShortNo: "all-member-owner", Status: 1,
	}))
	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: target, Name: "all-member target", ShortNo: "all-member-target", Status: 1,
	}))
	seedSpaceSeat(t, ctx, spaceID, owner)
	seedSpaceSeat(t, ctx, spaceID, target)
	seedProjectForGroupTest(t, ctx, projectID, spaceID, owner)
	seedProjectSeat(t, ctx, projectID, spaceID, target, 0)
	seedAllMemberGroupRow(t, ctx, dedicatedNo, projectID, spaceID, owner)
	seedGroupMemberRow(t, ctx, dedicatedNo, owner, MemberRoleCreator)

	// A Project member admission reaches the dedicated group and is idempotent
	// if the test IM datasource is unavailable after the DB commit.
	err := f.admitToAllMemberGroup(ctx, spaceID, dedicatedNo, target)
	if err != nil {
		require.ErrorIs(t, err, projectpkg.ErrAdmittedButNotSubscribed)
	}
	require.True(t, activeMemberExists(t, ctx, dedicatedNo, target),
		"active Project member must be admitted to its dedicated group")

	// An ordinary associated group with the same project_id is not the
	// dedicated pointer and must not receive the same target.
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: ordinaryNo, Name: "ordinary associated", Creator: owner,
		Status: GroupStatusNormal, Version: 1, SpaceID: spaceID, ProjectID: projectID,
	}))
	require.NoError(t, f.admitToAllMemberGroup(ctx, spaceID, ordinaryNo, target))
	require.False(t, activeMemberExists(t, ctx, ordinaryNo, target),
		"ordinary Project-associated groups remain native snapshots")

	// The real HTTP exit route is protected before IM unsubscribe or any DB
	// mutation. The dedicated member and group status must remain unchanged.
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/v1/groups/"+dedicatedNo+"/exit", bytes.NewReader(nil))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "err.server.group.all_member_group_protected")
	require.True(t, activeMemberExists(t, ctx, dedicatedNo, owner))
	var status int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT status FROM `group` WHERE group_no=?", dedicatedNo,
	).LoadOne(&status))
	require.Equal(t, GroupStatusNormal, status)
}
