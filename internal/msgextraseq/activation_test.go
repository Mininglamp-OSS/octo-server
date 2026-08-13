package msgextraseq_test

// Tests for the #627 operator activation surface (PR-3): Preflight (read-only
// floor recommendation) and Activate (legacy→transactional drain-barrier flip
// with floor lower/upper bound enforcement + idempotency). There is no online
// deactivate to test — rollback is a documented coordinated procedure (README §6).

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
	"github.com/go-sql-driver/mysql"
)

// TestRolloutOrdering_ActivateBeforeExpectedMode locks the activation runbook's
// mandatory sequencing (README §3 flip BEFORE §5 guard): the durable expected-mode
// guard (OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE=transactional) must only be
// enabled AFTER the DB flip commits. Enabling it while the state row is still
// legacy — e.g. a rolling deploy that ships the env before activation lands —
// fails every writer closed with ErrExpectedModeMismatch (a self-inflicted
// outage). Once Activate flips the row to transactional, the same writer succeeds.
// This is the read-side proof that the runbook's activate-first order is the safe
// one (Jerry-Xin PR #648 re-review).
func TestRolloutOrdering_ActivateBeforeExpectedMode(t *testing.T) {
	// Pre-activation: DB mode=legacy, but a replica already booted expecting
	// transactional (guard-before-flip — the misordering the runbook forbids).
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	t.Setenv("OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE", "transactional")
	s := msgextraseq.New(ctx)

	// Guard-before-flip: writers fail closed — the outage the §2 warning avoids.
	tx, err := ctx.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.ReserveTx(tx, "c-order", 2, 1); !errors.Is(err, msgextraseq.ErrExpectedModeMismatch) {
		t.Fatalf("legacy row + expected=transactional must fail closed, got %v", err)
	}
	_ = tx.Rollback()

	// Flip the DB to transactional. Activate does NOT consult the expected-mode
	// guard, so the operator can always run it first (runbook §3).
	flipped, err := s.Activate(0)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !flipped {
		t.Fatal("Activate should have flipped legacy->transactional")
	}

	// Post-activation (runbook §5 order): the same writer now succeeds because the
	// resolved mode matches the expected mode.
	if got := reserveCommitted(t, ctx, s, "c-order", 2, 1); len(got) != 1 {
		t.Fatalf("post-activation reserve len=%d want 1", len(got))
	}
}

