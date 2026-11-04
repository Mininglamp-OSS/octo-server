package project

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type archiveAPIFixture struct {
	p          *Project
	route      *wkhttp.WKHttp
	ownerToken string
}

// archiveAPISetup uses the existing lifecycle outbox and Fleet provisioning test seams.
// A project is not eligible for an archive request until the fake Fleet target has
// acknowledged its workspace ensure and the local provisioning row is ready.
func archiveAPISetup(t *testing.T) *archiveAPIFixture {
	t.Helper()
	_, p := setup(t)
	enableOutbox(t, p)
	route := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	fleet := newFakeTarget(t)
	enableProvisioning(t, p, fleetTargetOn(fleet))
	return &archiveAPIFixture{p: p, route: route, ownerToken: ownerToken}
}

func (f *archiveAPIFixture) createBoundProject(t *testing.T, name string) *Resp {
	t.Helper()
	created := createProjectOn(t, f.route, spaceA, f.ownerToken, name)
	f.p.processProvisioningJobs()
	rows := readProvisioningRows(t, created.ProjectID)
	require.Len(t, rows, 1)
	require.EqualValues(t, provisionStatusReady, rows[0].Status,
		"the archive fixture must represent a Fleet-confirmed workspace")
	require.NotNil(t, activatedAtOf(t, created.ProjectID),
		"the ready Fleet binding must have latched project activation")
	return created
}

func archiveAddMember(t *testing.T, f *archiveAPIFixture, projectID, uid string) string {
	t.Helper()
	token := seedUser(t, uid)
	seedSpaceMember(t, spaceA, uid, 0, 1)
	w := doOn(t, f.route, http.MethodPost,
		"/v1/projects/"+projectID+"/members/add", f.ownerToken, addMembersPayload(uid))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var outcomes []memberOutcome
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), w.Body.String())
	require.Len(t, outcomes, 1)
	require.True(t, outcomes[0].OK, outcomes[0].Reason)
	return token
}

func archiveProject(t *testing.T, f *archiveAPIFixture, projectID, token string, archived bool) *httptest.ResponseRecorder {
	t.Helper()
	return doOn(t, f.route, http.MethodPut, "/v1/projects/"+projectID+"/archive", token,
		map[string]any{"archived": archived})
}

type projectArchiveState struct {
	LifecycleVersion int64        `db:"lifecycle_version"`
	ArchiveVersion   int64        `db:"archive_version"`
	ArchivedAt       sql.NullTime `db:"archived_at"`
	MemberEpoch      int64        `db:"member_epoch"`
}

func projectArchiveStateOf(t *testing.T, projectID string) projectArchiveState {
	t.Helper()
	var rows []projectArchiveState
	_, err := testCtx.DB().SelectBySql(
		"SELECT lifecycle_version, archive_version, archived_at, member_epoch "+
			"FROM octo_project WHERE project_id = ?", projectID,
	).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0]
}

func archiveJSONObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var result map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &result), "body: %s", body)
	return result
}

func archiveJSONBool(t *testing.T, object map[string]json.RawMessage, key string) bool {
	t.Helper()
	var value bool
	require.NoError(t, json.Unmarshal(object[key], &value), "field %q: %s", key, object[key])
	return value
}

func archiveJSONInt64(t *testing.T, object map[string]json.RawMessage, key string) int64 {
	t.Helper()
	var value int64
	require.NoError(t, json.Unmarshal(object[key], &value), "field %q: %s", key, object[key])
	return value
}

func archiveCapability(t *testing.T, object map[string]json.RawMessage, key string) bool {
	t.Helper()
	var capabilities map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(object["capabilities"], &capabilities))
	return archiveJSONBool(t, capabilities, key)
}

func archiveList(t *testing.T, w *httptest.ResponseRecorder) []map[string]json.RawMessage {
	t.Helper()
	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows), "body: %s", w.Body.String())
	return rows
}

func archiveListProjectIDs(t *testing.T, rows []map[string]json.RawMessage) []string {
	t.Helper()
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		var id string
		require.NoError(t, json.Unmarshal(row["project_id"], &id))
		ids = append(ids, id)
	}
	return ids
}

func lifecycleEventsOf(t *testing.T, projectID string) []outboxRow {
	t.Helper()
	rows := outboxRows(t, projectID)
	return rows
}

func countLifecycleEvent(rows []outboxRow, eventType string) int {
	count := 0
	for _, row := range rows {
		if row.EventType == eventType {
			count++
		}
	}
	return count
}

