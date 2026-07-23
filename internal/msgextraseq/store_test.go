package msgextraseq_test

// Integration tests for the shared message_extra version allocator. They need a
// live MySQL, addressed via testutil.NewTestContext (which does NOT run
// module.Setup/migrations — avoiding the local migration-ledger fragility).
// The tables the allocator touches are ensured with CREATE TABLE IF NOT EXISTS,
// a no-op in CI where the real migration has already created them. Each test
// starts from empty tables and re-seeds the allocator-state singleton it needs.
//
// Override the DSN with OCTO_TEST_MYSQL_ADDR when the local MySQL is elsewhere.

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
	"github.com/gocraft/dbr/v2"
)

func testMySQLAddr() string {
	if v := os.Getenv("OCTO_TEST_MYSQL_ADDR"); v != "" {
		return v
	}
	return "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true"
}

// ensureSchema creates the tables the allocator reads/writes if they are absent.
// In CI they already exist (real migration), so every statement is a no-op.
func ensureSchema(t *testing.T, db *dbr.Session) {
	t.Helper()
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS `seq` (`key` VARCHAR(100) NOT NULL, `min_seq` BIGINT NOT NULL DEFAULT 0, `step` INT NOT NULL DEFAULT 0, PRIMARY KEY (`key`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE IF NOT EXISTS `message_extra` (`id` BIGINT NOT NULL AUTO_INCREMENT, `message_id` VARCHAR(20) NOT NULL DEFAULT '', `message_seq` BIGINT NOT NULL DEFAULT 0, `channel_id` VARCHAR(100) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL DEFAULT '', `channel_type` SMALLINT NOT NULL DEFAULT 0, `is_deleted` SMALLINT NOT NULL DEFAULT 0, `version` BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (`id`), UNIQUE KEY `message_id` (`message_id`), KEY `channel_version_idx` (`channel_id`,`channel_type`,`version`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE IF NOT EXISTS `octo_message_extra_channel_seq` (`channel_id` VARCHAR(100) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL DEFAULT '', `channel_type` SMALLINT NOT NULL DEFAULT 0, `last_version` BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (`channel_id`,`channel_type`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
		"CREATE TABLE IF NOT EXISTS `octo_message_extra_version_state` (`singleton_id` TINYINT UNSIGNED NOT NULL, `mode` TINYINT NOT NULL DEFAULT 0, `epoch` BIGINT UNSIGNED NOT NULL DEFAULT 0, `cutover_floor` BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (`singleton_id`), CONSTRAINT `chk_message_extra_version_singleton` CHECK (`singleton_id` = 1)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
	}
	for _, s := range stmts {
		if _, err := db.UpdateBySql(s).Exec(); err != nil {
			t.Fatalf("ensure schema: %v", err)
		}
	}
}

// setup returns a live context with the allocator-state singleton seeded to the
// given mode/floor and the allocator tables emptied.
func setup(t *testing.T, mode int, floor int64) *config.Context {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = testMySQLAddr()
	cfg.DB.Migration = false
	ctx := testutil.NewTestContext(cfg)
	db := ctx.DB()
	ensureSchema(t, db)
	for _, tbl := range []string{"octo_message_extra_channel_seq", "octo_message_extra_version_state", "message_extra", "seq"} {
		if _, err := db.UpdateBySql("DELETE FROM `" + tbl + "`").Exec(); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
	if _, err := db.InsertBySql(
		"INSERT INTO `octo_message_extra_version_state` (`singleton_id`,`mode`,`epoch`,`cutover_floor`) VALUES (1,?,0,?)",
		mode, floor,
	).Exec(); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return ctx
}

// lastVersion reads the persisted sequence cursor for a channel (0 if absent).
func lastVersion(t *testing.T, ctx *config.Context, channelID string, channelType uint8) int64 {
	t.Helper()
	var v int64
	err := ctx.DB().SelectBySql(
		"SELECT COALESCE(`last_version`,0) FROM `octo_message_extra_channel_seq` WHERE `channel_id`=? AND `channel_type`=?",
		channelID, channelType,
	).LoadOne(&v)
	if err != nil && err != dbr.ErrNotFound {
		t.Fatalf("read last_version: %v", err)
	}
	return v
}

// reserveCommitted runs one ReserveTx in its own committed transaction.
func reserveCommitted(t *testing.T, ctx *config.Context, s *msgextraseq.Store, channelID string, channelType uint8, count int) []int64 {
	t.Helper()
	tx, err := ctx.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.RollbackUnlessCommitted()
	versions, err := s.ReserveTx(tx, channelID, channelType, count)
	if err != nil {
		t.Fatalf("ReserveTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return versions
}

func TestReserveTx_TransactionalUniqueIncreasing(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 1000)
	s := msgextraseq.New(ctx)
	ch, ct := "c-uniq", uint8(2)

	seen := map[int64]bool{}
	var prev int64
	for i := 0; i < 5; i++ {
		v := reserveCommitted(t, ctx, s, ch, ct, 1)[0]
		if seen[v] {
			t.Fatalf("duplicate version %d", v)
		}
		if v <= prev {
			t.Fatalf("version %d not greater than prev %d", v, prev)
		}
		seen[v], prev = true, v
	}
	if got := lastVersion(t, ctx, ch, ct); got != prev {
		t.Fatalf("last_version=%d want %d", got, prev)
	}
}

func TestReserveTx_BatchContiguous(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 1000)
	s := msgextraseq.New(ctx)
	ch, ct := "c-batch", uint8(2)

	versions := reserveCommitted(t, ctx, s, ch, ct, 5)
	if len(versions) != 5 {
		t.Fatalf("len=%d want 5", len(versions))
	}
	for i := 1; i < len(versions); i++ {
		if versions[i] != versions[i-1]+1 {
			t.Fatalf("not contiguous: %v", versions)
		}
	}
	// A following single reservation continues immediately after the range.
	next := reserveCommitted(t, ctx, s, ch, ct, 1)[0]
	if next != versions[4]+1 {
		t.Fatalf("next=%d want %d", next, versions[4]+1)
	}
}

func TestReserveTx_Rollback(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 1000)
	s := msgextraseq.New(ctx)
	ch, ct := "c-rollback", uint8(2)

	// Establish the row.
	first := reserveCommitted(t, ctx, s, ch, ct, 1)[0]
	before := lastVersion(t, ctx, ch, ct)

	tx, err := ctx.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.ReserveTx(tx, ch, ct, 3); err != nil {
		t.Fatalf("ReserveTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := lastVersion(t, ctx, ch, ct); got != before {
		t.Fatalf("rollback leaked advance: last_version=%d want %d", got, before)
	}
	// The next committed reservation reuses the rolled-back range (gaps allowed).
	next := reserveCommitted(t, ctx, s, ch, ct, 1)[0]
	if next != first+1 {
		t.Fatalf("next=%d want %d", next, first+1)
	}
}

func TestReserveTx_TwoSessionSerialization(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 1000)
	s := msgextraseq.New(ctx)
	ch, ct := "c-serialize", uint8(2)
	reserveCommitted(t, ctx, s, ch, ct, 1) // materialize row

	tx1, err := ctx.DB().Begin()
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer tx1.RollbackUnlessCommitted()
	v1, err := s.ReserveTx(tx1, ch, ct, 1)
	if err != nil {
		t.Fatalf("tx1 reserve: %v", err)
	}

	done := make(chan int64, 1)
	errc := make(chan error, 1)
	go func() {
		tx2, err := ctx.DB().Begin()
		if err != nil {
			errc <- err
			return
		}
		defer tx2.RollbackUnlessCommitted()
		v2, err := s.ReserveTx(tx2, ch, ct, 1) // must block on tx1's row lock
		if err != nil {
			errc <- err
			return
		}
		if err := tx2.Commit(); err != nil {
			errc <- err
			return
		}
		done <- v2[0]
	}()

	// tx2 must not proceed while tx1 holds the lock.
	select {
	case <-done:
		t.Fatal("tx2 acquired the sequence before tx1 committed")
	case err := <-errc:
		t.Fatalf("tx2 error: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := tx1.Commit(); err != nil {
		t.Fatalf("tx1 commit: %v", err)
	}

	select {
	case v2 := <-done:
		if v2 <= v1[0] {
			t.Fatalf("v2=%d not greater than v1=%d", v2, v1[0])
		}
	case err := <-errc:
		t.Fatalf("tx2 error: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("tx2 did not proceed after tx1 committed")
	}
}

func TestReserveTx_DifferentChannelsNoGlobalLock(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 1000)
	s := msgextraseq.New(ctx)
	reserveCommitted(t, ctx, s, "c-A", 2, 1)
	reserveCommitted(t, ctx, s, "c-B", 2, 1)

	// Hold channel A's lock open; a reservation on channel B must not block.
	txA, err := ctx.DB().Begin()
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer txA.RollbackUnlessCommitted()
	if _, err := s.ReserveTx(txA, "c-A", 2, 1); err != nil {
		t.Fatalf("reserve A: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		txB, err := ctx.DB().Begin()
		if err != nil {
			done <- err
			return
		}
		defer txB.RollbackUnlessCommitted()
		if _, err := s.ReserveTx(txB, "c-B", 2, 1); err != nil {
			done <- err
			return
		}
		done <- txB.Commit()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("channel B blocked/errored behind channel A: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel B blocked behind channel A's lock (unexpected global lock)")
	}
	_ = txA.Commit()
}

func TestReserveTx_ConcurrentInitializers(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 1000)
	s := msgextraseq.New(ctx)
	ch, ct := "c-init-race", uint8(2)

	const n = 4
	var wg sync.WaitGroup
	results := make([][]int64, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tx, err := ctx.DB().Begin()
			if err != nil {
				errs[i] = err
				return
			}
			defer tx.RollbackUnlessCommitted()
			v, err := s.ReserveTx(tx, ch, ct, 1)
			if err != nil {
				errs[i] = err
				return
			}
			if err := tx.Commit(); err != nil {
				errs[i] = err
				return
			}
			results[i] = v
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[int64]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("initializer %d: %v (no 1062/private-baseline expected)", i, errs[i])
		}
		v := results[i][0]
		if seen[v] {
			t.Fatalf("duplicate version %d across concurrent initializers", v)
		}
		seen[v] = true
	}
	if got := lastVersion(t, ctx, ch, ct); got != int64(1000+n) {
		t.Fatalf("last_version=%d want %d", got, 1000+n)
	}
}

func TestReserveTx_Bootstrap(t *testing.T) {
	t.Run("floor wins when no evidence", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeTransactional, 5000)
		s := msgextraseq.New(ctx)
		v := reserveCommitted(t, ctx, s, "c-boot-floor", 2, 1)[0]
		if v != 5001 {
			t.Fatalf("first version=%d want 5001 (floor+1)", v)
		}
	})

	t.Run("existing message_extra max above floor wins", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeTransactional, 1000)
		s := msgextraseq.New(ctx)
		ch, ct := "c-boot-extra", uint8(2)
		if _, err := ctx.DB().InsertBySql(
			"INSERT INTO `message_extra` (`message_id`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,?)",
			"m1", ch, ct, 7777,
		).Exec(); err != nil {
			t.Fatalf("seed message_extra: %v", err)
		}
		v := reserveCommitted(t, ctx, s, ch, ct, 1)[0]
		if v != 7778 {
			t.Fatalf("first version=%d want 7778 (maxExtra+1)", v)
		}
	})

	t.Run("legacy seq min_seq above floor wins", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeTransactional, 1000)
		s := msgextraseq.New(ctx)
		ch, ct := "c-boot-legacy", uint8(2)
		if _, err := ctx.DB().InsertBySql(
			"INSERT INTO `seq` (`key`,`min_seq`,`step`) VALUES (?,?,1000)",
			fmt.Sprintf("seq:messageExtra:%s", ch), 9000,
		).Exec(); err != nil {
			t.Fatalf("seed seq: %v", err)
		}
		v := reserveCommitted(t, ctx, s, ch, ct, 1)[0]
		if v != 9001 {
			t.Fatalf("first version=%d want 9001 (legacySeq+1)", v)
		}
	})
}

