package group

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	workspace "github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/stretchr/testify/require"
)

type workspaceCreateIMRecorder struct {
	server *httptest.Server

	mu           sync.Mutex
	channelCalls int
}

func newWorkspaceCreateIMRecorder(t *testing.T, ctx *config.Context) *workspaceCreateIMRecorder {
	t.Helper()
	recorder := &workspaceCreateIMRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/channel", func(w http.ResponseWriter, _ *http.Request) {
		recorder.mu.Lock()
		recorder.channelCalls++
		recorder.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	recorder.server = httptest.NewServer(mux)
	cfg := ctx.GetConfig()
	previous := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = recorder.server.URL
	t.Cleanup(func() {
		cfg.WuKongIM.APIURL = previous
		recorder.server.Close()
	})
	return recorder
}

func (r *workspaceCreateIMRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.channelCalls
}

type workspaceCreateSnapshotBarrier struct {
	attempt int
	release chan struct{}
}

func installWorkspaceCreateSnapshotHook(t *testing.T, hook func() workspaceCreateSnapshotBarrier) {
	t.Helper()
	previous := workspaceCreateBeforeCurrentReadHook
	workspaceCreateBeforeCurrentReadHook = func() {
		barrier := hook()
		<-barrier.release
	}
	t.Cleanup(func() {
		workspaceCreateBeforeCurrentReadHook = previous
	})
}

func receiveWorkspaceCreateBarrier(t *testing.T, entered <-chan workspaceCreateSnapshotBarrier) workspaceCreateSnapshotBarrier {
	t.Helper()
	select {
	case barrier := <-entered:
		return barrier
	case <-time.After(5 * time.Second):
		t.Fatal("workspace create did not reach the snapshot barrier")
		return workspaceCreateSnapshotBarrier{}
	}
}

func seedWorkspaceCreateExpansionFixture(t *testing.T, g *Group, ctx *config.Context, owner, spaceID string, candidates ...string) *workspace.Workspace {
	t.Helper()
	allUsers := append([]string{owner}, candidates...)
	seedGroupWorkspaceUsers(t, g, allUsers...)
	seedSpaceWithMembers(t, ctx, spaceID, allUsers...)
	ws := createContractWorkspace(t, ctx, owner, spaceID, "Create snapshot")
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO app_config (version, invite_system_account_join_group_on) VALUES (1, 1)").Exec()
	require.NoError(t, err)
	return ws
}

func addWorkspaceCreateCandidate(t *testing.T, ctx *config.Context, owner, workspaceID, uid string) {
	t.Helper()
	_, err := workspace.NewService(ctx).AddMembers(workspace.Scope{UID: owner}, workspaceID, []workspace.MemberInput{{
		UID: uid, WorkspaceRole: workspace.WorkspaceRoleMember,
	}})
	require.NoError(t, err)
}

func countWorkspaceCreateRows(t *testing.T, ctx *config.Context) (int64, int64) {
	t.Helper()
	var groups, members int64
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("`group`").LoadOne(&groups))
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("group_member").LoadOne(&members))
	return groups, members
}

func createWorkspaceGroupAfterSnapshotMutation(t *testing.T, g *Group, owner, workspaceID string, mutate func()) (*CreateGroupServiceResp, error) {
	t.Helper()
	entered := make(chan workspaceCreateSnapshotBarrier, 2)
	attempt := 0
	installWorkspaceCreateSnapshotHook(t, func() workspaceCreateSnapshotBarrier {
		attempt++
		barrier := workspaceCreateSnapshotBarrier{attempt: attempt, release: make(chan struct{})}
		if attempt > 1 {
			close(barrier.release)
		}
		entered <- barrier
		return barrier
	})
	result := make(chan struct {
		resp *CreateGroupServiceResp
		err  error
	}, 1)
	go func() {
		resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
			Creator:     owner,
			WorkspaceID: workspaceID,
		})
		result <- struct {
			resp *CreateGroupServiceResp
			err  error
		}{resp: resp, err: err}
	}()
	barrier := receiveWorkspaceCreateBarrier(t, entered)
	require.Equal(t, 1, barrier.attempt)
	mutate()
	close(barrier.release)
	select {
	case outcome := <-result:
		return outcome.resp, outcome.err
	case <-time.After(5 * time.Second):
		t.Fatal("workspace create did not complete after snapshot mutation")
		return nil, nil
	}
}

func workspaceCreateMemberUIDs(rows []*MemberModel) []string {
	uids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row != nil && row.IsDeleted == 0 {
			uids = append(uids, row.UID)
		}
	}
	return uids
}