func TestProjectArchiveRequiresOwnerAndRepeatsIdempotently(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archive-permission")
	memberToken := archiveAddMember(t, f, created.ProjectID, "archive-member")

	memberArchive := archiveProject(t, f, created.ProjectID, memberToken, true)
	assertProjectErrorCode(t, memberArchive, "err.server.project.permission_denied")

	ownerActive := doOn(t, f.route, http.MethodGet, "/v1/projects/"+created.ProjectID, f.ownerToken, nil)
	require.Equal(t, http.StatusOK, ownerActive.Code, ownerActive.Body.String())
	active := archiveJSONObject(t, ownerActive.Body.Bytes())
	assert.True(t, archiveCapability(t, active, "can_archive"))
	assert.False(t, archiveCapability(t, active, "can_restore"))
	assert.True(t, archiveCapability(t, active, "can_transfer_owner"))

	firstArchive := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, firstArchive.Code, firstArchive.Body.String())
	archivedState := projectArchiveStateOf(t, created.ProjectID)
	assert.True(t, archivedState.ArchivedAt.Valid)
	assert.Equal(t, archivedState.LifecycleVersion, archivedState.ArchiveVersion,
		"the archive round is the lifecycle version that entered archived state")
	assert.Greater(t, archivedState.ArchiveVersion, int64(0))

	ownerArchived := doOn(t, f.route, http.MethodGet, "/v1/projects/"+created.ProjectID, f.ownerToken, nil)
	require.Equal(t, http.StatusOK, ownerArchived.Code, ownerArchived.Body.String())
	archived := archiveJSONObject(t, ownerArchived.Body.Bytes())
	assert.Equal(t, archivedState.ArchiveVersion, archiveJSONInt64(t, archived, "archive_version"))
	assert.True(t, archiveCapability(t, archived, "can_restore"))
	assert.False(t, archiveCapability(t, archived, "can_archive"))
	assert.True(t, archiveCapability(t, archived, "can_transfer_owner"))
	assert.False(t, archiveCapability(t, archived, "can_leave"))

	memberRestore := archiveProject(t, f, created.ProjectID, memberToken, false)
	assertProjectErrorCode(t, memberRestore, "err.server.project.permission_denied")

	beforeRetry := projectArchiveStateOf(t, created.ProjectID)
	rowsBeforeRetry := lifecycleEventsOf(t, created.ProjectID)
	repeatArchive := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, repeatArchive.Code, repeatArchive.Body.String())
	afterRetry := projectArchiveStateOf(t, created.ProjectID)
	assert.Equal(t, beforeRetry, afterRetry, "repeating the target state must not move either version or timestamp")
	assert.Equal(t, 1, countLifecycleEvent(lifecycleEventsOf(t, created.ProjectID), "project.archived"),
		"an idempotent archive retry must not append another transition event")
	assert.Equal(t, countLifecycleEvent(rowsBeforeRetry, "project.archived"),
		countLifecycleEvent(lifecycleEventsOf(t, created.ProjectID), "project.archived"))

	restore := archiveProject(t, f, created.ProjectID, f.ownerToken, false)
	require.Equal(t, http.StatusOK, restore.Code, restore.Body.String())
	restoredState := projectArchiveStateOf(t, created.ProjectID)
	assert.False(t, restoredState.ArchivedAt.Valid)
	assert.Equal(t, archivedState.ArchiveVersion, restoredState.ArchiveVersion,
		"restoring retains the completed archive round for its event and reminders")
	assert.Equal(t, archivedState.LifecycleVersion+1, restoredState.LifecycleVersion)

	beforeRestoreRetry := projectArchiveStateOf(t, created.ProjectID)
	repeatRestore := archiveProject(t, f, created.ProjectID, f.ownerToken, false)
	require.Equal(t, http.StatusOK, repeatRestore.Code, repeatRestore.Body.String())
	assert.Equal(t, beforeRestoreRetry, projectArchiveStateOf(t, created.ProjectID),
		"repeating restore must not advance lifecycle_version")
	events := lifecycleEventsOf(t, created.ProjectID)
	assert.Equal(t, 1, countLifecycleEvent(events, "project.archived"))
	assert.Equal(t, 1, countLifecycleEvent(events, "project.restored"))
	var archivedEvent, restoredEvent *outboxRow
	for i := range events {
		switch events[i].EventType {
		case "project.archived":
			archivedEvent = &events[i]
		case "project.restored":
			restoredEvent = &events[i]
		}
	}
	require.NotNil(t, archivedEvent)
	require.NotNil(t, restoredEvent)
	require.NotNil(t, archivedEvent.ProjectVersion)
	require.NotNil(t, restoredEvent.ProjectVersion)
	assert.Equal(t, archivedState.LifecycleVersion, *archivedEvent.ProjectVersion)
	assert.JSONEq(t, `{"reason":"owner_requested"}`, archivedEvent.Payload)
	assert.Equal(t, restoredState.LifecycleVersion, *restoredEvent.ProjectVersion)
	var restoredPayload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(restoredEvent.Payload), &restoredPayload))
	assert.Equal(t, archivedState.ArchiveVersion, archiveJSONInt64(t, restoredPayload, "archive_version"))
}

