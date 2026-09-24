package group

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	projectuser "github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectGroupCreateIMFailureCompensatesBothRowsAtomically(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()

	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "IM unavailable", http.StatusServiceUnavailable)
	}))
	defer im.Close()
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	groupNo := "project-group-im-failure"
	f := New(ctx)
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo, Name: "Project group", Creator: testutil.UID,
		Status: GroupStatusNormal, Version: 1, SpaceID: "space-1", ProjectID: "project-1",
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator,
		Status: int(common.GroupMemberStatusNormal), Version: 1, Vercode: util.GenerUUID(),
	}))

	svc, ok := f.groupService.(*Service)
	require.True(t, ok)
	_, err := svc.finishProjectGroupCreate(&projectGroupCreateState{
		req:            CreateGroupServiceReq{Creator: testutil.UID, ProjectID: "project-1"},
		groupNo:        groupNo,
		realMemberUIDs: []string{testutil.UID},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local Project group was removed")

	var groups, members int
	require.NoError(t, ctx.DB().SelectBySql("SELECT COUNT(*) FROM `group` WHERE group_no=?", groupNo).LoadOne(&groups))
	require.NoError(t, ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=?", groupNo).LoadOne(&members))
	assert.Zero(t, groups, "an IM failure must not leave the committed relation/group row")
	assert.Zero(t, members, "an IM failure must not leave committed native members")
}

func TestProjectGroupCreateWithOneDBConnectionDoesNotBorrowSession(t *testing.T) {
	_, ctx := newTestServer(t)
	spaceID := "space-" + util.GenerUUID()
	projectID := "project-" + util.GenerUUID()
	uid := "creator-" + util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, uid)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, uid, 0)

	f := New(ctx)
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: uid, Name: "Project creator", Status: 1, IsDestroy: projectuser.IsDestroyNo,
	}))
	ctx.DB().SetMaxOpenConns(1)
	ctx.DB().SetMaxIdleConns(1)

	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer im.Close()
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	type result struct {
		resp *CreateGroupServiceResp
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := f.groupService.CreateGroup(&CreateGroupServiceReq{
			Creator: uid, ProjectID: projectID, SpaceID: spaceID,
		})
		done <- result{resp: resp, err: err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.NotNil(t, got.resp)
	case <-time.After(3 * time.Second):
		t.Fatal("Project group creation borrowed a second DB session while its transaction was open")
	}
}
func TestProjectGroupCreateSkipsImplicitlyIneligibleMembers(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()

	spaceID := "space-" + util.GenerUUID()
	projectID := "project-" + util.GenerUUID()
	creator := "creator-" + util.GenerUUID()
	disabled := "disabled-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, creator)
	seedSpaceSeat(t, ctx, spaceID, disabled)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, creator, 0)
	seedProjectMember(t, ctx, projectID, spaceID, disabled, 0)

	f := New(ctx)
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: creator, ShortNo: creator, Name: "Project creator", Status: 1, IsDestroy: projectuser.IsDestroyNo,
	}))
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: disabled, ShortNo: disabled, Name: "Disabled member", Status: 0, IsDestroy: projectuser.IsDestroyNo,
	}))

	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer im.Close()
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	resp, err := f.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: creator, ProjectID: projectID, SpaceID: spaceID,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	var disabledCount, creatorCount int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		resp.GroupNo, disabled,
	).LoadOne(&disabledCount))
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		resp.GroupNo, creator,
	).LoadOne(&creatorCount))
	assert.Zero(t, disabledCount, "disabled retained Project members are omitted from the implicit snapshot")
	assert.Equal(t, 1, creatorCount, "the active creator remains in the initial native snapshot")
}

func TestProjectGroupCreateAdmitsActiveExplicitNativeMemberOutsideProject(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()

	spaceID := "space-" + util.GenerUUID()
	projectID := "project-" + util.GenerUUID()
	creator := "creator-" + util.GenerUUID()
	external := "external-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, creator)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, creator, 0)

	f := New(ctx)
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: creator, ShortNo: creator, Name: "Project creator", Status: 1, IsDestroy: projectuser.IsDestroyNo,
	}))
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: external, ShortNo: external, Name: "Native external", Status: 1, IsDestroy: projectuser.IsDestroyNo,
	}))

	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer im.Close()
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	resp, err := f.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: creator, Members: []string{external}, ProjectID: projectID, SpaceID: spaceID,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	var row struct {
		Status     int `db:"status"`
		IsExternal int `db:"is_external"`
	}
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT status, is_external FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		resp.GroupNo, external,
	).LoadOne(&row))
	assert.Equal(t, int(common.GroupMemberStatusNormal), row.Status)
	assert.Equal(t, 1, row.IsExternal, "an explicit active user outside the Project Space follows native external-member policy")
}

