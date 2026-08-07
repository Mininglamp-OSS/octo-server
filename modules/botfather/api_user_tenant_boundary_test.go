package botfather

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// Cross-Space negative tests for the uk tree, from the PR #713 review round 3.
//
// All three shapes here share one root cause: the tree's space_id injection only
// isolates a handler that READS the injected Space. u.get keys on a bare :uid and
// getGroupMessage / getThreadMessage gate on group membership alone, so for those
// the injection is a no-op and the confinement has to come from a route-local
// guard. Each test therefore runs the same fixtures twice — once with a key bound
// to the resource's Space and once with a key bound elsewhere — so a regression
// that removes the guard shows up as the second request succeeding, and a
// regression that over-blocks shows up as the first one failing.
// ===========================================================================

// insertTestGroupInSpaceWithMember creates an active group that belongs to spaceID
// and whose only member is uid. insertTestGroupWithMember leaves space_id empty,
// which exercises the "unattributed legacy group" branch rather than this one.
func insertTestGroupInSpaceWithMember(t *testing.T, ctx *config.Context, groupNo, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO `group` (group_no, name, creator, status, version, space_id) VALUES (?, ?, ?, 1, 1, ?)",
		groupNo, groupNo, uid, spaceID,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT IGNORE INTO group_member (group_no, uid, role, status, version, vercode) VALUES (?, ?, 2, 1, 1, ?)",
		groupNo, uid, util.GenerUUID(),
	).Exec()
	require.NoError(t, err)
}

// 🔴 Jerry-Xin blocker 2 / lml2468 P0 — group reads must be pinned to the key's
// bound Space, not merely to the owner's group membership.
//
// requireGroupMember passes here for BOTH requests: the owner really is an active
// member of a Space-B group. Without requireBoundSpaceGroup the Space-A key reads
// that group's messages, which breaks the per-key tenant confinement this PR
// advertises. The Space-B key is the positive control on identical fixtures.
func TestUserKeyGroupMessageIsPinnedToBoundSpace(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")

	// The owner belongs to both Spaces — that is what makes this a tenant-boundary
	// question rather than a membership question.
	spaceA := "sp_" + util.GenerUUID()[:8]
	spaceB := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceA, owner)
	insertTestSpace(t, ctx, spaceB, owner)

	groupNo := newGroupNo()
	insertTestGroupInSpaceWithMember(t, ctx, groupNo, spaceB, owner)

	const groupMsg int64 = 92001
	insertMessageRow(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), groupMsg, owner, spaceB)

	get := func(token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
			fmt.Sprintf("/v1/user/groups/%s/messages/%d", groupNo, groupMsg), token, nil))
		return w
	}

	inTenant := get(mintUserAPIKeyInSpace(t, ctx, owner, spaceB))
	require.Equal(t, http.StatusOK, inTenant.Code,
		"a key bound to the group's own Space must still read it: %s", inTenant.Body.String())
	var got syncedMessage
	require.NoError(t, json.Unmarshal(inTenant.Body.Bytes(), &got))
	assert.Equal(t, groupMsg, got.MessageID)

	crossTenant := get(mintUserAPIKeyInSpace(t, ctx, owner, spaceA))
	assert.NotEqual(t, http.StatusOK, crossTenant.Code,
		"a key bound to another Space must not read this group: %s", crossTenant.Body.String())
	assert.NotContains(t, crossTenant.Body.String(), "hello",
		"the refusal must not leak the message body")
}

// The same boundary for thread reads: a thread has no Space of its own, so its
// attribution is the parent group's and the guard is the same one (lml2468 P0
// named the thread route separately from the group route).
func TestUserKeyThreadMessageIsPinnedToBoundSpace(t *testing.T) {
	t.Setenv("DM_THREAD_ON", "true")
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")

	spaceA := "sp_" + util.GenerUUID()[:8]
	spaceB := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceA, owner)
	insertTestSpace(t, ctx, spaceB, owner)

	groupNo := newGroupNo()
	shortID := newShortID()
	insertTestGroupInSpaceWithMember(t, ctx, groupNo, spaceB, owner)
	insertTestThreadRow(t, ctx, groupNo, shortID, owner)

	channelID := threadChannelID(groupNo, shortID)
	const threadMsg int64 = 92101
	insertMessageRow(t, ctx, channelID, common.ChannelTypeCommunityTopic.Uint8(), threadMsg, owner, spaceB)

	get := func(token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
			fmt.Sprintf("/v1/user/groups/%s/threads/%s/messages/%d", groupNo, shortID, threadMsg), token, nil))
		return w
	}

	inTenant := get(mintUserAPIKeyInSpace(t, ctx, owner, spaceB))
	require.Equal(t, http.StatusOK, inTenant.Code,
		"a key bound to the parent group's Space must still read the thread: %s", inTenant.Body.String())

	crossTenant := get(mintUserAPIKeyInSpace(t, ctx, owner, spaceA))
	assert.NotEqual(t, http.StatusOK, crossTenant.Code,
		"a key bound to another Space must not read this thread: %s", crossTenant.Body.String())
	assert.NotContains(t, crossTenant.Body.String(), "hello")
}

