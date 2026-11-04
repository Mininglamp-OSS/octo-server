package project

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/internal/projectprovision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDriveHintFailureDoesNotChangeFleetOutboxResult(t *testing.T) {
	type lifecycleWireEvent struct {
		EventID        string          `json:"event_id"`
		EventType      string          `json:"event_type"`
		ProjectID      string          `json:"project_id"`
		SpaceID        string          `json:"space_id"`
		ProjectVersion *int64          `json:"project_version"`
		Payload        json.RawMessage `json:"payload"`
	}
	type driveHint struct {
		EventID     string `json:"event_id"`
		SpaceID     string `json:"space_id"`
		ProjectID   string `json:"project_id"`
		SubjectUID  string `json:"subject_uid"`
		MemberEpoch int64  `json:"member_epoch"`
	}
	type outboxSnapshot struct {
		Status    int    `db:"status"`
		Attempts  int    `db:"attempts"`
		LastError string `db:"last_error"`
	}
	type driveObservation struct {
		Method    string
		Path      string
		Token     string
		Body      driveHint
		DecodeErr error
		QueryErr  error
		Found     bool
		Outbox    outboxSnapshot
	}
	type fleetObservation struct {
		Method    string
		Path      string
		Body      lifecycleWireEvent
		DecodeErr error
	}

	driveToken := provTestDriveToken
	driveRequests := make(chan driveObservation, 1)
	driveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var observed driveObservation
		observed.Method = r.Method
		observed.Path = r.URL.Path
		observed.Token = r.Header.Get(projectprovision.HeaderInternalToken)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			observed.DecodeErr = err
		} else {
			observed.DecodeErr = json.Unmarshal(raw, &observed.Body)
		}
		if observed.DecodeErr == nil {
			var rows []outboxSnapshot
			_, observed.QueryErr = testCtx.DB().SelectBySql(
				"SELECT status, attempts, last_error FROM `octo_project_lifecycle_event` WHERE event_id = ?",
				observed.Body.EventID,
			).Load(&rows)
			if observed.QueryErr == nil && len(rows) == 1 {
				observed.Found = true
				observed.Outbox = rows[0]
			}
		}
		driveRequests <- observed
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer driveServer.Close()

	fleetRequests := make(chan fleetObservation, 1)
	fleetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var observed fleetObservation
		observed.Method = r.Method
		observed.Path = r.URL.Path
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			observed.DecodeErr = err
		} else {
			observed.DecodeErr = json.Unmarshal(raw, &observed.Body)
		}
		fleetRequests <- observed
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fleetServer.Close()

	t.Setenv(envLifecycleEventsEnabled, "false")
	t.Setenv(DriveInternalTokenEnv, driveToken)
	t.Setenv("OCTO_PROJECT_DRIVE_ACCESS_CHANGED_URL",
		driveServer.URL+"/internal/v1/docs/project-access-changed")
	_, p := setup(t)
	route := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "drive-hint-owner")
	seedSpaceMember(t, spaceA, "drive-hint-owner", 0, 1)
	memberToken := seedUser(t, "drive-hint-member")
	seedSpaceMember(t, spaceA, "drive-hint-member", 0, 1)
	created := createProjectOn(t, route, spaceA, ownerToken, "drive-hint-failure")
	added := doOn(t, route, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		addMembersPayload("drive-hint-member"))
	require.Equal(t, http.StatusOK, added.Code, added.Body.String())

	p.cfg.LifecycleEventsEnabled = true
	p.cfg.LifecycleEventURL = fleetServer.URL + "/internal/project-events"
	p.cfg.LifecycleEventSecret = provTestSecretFleet
	p.cfg.LifecycleEventTimeout = time.Second
	leave := doOn(t, route, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/leave", memberToken, nil)
	require.Equal(t, http.StatusOK, leave.Code, leave.Body.String())
	beforeDelivery := outboxRows(t, created.ProjectID)
	require.Len(t, beforeDelivery, 1, "the leave must be the only event queued for this project")
	require.Equal(t, LifecycleEventMemberRevoked, beforeDelivery[0].EventType)
	require.Equal(t, lifecycleEventPending, beforeDelivery[0].Status)
	require.Zero(t, beforeDelivery[0].Attempts)

	p.runLifecycleEventDelivery()

	var fleetEvent fleetObservation
	select {
	case fleetEvent = <-fleetRequests:
	case <-time.After(time.Second):
		t.Fatal("the Fleet lifecycle event was not delivered")
	}
	assert.NoError(t, fleetEvent.DecodeErr)
	assert.Equal(t, http.MethodPost, fleetEvent.Method)
	assert.Equal(t, "/internal/project-events", fleetEvent.Path)
	assert.Equal(t, LifecycleEventMemberRevoked, fleetEvent.Body.EventType)
	assert.Equal(t, created.ProjectID, fleetEvent.Body.ProjectID)

	var driveEvent driveObservation
	select {
	case driveEvent = <-driveRequests:
	case <-time.After(time.Second):
		t.Fatal("the member_revoked delivery did not issue its Drive source-invalidation hint")
	}
	assert.NoError(t, driveEvent.DecodeErr)
	assert.NoError(t, driveEvent.QueryErr)
	assert.Equal(t, http.MethodPost, driveEvent.Method)
	assert.Equal(t, "/internal/v1/docs/project-access-changed", driveEvent.Path)
	assert.Equal(t, driveToken, driveEvent.Token)
	assert.Equal(t, fleetEvent.Body.EventID, driveEvent.Body.EventID)
	assert.Equal(t, spaceA, driveEvent.Body.SpaceID)
	assert.Equal(t, created.ProjectID, driveEvent.Body.ProjectID)
	assert.Equal(t, "drive-hint-member", driveEvent.Body.SubjectUID)
	assert.Equal(t, epochOf(t, created.ProjectID), driveEvent.Body.MemberEpoch)
	assert.True(t, driveEvent.Found, "the Fleet row must already exist when its bypass hint is sent")
	assert.Equal(t, lifecycleEventDelivered, driveEvent.Outbox.Status,
		"the Fleet delivery receipt must be committed before the Drive bypass call")
	assert.Equal(t, 1, driveEvent.Outbox.Attempts,
		"a failed Drive hint must not charge another Fleet outbox attempt")
	assert.Empty(t, driveEvent.Outbox.LastError,
		"a failed Drive hint must not overwrite Fleet delivery state")

	afterDelivery := outboxRows(t, created.ProjectID)
	require.Len(t, afterDelivery, 1)
	assert.Equal(t, lifecycleEventDelivered, afterDelivery[0].Status)
	assert.Equal(t, 1, afterDelivery[0].Attempts)
	assert.Empty(t, afterDelivery[0].LastError)
}