func TestArchivedProjectAllowsOwnerTransferAndFormerOwnerLeave(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archived-owner-transfer")
	successorToken := archiveAddMember(t, f, created.ProjectID, "archived-successor")
	require.NotEmpty(t, successorToken)
	require.Equal(t, http.StatusOK, archiveProject(t, f, created.ProjectID, f.ownerToken, true).Code)
	archivedBefore := projectArchiveStateOf(t, created.ProjectID)

	transfer := doOn(t, f.route, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/owner", f.ownerToken,
		map[string]any{"uid": "archived-successor"})
	require.Equal(t, http.StatusOK, transfer.Code, transfer.Body.String())

	successor, err := f.p.db.queryMember(created.ProjectID, "archived-successor")
	require.NoError(t, err)
	require.NotNil(t, successor)
	assert.Equal(t, RoleOwner, successor.Role)
	assert.Equal(t, MemberStatusActive, successor.Status)
	assert.Zero(t, successor.Removing)
	formerOwner, err := f.p.db.queryMember(created.ProjectID, "owner1")
	require.NoError(t, err)
	require.NotNil(t, formerOwner)
	assert.Equal(t, RoleAdmin, formerOwner.Role)
	assert.Equal(t, MemberStatusActive, formerOwner.Status)
	assert.Zero(t, formerOwner.Removing)

	afterTransfer := projectArchiveStateOf(t, created.ProjectID)
	assert.True(t, afterTransfer.ArchivedAt.Valid)
	assert.Equal(t, archivedBefore.ArchiveVersion, afterTransfer.ArchiveVersion)
	assert.Equal(t, archivedBefore.LifecycleVersion, afterTransfer.LifecycleVersion,
		"ownership transfer changes membership, not archive lifecycle")
	assert.Equal(t, archivedBefore.MemberEpoch+1, afterTransfer.MemberEpoch)

	formerOwnerRestore := archiveProject(t, f, created.ProjectID, f.ownerToken, false)
	assertProjectErrorCode(t, formerOwnerRestore, "err.server.project.permission_denied")

	leave := doOn(t, f.route, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/leave", f.ownerToken, nil)
	require.Equal(t, http.StatusOK, leave.Code, leave.Body.String())

	formerOwner, err = f.p.db.queryMember(created.ProjectID, "owner1")
	require.NoError(t, err)
	require.NotNil(t, formerOwner)
	assert.Equal(t, RoleAdmin, formerOwner.Role)
	assert.Equal(t, MemberStatusActive, formerOwner.Status)
	assert.Equal(t, 1, formerOwner.Removing,
		"the former owner leaves as an admin and loses access while cleanup remains asynchronous")
	successor, err = f.p.db.queryMember(created.ProjectID, "archived-successor")
	require.NoError(t, err)
	require.NotNil(t, successor)
	assert.Equal(t, RoleOwner, successor.Role)
	assert.Zero(t, successor.Removing)

	afterLeave := projectArchiveStateOf(t, created.ProjectID)
	assert.True(t, afterLeave.ArchivedAt.Valid)
	assert.Equal(t, archivedBefore.ArchiveVersion, afterLeave.ArchiveVersion)
	assert.Equal(t, archivedBefore.LifecycleVersion, afterLeave.LifecycleVersion)
	assert.Equal(t, archivedBefore.MemberEpoch+2, afterLeave.MemberEpoch)
	formerOwnerRead := doOn(t, f.route, http.MethodGet,
		"/v1/projects/"+created.ProjectID, f.ownerToken, nil)
	assertProjectErrorCode(t, formerOwnerRead, "err.server.project.not_found")

	restore := archiveProject(t, f, created.ProjectID, successorToken, false)
	require.Equal(t, http.StatusOK, restore.Code, restore.Body.String())
	restoredState := projectArchiveStateOf(t, created.ProjectID)
	assert.False(t, restoredState.ArchivedAt.Valid)
	assert.Equal(t, archivedBefore.ArchiveVersion, restoredState.ArchiveVersion)
	assert.Equal(t, archivedBefore.LifecycleVersion+1, restoredState.LifecycleVersion)
	assert.Equal(t, afterLeave.MemberEpoch, restoredState.MemberEpoch)
}

func TestArchivedProjectAllowsAdminAndMemberLeaveButNeverOwner(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archived-leave")
	adminToken := archiveAddMember(t, f, created.ProjectID, "archived-admin")
	memberToken := archiveAddMember(t, f, created.ProjectID, "archived-member")
	promoteAdmin := doOn(t, f.route, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/archived-admin/role", f.ownerToken,
		map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, promoteAdmin.Code, promoteAdmin.Body.String())
	require.Equal(t, http.StatusOK, archiveProject(t, f, created.ProjectID, f.ownerToken, true).Code)
	archivedBefore := projectArchiveStateOf(t, created.ProjectID)

	adminDetail := doOn(t, f.route, http.MethodGet,
		"/v1/projects/"+created.ProjectID, adminToken, nil)
	require.Equal(t, http.StatusOK, adminDetail.Code, adminDetail.Body.String())
	assert.True(t, archiveCapability(t, archiveJSONObject(t, adminDetail.Body.Bytes()), "can_leave"))

	ownerServiceLeave := f.p.leaveProject(created.ProjectID, spaceA, "owner1")
	assert.ErrorIs(t, ownerServiceLeave, errLastOwnerMustTransfer,
		"the service must preserve the owner guard even while the project is archived")
	ownerHTTPLeave := doOn(t, f.route, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/leave", f.ownerToken, nil)
	assertProjectErrorCode(t, ownerHTTPLeave, "err.server.project.permission_denied")
	assert.Equal(t, archivedBefore, projectArchiveStateOf(t, created.ProjectID),
		"refusing the owner's leave must not alter lifecycle or membership state")

	adminLeave := doOn(t, f.route, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/leave", adminToken, nil)
	require.Equal(t, http.StatusOK, adminLeave.Code, adminLeave.Body.String())
	memberLeave := doOn(t, f.route, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/leave", memberToken, nil)
	require.Equal(t, http.StatusOK, memberLeave.Code, memberLeave.Body.String())

	afterLeaves := projectArchiveStateOf(t, created.ProjectID)
	assert.True(t, afterLeaves.ArchivedAt.Valid)
	assert.Equal(t, archivedBefore.ArchiveVersion, afterLeaves.ArchiveVersion)
	assert.Equal(t, archivedBefore.LifecycleVersion, afterLeaves.LifecycleVersion)
	assert.Equal(t, archivedBefore.MemberEpoch+2, afterLeaves.MemberEpoch)
	owner, err := f.p.db.queryMember(created.ProjectID, "owner1")
	require.NoError(t, err)
	require.NotNil(t, owner)
	assert.Equal(t, RoleOwner, owner.Role)
	assert.Zero(t, owner.Removing)
}

func TestProjectArchiveRechecksCurrentOwnerAfterRoleCacheWasWarm(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archive-stale-owner")
	archiveAddMember(t, f, created.ProjectID, "archive-successor")

	// Warm the middleware's cached owner role, then model a completed role change
	// without invalidating that cache. The archive transaction must trust the
	// locked membership row rather than this stale request-context role.
	warm := doOn(t, f.route, http.MethodGet, "/v1/projects/"+created.ProjectID, f.ownerToken, nil)
	require.Equal(t, http.StatusOK, warm.Code, warm.Body.String())
	before, err := f.p.db.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET role = ? WHERE project_id = ? AND uid = ?",
		RoleAdmin, created.ProjectID, "owner1").Exec()
	require.NoError(t, err)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE octo_project_member SET role = ? WHERE project_id = ? AND uid = ?",
		RoleOwner, created.ProjectID, "archive-successor").Exec()
	require.NoError(t, err)
	currentFormer, err := f.p.db.queryMember(created.ProjectID, "owner1")
	require.NoError(t, err)
	require.NotNil(t, currentFormer)
	require.Equal(t, RoleAdmin, currentFormer.Role)
	currentOwner, err := f.p.db.queryMember(created.ProjectID, "archive-successor")
	require.NoError(t, err)
	require.NotNil(t, currentOwner)
	require.Equal(t, RoleOwner, currentOwner.Role)

	w := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")
	after, err := f.p.db.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, before.LifecycleVersion, after.LifecycleVersion)
	assert.Zero(t, countLifecycleEvent(lifecycleEventsOf(t, created.ProjectID), "project.archived"))
}

func TestProjectArchiveFiltersAndPreservesMemberEpoch(t *testing.T) {
	f := archiveAPISetup(t)
	archivedProject := f.createBoundProject(t, "archive-filtered")
	activeProject := f.createBoundProject(t, "active-filtered")
	beforeEpoch := epochOf(t, archivedProject.ProjectID)

	w := archiveProject(t, f, archivedProject.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, beforeEpoch, epochOf(t, archivedProject.ProjectID),
		"archive and restore lifecycle state are not member mutations")

	for _, tc := range []struct {
		name      string
		query     string
		wantIDs   []string
		wantCount string
	}{
		{name: "default active", query: "", wantIDs: []string{activeProject.ProjectID}, wantCount: "1"},
		{name: "explicit active", query: "?archive_status=active", wantIDs: []string{activeProject.ProjectID}, wantCount: "1"},
		{name: "archived", query: "?archive_status=archived", wantIDs: []string{archivedProject.ProjectID}, wantCount: "1"},
		{name: "all", query: "?archive_status=all", wantIDs: []string{activeProject.ProjectID, archivedProject.ProjectID}, wantCount: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doOn(t, f.route, http.MethodGet,
				"/v1/space/"+spaceA+"/projects"+tc.query, f.ownerToken, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, tc.wantCount, w.Header().Get("X-Total-Count"))
			assert.ElementsMatch(t, tc.wantIDs, archiveListProjectIDs(t, archiveList(t, w)))
		})
	}
	for _, query := range []string{
		"?archive_status=unknown",
		"?archive_status=active&archive_status=archived",
	} {
		w := doOn(t, f.route, http.MethodGet,
			"/v1/space/"+spaceA+"/projects"+query, f.ownerToken, nil)
		assertProjectErrorCode(t, w, "err.server.project.request_invalid")
	}

	detail := doOn(t, f.route, http.MethodGet,
		"/v1/projects/"+archivedProject.ProjectID, f.ownerToken, nil)
	require.Equal(t, http.StatusOK, detail.Code, detail.Body.String())
	state := projectArchiveStateOf(t, archivedProject.ProjectID)
	detailFields := archiveJSONObject(t, detail.Body.Bytes())
	assert.Equal(t, state.ArchiveVersion, archiveJSONInt64(t, detailFields, "archive_version"))
	assert.Equal(t, state.LifecycleVersion, archiveJSONInt64(t, detailFields, "lifecycle_version"))
	assert.True(t, archiveCapability(t, detailFields, "can_restore"))
}

