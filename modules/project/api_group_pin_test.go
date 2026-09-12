package project

import (
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type projectGroupPinWriteRow struct {
	Pinned    int        `db:"pinned"`
	PinnedAt  *time.Time `db:"pinned_at"`
	UpdatedAt time.Time  `db:"updated_at"`
}

func readProjectGroupPinWriteRow(t *testing.T, spaceID, projectID, groupNo, uid string) (*projectGroupPinWriteRow, bool) {
	t.Helper()
	var rows []projectGroupPinWriteRow
	_, err := testCtx.DB().SelectBySql(
		"SELECT pinned, pinned_at, updated_at FROM octo_project_group_user_setting "+
			"WHERE space_id = ? AND project_id = ? AND group_no = ? AND uid = ?",
		spaceID, projectID, groupNo, uid,
	).Load(&rows)
	require.NoError(t, err)
	if len(rows) == 0 {
		return nil, false
	}
	return &rows[0], true
}

func TestUpdateProjectGroupSettingAllowsProjectMemberWithoutNativeMembership(t *testing.T) {
	srv, _ := setup(t)
	_, tokens, created := projectWithMembers(t, srv, "viewer")
	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/groups/"+groupNo+"/setting",
		tokens["viewer"], map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	list := doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/groups", tokens["viewer"], nil)
	require.Equal(t, http.StatusOK, list.Code, "body: %s", list.Body.String())
	var pinned bool
	for _, item := range decodeProjectGroupRelations(t, list) {
		if item.GroupNo == groupNo {
			pinned = item.Pinned
			break
		}
	}
	assert.True(t, pinned, "a Project member may pin relation metadata without native group membership")

	chat := doJSON(t, srv, http.MethodGet, "/v1/groups/"+groupNo, tokens["viewer"], nil)
	require.Equal(t, http.StatusBadRequest, chat.Code, "body: %s", chat.Body.String())
	assert.Contains(t, chat.Body.String(), "err.server.group.view_forbidden",
		"pinning must not grant native group chat access")

	var nativeRows int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no = ? AND uid = ?",
		groupNo, "viewer",
	).LoadOne(&nativeRows))
	assert.Zero(t, nativeRows, "pinning must not create a group_member row")
}

func TestUpdateProjectGroupSettingRepeatedPinKeepsTimestamp(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerToken, "group-pin-idempotent")
	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	path := "/v1/projects/" + created.ProjectID + "/groups/" + groupNo + "/setting"

	w := doJSON(t, srv, http.MethodPut, path, ownerToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	first, ok := readProjectGroupPinWriteRow(t, spaceA, created.ProjectID, groupNo, "owner1")
	require.True(t, ok)
	require.Equal(t, 1, first.Pinned)
	require.NotNil(t, first.PinnedAt)

	w = doJSON(t, srv, http.MethodPut, path, ownerToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	second, ok := readProjectGroupPinWriteRow(t, spaceA, created.ProjectID, groupNo, "owner1")
	require.True(t, ok)
	assert.Equal(t, first.PinnedAt, second.PinnedAt,
		"repeated pin must not refresh pinned_at")
	assert.Equal(t, first.UpdatedAt, second.UpdatedAt,
		"repeated pin must not refresh updated_at")

	w = doJSON(t, srv, http.MethodPut, path, ownerToken, map[string]any{"pinned": false})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	cleared, ok := readProjectGroupPinWriteRow(t, spaceA, created.ProjectID, groupNo, "owner1")
	require.True(t, ok)
	assert.Equal(t, 0, cleared.Pinned)
	assert.Nil(t, cleared.PinnedAt, "cancelling a pin clears its sort timestamp")

	w = doJSON(t, srv, http.MethodPut, path, ownerToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	repinned, ok := readProjectGroupPinWriteRow(t, spaceA, created.ProjectID, groupNo, "owner1")
	require.True(t, ok)
	assert.Equal(t, 1, repinned.Pinned)
	assert.NotNil(t, repinned.PinnedAt, "false-to-true must receive a new server timestamp")
}
func TestUpdateProjectGroupSettingRepairsLegacyPinnedTimestamp(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerToken, "group-pin-legacy-null")
	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)

	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO octo_project_group_user_setting "+
			"(space_id, project_id, group_no, uid, pinned, pinned_at, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 1, NULL, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		spaceA, created.ProjectID, groupNo, "owner1",
	).Exec()
	require.NoError(t, err)

	path := "/v1/projects/" + created.ProjectID + "/groups/" + groupNo + "/setting"
	w := doJSON(t, srv, http.MethodPut, path, ownerToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	repaired, ok := readProjectGroupPinWriteRow(t, spaceA, created.ProjectID, groupNo, "owner1")
	require.True(t, ok)
	assert.Equal(t, 1, repaired.Pinned)
	assert.NotNil(t, repaired.PinnedAt,
		"repeating pin must repair a legacy pinned row whose timestamp is NULL")
}

