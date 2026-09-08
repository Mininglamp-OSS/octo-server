package project

import (
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// newOutboxTestProject builds a bare instance for the validation paths, which
// all return before touching the transaction. Nothing here reaches the database.
func newOutboxTestProject(cfg Config) *Project {
	return &Project{cfg: cfg, Log: log.NewTLog("ProjectOutboxTest")}
}

func enabledOutboxConfig() Config {
	return Config{
		LifecycleEventsEnabled: true,
		LifecycleEventURL:      "https://peer.invalid/internal/project-events",
		LifecycleEventSecret:   strings.Repeat("t", 32),
		LifecycleEventTimeout:  time.Second,
	}
}

// TestEnqueueIsANoOpWhenDisabled pins the default posture: the switch is off, so
// nothing is written and no caller has to know about the switch.
func TestEnqueueIsANoOpWhenDisabled(t *testing.T) {
	p := newOutboxTestProject(Config{})
	// A nil transaction is safe precisely BECAUSE the gate returns first. If this
	// ever panics, the gate moved below the write and a disabled deployment
	// started building a backlog it can never deliver.
	if err := p.enqueueLifecycleEventTx(nil, lifecycleEventInput{
		EventType: LifecycleEventProjectCreated,
		ProjectID: "p", SpaceID: "s",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("disabled enqueue must be a silent no-op, got %v", err)
	}
}

// TestEnqueueFailsClosedOnIncompleteConfig is the half a plain on/off switch
// would miss. An enabled integration with no URL or no secret cannot
// deliver, so enqueueing would build a backlog whose head fails forever on the
// same misconfiguration. Refusing to enqueue keeps the table empty and leaves
// the problem visible in the startup log.
func TestEnqueueFailsClosedOnIncompleteConfig(t *testing.T) {
	cases := map[string]Config{
		"no url":    {LifecycleEventsEnabled: true, LifecycleEventSecret: strings.Repeat("t", 32)},
		"no secret": {LifecycleEventsEnabled: true, LifecycleEventURL: "https://peer.invalid/internal/project-events"},
		"neither":   {LifecycleEventsEnabled: true},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			p := newOutboxTestProject(cfg)
			if p.lifecycleEventsEnabled() {
				t.Fatal("an incompletely configured integration must not be considered enabled")
			}
			if err := p.enqueueLifecycleEventTx(nil, lifecycleEventInput{
				EventType: LifecycleEventProjectCreated, ProjectID: "p", SpaceID: "s",
			}, time.Now().UTC()); err != nil {
				t.Fatalf("must be a no-op rather than an error, got %v", err)
			}
		})
	}
}

// TestEnqueueRejectsUnknownEventType keeps a typo at the write, where the stack
// names the call site, instead of at the peer hours later where it surfaces as
// an unexplained rejection.
func TestEnqueueRejectsUnknownEventType(t *testing.T) {
	p := newOutboxTestProject(enabledOutboxConfig())
	err := p.enqueueLifecycleEventTx(nil, lifecycleEventInput{
		EventType: "project.renamed", ProjectID: "p", SpaceID: "s",
	}, time.Now().UTC())
	if err == nil {
		t.Fatal("an unknown event type must be refused")
	}
	if !strings.Contains(err.Error(), "project.renamed") {
		t.Errorf("the error should name the offending type, got %q", err)
	}
}

// TestEnqueueRequiresBothIdentifiers: space_id is not decoration. It is part of
// every predicate the peer will use to answer membership questions about this
// project, and an event missing it cannot be acted on.
func TestEnqueueRequiresBothIdentifiers(t *testing.T) {
	p := newOutboxTestProject(enabledOutboxConfig())
	for name, in := range map[string]lifecycleEventInput{
		"no project": {EventType: LifecycleEventArchived, SpaceID: "s"},
		"no space":   {EventType: LifecycleEventArchived, ProjectID: "p"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := p.enqueueLifecycleEventTx(nil, in, time.Now().UTC()); err == nil {
				t.Fatal("must be refused")
			}
		})
	}
}

