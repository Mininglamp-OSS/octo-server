package msgextraseq_test

// D4 (approach B) tests for #627: in transactional mode every message_extra
// writer must acquire the channel-sequence lock (ReserveTx) BEFORE any
// message-row lock, so the global lock order is uniform (state → channel-seq →
// message-row) and no AB-BA deadlock is possible. The card write path is the
// only writer that is message-row-first (in legacy mode, for PR#548 single-
// process monotonicity); it flips to seq-first in transactional mode. These
// tests exercise the raw lock orderings against live MySQL to prove:
//   - Mode(tx) reports the mode a caller uses to pick its lock order.
//   - Two seq-first writers contending on the same message never deadlock.
//   - The row-first-vs-seq-first ordering DOES deadlock — the exact hazard the
//     transactional seq-first card path removes.

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

func isLockError(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && (myErr.Number == 1213 || myErr.Number == 1205)
}

// TestMode verifies Mode(tx) reflects the seeded state row, defaults to legacy
// on a missing row, and honors the expected-mode guard — this is the value a
// card writer reads to choose its lock order.
func TestMode(t *testing.T) {
	t.Run("transactional seeded", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeTransactional, 0)
		s := msgextraseq.New(ctx)
		tx, err := ctx.DB().Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.RollbackUnlessCommitted()
		mode, err := s.Mode(tx)
		if err != nil {
			t.Fatalf("Mode: %v", err)
		}
		if mode != msgextraseq.ModeTransactional {
			t.Fatalf("Mode = %d, want transactional(%d)", mode, msgextraseq.ModeTransactional)
		}
	})

	t.Run("missing row defaults legacy", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeLegacy, 0)
		if _, err := ctx.DB().UpdateBySql("DELETE FROM `octo_message_extra_version_state`").Exec(); err != nil {
			t.Fatalf("delete state: %v", err)
		}
		s := msgextraseq.New(ctx)
		tx, err := ctx.DB().Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.RollbackUnlessCommitted()
		mode, err := s.Mode(tx)
		if err != nil {
			t.Fatalf("Mode: %v", err)
		}
		if mode != msgextraseq.ModeLegacy {
			t.Fatalf("Mode = %d, want legacy(%d) for a missing state row", mode, msgextraseq.ModeLegacy)
		}
	})
}

// seedChannelSeq pre-warms the per-channel sequence row so subsequent ReserveTx
// calls lock an existing record (not the first-use upsert path).
func seedChannelSeq(t *testing.T, db *dbr.Session, channelID string, channelType uint8) {
	t.Helper()
	if _, err := db.UpdateBySql(
		"INSERT INTO `octo_message_extra_channel_seq` (`channel_id`,`channel_type`,`last_version`) VALUES (?,?,0) ON DUPLICATE KEY UPDATE `last_version`=`last_version`",
		channelID, channelType,
	).Exec(); err != nil {
		t.Fatalf("seed channel seq: %v", err)
	}
}

