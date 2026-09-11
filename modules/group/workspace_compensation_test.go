package group

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

func TestGroupWorkspaceCreateCompensation(t *testing.T) {
	t.Run("im failure removes all local rows", func(t *testing.T) {
		ctx, g, wsID := setupWorkspaceCreateCompensationFixture(t)
		var imCalls int32
		imFailure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&imCalls, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer imFailure.Close()
		previousURL := ctx.GetConfig().WuKongIM.APIURL
		ctx.GetConfig().WuKongIM.APIURL = imFailure.URL
		defer func() { ctx.GetConfig().WuKongIM.APIURL = previousURL }()

		_, err := g.groupService.CreateGroup(&CreateGroupServiceReq{
			Creator: "ws-comp-owner", WorkspaceID: wsID, Name: "compensated group",
		})
		require.Error(t, err)
		require.Greater(t, atomic.LoadInt32(&imCalls), int32(0), "creation must reach the real HTTP IM boundary")
		requireWorkspaceCreateRows(t, ctx, wsID, 0, 0)
	})

	t.Run("cleanup delete failure preserves coherent local transaction", func(t *testing.T) {
		ctx, g, wsID := setupWorkspaceCreateCompensationFixture(t)
		var imCalls int32
		imFailure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&imCalls, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer imFailure.Close()
		previousURL := ctx.GetConfig().WuKongIM.APIURL
		ctx.GetConfig().WuKongIM.APIURL = imFailure.URL
		defer func() { ctx.GetConfig().WuKongIM.APIURL = previousURL }()

		trigger := fmt.Sprintf("trg_ws_comp_cleanup_%d", time.Now().UnixNano())
		_, err := ctx.DB().InsertBySql(
			"CREATE TRIGGER `" + trigger + "` BEFORE DELETE ON `group` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MYSQL_ERRNO = 30001",
		).Exec()
		require.NoError(t, err)
		defer func() {
			_, _ = ctx.DB().Exec("DROP TRIGGER IF EXISTS `" + trigger + "`")
		}()

		_, err = g.groupService.CreateGroup(&CreateGroupServiceReq{
			Creator: "ws-comp-owner", WorkspaceID: wsID, Name: "retained group",
		})
		require.Error(t, err)
		require.Greater(t, atomic.LoadInt32(&imCalls), int32(0), "creation must reach the real HTTP IM boundary")
		var sqlErr *mysql.MySQLError
		require.ErrorAs(t, err, &sqlErr)
		require.EqualValues(t, 30001, sqlErr.Number)

		var rows []*Model
		_, err = ctx.DB().Select("*").From("`group`").Where("workspace_id=?", wsID).Load(&rows)
		require.NoError(t, err)
		require.Len(t, rows, 1, "failed compensation must retain the group row")
		requireWorkspaceCreateRows(t, ctx, wsID, 1, 2)
		require.NotNil(t, rows[0].WorkspaceID)
		require.Equal(t, wsID, *rows[0].WorkspaceID)
		require.NotNil(t, rows[0].WorkspaceLinkedBy)
		require.Equal(t, "ws-comp-owner", *rows[0].WorkspaceLinkedBy)

		members, err := g.db.QueryMembersFirstNine(rows[0].GroupNo)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"ws-comp-owner", "ws-comp-member"}, workspaceCompensationMemberUIDs(members))
		for _, member := range members {
			require.Equal(t, int(common.GroupMemberStatusNormal), member.Status)
			require.Equal(t, 0, member.IsDeleted)
		}
	})
}

func setupWorkspaceCreateCompensationFixture(t *testing.T) (*config.Context, *Group, string) {
	t.Helper()
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	clearGroupWorkspaceUIDBuckets(ctx)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		clearGroupWorkspaceUIDBuckets(ctx)
	})
	wireI18nRendererForGroupTest(s)
	g := New(ctx)
	spaceID := fmt.Sprintf("ws-comp-space-%d", time.Now().UnixNano())
	seedGroupWorkspaceUsers(t, g, "ws-comp-owner", "ws-comp-member")
	seedSpaceWithMembers(t, ctx, spaceID, "ws-comp-owner", "ws-comp-member")
	ws := createContractWorkspace(t, ctx, "ws-comp-owner", spaceID, "compensation Workspace")
	addContractWorkspaceMembers(t, ctx, "ws-comp-owner", ws.WorkspaceID, "ws-comp-member")
	return ctx, g, ws.WorkspaceID
}

func requireWorkspaceCreateRows(t *testing.T, ctx *config.Context, workspaceID string, wantGroups, wantMembers int64) {
	t.Helper()
	var groups int64
	_, err := ctx.DB().Select("COUNT(*)").From("`group`").Where("workspace_id=?", workspaceID).Load(&groups)
	require.NoError(t, err)
	require.Equal(t, wantGroups, groups)
	var members int64
	_, err = ctx.DB().Select("COUNT(*)").From("group_member").
		Where("uid IN (?, ?)", "ws-comp-owner", "ws-comp-member").Load(&members)
	require.NoError(t, err)
	require.Equal(t, wantMembers, members)
}

func workspaceCompensationMemberUIDs(rows []*MemberModel) []string {
	uids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row != nil && row.IsDeleted == 0 {
			uids = append(uids, row.UID)
		}
	}
	return uids
}
