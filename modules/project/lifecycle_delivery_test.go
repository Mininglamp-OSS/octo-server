package project

import (
	"context"
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
	assert.EqualValues(t, lifecycleEventAbandoned, rows[0].Status)
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
