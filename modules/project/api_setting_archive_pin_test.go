package project

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type projectUserSettingStateForTest struct {
	Pinned                        sql.NullInt64 `db:"pinned"`
	PinnedAt                      sql.NullTime  `db:"pinned_at"`
	ArchiveNoticeDismissedVersion int64         `db:"archive_notice_dismissed_version"`
}

func archiveAddExistingSpaceMember(t *testing.T, f *archiveAPIFixture, projectID, uid string) {
	t.Helper()
	w := doOn(t, f.route, http.MethodPost,
		"/v1/projects/"+projectID+"/members/add", f.ownerToken, addMembersPayload(uid))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), w.Body.String())
	require.Len(t, outcomes, 1)
	require.True(t, outcomes[0].OK, outcomes[0].Reason)
}

func putProjectSetting(t *testing.T, f *archiveAPIFixture, projectID, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doOn(t, f.route, http.MethodPut, "/v1/projects/"+projectID+"/setting", token, body)
}

func projectUserSettingForTest(t *testing.T, projectID, uid string) projectUserSettingStateForTest {
	t.Helper()
	var rows []projectUserSettingStateForTest
	_, err := testCtx.DB().SelectBySql(
		"SELECT pinned, pinned_at, archive_notice_dismissed_version "+
			"FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		projectID, uid,
	).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0]
}

