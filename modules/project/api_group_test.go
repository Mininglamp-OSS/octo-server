package project

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------- fixtures ----------

// seedInactiveGroupMemberRow writes a group_member row that must NOT count as
// membership: is_deleted = 1 (left the group) or status <> 1 (not normal).
//
// The distinction matters because modules/group's own
// queryProjectGroupNosWithActiveMember checks is_deleted ALONE. That looser
// predicate is right where it is used — a cascade whose job is to remove rows can
// over-select harmlessly — and wrong for a read. Without this fixture the two
// predicates are indistinguishable in test.
func seedInactiveGroupMemberRow(t *testing.T, groupNo, uid string, isDeleted, status int) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, `version`, is_deleted, status, vercode, robot, invite_uid) "+
			"VALUES (?, ?, 0, 1, ?, ?, ?, 0, '')",
		groupNo, uid, isDeleted, status, util.GenerUUID(),
	).Exec()
	require.NoError(t, err)
}

// disbandGroupRow flips a group to disbanded the way the group module does —
// status only, group_member rows deliberately left in place.
func disbandGroupRow(t *testing.T, groupNo string) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `group` SET status = ? WHERE group_no = ?", groupStatusDisband, groupNo,
	).Exec()
	require.NoError(t, err)
}

func decodeGroupList(t *testing.T, w *httptest.ResponseRecorder) []*GroupResp {
	t.Helper()
	var resp []*GroupResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body: %s", w.Body.String())
	return resp
}

func groupNosOf(list []*GroupResp) []string {
	out := make([]string, 0, len(list))
	for _, g := range list {
		out = append(out, g.GroupNo)
	}
	return out
}

// ---------- the list ----------

// TestListProjectGroupsReturnsOnlyMyGroups is the endpoint's whole access-control
// story in one case: the list is scoped to the caller's own membership, so a
// project member sees the project groups they are IN and nothing else.
//
// The stand-in all-member group is a free negative: stubAllMemberGroup inserts a
// real `group` row attributed to the project but its admitter is a no-op, so it is
// a project group nobody is a member of. If the handler ever widens to "every
// group in the project", this case fails on that row alone.
func TestListProjectGroupsReturnsOnlyMyGroups(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-mine")

	mine := util.GenerUUID()
	theirs := util.GenerUUID()
	seedProjectGroup(t, mine, spaceA, created.ProjectID)
	seedProjectGroup(t, theirs, spaceA, created.ProjectID)
	seedGroupMemberRow(t, mine, "owner1")

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []string{mine}, groupNosOf(decodeGroupList(t, w)),
		"the list must be the caller's own groups: a project group they hold no seat in "+
			"discloses a group name they cannot act on, and I2 is a ceiling, not a floor")
}

// TestListProjectGroupsExcludesDisbandedGroups covers the filter that is load-bearing
// rather than cosmetic.
//
// Disband flips group.status and leaves group_member rows behind on purpose — no
// endpoint cleans them up. Without the filter every project member would keep
// seeing groups disbanded months ago, and the rows that make that happen are
// expected state, not corruption.
func TestListProjectGroupsExcludesDisbandedGroups(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-disband")

	live := util.GenerUUID()
	dead := util.GenerUUID()
	seedProjectGroup(t, live, spaceA, created.ProjectID)
	seedProjectGroup(t, dead, spaceA, created.ProjectID)
	seedGroupMemberRow(t, live, "owner1")
	seedGroupMemberRow(t, dead, "owner1")
	disbandGroupRow(t, dead)

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []string{live}, groupNosOf(decodeGroupList(t, w)),
		"a disbanded group keeps its group_member rows, so only the status filter excludes it")
}

// TestListProjectGroupsExcludesInactiveMembership pins the STRICTER of the two
// membership predicates in the tree.
//
// A row with is_deleted = 1 is someone who left; a row with status <> 1 is not a
// normal member. Either one passing would put a group the caller has left back in
// their project tree.
func TestListProjectGroupsExcludesInactiveMembership(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-inactive")

	left := util.GenerUUID()
	abnormal := util.GenerUUID()
	seedProjectGroup(t, left, spaceA, created.ProjectID)
	seedProjectGroup(t, abnormal, spaceA, created.ProjectID)
	seedInactiveGroupMemberRow(t, left, "owner1", 1, 1)     // left the group
	seedInactiveGroupMemberRow(t, abnormal, "owner1", 0, 0) // not a normal member

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, decodeGroupList(t, w),
		"is_deleted = 0 AND status = 1 is the canonical active-member predicate; a read that "+
			"checks only is_deleted returns groups the caller has left")
}

