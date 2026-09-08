package project

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// The project lifecycle outbox (O4).
//
// This file owns the PRODUCER side: the event vocabulary, the frozen payloads,
// and the one function a business transaction calls to publish. Delivery lives
// in lifecycle_event_worker.go, the wire call in lifecycle_client.go, and the SQL in
// db_lifecycle_event.go.

// Event types. These strings are wire contract with the consumer, so they are
// spelled here once and never assembled from parts.
const (
	// LifecycleEventProjectCreated announces a new project. The consumer provisions
	// its own record under the SAME id and the project is not usable until it
	// confirms — so this event is the only one whose failure is visible to the
	// person who triggered it.
	LifecycleEventProjectCreated = "project.created"
	// LifecycleEventMemberRevoked withdraws one member. This is the event whose loss
	// is a security failure rather than a stale display: until it lands, the
	// removed member can still execute on the consumer's side.
	LifecycleEventMemberRevoked = "project.member_revoked"
	// LifecycleEventArchived stops execution for the whole project.
	LifecycleEventArchived = "project.archived"
	// LifecycleEventRestored makes an archived project usable again. It does not
	// restore anything the revocations already tore down.
	LifecycleEventRestored = "project.restored"
	// LifecycleEventMetadataUpdated syncs the display copy of name and description.
	// The only event type whose loss is purely cosmetic.
	LifecycleEventMetadataUpdated = "project.metadata_updated"
)

// lifecycleEventTypes is the closed set. enqueueLifecycleEventTx refuses anything else,
// so a typo in a call site fails at the write rather than at the consumer, where
// it would surface as an unexplained rejection hours later.
var lifecycleEventTypes = map[string]bool{
	LifecycleEventProjectCreated:  true,
	LifecycleEventMemberRevoked:   true,
	LifecycleEventArchived:        true,
	LifecycleEventRestored:        true,
	LifecycleEventMetadataUpdated: true,
}

// Outbox row statuses.
const (
	lifecycleEventPending   = 0
	lifecycleEventDelivered = 1
	// lifecycleEventAbandoned is terminal. It is reached either by exhausting
	// attempts or by a refusal the consumer will never accept on retry — see
	// lifecycle_client.go's classification.
	lifecycleEventAbandoned = 2
)

// lifecycleEventInput is what a business transaction publishes.
//
// Payload is an ordinary Go value serialized ONCE, here, inside the caller's
// transaction. The bytes are then stored and replayed verbatim on every retry.
// Re-deriving them at delivery time would be a correctness bug rather than an
// optimization: the consumer fingerprints the payload behind the event id and
// answers a permanent conflict if the same id ever arrives with different
// content, so a rename landing between two attempts would poison the event.
type lifecycleEventInput struct {
	EventType string
	ProjectID string
	SpaceID   string
	// ProjectVersion is the project lifecycle_version as of the emitting
	// transaction, and it is NIL for member_revoked (contract §3).
	//
	// The nil is load-bearing rather than an omission. A consumer discards a
	// lifecycle statement whose version is not newer than what it holds — correct
	// for statements about the project, and catastrophic for a revocation, which
	// must be applied even when it arrives late: dropping it as stale leaves a
	// removed member executing. Revocations order by the member_epoch in their own
	// payload instead, against the roster, which is what they are about.
	ProjectVersion *int64
	Payload        any
	OccurredAt     time.Time
}

// ---------- payloads ----------
//
// Typed builders rather than raw maps at the call sites. A map lets a caller
// misspell a key and publish a well-formed event the consumer cannot act on;
// there is no schema check anywhere between here and the peer.

// projectCreatedPayload carries the creator and nothing else.
//
// No name, no description — see docs/project-lifecycle-contract.md §5. The name
// is user-supplied free text that octo-server holds authoritatively and does not
// egress; putting it here would make this a content channel, with its own
// escaping and disclosure questions, and would give the peer a second copy that
// can disagree with ours. A consumer that wants to display something builds a
// label from project_id or reads it back.
type projectCreatedPayload struct {
	CreatorUID string `json:"creator_uid"`
}

type memberRevokedPayload struct {
	SubjectUID string `json:"subject_uid"`
	// MemberEpoch is the roster epoch AFTER the revocation committed. The
	// consumer uses it to make the revocation idempotent across redeliveries and
	// to recognise a revocation it has already superseded.
	MemberEpoch int64  `json:"member_epoch"`
	Reason      string `json:"reason"`
}

type archivedPayload struct {
	Reason string `json:"reason"`
}