func TestCreateGroupWorkspaceWithOneConnection(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-create-one-connection-owner"
		member  = "gw-create-one-connection-member"
		spaceID = "gw-create-one-connection-space"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, spaceID, member)
	addWorkspaceCreateCandidate(t, ctx, owner, ws.WorkspaceID, member)

	ctx.DB().SetMaxOpenConns(1)
	ctx.DB().SetMaxIdleConns(1)
	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator:     owner,
		WorkspaceID: ws.WorkspaceID,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	members, err := g.db.QueryMembersFirstNine(resp.GroupNo)
	require.NoError(t, err)
	require.Len(t, members, 2)
}

func TestCreateGroupWorkspaceExpansionExhaustsRetryBudget(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-create-expansion-owner"
		spaceID = "gw-create-expansion-space"
		first   = "gw-create-expansion-first"
		second  = "gw-create-expansion-second"
		third   = "gw-create-expansion-third"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, spaceID, first, second, third)
	recorder := newWorkspaceCreateIMRecorder(t, ctx)
	entered := make(chan workspaceCreateSnapshotBarrier)
	attempt := 0
	installWorkspaceCreateSnapshotHook(t, func() workspaceCreateSnapshotBarrier {
		attempt++
		barrier := workspaceCreateSnapshotBarrier{attempt: attempt, release: make(chan struct{})}
		entered <- barrier
		return barrier
	})

	beforeGroups, beforeMembers := countWorkspaceCreateRows(t, ctx)
	done := make(chan error, 1)
	go func() {
		_, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
			Creator:     owner,
			WorkspaceID: ws.WorkspaceID,
		})
		done <- err
	}()
	for i, uid := range []string{first, second, third} {
		barrier := receiveWorkspaceCreateBarrier(t, entered)
		require.Equal(t, i+1, barrier.attempt)
		addWorkspaceCreateCandidate(t, ctx, owner, ws.WorkspaceID, uid)
		close(barrier.release)
	}
	select {
	case err := <-done:
		require.ErrorIs(t, err, workspace.ErrDependencyUnavailable)
	case <-time.After(5 * time.Second):
		t.Fatal("workspace create did not exhaust its retry budget")
	}
	groups, members := countWorkspaceCreateRows(t, ctx)
	require.Equal(t, beforeGroups, groups)
	require.Equal(t, beforeMembers, members)
	require.Equal(t, 0, recorder.calls())
}

func TestCreateGroupWorkspaceExpansionStabilizesOnSecondAttempt(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-create-stable-owner"
		spaceID = "gw-create-stable-space"
		first   = "gw-create-stable-first"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, spaceID, first)
	recorder := newWorkspaceCreateIMRecorder(t, ctx)
	entered := make(chan workspaceCreateSnapshotBarrier)
	attempt := 0
	installWorkspaceCreateSnapshotHook(t, func() workspaceCreateSnapshotBarrier {
		attempt++
		barrier := workspaceCreateSnapshotBarrier{attempt: attempt, release: make(chan struct{})}
		entered <- barrier
		return barrier
	})

	done := make(chan error, 1)
	go func() {
		_, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
			Creator:     owner,
			WorkspaceID: ws.WorkspaceID,
		})
		done <- err
	}()
	firstBarrier := receiveWorkspaceCreateBarrier(t, entered)
	require.Equal(t, 1, firstBarrier.attempt)
	addWorkspaceCreateCandidate(t, ctx, owner, ws.WorkspaceID, first)
	close(firstBarrier.release)
	secondBarrier := receiveWorkspaceCreateBarrier(t, entered)
	require.Equal(t, 2, secondBarrier.attempt)
	close(secondBarrier.release)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("workspace create did not complete after snapshot stabilized")
	}
	require.Equal(t, 1, recorder.calls())
}

func TestWorkspaceCreateCandidateExpansionRequiresRetry(t *testing.T) {
	prepared := []string{"owner", "member-a", "member-b"}
	current := []string{"owner", "member-a", "member-b", "member-c"}

	require.True(t, workspaceCreateCandidateExpanded(prepared, current))
	require.False(t, workspaceCreateCandidateExpanded(prepared, []string{"owner", "member-a"}))
	require.False(t, workspaceCreateCandidateExpanded(prepared, []string{"member-b", "owner", "member-a"}))
}

