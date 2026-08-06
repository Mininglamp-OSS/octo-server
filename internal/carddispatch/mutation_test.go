package carddispatch

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
	return nil
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
		if len(backend.writes)+len(backend.revisions)+len(backend.syncs) != 0 {
			t.Fatal("idempotent replay must not write CAS, revision, or CMD")
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
		if len(backend.revisions)+len(backend.syncs) != 0 {
			t.Fatal("concurrent replay must not append a duplicate revision or CMD")
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
