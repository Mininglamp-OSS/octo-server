package carddispatch

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

type fakeMutationBackend struct {
	message       storedCardMessage
	lookupErr     error
	revoked       bool
	deleted       bool
	lifecycleErr  error
	hashExists    bool
	hashErr       error
	writeConflict bool
	writeReplay   bool
	writeErr      error
	syncErr       error
	syncFailUntil int
	writes        []cardMutationWrite
	revisions     []cardMutationWrite
	syncs         []cardMutationWrite
	effective     string
	effectiveSeq  int64
	effectiveSet  bool
	effectiveErr  error
}

func (b *fakeMutationBackend) EffectiveContent(string) (string, int64, bool, error) {
	return b.effective, b.effectiveSeq, b.effectiveSet, b.effectiveErr
}

func (b *fakeMutationBackend) Lookup(_ context.Context, _ CardMutationRequest) (storedCardMessage, error) {
	return b.message, b.lookupErr
}

func (b *fakeMutationBackend) Lifecycle(string) (bool, bool, error) {
	return b.revoked, b.deleted, b.lifecycleErr
}

func (b *fakeMutationBackend) ContentHashExists(string, string) (bool, error) {
	return b.hashExists, b.hashErr
}

func (b *fakeMutationBackend) CASWrite(write cardMutationWrite) (bool, bool, error) {
	b.writes = append(b.writes, write)
	return b.writeConflict, b.writeReplay, b.writeErr
}

func (b *fakeMutationBackend) AppendRevision(write cardMutationWrite) error {
	b.revisions = append(b.revisions, write)
	return nil
}

func (b *fakeMutationBackend) Sync(write cardMutationWrite) error {
	b.syncs = append(b.syncs, write)
	// syncFailUntil models a TRANSIENT IM blip: the first N attempts fail and the
	// next one succeeds, so a retry loop is observably distinguishable from a
	// single-shot fanout.
	if b.syncFailUntil > 0 {
		if len(b.syncs) <= b.syncFailUntil {
			return errors.New("im blip")
		}
		return nil
	}
	return b.syncErr
}

// noSleepMutator builds a mutator whose fanout retry does not actually wait, so
// the bounded-retry tests stay instant.
func noSleepMutator(backend cardMutationBackend) *CardMutator {
	mutator := newCardMutator(backend)
	mutator.sleep = func(time.Duration) {}
	return mutator
}

