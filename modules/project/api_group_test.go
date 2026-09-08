package project

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// TestListProjectGroupsExcludesGroupsOutsideTheProject covers the two ways a group
// the caller IS in must still stay out of one project's list: it is Space-direct
// (an empty project_id), or it belongs to a different project in the same Space.
//
// The second half is what the space_id + project_id predicate buys. A query that
// filtered on project_id alone would still be correct here — and would stop being
// correct the moment it could not use group_space_project, whose LEADING column is
// space_id.
func TestListProjectGroupsExcludesGroupsOutsideTheProject(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-scope")
	other := createProjectVia(t, srv, spaceA, ownerTok, "groups-scope-other")

	spaceDirect := util.GenerUUID()
	otherProject := util.GenerUUID()
	seedProjectGroup(t, spaceDirect, spaceA, "")
	seedProjectGroup(t, otherProject, spaceA, other.ProjectID)
	seedGroupMemberRow(t, spaceDirect, "owner1")
	seedGroupMemberRow(t, otherProject, "owner1")

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, decodeGroupList(t, w),
		"a Space-direct group and another project's group are both groups the caller is in; "+
			"neither belongs to THIS project's list")
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
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-count")

	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	seedGroupMemberRow(t, groupNo, "owner1")
	seedGroupMemberRow(t, groupNo, "mate")
	seedInactiveGroupMemberRow(t, groupNo, "quitter", 1, 1)

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list := decodeGroupList(t, w)
	require.Len(t, list, 1)
	assert.Equal(t, 2, list[0].MemberCount, "the member who left must not be counted")
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
// It also pins the ordering. g.id ASC is creation order and the all-member group is
// by construction the project's first group, so it leads the list. That is the only
// reason the client can render 全员群 at the top without a dedicated flag.
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
		"the all-member group is the project's oldest group, so creation order puts it first — "+
			"which is what lets the client label it by comparing against all_member_group_no")
	assert.Equal(t, later, list[1].GroupNo)
}
