package msgextraseq_test

// Tests for the #627 operator activation surface (PR-3): Preflight (read-only
// floor recommendation) and Activate (legacy→transactional drain-barrier flip
// with floor lower/upper bound enforcement + idempotency). There is no online
// deactivate to test — rollback is a documented coordinated procedure (README §5).

import (
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
)

// TestLegacySeqKeyFormatMatchesRunbook locks the legacy GenSeq key scheme that
// two out-of-band consumers depend on: Preflight/Activate's observedMaxima keys
// the legacy boundary by the `seq:messageExtra:%` LIKE prefix, and the coordinated
// rollback runbook (tools/msgextra-version/README.md §5) raises those same
// `seq:messageExtra:<channel>` rows above the transactional high-water. Since
// rollback correctness now lives only in docs (there is no online Deactivate),
// this guards the runbook SQL against silently drifting from the allocator's key
// scheme (Jerry-Xin PR #648 re-review note). A pure assertion — no DB.
func TestLegacySeqKeyFormatMatchesRunbook(t *testing.T) {
	if got := common.MessageExtraSeqKey; got != "messageExtra" {
		t.Fatalf("MessageExtraSeqKey = %q, want %q — README §5 rollback SQL keys on seq:messageExtra:<channel>", got, "messageExtra")
	}
	const wantPrefix = "seq:messageExtra:%"
	if got := fmt.Sprintf("seq:%s:%%", common.MessageExtraSeqKey); got != wantPrefix {
		t.Fatalf("observedMaxima legacy LIKE prefix = %q, want %q", got, wantPrefix)
	}
}

func TestPreflightRecommendsMaxOfEvidence(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	s := msgextraseq.New(ctx)

	// No rows yet: recommended floor is 0, mode legacy.
	res, err := s.Preflight()
	if err != nil {
		t.Fatalf("Preflight (empty): %v", err)
	}
	if res.CurrentMode != msgextraseq.ModeLegacy || res.RecommendedFloor != 0 {
		t.Fatalf("empty preflight = %+v, want legacy/floor 0", res)
	}

	// message_extra max 500, legacy seq boundary 900 → recommend 900.
	if _, err := ctx.DB().UpdateBySql(
		"INSERT INTO `message_extra` (`message_id`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,?)",
		"m1", "ch-a", uint8(2), int64(500),
	).Exec(); err != nil {
		t.Fatalf("seed message_extra: %v", err)
	}
	if _, err := ctx.DB().UpdateBySql(
		"INSERT INTO `seq` (`key`,`min_seq`,`step`) VALUES (?,?,0)",
		"seq:messageExtra:ch-a", int64(900),
	).Exec(); err != nil {
		t.Fatalf("seed seq: %v", err)
	}
	res, err = s.Preflight()
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if res.MaxMessageExtraVersion != 500 || res.MaxLegacySeqBoundary != 900 || res.RecommendedFloor != 900 {
		t.Fatalf("preflight = %+v, want maxVer 500 / maxSeq 900 / floor 900", res)
	}
}

func TestActivateFlipsAndIsIdempotent(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	s := msgextraseq.New(ctx)
	if _, err := ctx.DB().UpdateBySql(
		"INSERT INTO `message_extra` (`message_id`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,?)",
		"m1", "ch-a", uint8(2), int64(120),
	).Exec(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	flipped, err := s.Activate(200)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !flipped {
		t.Fatal("Activate: expected flipped=true from legacy")
	}
	res, err := s.Preflight()
	if err != nil {
		t.Fatalf("Preflight after activate: %v", err)
	}
	if res.CurrentMode != msgextraseq.ModeTransactional || res.CurrentFloor != 200 || res.CurrentEpoch != 1 {
		t.Fatalf("post-activate state = %+v, want transactional/floor 200/epoch 1", res)
	}

	// Second activate is a no-op (idempotent), epoch unchanged.
	flipped, err = s.Activate(200)
	if err != nil {
		t.Fatalf("Activate (again): %v", err)
	}
	if flipped {
		t.Fatal("Activate: second call must be a no-op (flipped=false)")
	}
	res, _ = s.Preflight()
	if res.CurrentEpoch != 1 {
		t.Fatalf("epoch bumped on idempotent activate: %d", res.CurrentEpoch)
	}
}

func TestActivateRejectsFloorBelowObservedMax(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	s := msgextraseq.New(ctx)
	if _, err := ctx.DB().UpdateBySql(
		"INSERT INTO `message_extra` (`message_id`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,?)",
		"m1", "ch-a", uint8(2), int64(1000),
	).Exec(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Floor 999 is below the observed max 1000 → refused, state unchanged.
	if _, err := s.Activate(999); err == nil {
		t.Fatal("Activate: expected ErrFloorTooLow for a floor below the observed max")
	}
	res, _ := s.Preflight()
	if res.CurrentMode != msgextraseq.ModeLegacy {
		t.Fatalf("state changed despite refused activate: %+v", res)
	}
}

func TestActivateRejectsFloorAboveMax(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	s := msgextraseq.New(ctx)
	// A floor above MaxCutoverFloor would leave no headroom below 2^53-1 and make
	// every reservation fail ErrOverflow → refused, state unchanged.
	if _, err := s.Activate(msgextraseq.MaxCutoverFloor + 1); err == nil {
		t.Fatal("Activate: expected ErrFloorTooHigh for a floor above MaxCutoverFloor")
	}
	res, _ := s.Preflight()
	if res.CurrentMode != msgextraseq.ModeLegacy {
		t.Fatalf("state changed despite refused activate: %+v", res)
	}
	// The maximum floor itself is accepted.
	if flipped, err := s.Activate(msgextraseq.MaxCutoverFloor); err != nil || !flipped {
		t.Fatalf("Activate(MaxCutoverFloor) = (%v, %v), want (true, nil)", flipped, err)
	}
}