func TestCardMutatorPreservesOwnershipLifecycleCASAndReplay(t *testing.T) {
	original := testCardEnvelope(t, 0, true)
	terminal := testCardEnvelope(t, 42, false)

	t.Run("applies canonical terminal frame", func(t *testing.T) {
		backend := &fakeMutationBackend{message: storedCardMessage{
			MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original,
		}}
		mutator := newCardMutator(backend)
		result, err := mutator.Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(terminal),
		})
		if err != nil {
			t.Fatalf("Mutate() error = %v", err)
		}
		if !result.Applied || result.Replay {
			t.Fatalf("Mutate() result = %+v", result)
		}
		if len(backend.writes) != 1 || backend.writes[0].CardSeq != 42 {
			t.Fatalf("CAS writes = %+v", backend.writes)
		}
		if len(backend.revisions) != 1 || len(backend.syncs) != 1 {
			t.Fatalf("revision/sync counts = %d/%d, want 1/1", len(backend.revisions), len(backend.syncs))
		}
		if backend.writes[0].ContentEdit == string(terminal) {
			t.Fatal("Mutate() must persist cardmsg-normalized canonical bytes, not trust caller bytes verbatim")
		}
	})

	t.Run("transient frame skips revision but still syncs", func(t *testing.T) {
		transient := testCardEnvelope(t, 43, false)
		var envelope map[string]any
		if err := json.Unmarshal(transient, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope["transient"] = true
		transient, _ = json.Marshal(envelope)
		backend := &fakeMutationBackend{message: storedCardMessage{
			MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original,
		}}
		result, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(transient),
		})
		if err != nil || !result.Applied {
			t.Fatalf("Mutate(transient) = (%+v, %v)", result, err)
		}
		if len(backend.writes) != 1 || len(backend.syncs) != 1 {
			t.Fatalf("write/sync counts = %d/%d, want 1/1", len(backend.writes), len(backend.syncs))
		}
		if len(backend.revisions) != 0 {
			t.Fatalf("transient revisions = %d, want 0", len(backend.revisions))
		}
	})

	t.Run("byte identical frame is idempotent replay", func(t *testing.T) {
		backend := &fakeMutationBackend{
			message:    storedCardMessage{MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original},
			hashExists: true,
		}
		result, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(terminal),
		})
		if err != nil || !result.Replay || result.Applied {
			t.Fatalf("Mutate(replay) = (%+v, %v)", result, err)
		}
		if len(backend.writes) != 0 || len(backend.revisions) != 0 {
			t.Fatal("idempotent replay must not repeat the side-effecting CAS write or revision append")
		}
		// The contentless CMD IS re-sent. An identical stored frame proves the content
		// landed, never that a client was told: a prior attempt may have committed
		// content_edit and lost its fanout (Sync error, crash, lease expiry), and the
		// at-least-once card-action dispatcher replays it here. Skipping the signal
		// leaves the card actionable on every online client until a full conversation
		// re-fetch.
		if len(backend.syncs) != 1 {
			t.Fatalf("idempotent replay must re-fan-out the CMD, syncs = %d, want 1", len(backend.syncs))
		}
	})

	t.Run("concurrent identical CAS winner is idempotent replay", func(t *testing.T) {
		backend := &fakeMutationBackend{
			message:     storedCardMessage{MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original},
			writeReplay: true,
		}
		result, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(terminal),
		})
		if err != nil || !result.Replay || result.Applied {
			t.Fatalf("Mutate(concurrent replay) = (%+v, %v)", result, err)
		}
		if len(backend.revisions) != 0 {
			t.Fatal("concurrent replay must not append a duplicate revision")
		}
		// Contentless, idempotent signal: the CAS winner's own CMD may still be in
		// flight or already lost, so re-sending closes the window where neither
		// writer's fanout reached the client.
		if len(backend.syncs) != 1 {
			t.Fatalf("concurrent replay must re-fan-out the CMD, syncs = %d, want 1", len(backend.syncs))
		}
	})

	t.Run("transient fanout failure is retried until it lands", func(t *testing.T) {
		backend := &fakeMutationBackend{
			message:       storedCardMessage{MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original},
			syncFailUntil: 1,
		}
		result, err := noSleepMutator(backend).Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(terminal),
		})
		// A swallowed Sync error is re-driven by NOTHING: a successful Finalize makes
		// the dispatcher Ack the event outright, so the replay branches above never
		// see this failure. The in-process retry is therefore the ONLY recovery for a
		// transient IM blip that does not fail the mutation.
		if err != nil || !result.Applied {
			t.Fatalf("Mutate(transient sync failure) = (%+v, %v), want applied with nil error", result, err)
		}
		if len(backend.syncs) != 2 {
			t.Fatalf("transient fanout failure must be retried, syncs = %d, want 2", len(backend.syncs))
		}
		if len(backend.writes) != 1 {
			t.Fatalf("fanout retry must not re-write the CAS, writes = %d, want 1", len(backend.writes))
		}
	})

	t.Run("fanout failure keeps the committed mutation successful", func(t *testing.T) {
		backend := &fakeMutationBackend{
			message: storedCardMessage{MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original},
			syncErr: errors.New("im unavailable"),
		}
		result, err := noSleepMutator(backend).Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(terminal),
		})
		// The CMD stays SECONDARY to the authoritative content_edit: the frame is
		// already committed and a client that misses the signal still resolves the
		// terminal card on its next conversation fetch, so a dead IM must not fail an
		// otherwise-successful mutation — and must not push the card action toward the
		// dispatcher's DLQ, where its Redis idempotency claim would strand the card.
		if err != nil || !result.Applied {
			t.Fatalf("Mutate(sync failure) = (%+v, %v), want applied with nil error", result, err)
		}
		// The retry is BOUNDED: a hard IM outage must not hold the dispatcher worker.
		if len(backend.writes) != 1 || len(backend.syncs) != cardMutationFanoutMaxAttempts {
			t.Fatalf("writes/syncs = %d/%d, want 1/%d", len(backend.writes), len(backend.syncs), cardMutationFanoutMaxAttempts)
		}
	})

	t.Run("stale card seq conflict must not fan out", func(t *testing.T) {
		backend := &fakeMutationBackend{
			message:       storedCardMessage{MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original},
			writeConflict: true,
		}
		_, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(terminal),
		})
		// A conflict means a NEWER frame already won; this stale attempt committed
		// nothing, so it announces nothing and lets the caller surface the conflict.
		if !errors.Is(err, ErrCardMutationConflict) {
			t.Fatalf("Mutate(conflict) error = %v, want %v", err, ErrCardMutationConflict)
		}
		if len(backend.syncs) != 0 {
			t.Fatalf("conflicting stale write must not fan out, syncs = %d", len(backend.syncs))
		}
	})

	tests := []struct {
		name    string
		backend fakeMutationBackend
		req     CardMutationRequest
		wantErr error
	}{
		{
			name: "wrong sender",
			backend: fakeMutationBackend{message: storedCardMessage{
				MessageID: "1001", MessageSeq: 7, FromUID: "other", Payload: original,
			}},
			req:     CardMutationRequest{SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1, ContentEdit: string(terminal)},
			wantErr: ErrCardMutationForbidden,
		},
		{
			name: "revoked",
			backend: fakeMutationBackend{message: storedCardMessage{
				MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original,
			}, revoked: true},
			req:     CardMutationRequest{SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1, ContentEdit: string(terminal)},
			wantErr: ErrCardMutationNotFound,
		},
		{
			name: "stale card seq",
			backend: fakeMutationBackend{message: storedCardMessage{
				MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: original,
			}, writeConflict: true},
			req:     CardMutationRequest{SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1, ContentEdit: string(terminal)},
			wantErr: ErrCardMutationConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newCardMutator(&tt.backend).Mutate(context.Background(), tt.req)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Mutate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func testCardEnvelope(t *testing.T, cardSeq int64, withAction bool) []byte {
	t.Helper()
	card := map[string]interface{}{
		"type": "AdaptiveCard", "version": cardmsg.CardVersion,
		"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "Document access"}},
	}
	if withAction {
		card["actions"] = []interface{}{map[string]interface{}{
			"type": "Action.Submit", "id": "approve", "title": "Allow",
		}}
	}
	envelope := map[string]interface{}{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
		"profile": cardmsg.ProfileV2, "card": card, "space_id": "space-1",
	}
	if cardSeq > 0 {
		envelope["card_seq"] = cardSeq
	}
	raw, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatalf("marshal test card: %v", err)
	}
	return raw
}

// markedTestCardEnvelope is testCardEnvelope plus the server-authored catalog
// markers a Registry render carries: both top-level keys and the matching
// metadata.octo.template they are validated against.
func markedTestCardEnvelope(t *testing.T, cardSeq int64, withAction bool) []byte {
	t.Helper()
	var envelope map[string]interface{}
	if err := json.Unmarshal(testCardEnvelope(t, cardSeq, withAction), &envelope); err != nil {
		t.Fatal(err)
	}
	card := envelope["card"].(map[string]interface{})
	card["metadata"] = map[string]interface{}{
		"octo": map[string]interface{}{
			"protocol": "octo-card@1.0",
			"template": map[string]interface{}{"id": "docs.access-request", "version": "0.3.0"},
		},
	}
	envelope[cardmsg.CatalogTemplateRefKey] = map[string]interface{}{
		"id": "docs.access-request", "version": "0.3.0",
	}
	envelope[cardmsg.CatalogProvenanceKey] = cardmsg.CatalogProvenance{
		Version:       cardmsg.CatalogProvenanceVersion,
		PrincipalType: cardmsg.CatalogPrincipalWireInternalProducer,
		PrincipalID:   "docs-notify",
		SpaceID:       "space-1",
	}.MarshalMap()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// PR-C review round 6 (yujiawei P1-2), the structural half.
//
// The catalog markers are the card's identity and identity does not change
// under an edit. pkg/cardtmpl/updater.go states that, but only the paths that
// go through the updater honoured it: a caller assembling a replacement from
// its own key set simply dropped them, and both identity guards — the
// stored-version pin and the cross-Space refusal — begin `if markers.Has…`, so
// they went inert for that message. Enforcing it here means no future call site
// can silently downgrade a marked card, whatever frame it builds.
func TestCardMutatorRefusesToDropStoredCatalogMarkers(t *testing.T) {
	marked := markedTestCardEnvelope(t, 0, true)

	t.Run("a replacement that drops the markers is refused", func(t *testing.T) {
		backend := &fakeMutationBackend{message: storedCardMessage{
			MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: marked,
		}}
		_, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(testCardEnvelope(t, 42, false)), // legacy six-key rebuild
		})
		if !errors.Is(err, ErrCardMutationInvalid) {
			t.Fatalf("Mutate() error = %v, want ErrCardMutationInvalid", err)
		}
		if len(backend.writes) != 0 {
			t.Fatalf("a marker-erasing frame was persisted: %+v", backend.writes)
		}
	})

	t.Run("a replacement that carries them through is applied", func(t *testing.T) {
		backend := &fakeMutationBackend{message: storedCardMessage{
			MessageID: "1001", MessageSeq: 7, FromUID: "notification", Payload: marked,
		}}
		result, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(markedTestCardEnvelope(t, 42, false)),
		})
		if err != nil {
			t.Fatalf("Mutate() error = %v", err)
		}
		if !result.Applied || len(backend.writes) != 1 {
			t.Fatalf("result = %+v writes = %+v", result, backend.writes)
		}
	})

	t.Run("the legacy population is untouched", func(t *testing.T) {
		backend := &fakeMutationBackend{message: storedCardMessage{
			MessageID: "1001", MessageSeq: 7, FromUID: "notification",
			Payload: testCardEnvelope(t, 0, true),
		}}
		result, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
			SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
			ContentEdit: string(testCardEnvelope(t, 42, false)),
		})
		if err != nil || !result.Applied {
			t.Fatalf("an unmarked card must still be editable: result = %+v err = %v", result, err)
		}
	})
}