// metadataUpdatedPayload is deliberately EMPTY.
//
// The event is a change SIGNAL, not a sync: it says "this project's profile
// changed, at version N". That is the same shape member_epoch already has, and
// it is what keeps the name out of the wire (contract §5). An earlier draft
// carried name and description here.
//
// A struct rather than a bare map so the JSON is `{}` and not `null`: the
// contract says the payload key is always an object, and the two are different
// bytes to the fingerprint the consumer keys on.
type metadataUpdatedPayload struct{}

// restoredPayload is deliberately an empty object rather than a null: the
// contract says the payload key is always present, and an absent object and an
// empty one are different bytes to a fingerprint.
type restoredPayload struct{}

// ---------- envelope ----------

// lifecycleEventEnvelope is exactly what goes on the wire.
//
// Assembled at DELIVERY time from stored columns plus the stored payload bytes,
// not stored whole, so the envelope shape can be corrected without rewriting
// rows already queued. The payload — the only part the consumer fingerprints —
// is the part that stays byte-identical.
type lifecycleEventEnvelope struct {
	EventID        string          `json:"event_id"`
	EventType      string          `json:"event_type"`
	ProjectID      string          `json:"project_id"`
	SpaceID        string          `json:"space_id"`
	ProjectVersion *int64          `json:"project_version,omitempty"`
	OccurredAt     string          `json:"occurred_at"`
	Payload        json.RawMessage `json:"payload"`
}

// lifecycleEventRow is one outbox row as the worker sees it.
type lifecycleEventRow struct {
	ID             int64           `db:"id"`
	EventID        string          `db:"event_id"`
	EventType      string          `db:"event_type"`
	ProjectID      string          `db:"project_id"`
	SpaceID        string          `db:"space_id"`
	ProjectVersion *int64          `db:"project_version"`
	Payload        json.RawMessage `db:"payload"`
	OccurredAt     time.Time       `db:"occurred_at"`
	Attempts       int             `db:"attempts"`
}

// envelope renders the row for transmission. occurred_at is RFC3339 in UTC
// because the column is UTC by construction and the consumer parses a string.
func (r lifecycleEventRow) envelope() lifecycleEventEnvelope {
	return lifecycleEventEnvelope{
		EventID:        r.EventID,
		EventType:      r.EventType,
		ProjectID:      r.ProjectID,
		SpaceID:        r.SpaceID,
		ProjectVersion: r.ProjectVersion,
		OccurredAt:     r.OccurredAt.UTC().Format(time.RFC3339),
		Payload:        r.Payload,
	}
}

// ---------- enqueue ----------

// enqueueLifecycleEventTx records one event in the caller's transaction.
//
// It MUST be called inside the transaction that makes the change it reports.
// Publishing after the commit means a crash in between loses the publication
// with nothing downstream able to notice — the consumer cannot miss what it was
// never told about. That is the whole reason this table exists rather than an
// in-process event.
//
// Enqueue is gated by the same feature switch as delivery, deliberately. An
// event queued while delivery is off is a promise this deployment may not be
// able to keep: the consumer never saw the project.created that should have
// preceded it, so every later event for that project would be refused as
// referring to something unknown. Turning the integration on for a deployment
// that already has projects therefore needs a deliberate backfill, not an
// accidental replay of history — that backfill is not this function.
func (p *Project) enqueueLifecycleEventTx(tx *dbr.Tx, in lifecycleEventInput, now time.Time) error {
	if !p.lifecycleEventsEnabled() {
		return nil
	}
	if !lifecycleEventTypes[in.EventType] {
		return fmt.Errorf("project: unknown lifecycle event type %q", in.EventType)
	}
	if in.ProjectID == "" || in.SpaceID == "" {
		return fmt.Errorf("project: lifecycle event %s requires project_id and space_id", in.EventType)
	}
	payload, err := json.Marshal(in.Payload)
	if err != nil {
		return fmt.Errorf("project: marshal lifecycle event payload: %w", err)
	}
	eventID, err := newCanonicalUUID()
	if err != nil {
		return fmt.Errorf("project: generate lifecycle event id: %w", err)
	}
	occurred := in.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	row := lifecycleEventRow{
		EventID:        eventID,
		EventType:      in.EventType,
		ProjectID:      in.ProjectID,
		SpaceID:        in.SpaceID,
		ProjectVersion: in.ProjectVersion,
		Payload:        payload,
		OccurredAt:     occurred.UTC(),
	}
	if err := p.db.insertLifecycleEventTx(tx, row, now); err != nil {
		return err
	}
	lifecycleEventEnqueued.WithLabelValues(in.EventType).Inc()
	return nil
}
