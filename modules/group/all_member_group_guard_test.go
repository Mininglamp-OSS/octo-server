package group

// D7 — the four group operations a project's all-member group refuses.
//
// The all-member group's roster IS the project's roster (invariant I4), so each
// of these has a project-side equivalent that must be used instead. Left open,
// "all-member" stops being true the moment any group owner clicks disband, and
// the I4 reconcile scan starts reporting a violation nothing can repair.
//
// Every refusal is asserted to land BEFORE any side effect. That is not a
// stylistic preference: groupExit calls IMRemoveSubscriber before it has even
// looked up the caller's membership, so a guard placed next to the membership
// check would unsubscribe the user from the channel and THEN refuse — leaving
// them in the group and receiving nothing, with no path that puts the
// subscription back.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// d7Fixture builds a project group in which testutil.UID is the creator and one
// other uid is an ordinary member, and optionally registers it as the project's
// all-member group.
//
// The `registered` flag is the whole point: the SAME group, differing only in
// whether the project points at it, must be refused in one case and behave
// exactly as before in the other. Without the negative half, a guard that
// refused every project group would pass.
func d7Fixture(t *testing.T, ctx *config.Context, f *Group, other string, registered bool) (groupNo, projectID string) {
	t.Helper()
	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID = util.GenerUUID()
	groupNo = util.GenerUUID()

	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: testutil.UID, Name: "owner", ShortNo: "d7_" + util.GenerUUID()[:8]}))
	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: other, Name: "other", ShortNo: "d7_" + util.GenerUUID()[:8]}))

	seedSpaceSeat(t, ctx, spaceID, testutil.UID)
	seedSpaceSeat(t, ctx, spaceID, other)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, testutil.UID, 2)
	seedProjectMember(t, ctx, projectID, spaceID, other, 0)

	seedGroupRow(t, ctx, groupNo, spaceID, projectID)
	seedGroupMemberRow(t, ctx, groupNo, testutil.UID, MemberRoleCreator)
	seedGroupMemberRow(t, ctx, groupNo, other, MemberRoleCommon)

	if registered {
		_, err := ctx.DB().UpdateBySql(
			"UPDATE `octo_project` SET all_member_group_no = ? WHERE project_id = ?",
			groupNo, projectID,
		).Exec()
		require.NoError(t, err)
	}
	return groupNo, projectID
}

func d7Do(t *testing.T, s *server.Server, method, path string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		payload = []byte(util.ToJson(body))
	}
	req, err := http.NewRequest(method, path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// liveMemberCount counts undeleted rows, so a refusal that nevertheless removed
// somebody cannot pass by returning the right status code.
func liveMemberCount(t *testing.T, ctx *config.Context, groupNo string) int {
	t.Helper()
	var n int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no = ? AND is_deleted = 0", groupNo,
	).LoadOne(&n))
	return n
}

func groupStatusOf(t *testing.T, ctx *config.Context, groupNo string) int {
	t.Helper()
	var v []int
	_, err := ctx.DB().SelectBySql("SELECT status FROM `group` WHERE group_no = ?", groupNo).Load(&v)
	require.NoError(t, err)
	require.Len(t, v, 1)
	return v[0]
}

func creatorOf(t *testing.T, ctx *config.Context, groupNo string) string {
	t.Helper()
	var v []string
	_, err := ctx.DB().SelectBySql(
		"SELECT uid FROM group_member WHERE group_no = ? AND role = ? AND is_deleted = 0",
		groupNo, MemberRoleCreator,
	).Load(&v)
	require.NoError(t, err)
	require.Len(t, v, 1)
	return v[0]
}

// TestAllMemberGroupRefusesDisband — the group ends when the project does.
func TestAllMemberGroupRefusesDisband(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo, _ := d7Fixture(t, ctx, f, "d7_other_disband", true)
	w := d7Do(t, s, http.MethodDelete, "/v1/groups/"+groupNo+"/disband", nil)

	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.all_member_group_protected", env.Error.Code,
		"body=%s", w.Body.String())
	assert.Less(t, env.Error.HTTPStatus, 500, "a refusal is a caller error: body=%s", w.Body.String())
	assert.Equal(t, GroupStatusNormal, groupStatusOf(t, ctx, groupNo),
		"the group must not be disbanded")
}