// TestListProjectGroupsHidesAGroupThatBlacklistedMe pins the one case where this
// endpoint's membership predicate DIVERGES from the older group surfaces, so the
// divergence is a decision with a test behind it rather than a side effect.
//
// Blacklisting sets group_member.status = GroupMemberStatusBlacklist and leaves
// is_deleted = 0 — the blacklist branch of modules/group's
// ExistMemberActiveInternal spells that out. GET /v1/group/my filters is_deleted
// alone, so it still shows the group; this endpoint requires status = Normal, so
// it does not. That is deliberate: blacklisting is how a group denies access, and
// ExistMemberActive is the hardening line in front of group and thread reads for
// exactly this uid, so listing the group here would advertise a room the caller
// cannot open.
//
// The assertion covers both halves. Asserting only the absence would let a future
// change that ALSO broke /v1/group/my pass while destroying the property that
// makes this divergence deliberate rather than a bug.
func TestListProjectGroupsHidesAGroupThatBlacklistedMe(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-blacklist")

	banned := util.GenerUUID()
	seedProjectGroup(t, banned, spaceA, created.ProjectID)
	seedInactiveGroupMemberRow(t, banned, "owner1", 0, int(common.GroupMemberStatusBlacklist))

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, decodeGroupList(t, w),
		"a group that blacklisted the caller must not appear in their project tree: the "+
			"access gates already refuse them, so listing it advertises a room they cannot open")

	// The other half of the divergence, asserted so it cannot drift silently.
	mine := doJSON(t, srv, http.MethodGet, "/v1/group/my?space_id="+spaceA, ownerTok, nil)
	require.Equal(t, http.StatusOK, mine.Code, "body: %s", mine.Body.String())
	assert.Contains(t, mine.Body.String(), banned,
		"GET /v1/group/my filters is_deleted alone and still shows the group - if THIS "+
			"stops being true the divergence documented in listMyProjectGroups is gone and "+
			"its comment is now wrong")
}

// TestListProjectGroupsPagesInCreationOrder is the pagination CORRECTNESS case, as
// distinct from the bounds case below.
//
// Without it a swapped LIMIT/OFFSET pair ships green: they are adjacent ints with
// no compiler check, and page 1 (offset 0) reads correctly either way. It is also
// the only case that exercises the ORDER BY the module leans on for not dropping
// or duplicating rows between pages.
func TestListProjectGroupsPagesInCreationOrder(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-paging")

	// Seeded in order, so `group`.id ascends with the slice index.
	want := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		groupNo := util.GenerUUID()
		seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
		seedGroupMemberRow(t, groupNo, "owner1")
		want = append(want, groupNo)
	}

	base := "/v1/projects/" + created.ProjectID + "/groups"
	all := doJSON(t, srv, http.MethodGet, base, ownerTok, nil)
	require.Equal(t, http.StatusOK, all.Code, "body: %s", all.Body.String())
	require.Equal(t, want, groupNosOf(decodeGroupList(t, all)), "unpaged order must be by group id")

	// Page through one at a time: each page must be exactly the next element, and
	// the union must be the whole set with nothing dropped or repeated.
	var seen []string
	for page := 1; page <= 3; page++ {
		w := doJSON(t, srv, http.MethodGet,
			fmt.Sprintf("%s?page=%d&limit=1", base, page), ownerTok, nil)
		require.Equal(t, http.StatusOK, w.Code, "page %d body: %s", page, w.Body.String())
		got := groupNosOf(decodeGroupList(t, w))
		require.Len(t, got, 1, "page %d must hold exactly one row", page)
		assert.Equal(t, want[page-1], got[0],
			"page %d must be the %d-th group by creation order; a swapped LIMIT/OFFSET "+
				"pair reads correctly on page 1 and only breaks from page 2 on", page, page)
		seen = append(seen, got...)
	}
	assert.Equal(t, want, seen, "paging must cover every row exactly once")
}