// TestEventTypeStringsAreTheContract pins the wire values. They are compared as
// strings by the peer, so a refactor that "tidied" one into a constructed value
// would be invisible here and fatal there.
func TestEventTypeStringsAreTheContract(t *testing.T) {
	want := map[string]string{
		LifecycleEventProjectCreated:  "project.created",
		LifecycleEventMemberRevoked:   "project.member_revoked",
		LifecycleEventArchived:        "project.archived",
		LifecycleEventRestored:        "project.restored",
		LifecycleEventMetadataUpdated: "project.metadata_updated",
	}
	for got, expected := range want {
		if got != expected {
			t.Errorf("event type drifted: got %q, want %q", got, expected)
		}
		if !lifecycleEventTypes[got] {
			t.Errorf("%q is not in the accepted set, so enqueue would refuse it", got)
		}
	}
	if len(lifecycleEventTypes) != len(want) {
		t.Errorf("the accepted set has %d entries but %d are pinned here; "+
			"a new event type needs a wire-value assertion too", len(lifecycleEventTypes), len(want))
	}
}

// TestPayloadIsFrozenAtEnqueue is a source guard for the property the whole
// idempotency scheme rests on.
//
// The peer fingerprints the payload behind an event id and answers a permanent
// conflict when the same id arrives with different content. So the bytes must be
// serialized once, at enqueue, and replayed verbatim. If delivery ever re-derived
// them from live rows, a rename landing between two attempts would turn an
// ordinary redelivery into a permanent conflict — and it would only happen under
// a retry, which is exactly when nobody is watching.
func TestPayloadIsFrozenAtEnqueue(t *testing.T) {
	enqueue := readStripped(t, "lifecycle_event.go")
	if !strings.Contains(enqueue, "json.Marshal(in.Payload)") {
		t.Error("enqueueLifecycleEventTx must serialize the payload at enqueue time")
	}

	worker := readStripped(t, "lifecycle_event_worker.go")
	if strings.Contains(worker, "json.Marshal") {
		t.Error("the delivery worker must not marshal anything: the payload is stored bytes, " +
			"and re-deriving it would break idempotency on exactly the retry path")
	}
	if !strings.Contains(worker, "row.envelope()") {
		t.Error("delivery must send the stored row verbatim via envelope()")
	}
}

// TestOutboxWorkerDoesNotStartWhenDisabled guards the lesson this module already
// paid for once: an unconditional background worker ran three failing scans on
// every pod that had no projects and no traffic.
func TestOutboxWorkerDoesNotStartWhenDisabled(t *testing.T) {
	src := readStripped(t, "lifecycle_event_worker.go")
	start := funcBody(t, src, "func (p *Project) startLifecycleEventWorker(")
	gateAt := strings.Index(start, "lifecycleEventsEnabled()")
	scheduleAt := strings.Index(start, "p.ctx.Schedule")
	if gateAt < 0 {
		t.Fatal("startLifecycleEventWorker must consult lifecycleEventsEnabled()")
	}
	if scheduleAt < 0 {
		t.Fatal("startLifecycleEventWorker no longer schedules anything")
	}
	if gateAt > scheduleAt {
		t.Fatal("the enabled check must come BEFORE any Schedule call, or a deployment that " +
			"never enabled this feature still polls a table it does not use")
	}
}

// TestTruncateErrorBoundsTheColumn: last_error is VARCHAR(255) and is written
// from a path that can carry peer output. Truncating centrally means no call
// site can forget.
func TestTruncateErrorBoundsTheColumn(t *testing.T) {
	if got := truncateError(strings.Repeat("x", 300)); len(got) != 255 {
		t.Fatalf("want 255 characters, got %d", len(got))
	}
	if got := truncateError("short"); got != "short" {
		t.Fatalf("short values must pass through unchanged, got %q", got)
	}
}