// TestAllMemberGroupRefusesExit — leaving the group means leaving the project.
//
// Also the case where placement matters most: the refusal has to precede the
// IMRemoveSubscriber this handler issues before it checks membership at all.
func TestAllMemberGroupRefusesExit(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo, _ := d7Fixture(t, ctx, f, "d7_other_exit", true)
	w := d7Do(t, s, http.MethodPost, "/v1/groups/"+groupNo+"/exit", nil)

	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.all_member_group_protected", env.Error.Code,
		"body=%s", w.Body.String())
	assert.Equal(t, 2, liveMemberCount(t, ctx, groupNo), "nobody may leave")
}

// TestAllMemberGroupRefusesMemberRemoval — kicking goes through the project.
func TestAllMemberGroupRefusesMemberRemoval(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo, _ := d7Fixture(t, ctx, f, "d7_other_remove", true)
	w := d7Do(t, s, http.MethodPost, "/v1/groups/"+groupNo+"/members_delete",
		map[string]interface{}{"members": []string{"d7_other_remove"}})

	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.all_member_group_protected", env.Error.Code,
		"body=%s", w.Body.String())
	assert.Equal(t, 2, liveMemberCount(t, ctx, groupNo), "nobody may be removed")
}

// TestAllMemberGroupRefusesOwnerTransfer — the owner follows the project's owner
// (D6), driven from the project side.
func TestAllMemberGroupRefusesOwnerTransfer(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo, _ := d7Fixture(t, ctx, f, "d7_other_transfer", true)
	w := d7Do(t, s, http.MethodPost, "/v1/groups/"+groupNo+"/transfer/d7_other_transfer", nil)

	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.all_member_group_protected", env.Error.Code,
		"body=%s", w.Body.String())
	assert.Equal(t, testutil.UID, creatorOf(t, ctx, groupNo), "the creator must not change")
}

// TestAllMemberGroupRefusesBlacklist covers the FIFTH protected action, and the
// one the guard's own comment calls the easiest to miss.
//
// Blacklisting is not named "remove" and does not go through RemoveGroupMembers —
// it flips group_member.status to Blacklist and unsubscribes from the channel.
// For invariant I4 the effect is identical to a kick: the uid is still a project
// member and no longer in the all-member group's active set, so scan B reports a
// gap that nothing repairs (the project seat is unchanged, so no cascade revisits
// it, and the admitter only runs on a fresh add).
//
// The un-blacklist half is deliberately NOT refused, and that asymmetry is the
// reason this case exists: it puts somebody BACK into the active set, which runs
// with I4 rather than against it, and blocking it would leave an already
// blacklisted member permanently stuck. An asymmetric guard is exactly the kind
// that regresses silently, so both halves are asserted.
func TestAllMemberGroupRefusesBlacklist(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo, _ := d7Fixture(t, ctx, f, "d7_other_black", true)
	w := d7Do(t, s, http.MethodPost, "/v1/groups/"+groupNo+"/blacklist/add",
		map[string]interface{}{"uids": []string{"d7_other_black"}})

	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "err.server.group.all_member_group_protected", env.Error.Code,
		"body=%s", w.Body.String())
	assert.Equal(t, "blacklist", env.Error.Details["action"],
		"the client renders the guidance from this label")
	assert.Equal(t, 2, liveMemberCount(t, ctx, groupNo),
		"nobody may leave the active set through the blacklist door either")
	assert.Equal(t, int(common.GroupMemberStatusNormal), memberStatusOf(t, ctx, groupNo, "d7_other_black"),
		"and specifically the status must not have been flipped")

	// The other direction stays open.
	w = d7Do(t, s, http.MethodPost, "/v1/groups/"+groupNo+"/blacklist/remove",
		map[string]interface{}{"uids": []string{"d7_other_black"}})
	assert.NotContains(t, w.Body.String(), "all_member_group_protected",
		"un-blacklisting puts a member BACK into the active set, which is the direction I4 "+
			"wants; refusing it would strand anyone already blacklisted: %s", w.Body.String())
}

// TestOrdinaryProjectGroupIsNotBlacklistProtected is the blacklist half of the
// negative case: the same request on a group the project does not name as its
// all-member group must go through untouched.
func TestOrdinaryProjectGroupIsNotBlacklistProtected(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo, _ := d7Fixture(t, ctx, f, "d7_other_black_plain", false)
	w := d7Do(t, s, http.MethodPost, "/v1/groups/"+groupNo+"/blacklist/add",
		map[string]interface{}{"uids": []string{"d7_other_black_plain"}})

	assert.NotContains(t, w.Body.String(), "all_member_group_protected",
		"an ordinary project group keeps the blacklist: %s", w.Body.String())
}

