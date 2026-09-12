package group

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

type groupMyRoleTestRow struct {
	GroupNo string `json:"group_no"`
	Role    int    `json:"role"`
	SpaceID string `json:"space_id"`
}

func requestGroupMyRoleTest(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("token", testutil.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeGroupMyRoleTestRows(t *testing.T, rec *httptest.ResponseRecorder) []groupMyRoleTestRow {
	t.Helper()
	var rows []groupMyRoleTestRow
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows), rec.Body.String())
	return rows
}

func TestGroupMyRoleFilterAndRoleBackfill(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	_ = New(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))

	requester := testutil.UID
	spaceA := "group-my-a-" + util.GenerUUID()[:8]
	spaceB := "group-my-b-" + util.GenerUUID()[:8]
	spaceRevoked := "group-my-revoked-" + util.GenerUUID()[:8]
	seedSpaceWithMembers(t, ctx, spaceA, requester)
	seedSpaceWithMembers(t, ctx, spaceB, requester)
	seedSpaceWithMembers(t, ctx, spaceRevoked, requester)

	ownerA := util.GenerUUID()
	adminA := util.GenerUUID()
	memberA := util.GenerUUID()
	ownerB := util.GenerUUID()
	disbandedA := util.GenerUUID()
	blacklistedA := util.GenerUUID()
	externalA := util.GenerUUID()
	revoked := util.GenerUUID()
	for _, groupNo := range []string{ownerA, adminA, memberA, disbandedA, blacklistedA, externalA} {
		seedGroupRow(t, ctx, groupNo, spaceA, "")
	}
	seedGroupRow(t, ctx, ownerB, spaceB, "")
	seedGroupRow(t, ctx, revoked, spaceRevoked, "")

	seedGroupMemberRow(t, ctx, ownerA, requester, MemberRoleCreator)
	seedGroupMemberRow(t, ctx, adminA, requester, MemberRoleManager)
	seedGroupMemberRow(t, ctx, memberA, requester, MemberRoleCommon)
	seedGroupMemberRow(t, ctx, ownerB, requester, MemberRoleCreator)
	seedGroupMemberRow(t, ctx, disbandedA, requester, MemberRoleCreator)
	seedGroupMemberRow(t, ctx, blacklistedA, requester, MemberRoleCreator)
	seedGroupMemberRow(t, ctx, externalA, requester, MemberRoleCreator)
	seedGroupMemberRow(t, ctx, revoked, requester, MemberRoleCreator)

	_, err := ctx.DB().Update("group").Set("status", GroupStatusDisband).
		Where("group_no=?", disbandedA).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().Update("group_member").Set("status", int(common.GroupMemberStatusBlacklist)).
		Where("group_no=? AND uid=?", blacklistedA, requester).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().Update("group_member").Set("is_external", 1).
		Set("source_space_id", spaceB).
		Where("group_no=? AND uid=?", externalA, requester).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().Update("space_member").Set("status", 0).
		Where("space_id=? AND uid=?", spaceRevoked, requester).Exec()
	require.NoError(t, err)

	handler := s.GetRoute()
	managed := requestGroupMyRoleTest(t, handler, "/v1/group/my?role=owner,admin")
	require.Equal(t, http.StatusOK, managed.Code, managed.Body.String())
	managedRows := decodeGroupMyRoleTestRows(t, managed)
	require.Len(t, managedRows, 3)
	managedRoles := make(map[string]int, len(managedRows))
	managedSpaces := make(map[string]string, len(managedRows))
	for _, row := range managedRows {
		managedRoles[row.GroupNo] = row.Role
		managedSpaces[row.GroupNo] = row.SpaceID
	}
	require.Equal(t, MemberRoleCreator, managedRoles[ownerA])
	require.Equal(t, MemberRoleManager, managedRoles[adminA])
	require.Equal(t, MemberRoleCreator, managedRoles[ownerB])
	require.Equal(t, spaceA, managedSpaces[ownerA])
	require.Equal(t, spaceB, managedSpaces[ownerB])
	for _, excluded := range []string{memberA, disbandedA, blacklistedA, externalA, revoked} {
		require.NotContains(t, managedRoles, excluded)
	}

	managedA := requestGroupMyRoleTest(t, handler, "/v1/group/my?role=owner,admin&space_id="+spaceA)
	require.Equal(t, http.StatusOK, managedA.Code, managedA.Body.String())
	managedARows := decodeGroupMyRoleTestRows(t, managedA)
	require.Len(t, managedARows, 2)
	managedARoles := make(map[string]int, len(managedARows))
	for _, row := range managedARows {
		managedARoles[row.GroupNo] = row.Role
		require.Equal(t, spaceA, row.SpaceID)
	}
	require.Equal(t, MemberRoleCreator, managedARoles[ownerA])
	require.Equal(t, MemberRoleManager, managedARoles[adminA])

	revokedSpace := requestGroupMyRoleTest(t, handler, "/v1/group/my?role=owner&space_id="+spaceRevoked)
	require.Equal(t, http.StatusBadRequest, revokedSpace.Code, revokedSpace.Body.String())

	invalid := requestGroupMyRoleTest(t, handler, "/v1/group/my?role=owner,,admin")
	require.Equal(t, http.StatusBadRequest, invalid.Code, invalid.Body.String())

	for _, groupNo := range []string{ownerA, adminA, memberA} {
		_, err = ctx.DB().InsertInto("group_setting").
			Columns("group_no", "uid", "save", "version").
			Values(groupNo, requester, 1, 1).Exec()
		require.NoError(t, err)
	}
	saved := requestGroupMyRoleTest(t, handler, "/v1/group/my")
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	savedRows := decodeGroupMyRoleTestRows(t, saved)
	require.Len(t, savedRows, 3)
	savedRoles := make(map[string]int, len(savedRows))
	for _, row := range savedRows {
		savedRoles[row.GroupNo] = row.Role
	}
	require.Equal(t, MemberRoleCreator, savedRoles[ownerA])
	require.Equal(t, MemberRoleManager, savedRoles[adminA])
	require.Equal(t, MemberRoleCommon, savedRoles[memberA])

	spaceMode := requestGroupMyRoleTest(t, handler, "/v1/group/my?space_id="+spaceA)
	require.Equal(t, http.StatusOK, spaceMode.Code, spaceMode.Body.String())
	spaceRows := decodeGroupMyRoleTestRows(t, spaceMode)
	spaceRoles := make(map[string]int, len(spaceRows))
	for _, row := range spaceRows {
		spaceRoles[row.GroupNo] = row.Role
	}
	require.Equal(t, MemberRoleCreator, spaceRoles[ownerA])
	require.Equal(t, MemberRoleManager, spaceRoles[adminA])
	require.Equal(t, MemberRoleCommon, spaceRoles[memberA])
}