// prePRCRegistryEnvelope is what a Bot Registry card already in storage looks
// like: the merge-base renderPayload stamped `template_ref` only ("Retain only
// the canonical ref as server-authored provenance"), so every such frame is
// HasRef=true, HasProvenance=false.
//
// It is built here rather than reused from markedTestCardEnvelope because the
// bug this covers was invisible to a fixture built from the *new* send path —
// that path always writes both markers, so it can never produce the shape the
// production population actually has.
func prePRCRegistryEnvelope(t *testing.T, cardSeq int64, withAction bool) []byte {
	t.Helper()
	var envelope map[string]interface{}
	if err := json.Unmarshal(markedTestCardEnvelope(t, cardSeq, withAction), &envelope); err != nil {
		t.Fatal(err)
	}
	delete(envelope, cardmsg.CatalogProvenanceKey)
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// PR-C review round 8: one case per frame shape reachable in production today,
// against the mutation boundary every replacement passes through.
//
// Both blockers found in review shared a shape — a guard moved to a boundary,
// with the compatibility consequence for already-stored frames reasoned about
// in a comment instead of pinned by a test that starts from pre-change bytes.
// The grid is the answer to that: the axis that was missing is not a case, it
// is the stored side.
func TestCardMutatorMarkerRuleCoversEveryStoredFrameShape(t *testing.T) {
	for _, test := range []struct {
		name    string
		stored  func(*testing.T, int64, bool) []byte
		next    func(*testing.T, int64, bool) []byte
		wantErr bool
	}{
		{
			name:   "legacy stays legacy",
			stored: testCardEnvelope, next: testCardEnvelope,
		},
		{
			// The regression. Every Bot Registry card already in storage is this
			// shape, and an edit is the only way it can ever acquire provenance —
			// so refusing the acquire made those cards permanently uneditable,
			// with the refusal reported as "drops the stored catalog markers".
			name:   "a pre-PR-C ref-only frame may acquire provenance",
			stored: prePRCRegistryEnvelope, next: markedTestCardEnvelope,
		},
		{
			name:   "a marked frame may be replaced by the same markers",
			stored: markedTestCardEnvelope, next: markedTestCardEnvelope,
		},
		{
			name:   "a marked frame may not lose them",
			stored: markedTestCardEnvelope, next: testCardEnvelope,
			wantErr: true,
		},
		{
			name:   "a ref-only frame may not lose its ref",
			stored: prePRCRegistryEnvelope, next: testCardEnvelope,
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeMutationBackend{message: storedCardMessage{
				MessageID: "1001", MessageSeq: 7, FromUID: "notification",
				Payload: test.stored(t, 0, true),
			}}
			_, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
				SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
				ContentEdit: string(test.next(t, 42, false)),
			})
			if test.wantErr {
				if !errors.Is(err, ErrCardMutationInvalid) {
					t.Fatalf("Mutate() error = %v, want ErrCardMutationInvalid", err)
				}
				if len(backend.writes) != 0 {
					t.Fatalf("a refused replacement was persisted: %+v", backend.writes)
				}
				return
			}
			if err != nil {
				t.Fatalf("Mutate() error = %v, want success", err)
			}
			if len(backend.writes) != 1 {
				t.Fatalf("writes = %+v, want 1", backend.writes)
			}
		})
	}
}