func TestWorkspaceCreateCandidatePreparationIncludesBotAndExplicitMembers(t *testing.T) {
	got := prepareCreateGroupCandidates("owner", []string{"member-a", "member-a", ""}, "bot")
	require.Equal(t, []string{"owner", "member-a", "bot"}, got)
}
func TestCreateGroupWorkspaceJoinDuringSnapshotRetries(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-create-join-owner"
		joiner  = "gw-create-joiner"
		spaceID = "gw-create-join-space"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, spaceID, joiner)
	recorder := newWorkspaceCreateIMRecorder(t, ctx)
	entered := make(chan workspaceCreateSnapshotBarrier)
	attempt := 0
	installWorkspaceCreateSnapshotHook(t, func() workspaceCreateSnapshotBarrier {
		attempt++
		barrier := workspaceCreateSnapshotBarrier{attempt: attempt, release: make(chan struct{})}
		entered <- barrier
		return barrier
	})

	done := make(chan struct {
		resp *CreateGroupServiceResp
		err  error
	}, 1)
	go func() {
		resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
			Creator:     owner,
			WorkspaceID: ws.WorkspaceID,
		})
		done <- struct {
			resp *CreateGroupServiceResp
			err  error
		}{resp: resp, err: err}
	}()
	firstBarrier := receiveWorkspaceCreateBarrier(t, entered)
	require.Equal(t, 1, firstBarrier.attempt)
	addWorkspaceCreateCandidate(t, ctx, owner, ws.WorkspaceID, joiner)
	close(firstBarrier.release)
	secondBarrier := receiveWorkspaceCreateBarrier(t, entered)
	require.Equal(t, 2, secondBarrier.attempt)
	close(secondBarrier.release)

	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.resp)
		members, err := g.db.QueryMembersFirstNine(outcome.resp.GroupNo)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{owner, joiner}, workspaceCreateMemberUIDs(members))
	case <-time.After(5 * time.Second):
		t.Fatal("workspace create did not complete after a concurrent join")
	}
	require.Equal(t, 1, recorder.calls(), "one retry must still produce one post-commit IM call")
}

func TestCreateGroupWorkspaceLeaveDuringSnapshotExcludesMember(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-create-leave-owner"
		leaver  = "gw-create-leaver"
		spaceID = "gw-create-leave-space"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, spaceID, leaver)
	addWorkspaceCreateCandidate(t, ctx, owner, ws.WorkspaceID, leaver)

	resp, err := createWorkspaceGroupAfterSnapshotMutation(t, g, owner, ws.WorkspaceID, func() {
		_, updateErr := ctx.DB().Update("octo_workspace_member").Set("status", 0).
			Where("workspace_id=? AND uid=?", ws.WorkspaceID, leaver).Exec()
		require.NoError(t, updateErr)
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	members, err := g.db.QueryMembersFirstNine(resp.GroupNo)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{owner}, workspaceCreateMemberUIDs(members))
}

func TestCreateGroupWorkspaceDisabledAccountDuringSnapshotExcludesMember(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner    = "gw-create-disabled-owner"
		disabled = "gw-create-disabled-account"
		spaceID  = "gw-create-disabled-space"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, spaceID, disabled)
	addWorkspaceCreateCandidate(t, ctx, owner, ws.WorkspaceID, disabled)

	resp, err := createWorkspaceGroupAfterSnapshotMutation(t, g, owner, ws.WorkspaceID, func() {
		_, updateErr := ctx.DB().Update("user").Set("status", 0).Where("uid=?", disabled).Exec()
		require.NoError(t, updateErr)
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	members, err := g.db.QueryMembersFirstNine(resp.GroupNo)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{owner}, workspaceCreateMemberUIDs(members))
}

func TestCreateGroupWorkspaceSeatRevokedDuringSnapshotExcludesMember(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-create-seat-owner"
		revoked = "gw-create-seat-revoked"
		spaceID = "gw-create-seat-space"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, spaceID, revoked)
	addWorkspaceCreateCandidate(t, ctx, owner, ws.WorkspaceID, revoked)

	resp, err := createWorkspaceGroupAfterSnapshotMutation(t, g, owner, ws.WorkspaceID, func() {
		_, updateErr := ctx.DB().Update("space_member").Set("status", 0).
			Where("space_id=? AND uid=?", spaceID, revoked).Exec()
		require.NoError(t, updateErr)
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	members, err := g.db.QueryMembersFirstNine(resp.GroupNo)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{owner}, workspaceCreateMemberUIDs(members))
}

func TestCreateGroupWorkspaceAllowsValidExplicitExternalMember(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner    = "gw-create-external-owner"
		external = "gw-create-external-member"
		target   = "gw-create-external-target-space"
		source   = "gw-create-external-source-space"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, target)
	seedGroupWorkspaceUsers(t, g, external)
	seedSpaceWithMembers(t, ctx, source, external)

	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator:     owner,
		Members:     []string{external},
		WorkspaceID: ws.WorkspaceID,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	row, err := g.db.QueryWithGroupNo(resp.GroupNo)
	require.NoError(t, err)
	require.Equal(t, 1, row.IsExternalGroup)
	members, err := g.db.QueryMembersFirstNine(resp.GroupNo)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{owner, external}, workspaceCreateMemberUIDs(members))
	var externalMember *MemberModel
	for _, member := range members {
		if member != nil && member.UID == external {
			externalMember = member
			break
		}
	}
	require.NotNil(t, externalMember)
	require.Equal(t, 1, externalMember.IsExternal)
	require.Equal(t, source, externalMember.SourceSpaceID)
}

