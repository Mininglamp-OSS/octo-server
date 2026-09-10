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

// TestAllMemberProvisioningUsesTheLockedProjectSnapshot ensures the automatic
// adapter does not reinterpret its stale seed list as explicit native additions.
// A user can lose a Project seat after the seed was prepared but before the group
// transaction starts; only the transaction's locked snapshot may seed the native
// roster. Explicit user-created Project groups use a different path and remain
// allowed to add active native users.
func TestAllMemberProvisioningUsesTheLockedProjectSnapshot(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()

	spaceID := "space-" + util.GenerUUID()
	projectID := "project-" + util.GenerUUID()
	creator := "creator-" + util.GenerUUID()
	stale := "stale-" + util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, creator)
	seedSpaceSeat(t, ctx, spaceID, stale)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, creator, 0)

	f := New(ctx)
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: creator, Name: "Project creator", ShortNo: "spc-owner-" + util.GenerUUID()[:8],
		Status: 1, IsDestroy: projectuser.IsDestroyNo,
	}))
	require.NoError(t, f.userDB.Insert(&projectuser.Model{
		UID: stale, Name: "Stale Project member", ShortNo: "spc-stale-" + util.GenerUUID()[:8],
		Status: 1, IsDestroy: projectuser.IsDestroyNo,
	}))

	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer im.Close()
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	groupNo, err := f.provisionAllMemberGroup(ctx, projectmod.AllMemberGroupSeed{
		ProjectID: projectID,
		SpaceID:   spaceID,
		Creator:   creator,
		Members:   []string{stale},
	})
	require.NoError(t, err)
	require.NotEmpty(t, groupNo)

	var creatorCount, staleCount int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		groupNo, creator,
	).LoadOne(&creatorCount))
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		groupNo, stale,
	).LoadOne(&staleCount))
	assert.Equal(t, 1, creatorCount, "the locked Project snapshot must include the active creator")
	assert.Zero(t, staleCount, "automatic all-member provisioning must ignore stale seed members")
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