// TestOrdinaryProjectGroupIsNotProtected is the negative half, and it is what
// stops the guard from being "refuse every project group".
//
// The fixture is IDENTICAL except that the project does not name this group as
// its all-member group. A user's own group inside a project keeps every one of
// the four operations.
func TestOrdinaryProjectGroupIsNotProtected(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo, _ := d7Fixture(t, ctx, f, "d7_other_plain", false)
	w := d7Do(t, s, http.MethodPost, "/v1/groups/"+groupNo+"/transfer/d7_other_plain", nil)

	// Asserted on the raw body rather than through decodeEnvelope: when the guard
	// correctly stands aside the request SUCCEEDS, and a success carries no error
	// envelope to decode. The negative assertion has to work for both outcomes,
	// because the operation may still fail for unrelated reasons in a broker-less
	// environment — what it must never be is the D7 refusal.
	assert.NotContains(t, w.Body.String(), "err.server.group.all_member_group_protected",
		"an ordinary project group must keep all four operations")
}

// TestDetachedAllMemberGroupStopsBeingProtected covers the state P1's cascade
// produces and D5's predicate is written against.
//
// When a group's creator leaves the project and nobody left can inherit it, P1
// reverts the group to Space-direct and does NOT clear the project's pointer —
// modules/group may not write octo_project. If the guard trusted the project side
// alone it would keep refusing exit and disband on a group the project no longer
// owns, and its members would be stuck in it permanently.
func TestDetachedAllMemberGroupStopsBeingProtected(t *testing.T) {
	s, ctx := newTestServer(t)
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	groupNo, _ := d7Fixture(t, ctx, f, "d7_other_detached", true)
	// P1's detach: the group goes Space-direct, the project keeps pointing at it.
	_, err := ctx.DB().UpdateBySql(
		"UPDATE `group` SET project_id = '' WHERE group_no = ?", groupNo).Exec()
	require.NoError(t, err)

	w := d7Do(t, s, http.MethodDelete, "/v1/groups/"+groupNo+"/disband", nil)
	// Raw-body assertion for the same reason as the case above: standing aside
	// means the disband succeeds and there is no envelope.
	assert.NotContains(t, w.Body.String(), "err.server.group.all_member_group_protected",
		"a detached group must stop being protected, or its members are stuck in it forever")
	// Deliberately NOT asserting that the disband then succeeds. Whether it does
	// depends on the broker (disband notifies the channel), so pinning it here
	// would make this case fail for a reason that has nothing to do with the guard.
	// The claim under test is exactly "the D7 refusal does not fire", and that is
	// what the assertion above says.
}

// TestAllMemberGroupGuardTakesTheGroupRowFromItsCaller pins the C1 discipline
// that the guard's own doc comment states as a hard requirement: a Space-direct
// group must cost ZERO extra queries, and an ordinary project group at most one
// indexed point lookup.
//
// PR #855's review found the one call site that broke it. transferGrouper used a
// by-group-number variant that issued its own QueryWithGroupNo, and the very next
// statement called getGroupInfo — the same query. Space-direct groups paid +1 and
// project groups +2, against a budget of 0 and 1, and nothing caught it because
// the rule was written in a comment and asserted nowhere.
//
// A source guard rather than a query counter, deliberately: `ctx.DB()` hands out a
// fresh dbr session per call, so there is no shared EventReceiver to hook, and
// counting at the MySQL end would fold in the pool's own traffic. What actually
// regressed is the SHAPE — a guard entry point that can read the group row itself
// — so that is what this forbids. With no such entry point in the package, C1
// holds by construction instead of by every call site remembering it.
func TestAllMemberGroupGuardTakesTheGroupRowFromItsCaller(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	callSites := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		require.NoError(t, err)
		for i, line := range strings.Split(string(raw), "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			if strings.Contains(code, "refuseIfAllMemberGroupByNo(") {
				t.Errorf("%s:%d uses a guard variant that reads the group row itself. "+
					"Pass the row the handler already has (refuseIfAllMemberGroup), or, if "+
					"this call site genuinely cannot obtain one, re-add the variant together "+
					"with the reason — the extra query is the cost C1 budgets at zero for a "+
					"Space-direct group.", name, i+1)
			}
			if strings.Contains(code, "refuseIfAllMemberGroup(c,") {
				callSites++
			}
		}
	}
	require.Equal(t, 5, callSites,
		"expected the five D7 call sites (disband / exit / member removal / transfer / "+
			"blacklist); found %d. A new one is fine — cover it in this package's guard "+
			"tests and update the count. A missing one is the regression this floor exists "+
			"to catch.", callSites)
}