// TestUniformSeqFirstOrderDoesNotDeadlock runs two writers that both take the
// channel-sequence lock before touching the same message row — the uniform order
// approach B gives the card path in transactional mode. They must serialize on
// the sequence row and both commit, never deadlocking.
func TestUniformSeqFirstOrderDoesNotDeadlock(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 0)
	s := msgextraseq.New(ctx)
	const channelID, channelType, messageID = "ch-uniform", uint8(2), "msg-uniform"
	seedChannelSeq(t, ctx.DB(), channelID, channelType)
	// Seed the message row so the FOR UPDATE takes a record lock (no gap lock).
	if _, err := ctx.DB().UpdateBySql(
		"INSERT INTO `message_extra` (`message_id`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,0)",
		messageID, channelID, channelType,
	).Exec(); err != nil {
		t.Fatalf("seed message row: %v", err)
	}

	// seqFirstWrite models any writer using the uniform transactional order:
	// reserve the channel sequence, then lock+update the message row, then commit.
	seqFirstWrite := func() error {
		tx, err := ctx.DB().Begin()
		if err != nil {
			return err
		}
		defer tx.RollbackUnlessCommitted()
		versions, err := s.ReserveTx(tx, channelID, channelType, 1)
		if err != nil {
			return err
		}
		var cur int64
		if err := tx.SelectBySql("SELECT `version` FROM `message_extra` WHERE `message_id`=? FOR UPDATE", messageID).LoadOne(&cur); err != nil {
			return err
		}
		if _, err := tx.UpdateBySql("UPDATE `message_extra` SET `version`=? WHERE `message_id`=?", versions[0], messageID).Exec(); err != nil {
			return err
		}
		return tx.Commit()
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- seqFirstWrite()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("uniform seq-first writer failed (deadlock indicates a lock-order regression): %v", err)
		}
	}
	// All eight reservations committed on one channel → the persisted cursor is 8.
	if got := lastVersion(t, ctx, channelID, channelType); got != workers {
		t.Fatalf("channel cursor = %d, want %d after %d serialized reservations", got, workers, workers)
	}
}

// TestRowFirstVsSeqFirstDeadlocks reproduces the exact hazard the transactional
// seq-first card path removes: one writer locks the message row first then the
// sequence (the OLD card order), the other locks the sequence first then the
// message row (a non-card writer). Forced into an AB-BA interleaving they
// deadlock (MySQL 1213). This documents why the card path must NOT stay
// message-row-first in transactional mode.
func TestRowFirstVsSeqFirstDeadlocks(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 0)
	s := msgextraseq.New(ctx)
	const channelID, channelType, messageID = "ch-hazard", uint8(2), "msg-hazard"
	seedChannelSeq(t, ctx.DB(), channelID, channelType)
	if _, err := ctx.DB().UpdateBySql(
		"INSERT INTO `message_extra` (`message_id`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,0)",
		messageID, channelID, channelType,
	).Exec(); err != nil {
		t.Fatalf("seed message row: %v", err)
	}

	rowLocked := make(chan struct{})
	seqLocked := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	// Row-first (the OLD card order): lock the message row, then the sequence.
	go func() {
		defer wg.Done()
		tx, err := ctx.DB().Begin()
		if err != nil {
			errs <- err
			return
		}
		defer tx.RollbackUnlessCommitted()
		var cur int64
		if err := tx.SelectBySql("SELECT `version` FROM `message_extra` WHERE `message_id`=? FOR UPDATE", messageID).LoadOne(&cur); err != nil {
			errs <- err
			return
		}
		close(rowLocked)
		<-seqLocked
		_, err = s.ReserveTx(tx, channelID, channelType, 1) // blocks on the seq lock held by the other tx
		if err == nil {
			err = tx.Commit()
		}
		errs <- err
	}()

	// Seq-first (a non-card writer): reserve the sequence, then lock the message row.
	go func() {
		defer wg.Done()
		tx, err := ctx.DB().Begin()
		if err != nil {
			errs <- err
			return
		}
		defer tx.RollbackUnlessCommitted()
		if _, err := s.ReserveTx(tx, channelID, channelType, 1); err != nil {
			errs <- err
			return
		}
		close(seqLocked)
		<-rowLocked
		var cur int64
		err = tx.SelectBySql("SELECT `version` FROM `message_extra` WHERE `message_id`=? FOR UPDATE", messageID).LoadOne(&cur) // blocks on the row lock
		if err == nil {
			err = tx.Commit()
		}
		errs <- err
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out: expected an InnoDB deadlock (1213) from the row-first vs seq-first interleaving, but neither tx returned")
	}
	close(errs)

	sawLockError := false
	for err := range errs {
		if isLockError(err) {
			sawLockError = true
		}
	}
	if !sawLockError {
		t.Fatal("expected a 1213/1205 lock error from the row-first vs seq-first AB-BA interleaving; the transactional card path must be seq-first to avoid exactly this")
	}
}
