package project

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"

	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Engine tests for the PRODUCER side: that the write paths actually enqueue, in
// the transaction that makes the change they report.
//
// The unit tests in lifecycle_event_test.go cover the gate and the validation;
// they cannot see whether any real write path calls the function. A call site
// that was never added is silent — the integration simply never hears about the
// change, and the first symptom is a peer whose state diverged days ago.
//
// Every case here drives a PRIVATE router (mountProject) rather than the shared
// testSrv. The outbox switch is a field on the instance, and the shared router
// carries a different instance registered at module.Setup — requests through it
// would run with the switch off and every assertion below would fail for a
// reason that has nothing to do with the code under test.

type outboxRow struct {
	EventID        string  `db:"event_id"`
	EventType      string  `db:"event_type"`
	ProjectID      string  `db:"project_id"`
	SpaceID        string  `db:"space_id"`
	ProjectVersion *int64  `db:"project_version"`
	Payload        string  `db:"payload"`
	Status         int     `db:"status"`
	Attempts       int     `db:"attempts"`
	LastError      string  `db:"last_error"`
	LeaseUntil     *string `db:"lease_until"`
}

func outboxRows(t *testing.T, projectID string) []outboxRow {
	t.Helper()
	var rows []outboxRow
	_, err := testCtx.DB().SelectBySql(
		"SELECT event_id, event_type, project_id, space_id, project_version, payload, "+
			"status, attempts, last_error, lease_until "+
			"FROM `octo_project_lifecycle_event` WHERE project_id = ? ORDER BY id", projectID,
	).Load(&rows)
	require.NoError(t, err)
	return rows
}

// enableOutbox turns the outbox on for one instance. The switch is read from
// p.cfg on every enqueue, so this must be set before the router serves anything.
func enableOutbox(t *testing.T, p *Project) {
	t.Helper()
	p.cfg.LifecycleEventsEnabled = true
	p.cfg.LifecycleEventURL = "https://peer.invalid/internal/project-events"
	p.cfg.LifecycleEventSecret = strings.Repeat("s", 32)
}

// createProjectOn is createProjectVia against a private router.
func createProjectOn(t *testing.T, r *wkhttp.WKHttp, spaceID, token, name string) *Resp {
	t.Helper()
	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceID+"/projects", token,
		map[string]any{"name": name})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	return decodeResp(t, w)
}

// TestCreateEnqueuesProjectCreated pins the first event and its payload shape.
//
// The payload assertion is on the stored BYTES, not on a decoded struct: the
// contract's promise is that name and description never leave this repository,
// and a struct that gained a field would decode fine while shipping it.
func TestCreateEnqueuesProjectCreated(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "evtCreator")
	seedSpaceMember(t, spaceA, "evtCreator", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "event-create")

	rows := outboxRows(t, created.ProjectID)
	require.Len(t, rows, 1, "creation must enqueue exactly one event")
	row := rows[0]

	assert.Equal(t, LifecycleEventProjectCreated, row.EventType)
	assert.Equal(t, spaceA, row.SpaceID)
	assert.Equal(t, 36, len(row.EventID), "event_id must be a canonical hyphenated UUID: %q", row.EventID)
	require.NotNil(t, row.ProjectVersion, "a lifecycle statement must carry a version to order it")
	assert.EqualValues(t, 1, *row.ProjectVersion, "creation is version 1")

	assert.NotContains(t, row.Payload, "event-create",
		"the project NAME must never reach the payload: the contract keeps it inside this "+
			"repository, and putting it here makes the event a content channel")
	assert.NotContains(t, row.Payload, "description")
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(row.Payload), &payload))
	assert.Equal(t, map[string]any{"creator_uid": "evtCreator"}, payload,
		"project.created carries the creator and nothing else")
}

// TestProfileUpdateEnqueuesAnEmptyMetadataEvent pins that the event is a change
// SIGNAL rather than a sync — the same shape member_epoch already has.
func TestProfileUpdateEnqueuesAnEmptyMetadataEvent(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "evtUpdater")
	seedSpaceMember(t, spaceA, "evtUpdater", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "event-update")
	w := doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, token,
		map[string]any{"name": "event-update-renamed"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	rows := outboxRows(t, created.ProjectID)
	require.Len(t, rows, 2, "create + update")
	row := rows[1]

	assert.Equal(t, LifecycleEventMetadataUpdated, row.EventType)
	assert.JSONEq(t, `{}`, row.Payload,
		"the payload must be an EMPTY object: the event says the profile changed and at which "+
			"version, and a consumer that wants the new name reads it back")
	assert.NotContains(t, row.Payload, "renamed", "the new name must not travel with the event")
	require.NotNil(t, row.ProjectVersion)
	assert.Greater(t, *row.ProjectVersion, int64(1), "the update must advance the version")
}