func TestArchivedProjectReadsRemainAvailableAndBusinessWritesAreRejected(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archived-readonly")
	memberToken := archiveAddMember(t, f, created.ProjectID, "readonly-member")
	seedUser(t, "readonly-new-member")
	seedSpaceMember(t, spaceA, "readonly-new-member", 0, 1)
	beforeEpoch := epochOf(t, created.ProjectID)

	w := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, beforeEpoch, epochOf(t, created.ProjectID))

	detail := doOn(t, f.route, http.MethodGet, "/v1/projects/"+created.ProjectID, memberToken, nil)
	require.Equal(t, http.StatusOK, detail.Code, detail.Body.String())
	assert.Equal(t, beforeEpoch, archiveJSONInt64(t, archiveJSONObject(t, detail.Body.Bytes()), "member_epoch"),
		"archived detail must keep the current member epoch rather than disguising membership")
	members := doOn(t, f.route, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members", memberToken, nil)
	require.Equal(t, http.StatusOK, members.Code, members.Body.String())
	assert.Equal(t, "2", members.Header().Get("X-Total-Count"))

	writes := []struct {
		name   string
		method string
		path   string
		token  string
		body   any
	}{
		{name: "profile", method: http.MethodPut, path: "/v1/projects/" + created.ProjectID, token: f.ownerToken, body: map[string]any{"name": "must-not-rename"}},
		{name: "add member", method: http.MethodPost, path: "/v1/projects/" + created.ProjectID + "/members/add", token: f.ownerToken, body: addMembersPayload("readonly-new-member")},
		{name: "remove member", method: http.MethodPost, path: "/v1/projects/" + created.ProjectID + "/members/remove", token: f.ownerToken, body: map[string]any{"uids": []string{"readonly-member"}}},
		{name: "role change", method: http.MethodPut, path: "/v1/projects/" + created.ProjectID + "/members/readonly-member/role", token: f.ownerToken, body: map[string]any{"role": RoleAdmin}},
		{name: "disband", method: http.MethodDelete, path: "/v1/projects/" + created.ProjectID, token: f.ownerToken},
		{name: "collaboration role", method: http.MethodPost, path: "/v1/projects/" + created.ProjectID + "/collaboration-roles", token: f.ownerToken, body: map[string]any{"name": "New role"}},
		{name: "pin preference", method: http.MethodPut, path: "/v1/projects/" + created.ProjectID + "/setting", token: memberToken, body: map[string]any{"pinned": true}},
	}
	for _, tc := range writes {
		t.Run(tc.name, func(t *testing.T) {
			w := doOn(t, f.route, tc.method, tc.path, tc.token, tc.body)
			assertProjectErrorCode(t, w, "err.server.project.archived")
		})
	}

	state := projectArchiveStateOf(t, created.ProjectID)
	assert.True(t, state.ArchivedAt.Valid)
	assert.Equal(t, beforeEpoch, state.MemberEpoch, "all refused writes must leave the membership epoch untouched")
	row, err := f.p.db.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, "archived-readonly", row.Name)
	member, err := f.p.db.queryMember(created.ProjectID, "readonly-member")
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Equal(t, MemberStatusActive, member.Status)
	assert.Equal(t, RoleCommon, member.Role)
	var pinRows int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		created.ProjectID, "readonly-member").LoadOne(&pinRows))
	assert.Zero(t, pinRows, "the reminder exception must not permit unrelated pin writes")
}

