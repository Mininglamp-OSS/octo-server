package group

// A11 — un-blacklist — driven through its real entry point.
//
// Restoring a native group member is governed by the group's manager policy;
// Project membership is not an additional prerequisite.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memberStatusOf reads the column A11 flips.
func memberStatusOf(t *testing.T, ctx *config.Context, groupNo, uid string) int {
	t.Helper()
	var v []int
	_, err := ctx.DB().SelectBySql(
		"SELECT status FROM group_member WHERE group_no = ? AND uid = ? AND is_deleted = 0",
		groupNo, uid).Load(&v)
	require.NoError(t, err)
	require.Len(t, v, 1, "expected exactly one live member row for %s", uid)
	return v[0]
}

func seedBlacklistedMember(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, remark, role, `version`, status, vercode, "+
			"is_deleted, invite_uid, robot, forbidden_expir_time, is_external, source_space_id, created_at) "+
			"VALUES (?, ?, '', ?, 1, ?, ?, 0, '', 0, 0, 0, '', NOW())",
		groupNo, uid, MemberRoleCommon, int(common.GroupMemberStatusBlacklist), util.GenerUUID(),
	).Exec()
	require.NoError(t, err)
}

// a11Fixture builds a Project-linked group whose manager is the test user and
// holds one blacklisted member. The target may or may not have a Project seat;
// native restore behavior is the same.
func a11Fixture(t *testing.T, ctx *config.Context, f *Group, target string, targetInProject bool) (groupNo string) {
	t.Helper()
	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	groupNo = util.GenerUUID()

	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: testutil.UID, Name: "manager", ShortNo: "a11_" + util.GenerUUID()[:8]}))
	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: target, Name: "target", ShortNo: "a11_" + util.GenerUUID()[:8]}))

	seedSpaceSeat(t, ctx, spaceID, testutil.UID)
	seedSpaceSeat(t, ctx, spaceID, target)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, testutil.UID, 0)
	if targetInProject {
		seedProjectMember(t, ctx, projectID, spaceID, target, 0)
	}

	seedGroupRow(t, ctx, groupNo, spaceID, projectID)
	seedGroupMemberRow(t, ctx, groupNo, testutil.UID, MemberRoleCreator)
	seedBlacklistedMember(t, ctx, groupNo, target)
	return groupNo
}

func postUnblacklist(t *testing.T, s *server.Server, groupNo, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost,
		"/v1/groups/"+groupNo+"/blacklist/remove",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{"uids": []string{target}}))))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// TestUnblacklistRestoresANonProjectMember drives A11's real endpoint. A
// blacklisted native member without a Project seat remains restorable.
func TestUnblacklistRestoresANonProjectMember(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)

	groupNo := a11Fixture(t, ctx, f, "a11_outsider", false)

	w := postUnblacklist(t, s, groupNo, "a11_outsider")

	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, int(common.GroupMemberStatusNormal),
		memberStatusOf(t, ctx, groupNo, "a11_outsider"),
		"native un-blacklist must not require a Project seat")
}

// TestUnblacklistStillRestoresAProjectMember keeps the native restore behavior
// working for a Project member as well.
func TestUnblacklistStillRestoresAProjectMember(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo := a11Fixture(t, ctx, f, "a11_insider", true)

	w := postUnblacklist(t, s, groupNo, "a11_insider")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	assert.Equal(t, int(common.GroupMemberStatusNormal),
		memberStatusOf(t, ctx, groupNo, "a11_insider"),
		"an active project member must still be restorable")
}

// TestSpaceDirectUnblacklistIsUnaffected covers the same native policy for a
// Space-direct group.
func TestSpaceDirectUnblacklistIsUnaffected(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	groupNo := util.GenerUUID()

	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: testutil.UID, Name: "manager", ShortNo: "a11_" + util.GenerUUID()[:8]}))
	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: "a11_plain", Name: "plain", ShortNo: "a11_" + util.GenerUUID()[:8]}))
	seedSpaceSeat(t, ctx, spaceID, testutil.UID)
	seedSpaceSeat(t, ctx, spaceID, "a11_plain")
	seedGroupRow(t, ctx, groupNo, spaceID, "") // Space-direct
	seedGroupMemberRow(t, ctx, groupNo, testutil.UID, MemberRoleCreator)
	seedBlacklistedMember(t, ctx, groupNo, "a11_plain")

	w := postUnblacklist(t, s, groupNo, "a11_plain")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, int(common.GroupMemberStatusNormal),
		memberStatusOf(t, ctx, groupNo, "a11_plain"),
		"a Space-direct group must retain its native un-blacklist behavior")
}
