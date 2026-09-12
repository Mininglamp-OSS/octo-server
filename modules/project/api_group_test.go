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
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
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

// seedGroupMemberRow writes a normal native member for relation fixtures. Native
// membership is intentionally independent from the Project relation itself, so
// callers use this only when a fixture needs a concrete group_member row.
func seedGroupMemberRow(t *testing.T, groupNo, uid string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, `version`, status, vercode, is_deleted, "+
			"invite_uid, robot, forbidden_expir_time, is_external, source_space_id, created_at) "+
			"VALUES (?, ?, 0, 1, ?, ?, 0, '', 0, 0, 0, '', NOW())",
		groupNo, uid, int(common.GroupMemberStatusNormal), util.GenerUUID(),
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

func decodeGroupList(t *testing.T, w *httptest.ResponseRecorder) []ProjectGroupRelation {
	t.Helper()
	var resp []ProjectGroupRelation
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body: %s", w.Body.String())
	return resp
}

func decodeProjectGroupRelations(t *testing.T, w *httptest.ResponseRecorder) []ProjectGroupRelation {
	t.Helper()
	var resp []ProjectGroupRelation
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body: %s", w.Body.String())
	return resp
}

func groupNosOf(list []ProjectGroupRelation) []string {
	out := make([]string, 0, len(list))
	for _, g := range list {
		out = append(out, g.GroupNo)
	}
	return out
}

// ---------- the list ----------

// TestListProjectGroupsReturnsAllAssociatedGroups verifies that relation listing
// is scoped by Project membership, not native group membership. A caller sees
// every live relation even when they hold no group_member row.
func TestListProjectGroupsReturnsAllAssociatedGroups(t *testing.T) {
	srv, _ := setup(t)
	allMember := stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-associated")

	mine := util.GenerUUID()
	theirs := util.GenerUUID()
	seedProjectGroup(t, mine, spaceA, created.ProjectID)
	seedProjectGroup(t, theirs, spaceA, created.ProjectID)
	seedGroupMemberRow(t, mine, "owner1")

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []string{allMember.groupNo, mine, theirs}, groupNosOf(decodeGroupList(t, w)),
		"the relation list must include initial provisioning and must not filter on native group membership")
	assert.Equal(t, "3", w.Header().Get("X-Total-Count"))
}

// TestListProjectGroupsExcludesLegacyBoundAIContainers keeps historical
// AI-container relations out of both the direct Project list and the Sidebar
// batch projection. The relation write guard prevents new invalid rows, but
// reads must also fail closed for rows created before that guard existed.
func TestListProjectGroupsExcludesLegacyBoundAIContainers(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerToken, "groups-no-ai-container")

	regular := util.GenerUUID()
	legacyAI := util.GenerUUID()
	seedProjectGroup(t, regular, spaceA, created.ProjectID)
	seedProjectGroup(t, legacyAI, spaceA, created.ProjectID)
	_, err := testCtx.DB().Update("group").
		Set("purpose", aiteampkg.GroupPurpose).
		Where("group_no=?", legacyAI).Exec()
	require.NoError(t, err)

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list := decodeProjectGroupRelations(t, w)
	assert.Contains(t, groupNosOf(list), regular)
	assert.NotContains(t, groupNosOf(list), legacyAI,
		"legacy AI-container relation must not leak through the direct Project list")
	assert.Equal(t, fmt.Sprint(len(list)), w.Header().Get("X-Total-Count"),
		"the count query and page query must apply the same AI-container predicate")

	sidebar, err := ListProjectGroupRelationsByProjectIDs(
		testCtx, spaceA, "owner1", []string{created.ProjectID},
	)
	require.NoError(t, err)
	sidebarGroups := sidebar[created.ProjectID]
	assert.Contains(t, groupNosOf(sidebarGroups), regular)
	assert.NotContains(t, groupNosOf(sidebarGroups), legacyAI,
		"legacy AI-container relation must not leak through the Sidebar batch list")
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
	allMember := stubAllMemberGroup(t, util.GenerUUID())
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
	assert.Equal(t, []string{allMember.groupNo, live}, groupNosOf(decodeGroupList(t, w)),
		"a disbanded group keeps its group_member rows, so only the status filter excludes it")
}

// TestListProjectGroupsIgnoresNativeMemberState verifies that a relation list
// remains stable when native group membership changes independently.
func TestListProjectGroupsIgnoresNativeMemberState(t *testing.T) {
	srv, _ := setup(t)
	allMember := stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-native-state")

	left := util.GenerUUID()
	abnormal := util.GenerUUID()
	seedProjectGroup(t, left, spaceA, created.ProjectID)
	seedProjectGroup(t, abnormal, spaceA, created.ProjectID)
	seedInactiveGroupMemberRow(t, left, "owner1", 1, 1)
	seedInactiveGroupMemberRow(t, abnormal, "owner1", 0, 0)

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []string{allMember.groupNo, left, abnormal}, groupNosOf(decodeGroupList(t, w)),
		"native group membership is independent from Project relation visibility")
}