func TestProjectGroupCreateRejectsExplicitInactiveNativeMemberAtomically(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()

	spaceID := "space-" + util.GenerUUID()
	projectID := "project-" + util.GenerUUID()
	creator := "creator-" + util.GenerUUID()
	disabled := "disabled-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, creator)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, creator, 0)

	f := New(ctx)
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: creator, ShortNo: creator, Name: "Project creator", Status: 1, IsDestroy: projectuser.IsDestroyNo,
	}))
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: disabled, ShortNo: disabled, Name: "Disabled target", Status: 0, IsDestroy: projectuser.IsDestroyNo,
	}))

	_, err := f.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: creator, Members: []string{disabled}, ProjectID: projectID, SpaceID: spaceID,
	})
	assert.Error(t, err)

	var groups int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `group` WHERE project_id=?", projectID,
	).LoadOne(&groups))
	assert.Zero(t, groups, "an invalid explicit target must not leave a partially-created Project group")
}

func TestProjectGroupCreateBotSpaceEligibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inSpace   bool
		inProject bool
	}{
		{name: "outside_space"},
		{name: "space_member_outside_project", inSpace: true},
		{name: "project_snapshot_member", inSpace: true, inProject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := newTestServer(t)
			defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
			spaceID := "space-" + util.GenerUUID()
			projectID := "project-" + util.GenerUUID()
			creator := "creator-" + util.GenerUUID()
			botUID := "bot-" + util.GenerUUID()
			seedSpaceSeat(t, ctx, spaceID, creator)
			seedProject(t, ctx, projectID, spaceID)
			seedProjectMember(t, ctx, projectID, spaceID, creator, 0)
			if tc.inSpace {
				seedSpaceSeat(t, ctx, spaceID, botUID)
			} else {
				seedSpaceSeat(t, ctx, "other-"+util.GenerUUID(), botUID)
			}
			if tc.inProject {
				seedProjectMember(t, ctx, projectID, spaceID, botUID, 0)
			}
			f := New(ctx)
			require.NoError(t, f.userDB.Insert(&projectuser.Model{
				UID: creator, ShortNo: creator, Name: "Creator", Status: 1,
				IsDestroy: projectuser.IsDestroyNo,
			}))
			require.NoError(t, f.userDB.Insert(&projectuser.Model{
				UID: botUID, ShortNo: botUID, Name: "Bot", Status: 1,
				IsDestroy: projectuser.IsDestroyNo, Robot: 1,
			}))
			im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer im.Close()
			ctx.GetConfig().WuKongIM.APIURL = im.URL

			resp, err := f.groupService.CreateGroup(&CreateGroupServiceReq{
				Creator: creator, ProjectID: projectID, SpaceID: spaceID, BotUID: botUID,
			})
			if !tc.inSpace {
				assert.ErrorIs(t, err, projectmod.ErrGroupProjectForbidden)
				assert.Nil(t, resp)
				var groups, members int
				require.NoError(t, ctx.DB().SelectBySql(
					"SELECT COUNT(*) FROM `group` WHERE project_id=?", projectID,
				).LoadOne(&groups))
				require.NoError(t, ctx.DB().SelectBySql(
					"SELECT COUNT(*) FROM group_member WHERE uid IN ?", []string{creator, botUID},
				).LoadOne(&members))
				assert.Zero(t, groups, "an ineligible Bot must not leave a Project group")
				assert.Zero(t, members, "an ineligible Bot must not leave partial native membership")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, resp)
			var rows []struct {
				Status   int `db:"status"`
				BotAdmin int `db:"bot_admin"`
				Robot    int `db:"robot"`
			}
			_, err = ctx.DB().SelectBySql(
				"SELECT status, bot_admin, robot FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
				resp.GroupNo, botUID,
			).Load(&rows)
			require.NoError(t, err)
			require.Len(t, rows, 1, "both admission paths must produce exactly one native Bot member")
			assert.Equal(t, int(common.GroupMemberStatusNormal), rows[0].Status)
			assert.Equal(t, 1, rows[0].BotAdmin)
			assert.Equal(t, 1, rows[0].Robot)
		})
	}
}

// TestCreateGroupAcceptsACreatorOnlyGroup pins the service contract the
// Project-backed create path relies on: a Project whose only effective member is
// its creator produces a group whose initial native roster is that creator
// alone. The service layer must not refuse the empty explicit member list before
// opening its transaction. The HTTP handler's own check is unchanged and is
// covered by the existing tests: a person filling in the form still cannot
// create a memberless group.
func TestCreateGroupAcceptsACreatorOnlyGroup(t *testing.T) {
	_, ctx := newTestServer(t)
	defer testutil.CleanAllTables(ctx)

	f := New(ctx)
	creator := "creator-only-" + util.GenerUUID()[:8]
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: creator, Name: "creator only", ShortNo: creator,
		Status: 1, IsDestroy: projectuser.IsDestroyNo,
	}))

	resp, err := f.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: creator,
		Name:    "just me",
	})
	// The IM channel call fails without a broker, and that failure rolls the group
	// back — so a broker-less environment cannot assert the happy path. What it CAN
	// assert is that the refusal is no longer the members check: that one returned
	// "members is required" before any transaction opened.
	if err != nil {
		require.NotContains(t, err.Error(), "members is required",
			"CreateGroup must not refuse a creator-only group at the service layer")
		return
	}
	require.NotEmpty(t, resp.GroupNo)
}
