package project

import (
	"context"
	"github.com/google/uuid"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Worker-side engine tests: the delivery loop against real rows.
//
// The unit tests cover classification and the envelope; they cannot see the
// property this file exists for, which is what the loop does with the OTHER rows
// in a batch when one of them fails.

// scriptedSender fails whatever `fail` says to, and records what it was asked to
// send, in order.
//
// The decision takes the WHOLE envelope rather than an event type, because the
// property under test is per project: a rule keyed on type alone could not
// express "fail A's created but not B's", which is exactly the difference
// between a per-project block and a per-type one.
type scriptedSender struct {
	mu   sync.Mutex
	fail func(lifecycleEventEnvelope) (lifecycleDeliveryResult, bool)
	sent []string // "<project_id>/<event_type>", in the order Send was called
}

func (s *scriptedSender) Send(_ context.Context, env lifecycleEventEnvelope) lifecycleDeliveryResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, env.ProjectID+"/"+env.EventType)
	if s.fail != nil {
		if res, ok := s.fail(env); ok {
			return res
		}
	}
	return lifecycleDeliveryResult{OK: true, Class: lifecycleErrNone}
}

func (s *scriptedSender) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// TestATransientFailureHoldsBackOnlyItsOwnProject is the ordering rule.
//
// The queue is ordered globally, not per project, so a project.created that hits
// a transient 503 is rescheduled with backoff and drops behind the events queued
// after it. Delivering those first makes the peer answer "unknown project" — a
// plain 4xx, which this client classifies as TERMINAL. So one transient failure
// on the creation event could permanently abandon the member_revoked behind it,
// which is the loss this module treats as a security failure rather than a stale
// display.
func TestATransientFailureHoldsBackOnlyItsOwnProject(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "ordOwner")
	seedSpaceMember(t, spaceA, "ordOwner", 0, 1)

	// Project A: created, then renamed — two events, ordered.
	a := createProjectOn(t, r, spaceA, token, "order-a")
	w := doOn(t, r, "PUT", "/v1/projects/"+a.ProjectID, token, map[string]any{"name": "order-a2"})
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	// Project B: an unrelated project whose events must NOT be held back.
	b := createProjectOn(t, r, spaceA, token, "order-b")

	// Only A's creation event fails. B's is the SAME event type, so a block keyed
	// on type rather than on project would stall B too and this case would catch it.
	sender := &scriptedSender{fail: func(env lifecycleEventEnvelope) (lifecycleDeliveryResult, bool) {
		if env.ProjectID == a.ProjectID && env.EventType == LifecycleEventProjectCreated {
			return lifecycleDeliveryResult{Retryable: true, Class: lifecycleErrServer, Detail: "503"}, true
		}
		return lifecycleDeliveryResult{}, false
	}}
	p.lifecycleEventSender = sender
	p.runLifecycleEventDelivery()

	// A's created failed, so A's metadata_updated must NOT have been attempted.
	// B's created is a different project and shares the same event type, so it
	// proves the block is per project rather than per type.
	rowsA := outboxRows(t, a.ProjectID)
	require.Len(t, rowsA, 2)
	assert.EqualValues(t, lifecycleEventPending, rowsA[1].Status)
	assert.Zero(t, rowsA[1].Attempts,
		"the held-back event must have its claim attempt REFUNDED: it was never sent, and "+
			"charging it would retire a project's tail without one delivery attempt on it")
	assert.Empty(t, rowsA[1].LastError, "an event that was never sent has no error to report")

	assert.Equal(t, 1, rowsA[0].Attempts, "the head event was sent once and failed")

	rowsB := outboxRows(t, b.ProjectID)
	require.Len(t, rowsB, 1)
	assert.EqualValues(t, lifecycleEventDelivered, rowsB[0].Status,
		"an unrelated project must not be stalled by A's failure")

	// And the sender was asked for exactly two events: A's created (failed) and
	// B's created. A's metadata_updated never reached the wire.
	assert.Equal(t, []string{
		a.ProjectID + "/" + LifecycleEventProjectCreated,
		b.ProjectID + "/" + LifecycleEventProjectCreated,
	}, sender.snapshot(),
		"A's metadata_updated must not be sent while A's created is still owed")

	// A SECOND TICK, and this is the assertion that matters.
	//
	// The first version of this ordering rule was an in-memory `blocked` map in
	// the delivery loop, and a single-tick test made it read as pinned. It was
	// not: rescheduleLifecycleEvent pushes the failed head to now+backoff while a
	// released tail keeps its original next_attempt_at, and the claim orders by
	// next_attempt_at — so on the next tick the tail sorted FIRST, and during the
	// first backoff the head was not even due, so the tail was claimed ALONE with
	// an empty map. The rule now lives in the claim query, where it also holds
	// across replicas; a per-process map cannot see another pod's leased head at
	// all.
	//
	// A third tick, past the 2s first backoff, would deliver A's head and then its
	// tail. This stops at two because the property under test is that the tail
	// cannot overtake, not how long the backoff is.
	p.runLifecycleEventDelivery()
	for _, sent := range sender.snapshot() {
		assert.NotEqual(t, a.ProjectID+"/"+LifecycleEventMetadataUpdated, sent,
			"A's metadata_updated overtook A's still-owed project.created on a later tick. "+
				"The peer answers 'unknown project' — a plain 4xx, which is TERMINAL here — so "+
				"a revocation queued behind a transiently failed creation event is abandoned.")
	}

	// The tail is still pending and still unattempted: held back, not charged.
	rowsA = outboxRows(t, a.ProjectID)
	require.Len(t, rowsA, 2)
	assert.EqualValues(t, lifecycleEventPending, rowsA[1].Status)
	assert.Zero(t, rowsA[1].Attempts, "a row the claim declined to take must not be charged")
}

