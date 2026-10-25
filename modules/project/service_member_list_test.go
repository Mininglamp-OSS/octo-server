package project

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

func TestListProjectMembersInSpaceHonorsCanceledContext(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows, err := ListProjectMembersInSpace(ctx, testCtx.DB(), spaceA, "p", MemberReadPage{Limit: 50})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
}

func TestListProjectMembersInSpacePinsOneSnapshot(t *testing.T) {
	setup(t)
	seedSpace(t, spaceA, 1)
	iupProject(t, "roster-snapshot", spaceA, "owner", "Roster", "", "", 1, true)
	iupSeat(t, "roster-snapshot", "owner", spaceA, RoleOwner, MemberStatusActive, 0)
	conn, err := dbr.Open("mysql", testCtx.GetConfig().DB.MySQLAddr, nil)
	require.NoError(t, err)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	session := conn.NewSession(nil)
	_, err = session.DB.Exec("SET SESSION transaction_isolation = 'READ-COMMITTED'")
	require.NoError(t, err)
	// Change a seat after Space/Project qualification, before the count/page.
	// Inheriting READ COMMITTED would expose the new role in the in-flight page.
	seam := &iupListReadSeam{fire: func() {
		_, err := testCtx.DB().UpdateBySql("UPDATE octo_project_member SET role=? WHERE project_id=?", RoleCommon, "roster-snapshot").Exec()
		require.NoError(t, err)
	}}
	session.EventReceiver = seam
	page := MemberReadPage{Limit: 50}
	first, err := ListProjectMembersInSpace(context.Background(), session, spaceA, "roster-snapshot", page)
	require.NoError(t, err)
	require.True(t, seam.fired)
	require.Len(t, first.Members, 1)
	require.Equal(t, RoleOwner, first.Members[0].Role)
	require.Equal(t, "READ-COMMITTED", sessionIsolation(t, session))
	second, err := ListProjectMembersInSpace(context.Background(), session, spaceA, "roster-snapshot", page)
	require.NoError(t, err)
	require.Len(t, second.Members, 1)
	require.Equal(t, RoleCommon, second.Members[0].Role)
}

func TestListProjectMembersInSpaceRejectsCollationEquivalentScopeAliases(t *testing.T) {
	setup(t)
	seedSpace(t, spaceA, 1)
	iupProject(t, "roster-cafe", spaceA, "owner", "Roster", "", "", 1, true)
	for _, scope := range []struct{ space, project string }{
		{strings.ReplaceAll(spaceA, "a", "á"), "roster-cafe"},
		{spaceA, "roster-café"},
	} {
		rows, err := ListProjectMembersInSpace(context.Background(), testCtx.DB(), scope.space, scope.project, MemberReadPage{Limit: 50})
		require.ErrorIs(t, err, ErrMemberListNotFound)
		require.Nil(t, rows)
	}
}

func TestMemberReadPageParams(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  MemberReadPage
	}{
		{"", MemberReadPage{Limit: 50}},
		{"page=-1&limit=0", MemberReadPage{Limit: 50}},
		{"page=invalid&limit=invalid", MemberReadPage{Limit: 50}},
		{"page=2&limit=2", MemberReadPage{Offset: 2, Limit: 2}},
		{"page=100001&limit=201", MemberReadPage{Offset: 99999 * 200, Limit: 200}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			query, err := url.ParseQuery(tc.query)
			require.NoError(t, err)
			require.Equal(t, tc.want, MemberReadPageParams(query))
		})
	}
}
