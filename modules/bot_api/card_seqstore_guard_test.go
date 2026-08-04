package bot_api

import (
	"strings"
	"testing"
)

// TestCardWriteFallbackNilSeqStoreReturnsError verifies the #627 review fix: the
// local card-write fallbacks (used when cardMutator == nil, e.g. hand-built
// &BotAPI{} fixtures) return a clear error instead of nil-panicking when seqStore
// is also unset. Both guards return before touching ba.ctx, so a zero-value
// BotAPI is a valid, panic-free input here.
func TestCardWriteFallbackNilSeqStoreReturnsError(t *testing.T) {
	ba := &BotAPI{} // no cardMutator, no seqStore, no ctx

	t.Run("cardSeqCASWrite", func(t *testing.T) {
		conflict, err := ba.cardSeqCASWrite("m1", 1, "ch", 2, "{}", "hash", 0, 1)
		if err == nil {
			t.Fatal("expected an error when seqStore is nil, got nil")
		}
		if conflict {
			t.Fatal("conflict must be false on the guard error path")
		}
		if !strings.Contains(err.Error(), "seqStore") {
			t.Fatalf("error should name the missing seqStore, got: %v", err)
		}
	})

	t.Run("cardVersionInLockWrite", func(t *testing.T) {
		err := ba.cardVersionInLockWrite("m1", 1, "ch", 2, "{}", "hash", 0)
		if err == nil {
			t.Fatal("expected an error when seqStore is nil, got nil")
		}
		if !strings.Contains(err.Error(), "seqStore") {
			t.Fatalf("error should name the missing seqStore, got: %v", err)
		}
	})
}