// TestMemberRemovalEnqueuesRevocationWithoutAVersion is the event whose loss is
// a security failure rather than a stale display, so two properties matter.
func TestMemberRemovalEnqueuesRevocationWithoutAVersion(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "evtOwner")
	seedSpaceMember(t, spaceA, "evtOwner", 0, 1)
	seedUser(t, "evtTarget")
	seedSpaceMember(t, spaceA, "evtTarget", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "event-revoke")
	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		token, map[string]any{"uids": []string{"evtTarget"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/remove",
		token, map[string]any{"uids": []string{"evtTarget"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	rows := outboxRows(t, created.ProjectID)
	var revoked *outboxRow
	for i := range rows {
		if rows[i].EventType == LifecycleEventMemberRevoked {
			revoked = &rows[i]
		}
	}
	require.NotNil(t, revoked, "removing a member must enqueue a revocation")
	assert.Equal(t, "evtTarget", subjectOf(t, revoked.Payload))

	// NO project_version. A consumer discards a lifecycle statement not newer than
	// what it holds; applying that to a revocation would drop a late one as stale
	// and leave a removed member executing.
	assert.Nil(t, revoked.ProjectVersion,
		"member_revoked must carry NO project_version — a consumer that ordered revocations "+
			"by it would discard a late one and leave the removed member executing")

	var payload struct {
		SubjectUID  string `json:"subject_uid"`
		MemberEpoch int64  `json:"member_epoch"`
		Reason      string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal([]byte(revoked.Payload), &payload))
	assert.Equal(t, "evtTarget", payload.SubjectUID)
	assert.NotEmpty(t, payload.Reason)
	// The epoch must be the one the revocation produced, not a stale read. The
	// removal is the last membership write in this case, so the row's current epoch
	// IS the one the revocation left behind.
	assert.EqualValues(t, epochOf(t, created.ProjectID), payload.MemberEpoch,
		"the payload epoch must match the row the revocation left behind, or the consumer "+
			"cannot tell this revocation from one it has already superseded")
}

func subjectOf(t *testing.T, payload string) string {
	t.Helper()
	var p struct {
		SubjectUID string `json:"subject_uid"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &p))
	return p.SubjectUID
}

// TestOutboxStaysEmptyWhenDisabled is the default posture, asserted against the
// engine rather than the gate function.
//
// Every write path now calls the enqueue, so "the switch is off" has to hold at
// the table, not just in the branch a unit test can see. Note this case mounts
// its own router too: reaching the table through the shared server would pass
// whether or not the gate works, since that instance is off for unrelated
// reasons.
func TestOutboxStaysEmptyWhenDisabled(t *testing.T) {
	_, p := setup(t)
	require.False(t, p.lifecycleEventsEnabled(), "the default posture is OFF")
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "evtOff")
	seedSpaceMember(t, spaceA, "evtOff", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "event-off")
	w := doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, token,
		map[string]any{"name": "event-off-renamed"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	assert.Empty(t, outboxRows(t, created.ProjectID),
		"with the integration off, no write path may leave a row: a queued event the consumer "+
			"never saw a project.created for is one it can never accept")
}

// TestSpaceRemovalCascadeEnqueuesRevocation is O7: losing a Space seat revokes
// every project seat inside it, and the peer has to be told about each one.
//
// The project is created through the SHARED server, whose instance has the
// outbox off. So the only row this case can find is the one the cascade wrote —
// there is no create event to filter out, and a passing assertion cannot be
// explained by the wrong producer.
func TestSpaceRemovalCascadeEnqueuesRevocation(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "cascadeLeaver")
	// Enabled AFTER the setup writes, deliberately: see above.
	enableOutbox(t, p)

	removeSpaceMember(t, spaceA, "cascadeLeaver")
	require.NoError(t, runCascade(t, p, spaceA, "cascadeLeaver", "owner1", spacemod.MemberRemoveReasonKicked))

	rows := outboxRows(t, created.ProjectID)
	require.Len(t, rows, 1, "the cascade must enqueue exactly one revocation")
	assert.Equal(t, LifecycleEventMemberRevoked, rows[0].EventType)
	assert.Equal(t, "cascadeLeaver", subjectOf(t, rows[0].Payload))
	assert.Nil(t, rows[0].ProjectVersion, "a revocation carries no ordering token")
	assert.EqualValues(t, epochOf(t, created.ProjectID), epochOfPayload(t, rows[0].Payload),
		"the payload epoch must be the one this revocation produced")

	// Rerun, at the SEAT level rather than through runCascade.
	//
	// The job is retried on every backoff, so the enqueue has to be idempotent, and
	// it is — but only because it sits inside the same `changed` branch as the epoch
	// bump. Going through runCascade would not prove that: the outer step re-queries
	// for ACTIVE seats and finds none, so it never reaches this function at all, and
	// the assertion would pass with the enqueue moved outside the branch (verified by
	// mutation). Calling the seat closer directly is what puts the branch under test.
	//
	// A second row would be a second event id for one revocation, which the peer
	// deduplicates on event_id and therefore cannot collapse.
	again, err := p.deactivateSeatForCascade(created.ProjectID, spaceA, "cascadeLeaver",
		"owner1", spacemod.MemberRemoveReasonKicked)
	require.NoError(t, err)
	require.False(t, again, "the seat was already closed, so nothing changed")
	assert.Len(t, outboxRows(t, created.ProjectID), 1,
		"a rerun that changed no seat must not enqueue a second revocation")
}

func epochOfPayload(t *testing.T, payload string) int64 {
	t.Helper()
	var p struct {
		MemberEpoch int64 `json:"member_epoch"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &p))
	return p.MemberEpoch
}

// TestAnIdenticalUpdateEnqueuesNothing is the amplification guard.
//
// A client re-sending the name a project already has used to count as a change:
// the row was rewritten with identical values, lifecycle_version advanced, and
// one outbox row was enqueued. Events are delivered SEQUENTIALLY, ten per
// five-second tick, one HTTP round trip each — so a project admin in a loop
// could queue empty metadata_updated events in front of the member_revoked
// events whose delivery latency this module treats as security-relevant.
//
// It is also the rule the rest of the module already follows for member_epoch: a
// no-op does not move a counter, which is what lets a consumer trust that a
// moved counter means something moved.
func TestAnIdenticalUpdateEnqueuesNothing(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "noopUpdater")
	seedSpaceMember(t, spaceA, "noopUpdater", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "noop-update")
	before := lifecycleVersionOf(t, created.ProjectID)

	for i := 0; i < 3; i++ {
		w := doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, token,
			map[string]any{"name": "noop-update"})
		require.Equal(t, http.StatusOK, w.Code,
			"a request naming a field the project already matches is well formed, so it is "+
				"200 with the current row — not a 400: body: %s", w.Body.String())
	}

	assert.Len(t, outboxRows(t, created.ProjectID), 1,
		"only the creation event: three identical updates changed nothing, so there is "+
			"nothing to tell the peer about")
	assert.Equal(t, before, lifecycleVersionOf(t, created.ProjectID),
		"a no-op must not move lifecycle_version; a consumer orders on it and a version that "+
			"advances without a change makes every ordering decision meaningless")

	// And a REAL change still does both.
	w := doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, token,
		map[string]any{"name": "noop-update-really"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Len(t, outboxRows(t, created.ProjectID), 2, "a real change still enqueues")
	assert.Greater(t, lifecycleVersionOf(t, created.ProjectID), before, "and still bumps")
}

// TestAnUpdateNamingNoFieldIsStillRejected keeps the two cases apart. "Named
// nothing" is a malformed request; "named fields that already match" is a well
// formed one the project already satisfies. Folding them would either 400 a
// legitimate idempotent retry or accept an empty body.
func TestAnUpdateNamingNoFieldIsStillRejected(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "emptyUpdater")
	seedSpaceMember(t, spaceA, "emptyUpdater", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "empty-update")
	w := doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, token, map[string]any{})
	assert.NotEqual(t, http.StatusOK, w.Code,
		"an update naming no field at all must still be refused: body: %s", w.Body.String())
}