// TestAnAbandonedEventDoesNotStallItsProjectForever is the other half of the
// same rule. Abandoned is terminal — nothing retries it — so holding the
// project's queue behind it would convert one lost event into a project that
// never receives another one.
func TestAnAbandonedEventDoesNotStallItsProjectForever(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "abOwner")
	seedSpaceMember(t, spaceA, "abOwner", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "order-abandon")
	w := doOn(t, r, "PUT", "/v1/projects/"+created.ProjectID, token,
		map[string]any{"name": "order-abandon2"})
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())

	p.lifecycleEventSender = &scriptedSender{fail: func(env lifecycleEventEnvelope) (lifecycleDeliveryResult, bool) {
		if env.EventType == LifecycleEventProjectCreated {
			// Not retryable: a terminal refusal, abandoned on the first attempt.
			return lifecycleDeliveryResult{Retryable: false, Class: lifecycleErrRejected, Detail: "422"}, true
		}
		return lifecycleDeliveryResult{}, false
	}}
	p.runLifecycleEventDelivery()
	rows := outboxRows(t, created.ProjectID)
	require.Len(t, rows, 2)
	require.EqualValues(t, lifecycleEventAbandoned, rows[0].Status)
	assert.EqualValues(t, lifecycleEventPending, rows[1].Status,
		"the claim takes at most one pending row per project, so the tail waits for the "+
			"next tick — it is not delivered alongside the head")

	// The NEXT tick is where the property lives. The claim declines a project that
	// still has an older PENDING sibling; `abandoned` is not pending, so the tail
	// becomes claimable as soon as the head reaches a terminal state.
	//
	// That distinction is the whole point. Abandoned is terminal and nothing
	// retries it, so blocking on it would convert one lost event into a project
	// that never receives another — while blocking on a merely FAILED head is
	// exactly what has to happen.
	p.runLifecycleEventDelivery()
	rows = outboxRows(t, created.ProjectID)
	assert.EqualValues(t, lifecycleEventDelivered, rows[1].Status,
		"a terminally abandoned event must not block the ones behind it: nothing will ever "+
			"retry it, so the project would never receive another event")
}

// TestUnusableConfigNeverEnqueues is the fix for the failure that was silent in
// the worst way.
//
// The gate used to check only that the URL and secret were non-empty while the
// CLIENT enforced their shape, so a URL with a query string or a short secret
// enabled the enqueue side and could never build a sender. claimLifecycleEvents
// charges an attempt before delivery is attempted, so every tick burned budget on
// rows that never reached the abandon check — the table grew without bound (the
// purge only touches terminal rows) and not one revocation left the process.
func TestUnusableConfigNeverEnqueues(t *testing.T) {
	good := strings.Repeat("s", 32)
	for name, cfg := range map[string]Config{
		"query string": {LifecycleEventsEnabled: true, LifecycleEventSecret: good,
			LifecycleEventURL: "https://peer.invalid/hook?v=1"},
		"no path": {LifecycleEventsEnabled: true, LifecycleEventSecret: good,
			LifecycleEventURL: "https://peer.invalid"},
		"relative": {LifecycleEventsEnabled: true, LifecycleEventSecret: good,
			LifecycleEventURL: "/internal/project-events"},
		"short secret": {LifecycleEventsEnabled: true, LifecycleEventSecret: strings.Repeat("s", 31),
			LifecycleEventURL: "https://peer.invalid/internal/project-events"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newOutboxTestProject(cfg)
			require.False(t, p.lifecycleEventsEnabled(),
				"the gate must refuse a configuration the delivery client cannot use; "+
					"otherwise the outbox grows forever and nothing is ever delivered")
			// A nil transaction is safe only BECAUSE the gate returns first.
			require.NoError(t, p.enqueueLifecycleEventTx(nil, lifecycleEventInput{
				EventType: LifecycleEventProjectCreated, ProjectID: "p", SpaceID: "s",
			}, time.Now().UTC()))
		})
	}
}