func TestArchiveNoticeDismissalIsPerMemberRoundScopedAndPreservesPin(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archive-notice")
	firstToken := archiveAddMember(t, f, created.ProjectID, "notice-first")
	secondToken := archiveAddMember(t, f, created.ProjectID, "notice-second")
	outsiderToken := seedUser(t, "notice-outsider")
	seedSpaceMember(t, spaceA, "notice-outsider", 0, 1)

	pinned := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", firstToken,
		map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, pinned.Code, pinned.Body.String())
	beforeEpoch := epochOf(t, created.ProjectID)
	archived := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, archived.Code, archived.Body.String())
	firstArchiveState := projectArchiveStateOf(t, created.ProjectID)
	firstVersion := firstArchiveState.ArchiveVersion
	assert.Equal(t, beforeEpoch, firstArchiveState.MemberEpoch)

	noticePath := "/v1/space/" + spaceA + "/projects/archive-notices"
	for _, tc := range []struct {
		name  string
		token string
		want  string
		count string
	}{
		{name: "first member", token: firstToken, want: created.ProjectID, count: "1"},
		{name: "second member without a setting row", token: secondToken, want: created.ProjectID, count: "1"},
		{name: "space-only outsider", token: outsiderToken, count: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doOn(t, f.route, http.MethodGet, noticePath, tc.token, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, tc.count, w.Header().Get("X-Total-Count"))
			rows := archiveList(t, w)
			if tc.want == "" {
				assert.Empty(t, rows)
				return
			}
			require.Len(t, rows, 1)
			assert.Equal(t, tc.want, string(bytesTrimQuotes(rows[0]["project_id"])))
			assert.Equal(t, firstVersion, archiveJSONInt64(t, rows[0], "archive_version"))
			assert.NotEmpty(t, string(bytesTrimQuotes(rows[0]["archived_at"])))
		})
	}

	var secondSettingRows int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		created.ProjectID, "notice-second").LoadOne(&secondSettingRows))
	assert.Zero(t, secondSettingRows, "reading notices must not pre-create preferences for offline members")

	dismiss := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", firstToken,
		map[string]any{"archive_notice_dismissed_version": firstVersion})
	require.Equal(t, http.StatusOK, dismiss.Code, dismiss.Body.String())
	assert.True(t, decodeResp(t, dismiss).Pinned,
		"dismissing a reminder must not overwrite the same user's existing pin")
	var dismissedVersion int64
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT archive_notice_dismissed_version FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		created.ProjectID, "notice-first").LoadOne(&dismissedVersion))
	assert.Equal(t, firstVersion, dismissedVersion)

	firstNotices := doOn(t, f.route, http.MethodGet, noticePath, firstToken, nil)
	require.Equal(t, http.StatusOK, firstNotices.Code, firstNotices.Body.String())
	assert.Empty(t, archiveList(t, firstNotices), "the preference affects only this member")
	secondNotices := doOn(t, f.route, http.MethodGet, noticePath, secondToken, nil)
	require.Equal(t, http.StatusOK, secondNotices.Code, secondNotices.Body.String())
	assert.Len(t, archiveList(t, secondNotices), 1)

	repeatedDismiss := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", firstToken,
		map[string]any{"archive_notice_dismissed_version": firstVersion})
	require.Equal(t, http.StatusOK, repeatedDismiss.Code, repeatedDismiss.Body.String())

	restore := archiveProject(t, f, created.ProjectID, f.ownerToken, false)
	require.Equal(t, http.StatusOK, restore.Code, restore.Body.String())
	afterRestore := doOn(t, f.route, http.MethodGet, noticePath, secondToken, nil)
	require.Equal(t, http.StatusOK, afterRestore.Code, afterRestore.Body.String())
	assert.Empty(t, archiveList(t, afterRestore), "restored projects have no current archive reminder")

	rearchive := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, rearchive.Code, rearchive.Body.String())
	secondArchiveState := projectArchiveStateOf(t, created.ProjectID)
	assert.Greater(t, secondArchiveState.ArchiveVersion, firstVersion,
		"a new archive round must replace the old reminder version")
	staleDismiss := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", firstToken,
		map[string]any{"archive_notice_dismissed_version": firstVersion})
	assertProjectErrorCode(t, staleDismiss, "err.server.project.archive_version_conflict")
	currentDismiss := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", firstToken,
		map[string]any{"archive_notice_dismissed_version": secondArchiveState.ArchiveVersion})
	require.Equal(t, http.StatusOK, currentDismiss.Code, currentDismiss.Body.String())
}