func TestUpdateProjectGroupSettingRejectsInvalidBodyAndRelations(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	ownerToken := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerToken, "group-pin-validation")

	validGroup := util.GenerUUID()
	unboundGroup := util.GenerUUID()
	disbandedGroup := util.GenerUUID()
	crossSpaceGroup := util.GenerUUID()
	seedProjectGroup(t, validGroup, spaceA, created.ProjectID)
	seedProjectGroup(t, unboundGroup, spaceA, "")
	seedProjectGroup(t, disbandedGroup, spaceA, created.ProjectID)
	disbandGroupRow(t, disbandedGroup)
	seedProjectGroup(t, crossSpaceGroup, spaceB, created.ProjectID)

	path := func(groupNo string) string {
		return "/v1/projects/" + created.ProjectID + "/groups/" + groupNo + "/setting"
	}
	for _, body := range []map[string]any{
		{},
		{"pinned": nil},
		{"pinned": "true"},
	} {
		w := doJSON(t, srv, http.MethodPut, path(validGroup), ownerToken, body)
		require.Equal(t, http.StatusBadRequest, w.Code, "body=%v response=%s", body, w.Body.String())
		assertProjectErrorCode(t, w, "err.server.project.request_invalid")
	}

	for _, groupNo := range []string{unboundGroup, disbandedGroup, crossSpaceGroup} {
		w := doJSON(t, srv, http.MethodPut, path(groupNo), ownerToken, map[string]any{"pinned": true})
		require.Equal(t, http.StatusBadRequest, w.Code, "group=%s body=%s", groupNo, w.Body.String())
		assertProjectErrorCode(t, w, "err.server.project.not_found")
		_, exists := readProjectGroupPinWriteRow(t, spaceA, created.ProjectID, groupNo, "owner1")
		assert.False(t, exists, "a refused relation must not leave a preference row")
	}
}

func TestUpdateProjectGroupSettingDoesNotCrossProject(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	first := createProjectVia(t, srv, spaceA, ownerToken, "group-pin-first")
	second := createProjectVia(t, srv, spaceA, ownerToken, "group-pin-second")
	firstGroup := util.GenerUUID()
	secondGroup := util.GenerUUID()
	seedProjectGroup(t, firstGroup, spaceA, first.ProjectID)
	seedProjectGroup(t, secondGroup, spaceA, second.ProjectID)

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+first.ProjectID+"/groups/"+firstGroup+"/setting",
		ownerToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	otherProject := doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+second.ProjectID+"/groups", ownerToken, nil)
	require.Equal(t, http.StatusOK, otherProject.Code, "body: %s", otherProject.Body.String())
	items := decodeProjectGroupRelations(t, otherProject)
	var otherPinned bool
	for _, item := range items {
		if item.GroupNo == secondGroup {
			otherPinned = item.Pinned
			break
		}
	}
	assert.False(t, otherPinned, "a preference in one Project must not affect another Project")
}

func TestUpdateProjectGroupSettingRejectsNonProjectMember(t *testing.T) {
	srv, _ := setup(t)
	_, _, created := projectWithMembers(t, srv)
	outsiderToken := seedUser(t, "pin-outsider")
	seedSpaceMember(t, spaceA, "pin-outsider", 0, 1)
	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)

	w := doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/groups/"+groupNo+"/setting",
		outsiderToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assertProjectErrorCode(t, w, "err.server.project.not_found")

	var rows int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_project_group_user_setting "+
			"WHERE space_id = ? AND project_id = ? AND group_no = ? AND uid = ?",
		spaceA, created.ProjectID, groupNo, "pin-outsider",
	).LoadOne(&rows))
	assert.Zero(t, rows, "a non-Project member refusal must not write a preference row")
}

func TestUpdateProjectGroupSettingRetainsPreferenceAcrossUnbindAndRebind(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv)
	groupNo := seedProjectGroupWithMember(t, spaceA, created.ProjectID, "native-member")
	pinPath := "/v1/projects/" + created.ProjectID + "/groups/" + groupNo + "/setting"
	relationPath := "/v1/groups/" + groupNo + "/project"
	listPath := "/v1/projects/" + created.ProjectID + "/groups?limit=20"

	w := doJSON(t, srv, http.MethodPut, pinPath, ownerToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, w.Code, "pin body: %s", w.Body.String())
	w = doJSON(t, srv, http.MethodDelete, relationPath, ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "unbind body: %s", w.Body.String())

	w = doJSON(t, srv, http.MethodGet, listPath, ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "hidden list body: %s", w.Body.String())
	for _, item := range decodeProjectGroupRelations(t, w) {
		assert.NotEqual(t, groupNo, item.GroupNo, "an unbound group must be hidden from its old Project")
	}

	w = doJSON(t, srv, http.MethodPut, relationPath, ownerToken,
		map[string]any{"project_id": created.ProjectID})
	require.Equal(t, http.StatusOK, w.Code, "rebind body: %s", w.Body.String())
	w = doJSON(t, srv, http.MethodGet, listPath, ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "restored list body: %s", w.Body.String())
	var restored bool
	for _, item := range decodeProjectGroupRelations(t, w) {
		if item.GroupNo == groupNo {
			restored = item.Pinned
		}
	}
	assert.True(t, restored, "rebinding the same Project must restore the retained personal pin")
}
