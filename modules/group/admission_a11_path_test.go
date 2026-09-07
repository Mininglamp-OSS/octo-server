package group

// A11 — un-blacklist — driven through its REAL entry point.
//
// PR #844's review found that the I2 matrix passes ten entry-point label
// STRINGS to one helper that calls the funnel directly, so ten subtests
// exercised one code path. The labels prove nothing about whether ten paths call
// the funnel, with the right project_id.
//
// A11 is the one to close first, and both reviewers said so independently: it is
// the only converged path that does NOT go through admitOrRestoreMembersTx. It
// calls assertAdmissibleTx itself and then flips group_member.status back to
// Normal. A gate installed only in the admission primitives cannot see it — which
// is precisely the argument admission.go:86-93 makes for the funnel existing at
// all, made about the one path that is outside the funnel.
//
// The assertion is on group_member.status, not on the status code. A handler that
// returned 400 while having restored the row would pass a code assertion and
// break the invariant.

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

// a11Fixture builds a project group whose manager is the test user and which
// holds one blacklisted member. Whether that member is in the project is the
// variable each case sets.
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

// TestUnblacklistCannotRestoreANonProjectMember drives A11's real endpoint.
//
// The uid holds a Space seat and a group_member row that is merely blacklisted,
// and is NOT a member of the group's project. Restoring them would put an active
// member row back into a project group — I2 violated, by a handler that never
// touches an admission primitive.
func TestUnblacklistCannotRestoreANonProjectMember(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo := a11Fixture(t, ctx, f, "a11_outsider", false)

	w := postUnblacklist(t, s, groupNo, "a11_outsider")

	assert.Equal(t, int(common.GroupMemberStatusBlacklist),
		memberStatusOf(t, ctx, groupNo, "a11_outsider"),
		"un-blacklisting must NOT restore a uid who is not in the group's project; "+
			"body=%s", w.Body.String())

	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.project_member_required", env.Error.Code,
		"the refusal must surface as the code registered for it: body=%s", w.Body.String())
	assert.Less(t, env.Error.HTTPStatus, 500,
		"a refusal is a caller error: body=%s", w.Body.String())
}

// TestUnblacklistStillRestoresAProjectMember is the other half: the gate must
// refuse the outsider without breaking the feature for everyone else. Without
// this, deleting the whole handler would pass the case above.
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

// TestUnblacklistCannotRestoreASeatThatIsClosing pins the `removing = 1` half of
// the gate on this path. The seat still reads status = 1, so a check that asked
// only "is there an active row" would pass it — and the uid would be restored
// into a group whose rows the cascade is at that moment tearing down.
func TestUnblacklistCannotRestoreASeatThatIsClosing(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	groupNo := util.GenerUUID()

	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: testutil.UID, Name: "manager", ShortNo: "a11_" + util.GenerUUID()[:8]}))
	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: "a11_leaving", Name: "leaving", ShortNo: "a11_" + util.GenerUUID()[:8]}))
	seedSpaceSeat(t, ctx, spaceID, testutil.UID)
	seedSpaceSeat(t, ctx, spaceID, "a11_leaving")
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, testutil.UID, 0)
	seedProjectMember(t, ctx, projectID, spaceID, "a11_leaving", 1) // removing = 1
	seedGroupRow(t, ctx, groupNo, spaceID, projectID)
	seedGroupMemberRow(t, ctx, groupNo, testutil.UID, MemberRoleCreator)
	seedBlacklistedMember(t, ctx, groupNo, "a11_leaving")

	w := postUnblacklist(t, s, groupNo, "a11_leaving")

	assert.Equal(t, int(common.GroupMemberStatusBlacklist),
		memberStatusOf(t, ctx, groupNo, "a11_leaving"),
		"a seat at removing = 1 is not a member; un-blacklisting must refuse it: body=%s",
		w.Body.String())
}

// TestSpaceDirectUnblacklistIsUnaffected keeps the gate from becoming a
// regression on the group shape that is most of the product: with no project
// attribution the gate short-circuits before any query, and the handler behaves
// exactly as it did before P1.
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
		"a Space-direct group must be unaffected by the project gate")
}