func projectIDsInSpaceForTest(t *testing.T, f *archiveAPIFixture, token, query string) []string {
	t.Helper()
	w := doOn(t, f.route, http.MethodGet, "/v1/space/"+spaceA+"/projects"+query, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return archiveListProjectIDs(t, archiveList(t, w))
}

func TestArchivedPinnedProjectCanBeUnpinnedToFreeQuota(t *testing.T) {
	f := archiveAPISetup(t)
	f.p.cfg.MaxPinned = 2
	projects := []*Resp{
		f.createBoundProject(t, "archive-pin-cap-a"),
		f.createBoundProject(t, "archive-pin-cap-b"),
		f.createBoundProject(t, "archive-pin-cap-active"),
		f.createBoundProject(t, "archive-pin-cap-spare"),
	}
	uid := "archive-pin-cap-member"
	memberToken := seedUser(t, uid)
	seedSpaceMember(t, spaceA, uid, 0, 1)
	for _, project := range projects {
		archiveAddExistingSpaceMember(t, f, project.ProjectID, uid)
	}

	for _, project := range projects[:2] {
		w := putProjectSetting(t, f, project.ProjectID, memberToken, map[string]any{"pinned": true})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	assert.Equal(t, 2, countPinnedForTest(t, spaceA, "archive-pin-cap-member"))

	for _, project := range projects[:2] {
		w := archiveProject(t, f, project.ProjectID, f.ownerToken, true)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	archivedDetail := doOn(t, f.route, http.MethodGet,
		"/v1/projects/"+projects[0].ProjectID, memberToken, nil)
	require.Equal(t, http.StatusOK, archivedDetail.Code, archivedDetail.Body.String())
	assert.True(t, decodeResp(t, archivedDetail).Pinned,
		"archiving does not erase a member's existing preference")
	assert.ElementsMatch(t, []string{projects[0].ProjectID, projects[1].ProjectID},
		projectIDsInSpaceForTest(t, f, memberToken, "?archive_status=archived"),
		"members retain archived-project reads")

	archivedRepin := putProjectSetting(t, f, projects[0].ProjectID, memberToken, map[string]any{"pinned": true})
	assertProjectErrorCode(t, archivedRepin, "err.server.project.archived")
	atCap := putProjectSetting(t, f, projects[2].ProjectID, memberToken, map[string]any{"pinned": true})
	assertProjectErrorCode(t, atCap, "err.server.project.quota_pinned")

	unpin := putProjectSetting(t, f, projects[0].ProjectID, memberToken, map[string]any{"pinned": false})
	require.Equal(t, http.StatusOK, unpin.Code, unpin.Body.String())
	assert.False(t, decodeResp(t, unpin).Pinned)
	pinActive := putProjectSetting(t, f, projects[2].ProjectID, memberToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, pinActive.Code, pinActive.Body.String())

	restore := archiveProject(t, f, projects[0].ProjectID, f.ownerToken, false)
	require.Equal(t, http.StatusOK, restore.Code, restore.Body.String())
	restoredDetail := doOn(t, f.route, http.MethodGet,
		"/v1/projects/"+projects[0].ProjectID, memberToken, nil)
	require.Equal(t, http.StatusOK, restoredDetail.Code, restoredDetail.Body.String())
	assert.False(t, decodeResp(t, restoredDetail).Pinned,
		"restoring a project must not resurrect the preference explicitly cleared while archived")
	assert.Equal(t, 2, countPinnedForTest(t, spaceA, "archive-pin-cap-member"))

	stillAtCap := putProjectSetting(t, f, projects[3].ProjectID, memberToken, map[string]any{"pinned": true})
	assertProjectErrorCode(t, stillAtCap, "err.server.project.quota_pinned")
	spareDetail := doOn(t, f.route, http.MethodGet,
		"/v1/projects/"+projects[3].ProjectID, memberToken, nil)
	require.Equal(t, http.StatusOK, spareDetail.Code, spareDetail.Body.String())
	assert.False(t, decodeResp(t, spareDetail).Pinned, "a refused extra pin must not land")
}

func TestRepeatedProjectPinPreservesPinnedAtAndListOrder(t *testing.T) {
	f := archiveAPISetup(t)
	first := f.createBoundProject(t, "repeat-project-pin-a")
	second := f.createBoundProject(t, "repeat-project-pin-b")
	for _, project := range []*Resp{first, second} {
		w := putProjectSetting(t, f, project.ProjectID, f.ownerToken, map[string]any{"pinned": true})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	firstPinnedAt := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	secondPinnedAt := time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		projectID string
		pinnedAt  time.Time
	}{{first.ProjectID, firstPinnedAt}, {second.ProjectID, secondPinnedAt}} {
		_, err := testCtx.DB().UpdateBySql(
			"UPDATE octo_project_user_setting SET pinned_at = ? WHERE project_id = ? AND uid = ?",
			tc.pinnedAt, tc.projectID, "owner1",
		).Exec()
		require.NoError(t, err)
	}

	before := projectUserSettingForTest(t, first.ProjectID, "owner1")
	require.True(t, before.PinnedAt.Valid)
	beforeOrder := projectIDsInSpaceForTest(t, f, f.ownerToken, "")
	require.Equal(t, []string{second.ProjectID, first.ProjectID}, beforeOrder)

	repeat := putProjectSetting(t, f, first.ProjectID, f.ownerToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, repeat.Code, repeat.Body.String())
	after := projectUserSettingForTest(t, first.ProjectID, "owner1")
	require.True(t, after.PinnedAt.Valid)
	assert.True(t, before.PinnedAt.Time.Equal(after.PinnedAt.Time),
		"repeating pinned=true must not refresh the original pin time")
	assert.Equal(t, beforeOrder, projectIDsInSpaceForTest(t, f, f.ownerToken, ""),
		"a duplicate pin must not reorder an older pin ahead of a newer one")
}

func TestArchivedProjectUnpinAndNoticeDismissalPreserveBothPreferences(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archived-unpin-notice")
	memberToken := archiveAddMember(t, f, created.ProjectID, "archived-unpin-notice-member")

	pin := putProjectSetting(t, f, created.ProjectID, memberToken, map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, pin.Code, pin.Body.String())
	archived := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, archived.Code, archived.Body.String())
	version := projectArchiveStateOf(t, created.ProjectID).ArchiveVersion

	unpinAndDismiss := putProjectSetting(t, f, created.ProjectID, memberToken, map[string]any{
		"pinned": false, "archive_notice_dismissed_version": version,
	})
	require.Equal(t, http.StatusOK, unpinAndDismiss.Code, unpinAndDismiss.Body.String())
	assert.False(t, decodeResp(t, unpinAndDismiss).Pinned)

	setting := projectUserSettingForTest(t, created.ProjectID, "archived-unpin-notice-member")
	require.True(t, setting.Pinned.Valid)
	assert.EqualValues(t, 0, setting.Pinned.Int64)
	assert.False(t, setting.PinnedAt.Valid)
	assert.Equal(t, version, setting.ArchiveNoticeDismissedVersion,
		"the archived unpin and current-round dismissal must both survive the shared-row write")

	notices := doOn(t, f.route, http.MethodGet,
		"/v1/space/"+spaceA+"/projects/archive-notices", memberToken, nil)
	require.Equal(t, http.StatusOK, notices.Code, notices.Body.String())
	assert.Empty(t, archiveList(t, notices), "dismissing the current round must remove its notice")
}