func TestCreateGroupWorkspaceInvalidExplicitAccountRejectsWholeRequest(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-create-invalid-explicit-owner"
		invalid = "gw-create-invalid-explicit-account"
		spaceID = "gw-create-invalid-explicit-space"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, spaceID)
	seedGroupWorkspaceUsers(t, g, invalid)
	_, err := ctx.DB().Update("user").Set("status", 0).Where("uid=?", invalid).Exec()
	require.NoError(t, err)
	beforeGroups, beforeMembers := countWorkspaceCreateRows(t, ctx)

	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator:     owner,
		Members:     []string{invalid},
		WorkspaceID: ws.WorkspaceID,
	})
	require.ErrorIs(t, err, workspace.ErrCandidateIneligible)
	require.Nil(t, resp)
	afterGroups, afterMembers := countWorkspaceCreateRows(t, ctx)
	require.Equal(t, beforeGroups, afterGroups)
	require.Equal(t, beforeMembers, afterMembers)
}

func TestCreateGroupWorkspaceBotInsertFailureRollsBackRowsAndIM(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner   = "gw-create-bot-failure-owner"
		member  = "gw-create-bot-failure-member"
		bot     = "gw-create-bot-failure-bot"
		spaceID = "gw-create-bot-failure-space"
	)
	ws := seedWorkspaceCreateExpansionFixture(t, g, ctx, owner, spaceID, member, bot)
	addWorkspaceCreateCandidate(t, ctx, owner, ws.WorkspaceID, member)
	_, err := ctx.DB().Update("user").Set("robot", 1).Where("uid=?", bot).Exec()
	require.NoError(t, err)
	recorder := newWorkspaceCreateIMRecorder(t, ctx)
	trigger := fmt.Sprintf("trg_gw_bot_insert_%d", time.Now().UnixNano())
	_, err = ctx.DB().InsertBySql(fmt.Sprintf(
		"CREATE TRIGGER `%s` BEFORE INSERT ON `group_member` FOR EACH ROW BEGIN IF NEW.robot = 1 THEN SIGNAL SQLSTATE '45000' SET MYSQL_ERRNO = 30002; END IF; END",
		trigger)).Exec()
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ctx.DB().Exec("DROP TRIGGER IF EXISTS `" + trigger + "`")
	})
	beforeGroups, beforeMembers := countWorkspaceCreateRows(t, ctx)

	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator:     owner,
		BotUID:      bot,
		WorkspaceID: ws.WorkspaceID,
	})
	require.Error(t, err)
	require.Nil(t, resp)
	afterGroups, afterMembers := countWorkspaceCreateRows(t, ctx)
	require.Equal(t, beforeGroups, afterGroups)
	require.Equal(t, beforeMembers, afterMembers)
	require.Equal(t, 0, recorder.calls(), "a bot insert failure must not reach IM")
}

func TestCreateRegularGroupWithOneConnection(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner  = "gw-reg-owner"
		member = "gw-reg-member"
	)
	seedGroupWorkspaceUsers(t, g, owner, member)
	ctx.DB().SetMaxOpenConns(1)
	ctx.DB().SetMaxIdleConns(1)

	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: owner,
		Members: []string{member},
		Name:    "regular one connection",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	members, err := g.db.QueryMembersFirstNine(resp.GroupNo)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{owner, member}, workspaceCreateMemberUIDs(members))
}

func TestCreateRegularGroupSkipsMissingAndDestroyedMembers(t *testing.T) {
	_, ctx, g := setupGroupWorkspaceContract(t)
	const (
		owner     = "gw-reg-filter-owner"
		active    = "gw-reg-filter-active"
		destroyed = "gw-reg-filter-destroyed"
		missing   = "gw-reg-filter-missing"
	)
	seedGroupWorkspaceUsers(t, g, owner, active, destroyed)
	_, err := ctx.DB().Update("user").Set("is_destroy", 2).Where("uid=?", destroyed).Exec()
	require.NoError(t, err)

	resp, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
		Creator: owner,
		Members: []string{active, destroyed, missing},
		Name:    "regular filtered members",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	members, err := g.db.QueryMembersFirstNine(resp.GroupNo)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{owner, active}, workspaceCreateMemberUIDs(members))
}
