package project

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedProjectGroupUserSettingForList(t *testing.T, spaceID, projectID, groupNo, uid string, pinned bool, pinnedAt *time.Time) {
	t.Helper()
	flag := 0
	if pinned {
		flag = 1
		require.NotNil(t, pinnedAt, "a pinned row needs its server-side sort time")
	}
	var sortAt interface{}
	if pinnedAt != nil {
		sortAt = *pinnedAt
	}
	now := time.Now().UTC()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO octo_project_group_user_setting "+
			"(space_id, project_id, group_no, uid, pinned, pinned_at, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		spaceID, projectID, groupNo, uid, flag, sortAt, now, now,
	).Exec()
	require.NoError(t, err)
}

func updateProjectGroupUserSettingForList(t *testing.T, spaceID, projectID, groupNo, uid string, pinned bool, pinnedAt *time.Time) {
	t.Helper()
	flag := 0
	if pinned {
		flag = 1
		require.NotNil(t, pinnedAt, "a pinned row needs its server-side sort time")
	}
	var sortAt interface{}
	if pinnedAt != nil {
		sortAt = *pinnedAt
	}
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE octo_project_group_user_setting SET pinned = ?, pinned_at = ?, updated_at = ? "+
			"WHERE space_id = ? AND project_id = ? AND group_no = ? AND uid = ?",
		flag, sortAt, time.Now().UTC(), spaceID, projectID, groupNo, uid,
	).Exec()
	require.NoError(t, err)
}

func projectRelationNosForPinTest(items []ProjectGroupRelation) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.GroupNo)
	}
	return out
}

func TestListProjectGroupsPinsBeforePaginationForActor(t *testing.T) {
	srv, _ := setup(t)
	allMember := stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "pin-owner")
	seedSpaceMember(t, spaceA, "pin-owner", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "groups-pin-page")

	first := util.GenerUUID()
	second := util.GenerUUID()
	last := util.GenerUUID()
	seedProjectGroup(t, first, spaceA, created.ProjectID)
	seedProjectGroup(t, second, spaceA, created.ProjectID)
	seedProjectGroup(t, last, spaceA, created.ProjectID)
	now := time.Now().UTC()
	seedProjectGroupUserSettingForList(t, spaceA, created.ProjectID, first, "pin-owner", true, timePtr(now.Add(-time.Minute)))
	seedProjectGroupUserSettingForList(t, spaceA, created.ProjectID, second, "pin-owner", true, timePtr(now))

	base := "/v1/projects/" + created.ProjectID + "/groups"
	want := []string{second, first, allMember.groupNo, last}
	var seen []string
	for page := 1; page <= len(want); page++ {
		w := doJSON(t, srv, http.MethodGet, fmt.Sprintf("%s?page=%d&limit=1", base, page), token, nil)
		require.Equal(t, http.StatusOK, w.Code, "page %d body: %s", page, w.Body.String())
		items := decodeProjectGroupRelations(t, w)
		require.Len(t, items, 1, "page %d must contain one relation", page)
		seen = append(seen, items[0].GroupNo)
		if page <= 2 {
			assert.True(t, items[0].Pinned, "page %d should be occupied by a pinned relation", page)
		}
	}
	assert.Equal(t, want, seen, "pinned relations must be ordered before LIMIT/OFFSET pagination")
	lastPage := doJSON(t, srv, http.MethodGet, base+"?page=1&limit=10", token, nil)
	require.Equal(t, http.StatusOK, lastPage.Code, "body: %s", lastPage.Body.String())
	assert.Equal(t, "4", lastPage.Header().Get("X-Total-Count"), "pinning must not change the relation count")
}

func TestListProjectGroupsPinsArePrivateToActor(t *testing.T) {
	srv, _ := setup(t)
	_, tokens, created := projectWithMembers(t, srv, "pin-other")
	first := util.GenerUUID()
	second := util.GenerUUID()
	seedProjectGroup(t, first, spaceA, created.ProjectID)
	seedProjectGroup(t, second, spaceA, created.ProjectID)

	before := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups?limit=20", tokens["pin-other"], nil)
	require.Equal(t, http.StatusOK, before.Code, "body: %s", before.Body.String())
	beforeItems := decodeProjectGroupRelations(t, before)
	seedProjectGroupUserSettingForList(t, spaceA, created.ProjectID, first, "owner1", true, timePtr(time.Now().UTC()))

	owner := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups?limit=20", tokens["owner1"], nil)
	require.Equal(t, http.StatusOK, owner.Code, "body: %s", owner.Body.String())
	ownerItems := decodeProjectGroupRelations(t, owner)
	require.NotEmpty(t, ownerItems)
	require.True(t, ownerItems[0].Pinned, "the actor's own setting must be visible")
	require.Equal(t, first, ownerItems[0].GroupNo, "the pinned group must move to the actor's front")

	after := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups?limit=20", tokens["pin-other"], nil)
	require.Equal(t, http.StatusOK, after.Code, "body: %s", after.Body.String())
	afterItems := decodeProjectGroupRelations(t, after)
	assert.Equal(t, projectRelationNosForPinTest(beforeItems), projectRelationNosForPinTest(afterItems),
		"one actor's Project-group pin must not reorder another actor's list")
	for _, item := range afterItems {
		assert.False(t, item.Pinned, "the other actor must not see another user's setting as pinned")
	}
}