// Identity does not change under an edit, so a replacement that keeps both keys
// and rewrites one of the values is refused too. Presence-only comparison let
// that through; no in-tree caller reaches it, which is why it needs its own
// test rather than waiting for one to appear.
func TestCardMutatorRefusesToRewriteStoredMarkerValues(t *testing.T) {
	rewrite := func(t *testing.T, mutate func(map[string]interface{})) []byte {
		t.Helper()
		var envelope map[string]interface{}
		if err := json.Unmarshal(markedTestCardEnvelope(t, 42, false), &envelope); err != nil {
			t.Fatal(err)
		}
		mutate(envelope)
		raw, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{
			name: "a different producer",
			mutate: func(envelope map[string]interface{}) {
				envelope[cardmsg.CatalogProvenanceKey] = cardmsg.CatalogProvenance{
					Version:       cardmsg.CatalogProvenanceVersion,
					PrincipalType: cardmsg.CatalogPrincipalWireInternalProducer,
					PrincipalID:   "someone-else",
					SpaceID:       "space-1",
				}.MarshalMap()
			},
		},
		{
			name: "a different Space",
			mutate: func(envelope map[string]interface{}) {
				envelope[cardmsg.CatalogProvenanceKey] = cardmsg.CatalogProvenance{
					Version:       cardmsg.CatalogProvenanceVersion,
					PrincipalType: cardmsg.CatalogPrincipalWireInternalProducer,
					PrincipalID:   "docs-notify",
					SpaceID:       "space-elsewhere",
				}.MarshalMap()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeMutationBackend{message: storedCardMessage{
				MessageID: "1001", MessageSeq: 7, FromUID: "notification",
				Payload: markedTestCardEnvelope(t, 0, true),
			}}
			_, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
				SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
				ContentEdit: string(rewrite(t, test.mutate)),
			})
			if !errors.Is(err, ErrCardMutationInvalid) {
				t.Fatalf("Mutate() error = %v, want ErrCardMutationInvalid", err)
			}
			if len(backend.writes) != 0 {
				t.Fatalf("a rewritten marker was persisted: %+v", backend.writes)
			}
		})
	}
}