// TestListProjectGroupsExcludesGroupsOutsideTheProject covers the three ways a group
// the caller IS in must still stay out of one project's list: it is Space-direct
// (an empty project_id), it belongs to a different project in the same Space, or it
// carries this project's id while sitting in a DIFFERENT Space.
//
// The third case is the only one that pins `g.space_id = ?`, and PR #861's review
// found it missing: with only the first two, deleting that predicate from the DAO
// left every case in this file green. project_id alone excludes both same-Space
// negatives, and the cross-Space case in ...RefusalsAreIndistinguishable is refused
// by the middleware before the query ever runs.
//
// It is worth pinning because nothing in the schema constrains it. `group.project_id`
// has no foreign key, so a row can carry a project id from another Space — the DAO
// calls that predicate the Space isolation boundary, and a refactor chasing a query
// plan could drop it (it is also group_space_project's leading column) with nothing
// to object.
func TestListProjectGroupsExcludesGroupsOutsideTheProject(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-scope")
	other := createProjectVia(t, srv, spaceA, ownerTok, "groups-scope-other")

	spaceDirect := util.GenerUUID()
	otherProject := util.GenerUUID()
	crossSpace := util.GenerUUID()
	seedProjectGroup(t, spaceDirect, spaceA, "")
	seedProjectGroup(t, otherProject, spaceA, other.ProjectID)
	// This project's id, another Space's group. Only the space_id predicate keeps
	// it out; seeded in raw SQL because no endpoint can produce it.
	seedProjectGroup(t, crossSpace, spaceB, created.ProjectID)
	seedGroupMemberRow(t, spaceDirect, "owner1")
	seedGroupMemberRow(t, otherProject, "owner1")
	seedGroupMemberRow(t, crossSpace, "owner1")

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, decodeGroupList(t, w),
		"a Space-direct group, another project's group and a group carrying this project's "+
			"id in another Space are all groups the caller is in; none belongs to THIS "+
			"project's list, and only g.space_id = ? excludes the third")
}

// TestListProjectGroupsCountsActiveMembersOnly pins member_count against the same
// predicate the list itself uses, so the number cannot disagree with the row it
// sits on.
func TestListProjectGroupsCountsActiveMembersOnly(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "mate")
	seedUser(t, "quitter")
	seedUser(t, "banned")
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-count")

	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	seedGroupMemberRow(t, groupNo, "owner1")
	seedGroupMemberRow(t, groupNo, "mate")
	seedInactiveGroupMemberRow(t, groupNo, "quitter", 1, 1)
	// The blacklisted row is what pins the status half of the count predicate, and
	// PR #861's review found it missing: with only the quitter, dropping
	// `AND status = ?` from countActiveGroupMembers still read 2 and still passed.
	// The blacklist case elsewhere in this file cannot cover it either — there the
	// caller is the banned one, so they get an empty list and no count is observed.
	seedInactiveGroupMemberRow(t, groupNo, "banned", 0, int(common.GroupMemberStatusBlacklist))

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list := decodeGroupList(t, w)
	require.Len(t, list, 1)
	assert.Equal(t, 2, list[0].MemberCount,
		"neither the member who left nor the blacklisted one may be counted: a count that "+
			"disagrees with the list it sits in makes the endpoint contradict ITSELF, which "+
			"is worse than the accepted cross-surface difference with QueryMemberCount")
}

// TestListProjectGroupsGivesANonMemberAnEmptyList pins the decision NOT to add a
// role gate.
//
// A Space admin can read a space_listed project's metadata without joining it. The
// roster refuses them (who is in a project is not part of its metadata), but this
// endpoint has nothing to withhold: it returns only groups the caller is already in.
// Answering 403 here would make "you are not a project member" observable on a route
// that currently discloses nothing.
func TestListProjectGroupsGivesANonMemberAnEmptyList(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	adminTok := seedUser(t, "spaceadmin")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceA, "spaceadmin", 1, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-nonmember")

	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	seedGroupMemberRow(t, groupNo, "owner1")

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", adminTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, decodeGroupList(t, w),
		"a Space admin who never joined has no groups here; the true answer is an empty "+
			"list, not a refusal")
}