func TestListProjectGroupsUnpinRestoresLegacyOrder(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "pin-unpin")
	seedSpaceMember(t, spaceA, "pin-unpin", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "groups-pin-unpin")

	first := util.GenerUUID()
	second := util.GenerUUID()
	seedProjectGroup(t, first, spaceA, created.ProjectID)
	seedProjectGroup(t, second, spaceA, created.ProjectID)
	base := "/v1/projects/" + created.ProjectID + "/groups?limit=20"
	baselineResponse := doJSON(t, srv, http.MethodGet, base, token, nil)
	require.Equal(t, http.StatusOK, baselineResponse.Code, "body: %s", baselineResponse.Body.String())
	baseline := decodeProjectGroupRelations(t, baselineResponse)

	seedProjectGroupUserSettingForList(t, spaceA, created.ProjectID, first, "pin-unpin", true, timePtr(time.Now().UTC()))
	pinnedResponse := doJSON(t, srv, http.MethodGet, base, token, nil)
	require.Equal(t, http.StatusOK, pinnedResponse.Code, "body: %s", pinnedResponse.Body.String())
	pinned := decodeProjectGroupRelations(t, pinnedResponse)
	require.NotEmpty(t, pinned)
	require.Equal(t, first, pinned[0].GroupNo)

	updateProjectGroupUserSettingForList(t, spaceA, created.ProjectID, first, "pin-unpin", false, nil)
	unPinnedResponse := doJSON(t, srv, http.MethodGet, base, token, nil)
	require.Equal(t, http.StatusOK, unPinnedResponse.Code, "body: %s", unPinnedResponse.Body.String())
	unPinned := decodeProjectGroupRelations(t, unPinnedResponse)
	assert.Equal(t, projectRelationNosForPinTest(baseline), projectRelationNosForPinTest(unPinned),
		"cancelling a pin must restore the old stable relation order")
	for _, item := range unPinned {
		assert.False(t, item.Pinned)
	}
}

func TestListProjectGroupsPinnedTimeTieUsesGroupIDOrder(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "pin-tie")
	seedSpaceMember(t, spaceA, "pin-tie", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "groups-pin-tie")

	first := util.GenerUUID()
	second := util.GenerUUID()
	seedProjectGroup(t, first, spaceA, created.ProjectID)
	seedProjectGroup(t, second, spaceA, created.ProjectID)
	tie := time.Date(2026, time.September, 11, 12, 0, 0, 123456000, time.UTC)
	seedProjectGroupUserSettingForList(t, spaceA, created.ProjectID, first, "pin-tie", true, &tie)
	seedProjectGroupUserSettingForList(t, spaceA, created.ProjectID, second, "pin-tie", true, &tie)

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups?limit=20", token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	items := decodeProjectGroupRelations(t, w)
	var pinned []string
	for _, item := range items {
		if item.Pinned {
			pinned = append(pinned, item.GroupNo)
		}
	}
	assert.Equal(t, []string{first, second}, pinned, "equal pin times must fall back to g.id ASC")

	repeat := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups?limit=20", token, nil)
	require.Equal(t, http.StatusOK, repeat.Code, "body: %s", repeat.Body.String())
	assert.Equal(t, projectRelationNosForPinTest(items), projectRelationNosForPinTest(decodeProjectGroupRelations(t, repeat)),
		"repeated reads must preserve the tie order")
}

func TestListProjectGroupsPinSettingDoesNotWidenRelationScope(t *testing.T) {
	srv, _ := setup(t)
	stubAllMemberGroup(t, util.GenerUUID())
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	token := seedUser(t, "pin-scope")
	seedSpaceMember(t, spaceA, "pin-scope", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "groups-pin-scope")

	associated := util.GenerUUID()
	foreignProject := util.GenerUUID()
	crossSpace := util.GenerUUID()
	seedProjectGroup(t, associated, spaceA, created.ProjectID)
	seedProjectGroup(t, foreignProject, spaceA, util.GenerUUID())
	seedProjectGroup(t, crossSpace, spaceB, created.ProjectID)
	when := time.Now().UTC()
	seedProjectGroupUserSettingForList(t, spaceA, created.ProjectID, associated, "pin-scope", true, timePtr(when))
	seedProjectGroupUserSettingForList(t, spaceA, created.ProjectID, foreignProject, "pin-scope", true, timePtr(when.Add(time.Minute)))
	seedProjectGroupUserSettingForList(t, spaceB, created.ProjectID, crossSpace, "pin-scope", true, timePtr(when.Add(2*time.Minute)))

	w := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/groups?limit=20", token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	items := decodeProjectGroupRelations(t, w)
	seen := projectRelationNosForPinTest(items)
	assert.Contains(t, seen, associated)
	assert.NotContains(t, seen, foreignProject, "a setting row must not create a relation")
	assert.NotContains(t, seen, crossSpace, "a setting row in another Space must not cross the relation boundary")
}

func timePtr(value time.Time) *time.Time {
	return &value
}