func lifecycleVersionOf(t *testing.T, projectID string) int64 {
	t.Helper()
	var v []int64
	_, err := testCtx.DB().SelectBySql(
		"SELECT lifecycle_version FROM `octo_project` WHERE project_id = ?", projectID).Load(&v)
	require.NoError(t, err)
	require.Len(t, v, 1)
	return v[0]
}

// TestANoOpUpdateWritesNoAuditEntry is the other half of the no-op fix.
//
// The service stopped writing, but the handler still called p.audit on any
// err == nil — so the audit log recorded a change that never happened. That is
// verbatim the argument the errNoFieldsToUpdate branch is made from, arriving
// through the other door.
func TestANoOpUpdateWritesNoAuditEntry(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	rec := &auditRecorder{}
	p.auditSink = rec.sink
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "auditUpdater")
	seedSpaceMember(t, spaceA, "auditUpdater", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "audit-noop")
	before := len(rec.byAction(auditUpdate))

	w := doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, token,
		map[string]any{"name": "audit-noop"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, before, len(rec.byAction(auditUpdate)),
		"a request that changed nothing must not be audited as an update")

	w = doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, token,
		map[string]any{"name": "audit-noop-real"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, before+1, len(rec.byAction(auditUpdate)), "a real change is still audited")
}