// 🔴 Jerry-Xin blocker 1 / lml2468 P0 — /users/:uid must not resolve profiles
// outside the key's bound Space.
//
// u.get calls GetUserDetail(uid, loginUID) on the global user table and never reads
// the request Space, so without requireBoundSpaceMember a Space-A key returns a
// Space-B-only user's profile. The in-Space colleague is the positive control: the
// guard must narrow the reachable set to the bound Space's members, not disable the
// route.
func TestUserKeyUserDetailIsPinnedToBoundSpace(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	colleague := "u_" + util.GenerUUID()[:8]
	outsider := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")
	insertTestUser(t, ctx, colleague, "colleague")
	insertTestUser(t, ctx, outsider, "outsider")

	spaceA := "sp_" + util.GenerUUID()[:8]
	spaceB := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceA, owner)
	insertTestSpace(t, ctx, spaceA, colleague)
	insertTestSpace(t, ctx, spaceB, outsider)

	token := mintUserAPIKeyInSpace(t, ctx, owner, spaceA)
	get := func(uid string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, "/v1/user/users/"+uid, token, nil))
		return w
	}

	self := get(owner)
	assert.Equal(t, http.StatusOK, self.Code,
		"the key owner's own profile is not a tenant-scoped resource: %s", self.Body.String())

	peer := get(colleague)
	assert.Equal(t, http.StatusOK, peer.Code,
		"a member of the bound Space must stay resolvable: %s", peer.Body.String())

	cross := get(outsider)
	assert.NotEqual(t, http.StatusOK, cross.Code,
		"a user outside the bound Space must not resolve: %s", cross.Body.String())
	assert.NotContains(t, cross.Body.String(), "outsider",
		"the refusal must not leak the target's profile fields")
}

// 🔴 PR #713 review round 4 P1-1 — the second tenant anchor on /users/:uid.
//
// u.get reads group_no as well as :uid, and neither GetGroupMember nor IsShowShortNo
// checks whether the CALLER is in that group. So a Space-A-bound key naming a Space-B
// group used to get that group's member block for the target, including vercode
// (consumed as invite_vercode in the friend-add flow) and the external-member markers
// source_space_id / source_space_name — another Space's id and human-readable name.
//
// requireBoundSpaceMember strips the parameter, so the profile still resolves for an
// in-Space target while no group-scoped field comes back. The target here is a
// genuine Space-A colleague, which is what isolates this from the :uid gap already
// covered above: the only thing under test is group_no.
func TestUserKeyUserDetailIgnoresCallerSuppliedGroupNo(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	colleague := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")
	insertTestUser(t, ctx, colleague, "colleague")

	spaceA := "sp_" + util.GenerUUID()[:8]
	spaceB := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceA, owner)
	insertTestSpace(t, ctx, spaceA, colleague)
	insertTestSpace(t, ctx, spaceB, colleague)

	// A Space-B group the colleague belongs to and the key owner does not.
	groupNo := newGroupNo()
	insertTestGroupInSpaceWithMember(t, ctx, groupNo, spaceB, colleague)

	get := func(token, query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
			"/v1/user/users/"+colleague+query, token, nil))
		return w
	}

	token := mintUserAPIKeyInSpace(t, ctx, owner, spaceA)

	plain := get(token, "")
	require.Equal(t, http.StatusOK, plain.Code,
		"an in-Space colleague must still resolve: %s", plain.Body.String())

	withGroup := get(token, "?group_no="+groupNo)
	require.Equal(t, http.StatusOK, withGroup.Code, withGroup.Body.String())

	// The core assertion: group_no had no effect at all. This is stronger than
	// checking individual fields, and it is what the strip guarantees.
	assert.JSONEq(t, plain.Body.String(), withGroup.Body.String(),
		"group_no is stripped, so the response must be identical to the one without it")

	// Field-level backstops. vercode / join_group_* are always present in the
	// response shape (so assert on their VALUES, not on the key names), while the
	// external-member markers live one level down under the omitempty `group_member`
	// key — asserting them against the top-level map would pass whether or not a
	// group resolved, which is the same mistake as asserting on a key name.
	var got map[string]any
	require.NoError(t, json.Unmarshal(withGroup.Body.Bytes(), &got))
	assert.Empty(t, got["vercode"],
		"vercode is the friend-add invite capability; a caller-named group must not fill it")
	assert.Empty(t, got["join_group_invite_uid"])
	assert.Empty(t, got["join_group_invite_name"])
	assert.NotContains(t, got, "group_member",
		"group_member is omitempty, so no group resolving means the key is absent entirely — "+
			"its presence would mean the strip did not take effect")
	assert.NotContains(t, withGroup.Body.String(), spaceB,
		"no Space-B identifier may reach a Space-A-bound key")
}