// TestTheGateAndTheClientAgree pins the property directly rather than through
// the cases above: whatever the client refuses, the gate must refuse. The two
// disagreeing is the whole defect, and a future URL rule added to only one of
// them would recreate it.
func TestTheGateAndTheClientAgree(t *testing.T) {
	good := strings.Repeat("s", 32)
	for _, tc := range []struct{ url, secret string }{
		{"https://peer.invalid/internal/project-events", good},
		{"https://peer.invalid/hook?v=1", good},
		{"https://peer.invalid/", good},
		{"https://peer.invalid", good},
		{"://nonsense", good},
		{"https://peer.invalid/internal/project-events", strings.Repeat("s", 31)},
		{"https://peer.invalid/internal/project-events", ""},
		{"", good},
	} {
		_, clientErr := newLifecycleHTTPClient(tc.url, tc.secret, time.Second)
		gateOK := newOutboxTestProject(Config{
			LifecycleEventsEnabled: true, LifecycleEventURL: tc.url, LifecycleEventSecret: tc.secret,
		}).lifecycleEventsEnabled()
		if (clientErr == nil) != gateOK {
			t.Errorf("url=%q secret=%d bytes: client accepts=%v but gate enables=%v",
				tc.url, len(tc.secret), clientErr == nil, gateOK)
		}
	}
}