// PR-C review round 9 (Jerry-Xin and yujiawei, independently): the guard read
// message.Payload, the immutable original, while edits persist to
// message_extra.content_edit. Snapshot honours that distinction twelve lines
// above; Mutate did not.
//
// The population this strands is the one the round-8 relaxation created. A
// pre-PR-C card is HasProvenance=false in the original forever, so once the
// first edit legitimately acquires provenance into content_edit, every later
// edit was compared against bytes that no longer describe the card: the
// provenance could be dropped outright, or kept and rewritten, and both clauses
// were dead.
//
// The missing grid axis is not a case, it is the second dimension — the
// effective frame differing from the original.
func TestCardMutatorComparesAgainstTheEffectiveFrameNotTheOriginal(t *testing.T) {
	for _, test := range []struct {
		name      string
		original  func(*testing.T, int64, bool) []byte
		effective func(*testing.T, int64, bool) []byte
		next      func(*testing.T, int64, bool) []byte
		wantErr   bool
	}{
		{
			// The exact sequence: pre-PR-C card, edit #1 acquires provenance,
			// edit #2 drops it. Against the original this looked like no change.
			name:     "provenance acquired by an earlier edit may not be dropped by a later one",
			original: prePRCRegistryEnvelope, effective: markedTestCardEnvelope, next: prePRCRegistryEnvelope,
			wantErr: true,
		},
		{
			name:     "nor rewritten once the effective frame carries it",
			original: prePRCRegistryEnvelope, effective: markedTestCardEnvelope,
			next: func(t *testing.T, seq int64, act bool) []byte {
				t.Helper()
				var envelope map[string]interface{}
				if err := json.Unmarshal(markedTestCardEnvelope(t, seq, act), &envelope); err != nil {
					t.Fatal(err)
				}
				envelope[cardmsg.CatalogProvenanceKey] = cardmsg.CatalogProvenance{
					Version:       cardmsg.CatalogProvenanceVersion,
					PrincipalType: cardmsg.CatalogPrincipalWireInternalProducer,
					PrincipalID:   "someone-else",
					SpaceID:       "space-1",
				}.MarshalMap()
				raw, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			},
			wantErr: true,
		},
		{
			name:     "carrying the acquired provenance through is applied",
			original: prePRCRegistryEnvelope, effective: markedTestCardEnvelope, next: markedTestCardEnvelope,
		},
		{
			// The gate that used to skip the whole check when the *original*
			// carried nothing: a legacy original whose effective frame has since
			// been marked still gets the protection.
			name:     "an unmarked original does not disable the check",
			original: testCardEnvelope, effective: markedTestCardEnvelope, next: testCardEnvelope,
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			effective := test.effective(t, 41, false)
			backend := &fakeMutationBackend{
				message: storedCardMessage{
					MessageID: "1001", MessageSeq: 7, FromUID: "notification",
					Payload: test.original(t, 0, true),
				},
				effective: string(effective), effectiveSeq: 41, effectiveSet: true,
			}
			_, err := newCardMutator(backend).Mutate(context.Background(), CardMutationRequest{
				SenderUID: "notification", MessageID: "1001", ChannelID: "user-b", ChannelType: 1,
				ContentEdit: string(test.next(t, 42, false)),
			})
			if test.wantErr {
				if !errors.Is(err, ErrCardMutationInvalid) {
					t.Fatalf("Mutate() error = %v, want ErrCardMutationInvalid", err)
				}
				if len(backend.writes) != 0 {
					t.Fatalf("a refused replacement was persisted: %+v", backend.writes)
				}
				return
			}
			if err != nil {
				t.Fatalf("Mutate() error = %v, want success", err)
			}
			if len(backend.writes) != 1 {
				t.Fatalf("writes = %+v, want 1", backend.writes)
			}
		})
	}
}