func TestReserveTx_FloorRaiseOnExistingRow(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 1000)
	s := msgextraseq.New(ctx)
	ch, ct := "c-floor-raise", uint8(2)
	// Simulate a row that survived an emergency legacy interval below a new floor.
	if _, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_message_extra_channel_seq` (`channel_id`,`channel_type`,`last_version`) VALUES (?,?,?)",
		ch, ct, 500,
	).Exec(); err != nil {
		t.Fatalf("seed seq row: %v", err)
	}
	v := reserveCommitted(t, ctx, s, ch, ct, 1)[0]
	if v != 1001 {
		t.Fatalf("version=%d want 1001 (raised to floor+1)", v)
	}
}

func TestReserveTx_CountBounds(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 1000)
	s := msgextraseq.New(ctx)
	ch, ct := "c-bounds", uint8(2)
	for _, count := range []int{0, -1, msgextraseq.MaxReserveCount + 1} {
		tx, err := ctx.DB().Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := s.ReserveTx(tx, ch, ct, count); err != msgextraseq.ErrInvalidCount {
			t.Fatalf("count=%d err=%v want ErrInvalidCount", count, err)
		}
		_ = tx.Rollback()
	}
	if got := lastVersion(t, ctx, ch, ct); got != 0 {
		t.Fatalf("rejected counts mutated the row: last_version=%d", got)
	}
}

func TestReserveTx_Overflow(t *testing.T) {
	floor := int64(1)<<53 - 2 // 2^53-2: room for exactly one value.
	ctx := setup(t, msgextraseq.ModeTransactional, floor)
	s := msgextraseq.New(ctx)
	ch, ct := "c-overflow", uint8(2)

	// count=1 lands exactly on 2^53-1 and is allowed.
	if v := reserveCommitted(t, ctx, s, ch, ct, 1)[0]; v != floor+1 {
		t.Fatalf("version=%d want %d", v, floor+1)
	}
	// The next reservation would cross 2^53-1 and must fail without mutation.
	before := lastVersion(t, ctx, ch, ct)
	tx, err := ctx.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.ReserveTx(tx, ch, ct, 1); err != msgextraseq.ErrOverflow {
		t.Fatalf("err=%v want ErrOverflow", err)
	}
	_ = tx.Rollback()
	if got := lastVersion(t, ctx, ch, ct); got != before {
		t.Fatalf("overflow mutated the row: last_version=%d want %d", got, before)
	}
}

func TestReserveTx_LegacyModeDistinct(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeLegacy, 0)
	s := msgextraseq.New(ctx)
	// Legacy mode must not touch the transactional table.
	versions := reserveCommitted(t, ctx, s, "c-legacy", 2, 3)
	if len(versions) != 3 {
		t.Fatalf("len=%d want 3", len(versions))
	}
	seen := map[int64]bool{}
	for _, v := range versions {
		if seen[v] {
			t.Fatalf("legacy duplicate %d", v)
		}
		seen[v] = true
	}
	if got := lastVersion(t, ctx, "c-legacy", 2); got != 0 {
		t.Fatalf("legacy mode wrote the transactional table: last_version=%d", got)
	}
}

func TestReserveTx_StateDefaultsAndFailClosed(t *testing.T) {
	t.Run("missing state row defaults to legacy", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeTransactional, 1000)
		if _, err := ctx.DB().UpdateBySql("DELETE FROM `octo_message_extra_version_state`").Exec(); err != nil {
			t.Fatalf("delete state: %v", err)
		}
		s := msgextraseq.New(ctx)
		// No row → legacy default (safe pre-migration behavior): reservation
		// succeeds via GenSeq and does not touch the transactional table.
		versions := reserveCommitted(t, ctx, s, "c-missing", 2, 1)
		if len(versions) != 1 {
			t.Fatalf("len=%d want 1", len(versions))
		}
		if got := lastVersion(t, ctx, "c-missing", 2); got != 0 {
			t.Fatalf("missing-row default wrote the transactional table: last_version=%d", got)
		}
	})

	t.Run("unknown mode fails closed", func(t *testing.T) {
		ctx := setup(t, 9 /* unknown */, 1000)
		s := msgextraseq.New(ctx)
		tx, _ := ctx.DB().Begin()
		if _, err := s.ReserveTx(tx, "c-x", 2, 1); !errors.Is(err, msgextraseq.ErrUnknownMode) {
			t.Fatalf("err=%v want ErrUnknownMode", err)
		}
		_ = tx.Rollback()
	})
}

// TestStateSingletonConstraint proves the schema structurally forbids a second
// state row (CHECK singleton_id=1), so the "duplicate row" case the spec requires
// to fail closed cannot arise (the reader only trusts id=1).
func TestStateSingletonConstraint(t *testing.T) {
	ctx := setup(t, msgextraseq.ModeTransactional, 1000)
	if _, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_message_extra_version_state` (`singleton_id`,`mode`,`epoch`,`cutover_floor`) VALUES (2,1,0,0)",
	).Exec(); err == nil {
		t.Fatal("inserting a second state row (singleton_id=2) succeeded; CHECK (singleton_id=1) not enforced")
	}
}