// TestExhaustedRowsAreSweptRatherThanBlockingTheirProject is the other half of
// the claim-time budget predicate.
//
// The budget used to be evaluated only after Send returned, so a worker that
// died between claiming and completing re-charged an attempt on every lease
// expiry and the row never reached a terminal state. Adding `attempts < max` to
// the claim alone would make that strictly worse: the row becomes unclaimable,
// and because the claim declines any project with an older pending sibling, one
// such row blocks its project's queue permanently.
func TestExhaustedRowsAreSweptRatherThanBlockingTheirProject(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "sweepOwner")
	seedSpaceMember(t, spaceA, "sweepOwner", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "sweep-me")
	w := doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, token,
		map[string]any{"name": "sweep-me-2"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Reproduce the crash shape: the head row has spent its whole budget without
	// ever reaching a terminal state.
	rows := outboxRows(t, created.ProjectID)
	require.Len(t, rows, 2)
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_lifecycle_event` SET attempts = ? WHERE event_id = ?",
		lifecycleEventMaxAttempts, rows[0].EventID).Exec()
	require.NoError(t, err)

	sender := &scriptedSender{}
	p.lifecycleEventSender = sender

	// Before the sweep the project is stuck: the head cannot be claimed (no budget)
	// and the tail cannot be claimed (an older pending sibling exists).
	p.runLifecycleEventDelivery()
	assert.Empty(t, sender.snapshot(),
		"an exhausted head must not be claimed, and it must block its own tail while pending")

	p.sweepExhaustedLifecycleEvents()
	after := outboxRows(t, created.ProjectID)
	assert.EqualValues(t, lifecycleEventAbandoned, after[0].Status,
		"the sweep must give the row the terminal state the delivery path can no longer reach")
	assert.NotEmpty(t, after[0].LastError, "and say why, in the column the runbook reads")

	// And the project moves again.
	p.runLifecycleEventDelivery()
	assert.Equal(t, []string{created.ProjectID + "/" + LifecycleEventMetadataUpdated},
		sender.snapshot(), "once the head is terminal the tail is claimable")
}

// enqueueDirect writes n pending events for one project without going through a
// write path, so a burst can be built without 200 HTTP requests.
func enqueueDirect(t *testing.T, projectID, spaceID string, n int) {
	t.Helper()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		_, err := testCtx.DB().InsertBySql(
			"INSERT INTO `octo_project_lifecycle_event` "+
				"(event_id, event_type, project_id, space_id, payload, occurred_at, "+
				" next_attempt_at, created_at) "+
				"VALUES (?, ?, ?, ?, '{}', ?, ?, ?)",
			uuid.NewString(), LifecycleEventMemberRevoked, projectID, spaceID,
			now, now, now,
		).Exec()
		require.NoError(t, err)
	}
}

// TestOneTickDrainsAProjectsBurst is P2-2: ordering requires a project's events
// go in SEQUENCE, not one per poll interval.
//
// The claim returns at most one row per project, which is what makes ordering
// correct and correct across replicas. Left at that, throughput was exactly one
// event per five-second tick — and POST /:project_id/members/remove enqueues one
// revocation per uid on the SAME project, up to 200 by default. That is ~1000
// seconds to drain against a perfectly healthy peer, on the event class this
// module treats as a security failure to lose, and it drives the age gauge past
// any threshold after every bulk removal.
func TestOneTickDrainsAProjectsBurst(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "burstOwner")
	seedSpaceMember(t, spaceA, "burstOwner", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "burst")
	enqueueDirect(t, created.ProjectID, spaceA, 6)

	sender := &scriptedSender{}
	p.lifecycleEventSender = sender
	p.runLifecycleEventDelivery()

	rows := outboxRows(t, created.ProjectID)
	require.Len(t, rows, 7, "creation event plus the burst")
	for i, row := range rows {
		assert.EqualValues(t, lifecycleEventDelivered, row.Status,
			"row %d must be delivered in the SAME tick: one event per tick per project "+
				"is a throughput rule the ordering rule does not need", i)
	}
	assert.Len(t, sender.snapshot(), 7, "all seven reached the wire in one tick")
}

// TestTheDrainStillStopsAtTheFirstFailure: draining faster must not weaken the
// property the drain sits inside. An event still owed to the peer must not be
// overtaken by the ones behind it, whether the overtaking would happen on a
// later tick or inside this one.
func TestTheDrainStillStopsAtTheFirstFailure(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "drainStop")
	seedSpaceMember(t, spaceA, "drainStop", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "drain-stop")
	enqueueDirect(t, created.ProjectID, spaceA, 4)

	// The creation event succeeds; the FIRST revocation behind it fails.
	failed := 0
	p.lifecycleEventSender = &scriptedSender{
		fail: func(env lifecycleEventEnvelope) (lifecycleDeliveryResult, bool) {
			if env.EventType == LifecycleEventMemberRevoked {
				failed++
				if failed == 1 {
					return lifecycleDeliveryResult{
						Retryable: true, Class: lifecycleErrServer, Detail: "503"}, true
				}
			}
			return lifecycleDeliveryResult{}, false
		},
	}
	p.runLifecycleEventDelivery()

	assert.Equal(t, 1, failed,
		"the drain must stop at the first event still owed: the ones behind it would "+
			"reach the peer out of order, and a peer answering 'unknown' gives a plain "+
			"4xx, which is terminal here")

	rows := outboxRows(t, created.ProjectID)
	require.Len(t, rows, 5)
	assert.EqualValues(t, lifecycleEventDelivered, rows[0].Status, "the head succeeded")
	assert.EqualValues(t, lifecycleEventPending, rows[1].Status, "the failure is rescheduled")
	for i := 2; i < len(rows); i++ {
		assert.EqualValues(t, lifecycleEventPending, rows[i].Status,
			"row %d must not overtake the still-owed row above it", i)
		assert.Zero(t, rows[i].Attempts, "and must not be charged for a send that never happened")
	}
}

// TestTheSweepDrainsAcrossPasses is P2-5. purgeLifecycleEvents loops until a
// batch comes back short — "a fixed cap below the arrival rate is not a slower
// purge, it is no purge" — and the argument is stronger here, because every
// unswept row blocks its OWN project's queue.
func TestTheSweepDrainsAcrossPasses(t *testing.T) {
	_, p := setup(t)
	enableOutbox(t, p)
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "sweepDrain")
	seedSpaceMember(t, spaceA, "sweepDrain", 0, 1)

	created := createProjectOn(t, r, spaceA, token, "sweep-drain")
	// More than one batch of rows that spent their budget without a terminal outcome.
	enqueueDirect(t, created.ProjectID, spaceA, lifecycleEventBatch+5)
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_lifecycle_event` SET attempts = ? WHERE project_id = ?",
		lifecycleEventMaxAttempts, created.ProjectID).Exec()
	require.NoError(t, err)

	p.sweepExhaustedLifecycleEvents()

	for i, row := range outboxRows(t, created.ProjectID) {
		assert.EqualValues(t, lifecycleEventAbandoned, row.Status,
			"row %d must be swept in one tick: a fixed cap of %d per pass leaves the rest "+
				"blocking their own project's queue until the next tick, and a crash-looping "+
				"pod produces exactly that burst", i, lifecycleEventBatch)
	}
}
