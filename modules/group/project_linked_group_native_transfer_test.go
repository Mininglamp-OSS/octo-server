package group

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/require"
)

// TestLegacyProjectGroupNativeOwnerTransferFollowsOrdinaryRules pins the
// contract for a historical Project-linked group: it stays an ordinary native
// group, and native group operations are authorized by the group's own roles
// alone
// (docs/specs/2026-09-10-project-prd-alignment-design.md「Project 关联群的统一语义」).
//
// The transfer moves the creator role to the target and leaves the group's
// Project relation untouched; no Project seat is required to hold or transfer
// the native creator role.
func TestLegacyProjectGroupNativeOwnerTransferFollowsOrdinaryRules(t *testing.T) {
	s, ctx := newTestServer(t)
	// The transfer opens an event before writing the roles, and this test
	// server leaves ctx.Event nil. Stub only that dispatcher: the HTTP route,
	// its authorization, and every database write stay real.
	ctx.Event = noopGroupEvent{}
	wireI18nRendererForGroupTest(s)
	defer testutil.CleanAllTables(ctx)

	f := New(ctx)

	suffix := util.GenerUUID()[:12]
	spaceID := "space-legacy-transfer-" + suffix
	projectID := "project-legacy-transfer-" + suffix
	groupNo := "group-legacy-transfer-" + suffix
	operator := testutil.UID
	target := "legacy-transfer-target-" + suffix

	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: target, Name: "legacy transfer target", ShortNo: target, Status: 1,
	}))
	seedSpaceSeat(t, ctx, spaceID, operator)
	seedSpaceSeat(t, ctx, spaceID, target)

	// The historical shape: a Project the group is linked to, the linked native
	// group, and its two native members. No Project seats are seeded on purpose:
	// native group authorization must not depend on them.
	seedProject(t, ctx, projectID, spaceID)
	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo, Name: "legacy linked group", Creator: operator,
		Status: GroupStatusNormal, Version: 1, SpaceID: spaceID, ProjectID: projectID,
	}))
	seedGroupMemberRow(t, ctx, groupNo, operator, MemberRoleCreator)
	seedGroupMemberRow(t, ctx, groupNo, target, MemberRoleCommon)

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/v1/groups/"+groupNo+"/transfer/"+target, nil)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code,
		"a native owner transfer on a historical Project group follows ordinary group rules; "+
			"body=%s", w.Body.String())

	formerOwner, err := f.db.QueryMemberWithUID(operator, groupNo)
	require.NoError(t, err)
	require.NotNil(t, formerOwner, "the former owner must stay in the group as an ordinary member")
	require.Equal(t, MemberRoleCommon, formerOwner.Role)

	newOwner, err := f.db.QueryMemberWithUID(target, groupNo)
	require.NoError(t, err)
	require.NotNil(t, newOwner)
	require.Equal(t, MemberRoleCreator, newOwner.Role)

	var linkedProject string
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT project_id FROM `group` WHERE group_no = ?", groupNo,
	).LoadOne(&linkedProject))
	require.Equal(t, projectID, linkedProject,
		"a native owner transfer must not change the group's Project relation")
}