// TestLegacySeqKeyFormatMatchesRunbook locks the legacy GenSeq key scheme that
// two out-of-band consumers depend on: Preflight/Activate's observedMaxima keys
// the legacy boundary by the `seq:messageExtra:%` LIKE prefix, and the coordinated
// rollback runbook (docs/msgextra-cutover-runbook.md §6) raises those same
// `seq:messageExtra:<channel>` rows above the transactional high-water. Since
// rollback correctness now lives only in docs (there is no online Deactivate),
// this guards the runbook SQL against silently drifting from the allocator's key
// scheme (Jerry-Xin PR #648 re-review note). A pure assertion — no DB.
func TestLegacySeqKeyFormatMatchesRunbook(t *testing.T) {
	if got := common.MessageExtraSeqKey; got != "messageExtra" {
		t.Fatalf("MessageExtraSeqKey = %q, want %q — README §6 rollback SQL keys on seq:messageExtra:<channel>", got, "messageExtra")
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

func TestPreflightClassifiesPoisonedRedisCursorWithoutBlockingActivation(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	s := msgextraseq.New(ctx)

	if _, err := ctx.DB().UpdateBySql(
		"INSERT INTO `message_extra` (`message_id`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,?)",
		"cursor-evidence-message", "cursor-evidence-channel", uint8(2), int64(1000),
	).Exec(); err != nil {
		t.Fatalf("seed message_extra: %v", err)
	}

	// A legitimate cursor is at or below the globally issued DB/legacy ceiling.
	// A historical poisoned cursor above that ceiling cannot have been returned
	// by the server and must not force activation to an impossible floor.
	const (
		key             = "messageExtraVersion:msgextraseq-preflight-test"
		validField      = "valid-channel-2"
		ceilingField    = "ceiling-channel-2"
		poisonField     = "poison-channel-2"
		malformedField  = "malformed-channel-2"
		negativeField   = "negative-channel-2"
		validCursor     = int64(900)
		ceilingCursor   = int64(1000)
		poisonCursor    = msgextraseq.MaxCutoverFloor
		malformedCursor = "not-an-integer"
		negativeCursor  = "-1"
	)
	redis := ctx.GetRedisConn()
	if err := redis.Hmset(
		key,
		validField, fmt.Sprintf("%d", validCursor),
		ceilingField, fmt.Sprintf("%d", ceilingCursor),
		poisonField, fmt.Sprintf("%d", poisonCursor),
		malformedField, malformedCursor,
		negativeField, negativeCursor,
	); err != nil {
		t.Fatalf("seed Redis cursors: %v", err)
	}
	t.Cleanup(func() { _ = redis.Del(key) })

	res, err := s.Preflight()
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if res.MaxRedisCursor != ceilingCursor {
		t.Fatalf("MaxRedisCursor=%d want issued ceiling %d", res.MaxRedisCursor, ceilingCursor)
	}
	if res.InvalidRedisCursorFieldCount < 3 {
		t.Fatalf("InvalidRedisCursorFieldCount=%d want at least 3", res.InvalidRedisCursorFieldCount)
	}
	if res.RedisCursorKeyCount < 1 || res.RedisCursorFieldCount < 5 {
		t.Fatalf("Redis scan counts=(keys=%d fields=%d), want at least one key/five fields", res.RedisCursorKeyCount, res.RedisCursorFieldCount)
	}
	if res.RecommendedFloor != 1000 {
		t.Fatalf("RecommendedFloor=%d want authoritative issued max 1000", res.RecommendedFloor)
	}

	flipped, err := s.Activate(res.RecommendedFloor)
	if err != nil || !flipped {
		t.Fatalf("Activate with poisoned Redis cursor=(%v,%v), want (true,nil)", flipped, err)
	}
	state, err := s.Preflight()
	if err != nil {
		t.Fatalf("Preflight after activation: %v", err)
	}
	if state.CurrentMode != msgextraseq.ModeTransactional {
		t.Fatalf("unexpected state after activation: %+v", state)
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

func TestPreflightAndActivateRejectMissingStateRow(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	s := msgextraseq.New(ctx)
	if _, err := ctx.DB().UpdateBySql("DELETE FROM `octo_message_extra_version_state`").Exec(); err != nil {
		t.Fatalf("delete state row: %v", err)
	}

	if _, err := s.Preflight(); !errors.Is(err, msgextraseq.ErrStateRowMissing) {
		t.Fatalf("Preflight error=%v want ErrStateRowMissing", err)
	}
	if _, err := s.Activate(0); !errors.Is(err, msgextraseq.ErrStateRowMissing) {
		t.Fatalf("Activate error=%v want ErrStateRowMissing", err)
	}
}

func TestActivateRejectsUnknownMode(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	s := msgextraseq.New(ctx)
	if _, err := ctx.DB().UpdateBySql(
		"UPDATE `octo_message_extra_version_state` SET `mode`=? WHERE `singleton_id`=1",
		99,
	).Exec(); err != nil {
		t.Fatalf("seed unknown mode: %v", err)
	}

	if _, err := s.Activate(0); !errors.Is(err, msgextraseq.ErrUnknownMode) {
		t.Fatalf("Activate error=%v want ErrUnknownMode", err)
	}
}

// TestLegacyToTransactionalRolloutOrdering exercises the production order in
// docs/msgextra-cutover-runbook.md: upgraded replicas first run with no expected
// mode assertion, the DB-authoritative state flips, existing processes observe
// transactional mode immediately, and only then do restarted replicas enable
// the durable transactional guard.
func TestLegacyToTransactionalRolloutOrdering(t *testing.T) {
	const env = "OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE"
	t.Setenv(env, "")

	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	preCutoverStore := msgextraseq.New(ctx)
	channelID, channelType := "c-rollout", uint8(2)

	legacyVersion := reserveCommitted(t, ctx, preCutoverStore, channelID, channelType, 1)[0]
	if got := lastVersion(t, ctx, channelID, channelType); got != 0 {
		t.Fatalf("legacy write touched transactional sequence row: last_version=%d", got)
	}

	preflight, err := preCutoverStore.Preflight()
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	flipped, err := preCutoverStore.Activate(preflight.RecommendedFloor)
	if err != nil || !flipped {
		t.Fatalf("Activate = (%v, %v), want (true, nil)", flipped, err)
	}

	// Existing processes parsed an unset guard at startup, but select the new
	// allocator immediately because every transaction rereads the DB state row.
	transactionalVersion := reserveCommitted(t, ctx, preCutoverStore, channelID, channelType, 1)[0]
	if transactionalVersion <= legacyVersion {
		t.Fatalf("post-activate version=%d must exceed legacy version=%d", transactionalVersion, legacyVersion)
	}
	if got := lastVersion(t, ctx, channelID, channelType); got != transactionalVersion {
		t.Fatalf("transactional sequence last_version=%d want %d", got, transactionalVersion)
	}

	// The guard is enabled only after the DB flip. A newly restarted process then
	// accepts writes because its expectation matches the authoritative mode.
	t.Setenv(env, "transactional")
	guardedStore := msgextraseq.New(ctx)
	guardedVersion := reserveCommitted(t, ctx, guardedStore, channelID, channelType, 1)[0]
	if guardedVersion != transactionalVersion+1 {
		t.Fatalf("guarded version=%d want %d", guardedVersion, transactionalVersion+1)
	}
}

// TestActivateFailsFastWhenWriterDrainIncomplete locks the operational safety
// contract: if a writer still holds the shared state lock, activation must time
// out quickly instead of queueing an exclusive lock that stalls later writers.
func TestActivateFailsFastWhenWriterDrainIncomplete(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	s := msgextraseq.New(ctx)

	writerTx, err := ctx.DB().Begin()
	if err != nil {
		t.Fatalf("begin writer tx: %v", err)
	}
	defer writerTx.RollbackUnlessCommitted()
	if _, err := s.Mode(writerTx); err != nil {
		t.Fatalf("lock state FOR SHARE: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.Activate(0)
		done <- err
	}()

	const failFastDeadline = 6 * time.Second
	select {
	case err := <-done:
		var mysqlErr *mysql.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1205 {
			t.Fatalf("Activate error=%v, want MySQL 1205 lock-wait timeout", err)
		}
	case <-time.After(failFastDeadline):
		// Release the blocker so the goroutine cannot leak after this failure.
		_ = writerTx.Rollback()
		select {
		case err := <-done:
			t.Fatalf("Activate did not fail within %s; completed only after releasing the writer lock: %v", failFastDeadline, err)
		case <-time.After(failFastDeadline):
			t.Fatalf("Activate remained blocked for more than %s after releasing the writer lock", 2*failFastDeadline)
		}
	}

	_ = writerTx.Rollback()
	state, err := s.Preflight()
	if err != nil {
		t.Fatalf("Preflight after timeout: %v", err)
	}
	if state.CurrentMode != msgextraseq.ModeLegacy {
		t.Fatalf("activation changed mode despite lock timeout: %+v", state)
	}
}

func TestActivateRestoresSessionLockWaitTimeout(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	db := ctx.DB()
	// Force all statements through one pooled connection so this test observes
	// the exact session Activate temporarily modifies.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	var original int64
	if err := db.SelectBySql("SELECT @@SESSION.innodb_lock_wait_timeout").LoadOne(&original); err != nil {
		t.Fatalf("read original lock-wait timeout: %v", err)
	}
	sentinel := original + 7
	if _, err := db.UpdateBySql("SET SESSION innodb_lock_wait_timeout = ?", sentinel).Exec(); err != nil {
		t.Fatalf("set sentinel lock-wait timeout: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.UpdateBySql("SET SESSION innodb_lock_wait_timeout = ?", original).Exec()
	})

	s := msgextraseq.New(ctx)
	if flipped, err := s.Activate(0); err != nil || !flipped {
		t.Fatalf("Activate = (%v, %v), want (true, nil)", flipped, err)
	}

	var after int64
	if err := db.SelectBySql("SELECT @@SESSION.innodb_lock_wait_timeout").LoadOne(&after); err != nil {
		t.Fatalf("read restored lock-wait timeout: %v", err)
	}
	if after != sentinel {
		t.Fatalf("session lock-wait timeout=%d after Activate, want restored sentinel %d", after, sentinel)
	}
}