// sidebarProjectSectionType is category's sidebarSectionTypeProject. The constant
// is unexported in that module, so this test names the value with the module that
// defines it rather than importing an implementation detail.
const sidebarProjectSectionType = 2

// TestSidebarKeepsNeverPinnedProjectVisibleAfterArchiveNoticeDismissalAndRestore
// catches the projection bug where dismissing an archive reminder was stored as an
// explicit unpin. The two intents share octo_project_user_setting, but only an
// explicit unpin may hide the Project from the personal Follow list: a member who
// never pinned anything still expects the Project in their sidebar, both while the
// Project is archived and after it is restored.
func TestSidebarKeepsNeverPinnedProjectVisibleAfterArchiveNoticeDismissalAndRestore(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archive-notice-sidebar")
	memberToken := archiveAddMember(t, f, created.ProjectID, "sidebar-notice-member")

	// No pin preference was ever written; membership alone makes the Project
	// visible in the sidebar.
	assertProjectInFollow(t, testSrv, memberToken, created.ProjectID, true)

	archived := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, archived.Code, archived.Body.String())
	version := projectArchiveStateOf(t, created.ProjectID).ArchiveVersion

	dismiss := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", memberToken,
		map[string]any{"archive_notice_dismissed_version": version})
	require.Equal(t, http.StatusOK, dismiss.Code, dismiss.Body.String())
	assert.False(t, decodeResp(t, dismiss).Pinned,
		"dismissing a reminder must not report the Project as pinned")
	var settingRows int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		created.ProjectID, "sidebar-notice-member").LoadOne(&settingRows))
	require.Equal(t, 1, settingRows,
		"the dismissal must create the preference row this regression is about")

	// The dismissal creates a preference row. That row is not an unpin, so the
	// sidebar must keep listing the archived Project.
	assertProjectInFollow(t, testSrv, memberToken, created.ProjectID, true)

	// After restore the same preference row must still not act as an opt-out.
	// The ordering row is removed first so the read has to go through the
	// membership repair path, which applies the same visibility rule.
	restore := archiveProject(t, f, created.ProjectID, f.ownerToken, false)
	require.Equal(t, http.StatusOK, restore.Code, restore.Body.String())
	_, err := testCtx.DB().Exec(
		"DELETE FROM octo_sidebar_section WHERE uid=? AND space_id=? AND section_type=? AND ref_id=?",
		"sidebar-notice-member", spaceA, sidebarProjectSectionType, created.ProjectID)
	require.NoError(t, err)
	assertProjectInFollow(t, testSrv, memberToken, created.ProjectID, true)
}