// TestReserveTx_BatchPaginatesViaCursor exercises the real delta-sync read
// contract: reserve a range, persist message_extra rows carrying those versions,
// then page through them with `version > cursor ORDER BY version ASC LIMIT k`
// over the composite index and assert every version is returned exactly once,
// in order, with no gaps or duplicates.
func TestReserveTx_BatchPaginatesViaCursor(t *testing.T) {
	floor := int64(1000)
	ctx := setup(t, msgextraseq.ModeTransactional, floor)
	s := msgextraseq.New(ctx)
	ch, ct := "c-cursor", uint8(2)
	const n = 7

	versions := reserveCommitted(t, ctx, s, ch, ct, n)
	for i, v := range versions {
		if _, err := ctx.DB().InsertBySql(
			"INSERT INTO `message_extra` (`message_id`,`message_seq`,`channel_id`,`channel_type`,`version`) VALUES (?,?,?,?,?)",
			fmt.Sprintf("cur-%d", i), i+1, ch, ct, v,
		).Exec(); err != nil {
			t.Fatalf("seed message_extra: %v", err)
		}
	}

	// Page through with limit=3, advancing the cursor like the sync endpoint.
	var got []int64
	cursor := floor // start below the first reserved version
	for {
		var page []int64
		_, err := ctx.DB().SelectBySql(
			"SELECT `version` FROM `message_extra` WHERE `channel_id`=? AND `channel_type`=? AND `version`>? ORDER BY `version` ASC LIMIT 3",
			ch, ct, cursor,
		).Load(&page)
		if err != nil {
			t.Fatalf("paginate: %v", err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page...)
		cursor = page[len(page)-1]
	}

	if len(got) != n {
		t.Fatalf("paginated %d rows want %d (gaps or duplicates)", len(got), n)
	}
	for i, v := range got {
		if v != versions[i] {
			t.Fatalf("page[%d]=%d want %d (order/coverage mismatch)", i, v, versions[i])
		}
	}
}

// TestExpectedModeGuard proves the D9 read-side safety net: a deployment that
// declares OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE=transactional fails closed
// when the resolved mode (including a missing row → legacy) is not transactional,
// so a lost/deleted state row cannot silently revert to legacy after cutover.
// Unset (the default used by every other test) makes no assertion.
func TestExpectedModeGuard(t *testing.T) {
	const env = "OCTO_MESSAGE_EXTRA_VERSION_EXPECTED_MODE"

	t.Run("expected transactional, missing row fails closed", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeTransactional, 1000)
		if _, err := ctx.DB().UpdateBySql("DELETE FROM `octo_message_extra_version_state`").Exec(); err != nil {
			t.Fatalf("delete state: %v", err)
		}
		t.Setenv(env, "transactional")
		s := msgextraseq.New(ctx)
		tx, _ := ctx.DB().Begin()
		if _, err := s.ReserveTx(tx, "c-x", 2, 1); !errors.Is(err, msgextraseq.ErrExpectedModeMismatch) {
			t.Fatalf("err=%v want ErrExpectedModeMismatch", err)
		}
		_ = tx.Rollback()
	})

	t.Run("expected transactional, transactional row ok", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeTransactional, 1000)
		t.Setenv(env, "transactional")
		s := msgextraseq.New(ctx)
		if got := reserveCommitted(t, ctx, s, "c-ok", 2, 1); len(got) != 1 {
			t.Fatalf("len=%d want 1", len(got))
		}
	})

	t.Run("expected legacy, transactional row fails closed", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeTransactional, 1000)
		t.Setenv(env, "legacy")
		s := msgextraseq.New(ctx)
		tx, _ := ctx.DB().Begin()
		if _, err := s.ReserveTx(tx, "c-x", 2, 1); !errors.Is(err, msgextraseq.ErrExpectedModeMismatch) {
			t.Fatalf("err=%v want ErrExpectedModeMismatch", err)
		}
		_ = tx.Rollback()
	})

	t.Run("malformed expected fails closed", func(t *testing.T) {
		ctx := setup(t, msgextraseq.ModeLegacy, 0)
		t.Setenv(env, "bogus")
		s := msgextraseq.New(ctx)
		tx, _ := ctx.DB().Begin()
		if _, err := s.ReserveTx(tx, "c-x", 2, 1); !errors.Is(err, msgextraseq.ErrExpectedModeMismatch) {
			t.Fatalf("err=%v want ErrExpectedModeMismatch", err)
		}
		_ = tx.Rollback()
	})
}