// TestListProjectGroupsRefusalsAreIndistinguishable inherits projectMiddleware's
// anti-enumeration contract on the new route, and asserts the three refusals against
// EACH OTHER rather than against a status code — comparing each to 400 would pass
// even if the bodies differed, which is the whole thing being prevented.
func TestListProjectGroupsRefusalsAreIndistinguishable(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	ownerTok := seedUser(t, "owner1")
	strangerTok := seedUser(t, "stranger")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceA, "stranger", 0, 1)
	// The foreign project lives in a Space the stranger has no seat in.
	foreignOwnerTok := seedUser(t, "owner2")
	seedSpaceMember(t, spaceB, "owner2", 0, 1)
	foreign := createProjectVia(t, srv, spaceB, foreignOwnerTok, "groups-foreign")

	unlistedProject := createProjectVia(t, srv, spaceA, ownerTok, "groups-unlisted")
	upd := doJSON(t, srv, http.MethodPut, "/v1/projects/"+unlistedProject.ProjectID, ownerTok,
		map[string]any{"discoverability": DiscoverabilityUnlisted})
	require.Equal(t, http.StatusOK, upd.Code, "body: %s", upd.Body.String())

	nonexistent := doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+util.GenerUUID()+"/groups", strangerTok, nil)
	crossSpace := doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+foreign.ProjectID+"/groups", strangerTok, nil)
	unlisted := doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+unlistedProject.ProjectID+"/groups", strangerTok, nil)

	assertProjectErrorCode(t, nonexistent, "err.server.project.not_found")
	for name, w := range map[string]*httptest.ResponseRecorder{
		"cross-space": crossSpace,
		"unlisted":    unlisted,
	} {
		assert.Equal(t, nonexistent.Code, w.Code, "%s: status must match nonexistent", name)
		assert.JSONEq(t, nonexistent.Body.String(), w.Body.String(),
			"%s must be byte-identical to a nonexistent project: telling them apart is an "+
				"oracle for which project ids are real and which Space they live in", name)
	}
}

// TestListProjectGroupsPaginationIsBounded re-applies pageParams' own regression to
// the new route.
//
// `?page=9223372036854775807` used to overflow (page-1)*limit to a NEGATIVE offset,
// which MySQL rejects with 1064, which the handler then reported as an Internal 500.
// Any Space member could turn a query parameter into self-serve alert noise.
func TestListProjectGroupsPaginationIsBounded(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-page")

	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	seedGroupMemberRow(t, groupNo, "owner1")

	base := "/v1/projects/" + created.ProjectID + "/groups"
	for _, q := range []string{
		"?page=9223372036854775807",
		"?page=9223372036854775807&limit=9223372036854775807",
		"?page=-1&limit=-1",
		"?limit=100000",
	} {
		w := doJSON(t, srv, http.MethodGet, base+q, ownerTok, nil)
		require.Equal(t, http.StatusOK, w.Code, "%s must not become a 5xx: %s", q, w.Body.String())
	}

	far := doJSON(t, srv, http.MethodGet, base+"?page=1000&limit=200", ownerTok, nil)
	require.Equal(t, http.StatusOK, far.Code, "body: %s", far.Body.String())
	assert.Empty(t, decodeGroupList(t, far), "a page past the end is an empty page, not an error")
}

// TestListProjectGroupsIsOnTheAuthenticatedGroup checks the middleware chain where
// it is DECLARED, not by hammering the route until it 429s.
//
// TestAuthChainOrder makes the argument and this case inherits it: SharedUIDRateLimiter
// reads the uid AuthMiddleware puts in the context and fails OPEN without one, so a
// route mounted in the wrong order looks rate-limited and is not — and a hammer test
// passes either way, because some other limiter eventually answers. TestAuthChainOrder
// already pins that every r.Group mounts AuthMiddleware immediately followed by
// SharedUIDRateLimiter, and that no route hangs directly off the router. What is left
// to check for THIS route is the one thing those two cannot see: that it was added to
// the authenticated group rather than to a new bare one.
func TestListProjectGroupsIsOnTheAuthenticatedGroup(t *testing.T) {
	src := readStripped(t, "api.go")
	if !strings.Contains(src, `projectScoped.GET("/:project_id/groups"`) {
		t.Fatal("GET /:project_id/groups must be registered on the projectScoped group, which is " +
			"what mounts AuthMiddleware, SharedUIDRateLimiter and projectMiddleware on it")
	}
}