// TestSidebarHidesExplicitlyUnpinnedProjectAfterArchiveNoticeDismissal guards the
// other half of that distinction: once the preference row exists only because the
// member dismissed a reminder, an explicit pin=false must still be recorded as the
// durable sidebar opt-out, and repeated reads must not resurrect the Project.
func TestSidebarHidesExplicitlyUnpinnedProjectAfterArchiveNoticeDismissal(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archive-notice-unpin")
	memberToken := archiveAddMember(t, f, created.ProjectID, "sidebar-unpin-member")

	archived := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, archived.Code, archived.Body.String())
	version := projectArchiveStateOf(t, created.ProjectID).ArchiveVersion
	dismiss := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", memberToken,
		map[string]any{"archive_notice_dismissed_version": version})
	require.Equal(t, http.StatusOK, dismiss.Code, dismiss.Body.String())
	var settingRows int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		created.ProjectID, "sidebar-unpin-member").LoadOne(&settingRows))
	require.Equal(t, 1, settingRows,
		"the explicit unpin below must overwrite the reminder-created preference row")

	// Pin writes are rejected while archived, so restore before unpinning. The
	// explicit false must overwrite the reminder row as the user's choice.
	restore := archiveProject(t, f, created.ProjectID, f.ownerToken, false)
	require.Equal(t, http.StatusOK, restore.Code, restore.Body.String())
	unpin := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", memberToken,
		map[string]any{"pinned": false})
	require.Equal(t, http.StatusOK, unpin.Code, unpin.Body.String())
	assert.False(t, decodeResp(t, unpin).Pinned)

	assertProjectInFollow(t, testSrv, memberToken, created.ProjectID, false)
	// The second read exercises the membership repair path: an explicit unpin is
	// a personal opt-out and must survive it.
	assertProjectInFollow(t, testSrv, memberToken, created.ProjectID, false)
}