// TestListProjectGroupsIncludesBlacklistedNativeMember verifies that native
// blacklist state does not alter Project relation metadata visibility.
func TestListProjectGroupsIncludesBlacklistedNativeMember(t *testing.T) {
	srv, _ := setup(t)
	allMember := stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-blacklist")

	banned := util.GenerUUID()
	seedProjectGroup(t, banned, spaceA, created.ProjectID)
	seedInactiveGroupMemberRow(t, banned, "owner1", 0, int(common.GroupMemberStatusBlacklist))

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []string{allMember.groupNo, banned}, groupNosOf(decodeGroupList(t, w)))
}
func TestListProjectGroupsPagesInCreationOrder(t *testing.T) {
	srv, _ := setup(t)
	allMember := stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-paging")

	// Seeded in order, so `group`.id ascends with the slice index.
	want := []string{allMember.groupNo}
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
	for page := 1; page <= 4; page++ {
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
	allMember := stubAllMemberGroup(t, util.GenerUUID())
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
	assert.Equal(t, []string{allMember.groupNo}, groupNosOf(decodeGroupList(t, w)),
		"only the initial all-member relation belongs to this project; a Space-direct group, "+
			"another project's group and a cross-Space group carrying this project's id stay out")
}

// TestListProjectGroupsDoesNotExposeNativeMemberCounts keeps the relation DTO
// free of chat membership metadata and proves the row remains visible.
func TestListProjectGroupsDoesNotExposeNativeMemberCounts(t *testing.T) {
	srv, _ := setup(t)
	allMember := stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-count")

	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	seedGroupMemberRow(t, groupNo, "owner1")
	seedInactiveGroupMemberRow(t, groupNo, "quitter", 1, 1)
	seedInactiveGroupMemberRow(t, groupNo, "banned", 0, int(common.GroupMemberStatusBlacklist))

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list := decodeGroupList(t, w)
	require.Len(t, list, 2)
	assert.Contains(t, groupNosOf(list), allMember.groupNo)
	var relation *ProjectGroupRelation
	for index := range list {
		if list[index].GroupNo == groupNo {
			relation = &list[index]
			break
		}
	}
	require.NotNil(t, relation)
	assert.Equal(t, groupNo, relation.GroupNo)
	encoded, err := json.Marshal(relation)
	require.NoError(t, err)
	var relationJSON map[string]any
	require.NoError(t, json.Unmarshal(encoded, &relationJSON))
	_, hasMemberCount := relationJSON["member_count"]
	assert.False(t, hasMemberCount,
		"relation DTOs must not smuggle native chat member counts into the Project surface")
}

// TestListProjectGroupsRefusesNonProjectMembers keeps the Project membership
// boundary from widening to Space-admin read access.
func TestListProjectGroupsRefusesNonProjectMembers(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	adminTok := seedUser(t, "spaceadmin")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceA, "spaceadmin", 1, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-nonmember")

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", adminTok, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assertProjectErrorCode(t, w, "err.server.project.not_found")
	env := decodeProjectEnvelope(t, w.Body.Bytes())
	assert.Equal(t, http.StatusNotFound, env.Error.HTTPStatus,
		"direct read refusals use D14 wire 400 with semantic 404")
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

// TestListProjectGroupsReturnsRelationMetadata verifies that the endpoint
// exposes only relation fields, including the nullable actor for legacy rows.
func TestListProjectGroupsReturnsRelationMetadata(t *testing.T) {
	srv, _ := setup(t)
	allMember := stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerTok, "groups-relation-fields")

	legacy := util.GenerUUID()
	current := util.GenerUUID()
	seedProjectGroup(t, legacy, spaceA, created.ProjectID)
	seedProjectGroup(t, current, spaceA, created.ProjectID)
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `group` SET project_linked_by = ? WHERE group_no = ?", "owner1", current,
	).Exec()
	require.NoError(t, err)

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups", ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list := decodeProjectGroupRelations(t, w)
	require.Len(t, list, 3)
	byNo := map[string]ProjectGroupRelation{}
	for _, item := range list {
		byNo[item.GroupNo] = item
	}

	require.Contains(t, byNo, legacy)
	assert.Equal(t, created.ProjectID, byNo[legacy].ProjectID)
	assert.Nil(t, byNo[legacy].LinkedBy)
	require.Contains(t, byNo, current)
	require.Contains(t, byNo, allMember.groupNo)
	assert.Equal(t, "owner1", derefProjectLinkedBy(byNo[current].LinkedBy))
}

func derefProjectLinkedBy(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