// TestListProjectGroupsIncludesTheAllMemberGroup runs with the REAL hooks — no
// stand-in — so it exercises the path a client actually gets: #855 provisions the
// all-member group with the project and seats the creator in it, and this endpoint
// must return it without knowing anything about it.
//
// It also exercises the ordering for the FRESH-provisioning case this test seeds,
// which is the common one. Not a guarantee: the DAO documents that position is a
// convenience and never the contract, because ensureAllMemberGroup rebuilds the
// group on a later write path with a fresh, higher id. The client labels 全员群 by
// comparing against all_member_group_no, which is right in both cases.
func TestListProjectGroupsIncludesTheAllMemberGroup(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-allmember")
	require.NotEmpty(t, created.AllMemberGroupNo,
		"the real provisioner is registered in this binary, so the create must produce a group")

	later := util.GenerUUID()
	seedProjectGroup(t, later, spaceA, created.ProjectID)
	seedGroupMemberRow(t, later, "owner1")

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list := decodeGroupList(t, w)
	require.Len(t, list, 2)
	assert.Equal(t, created.AllMemberGroupNo, list[0].GroupNo,
		"provisioned with the project, so in THIS scenario creation order puts it first; "+
			"a rebuilt group sorts wherever its new id falls, which is why the client "+
			"labels it by comparing against all_member_group_no rather than by position")
	assert.Equal(t, later, list[1].GroupNo)
}

// seedProjectGroupWithAvatar seeds a project group with the avatar columns SET, so
// the display fields can be asserted against values that are distinguishable from
// each other and from the zero value.
func seedProjectGroupWithAvatar(t *testing.T, groupNo, spaceID, projectID, name, avatarText string, avatarColor int) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, `version`, space_id, project_id, "+
			"is_named, avatar_text, avatar_color, is_upload_avatar) "+
			"VALUES (?, ?, '', 1, 1, ?, ?, 1, ?, ?, 1)",
		groupNo, name, spaceID, projectID, avatarText, avatarColor,
	).Exec()
	require.NoError(t, err)
}

// TestListProjectGroupsMapsEveryDisplayField covers the six fields the handler
// hand-maps in a struct literal, which until PR #861's review nothing asserted:
// every case went through group_no and member_count only.
//
// Two failure modes it catches, both of which ship green otherwise. A copy-paste
// slip in the literal — AvatarText: g.Name — reads plausibly and renders wrong on
// every project card. And changing AvatarColor from *int to int would turn null,
// which means "derive the colour from group_no", into 0, which is a real palette
// index: every unstyled group would silently acquire the first colour. That is
// exactly the client-side drift the field set's own comment says it exists to
// prevent, so it is worth more than a comment.
func TestListProjectGroupsMapsEveryDisplayField(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-fields")

	styled := util.GenerUUID()
	seedProjectGroupWithAvatar(t, styled, spaceA, created.ProjectID, "关键供应商来料异常", "来料", 3)
	seedGroupMemberRow(t, styled, "owner1")

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list := decodeGroupList(t, w)
	require.Len(t, list, 1)
	g := list[0]

	assert.Equal(t, styled, g.GroupNo)
	assert.Equal(t, "关键供应商来料异常", g.Name, "name must come from name, not from another column")
	assert.Equal(t, 1, g.IsNamed)
	assert.Equal(t, "来料", g.AvatarText,
		"avatar_text must come from avatar_text: a literal that reads g.Name here renders "+
			"plausibly and is wrong on every card")
	require.NotNil(t, g.AvatarColor, "a set palette index must survive the mapping")
	assert.Equal(t, 3, *g.AvatarColor)
	assert.Equal(t, 1, g.IsUploadAvatar)

	// The unset case, which is the one a type change breaks.
	plain := util.GenerUUID()
	seedProjectGroup(t, plain, spaceA, created.ProjectID)
	seedGroupMemberRow(t, plain, "owner1")

	w = doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	byNo := map[string]*GroupResp{}
	for _, item := range decodeGroupList(t, w) {
		byNo[item.GroupNo] = item
	}
	require.Contains(t, byNo, plain)
	assert.Nil(t, byNo[plain].AvatarColor,
		"an unset avatar_color must stay null — it means \"derive the colour from "+
			"group_no\", and a non-pointer field would report 0, which is a real palette index")
	assert.Empty(t, byNo[plain].AvatarText)
	assert.Zero(t, byNo[plain].IsUploadAvatar)
}