// TestSettingPinWritesPreserveArchiveNoticeDismissal catches a pin upsert that
// rewrites more of the preference row than the pin columns. The existing
// TestArchiveNoticeDismissalIsPerMemberRoundScopedAndPreservesPin covers the other
// direction (pinning first, then dismissing the reminder).
func TestSettingPinWritesPreserveArchiveNoticeDismissal(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archive-notice-pin-writes")
	memberToken := archiveAddMember(t, f, created.ProjectID, "notice-pin-member")
	noticePath := "/v1/space/" + spaceA + "/projects/archive-notices"

	archived := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, archived.Code, archived.Body.String())
	version := projectArchiveStateOf(t, created.ProjectID).ArchiveVersion

	dismiss := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", memberToken,
		map[string]any{"archive_notice_dismissed_version": version})
	require.Equal(t, http.StatusOK, dismiss.Code, dismiss.Body.String())
	assert.False(t, decodeResp(t, dismiss).Pinned,
		"a dismissal must not fabricate a pin")
	dismissedNotices := doOn(t, f.route, http.MethodGet, noticePath, memberToken, nil)
	require.Equal(t, http.StatusOK, dismissedNotices.Code, dismissedNotices.Body.String())
	assert.Empty(t, archiveList(t, dismissedNotices))

	// Pin writes are rejected while archived; restore, then pin. The pin upsert
	// must update only the pin columns and keep the dismissed round.
	restore := archiveProject(t, f, created.ProjectID, f.ownerToken, false)
	require.Equal(t, http.StatusOK, restore.Code, restore.Body.String())
	pin := doOn(t, f.route, http.MethodPut, "/v1/projects/"+created.ProjectID+"/setting", memberToken,
		map[string]any{"pinned": true})
	require.Equal(t, http.StatusOK, pin.Code, pin.Body.String())
	assert.True(t, decodeResp(t, pin).Pinned)
	var storedVersion int64
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT archive_notice_dismissed_version FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		created.ProjectID, "notice-pin-member").LoadOne(&storedVersion))
	assert.Equal(t, version, storedVersion,
		"pinning must rewrite only the pin columns, not the dismissed archive round")
	assertProjectInFollow(t, testSrv, memberToken, created.ProjectID, true)

	// A new archive round is a new reminder, so the old dismissal must not
	// silence it.
	rearchive := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, rearchive.Code, rearchive.Body.String())
	notices := doOn(t, f.route, http.MethodGet, noticePath, memberToken, nil)
	require.Equal(t, http.StatusOK, notices.Code, notices.Body.String())
	assert.Len(t, archiveList(t, notices), 1)
}

func TestArchivedProjectStillAllowsSpaceSecurityRevocation(t *testing.T) {
	f := archiveAPISetup(t)
	created := f.createBoundProject(t, "archived-space-revocation")
	archiveAddMember(t, f, created.ProjectID, "archived-space-revoked")
	beforeEpoch := epochOf(t, created.ProjectID)

	archived := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
	require.Equal(t, http.StatusOK, archived.Code, archived.Body.String())

	// Losing the Space seat is a security revoke, not a user-initiated business
	// write. The archived read-only guard must not block the registered cleanup.
	removeSpaceMember(t, spaceA, "archived-space-revoked")
	require.NoError(t, runCascade(t, f.p, spaceA, "archived-space-revoked", "owner1", removalReasonKicked))

	member, err := f.p.db.queryMember(created.ProjectID, "archived-space-revoked")
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Equal(t, MemberStatusRemoved, member.Status)
	assert.Greater(t, epochOf(t, created.ProjectID), beforeEpoch,
		"the forced membership revoke still advances the roster epoch while archived")
}

func TestProjectArchiveRejectsUnavailableLifecycleOrBindingWithoutMutation(t *testing.T) {
	t.Run("lifecycle disabled", func(t *testing.T) {
		f := archiveAPISetup(t)
		created := f.createBoundProject(t, "archive-lifecycle-disabled")
		before := projectArchiveStateOf(t, created.ProjectID)
		f.p.cfg.LifecycleEventsEnabled = false

		response := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
		assertProjectErrorCode(t, response, "err.server.project.disabled")
		assert.Equal(t, before, projectArchiveStateOf(t, created.ProjectID))
		assert.Zero(t, countLifecycleEvent(lifecycleEventsOf(t, created.ProjectID), "project.archived"))
	})
	t.Run("Fleet binding pending", func(t *testing.T) {
		f := archiveAPISetup(t)
		created := createProjectOn(t, f.route, spaceA, f.ownerToken, "archive-pending-binding")
		before := projectArchiveStateOf(t, created.ProjectID)

		response := archiveProject(t, f, created.ProjectID, f.ownerToken, true)
		assertProjectErrorCode(t, response, "err.server.project.not_ready")
		envelope := decodeProjectEnvelope(t, response.Body.Bytes())
		assert.Equal(t, http.StatusServiceUnavailable, envelope.Error.HTTPStatus)
		assert.Equal(t, before, projectArchiveStateOf(t, created.ProjectID))
		assert.Zero(t, countLifecycleEvent(lifecycleEventsOf(t, created.ProjectID), "project.archived"))
	})
}

func bytesTrimQuotes(raw json.RawMessage) []byte {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return raw
	}
	return []byte(value)
}
