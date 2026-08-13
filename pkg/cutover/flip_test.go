package cutover_test

// Integration tests for the shared flip/state primitives. They need a live
// MySQL, addressed via testutil.NewTestContext (no module.Setup/migrations),
// and operate on a dedicated scratch table shaped like the domains' state
// tables. Override the DSN with OCTO_TEST_MYSQL_ADDR when the local MySQL is
// elsewhere.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/cutover"
	"github.com/gocraft/dbr/v2"
)

const testStateTable = "octo_cutover_test_state"

func testMySQLAddr() string {
	if v := os.Getenv("OCTO_TEST_MYSQL_ADDR"); v != "" {
		return v
	}
	return "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true"
}

// setupDB returns a live session with the scratch state table created and
// emptied. Seeding the singleton is left to each test so the missing-row case
// stays reachable.
func setupDB(t *testing.T) *dbr.Session {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = testMySQLAddr()
	cfg.DB.Migration = false
	ctx := testutil.NewTestContext(cfg)
	db := ctx.DB()
	if _, err := db.UpdateBySql(
		"CREATE TABLE IF NOT EXISTS `" + testStateTable + "` (" +
			"`singleton_id` TINYINT UNSIGNED NOT NULL, " +
			"`mode` TINYINT NOT NULL DEFAULT 0, " +
			"`epoch` BIGINT UNSIGNED NOT NULL DEFAULT 0, " +
			"`cutover_floor` BIGINT NOT NULL DEFAULT 0, " +
			"PRIMARY KEY (`singleton_id`), " +
			"CONSTRAINT `chk_cutover_test_singleton` CHECK (`singleton_id` = 1)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
	).Exec(); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := db.UpdateBySql("DELETE FROM `" + testStateTable + "`").Exec(); err != nil {
		t.Fatalf("clean state table: %v", err)
	}
	// Drop the scratch table when the test finishes rather than leaving it in
	// the shared `test` schema: a leftover table is one more thing a concurrent
	// package's CleanAllTables sweeps, and it would otherwise persist with the
	// last test's mode still set.
	t.Cleanup(func() {
		if _, err := db.UpdateBySql("DROP TABLE IF EXISTS `" + testStateTable + "`").Exec(); err != nil {
			t.Logf("drop scratch state table: %v", err)
		}
	})
	return db
}

func seedState(t *testing.T, db *dbr.Session, mode int, epoch uint64, floor int64) {
	t.Helper()
	if _, err := db.InsertBySql(
		"INSERT INTO `"+testStateTable+"` (`singleton_id`,`mode`,`epoch`,`cutover_floor`) VALUES (1,?,?,?)",
		mode, epoch, floor,
	).Exec(); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

func TestReadState(t *testing.T) {
	db := setupDB(t)

	// Missing row → ErrStateMissing.
	if _, err := cutover.ReadState(context.Background(), db, testStateTable); !errors.Is(err, cutover.ErrStateMissing) {
		t.Fatalf("missing row: err=%v want ErrStateMissing", err)
	}

	// Missing table → also ErrStateMissing (MySQL 1146), never a raw error:
	// "not migrated yet" and "authority unreachable" must stay distinguishable.
	if _, err := cutover.ReadState(context.Background(), db, "octo_cutover_test_absent"); !errors.Is(err, cutover.ErrStateMissing) {
		t.Fatalf("missing table: err=%v want ErrStateMissing", err)
	}

	seedState(t, db, cutover.ModeActive, 3, 4200)
	st, err := cutover.ReadState(context.Background(), db, testStateTable)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if st.Mode != cutover.ModeActive || st.Epoch != 3 || st.Floor != 4200 || !st.Active() {
		t.Fatalf("state=%+v want active epoch=3 floor=4200", st)
	}
}

func readRow(t *testing.T, db *dbr.Session) cutover.State {
	t.Helper()
	st, err := cutover.ReadState(context.Background(), db, testStateTable)
	if err != nil {
		t.Fatalf("read back state: %v", err)
	}
	return st
}

func TestFlipHappyPathAndIdempotency(t *testing.T) {
	db := setupDB(t)
	seedState(t, db, cutover.ModeInactive, 0, 0)

	observed := func(*dbr.Tx) (int64, error) { return 100, nil }
	flipped, epoch, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{
		Table: testStateTable, Floor: 100, Observe: observed,
	})
	if err != nil || !flipped || epoch != 1 {
		t.Fatalf("flip = (%v,%d,%v), want (true,1,nil)", flipped, epoch, err)
	}
	if st := readRow(t, db); !st.Active() || st.Epoch != 1 || st.Floor != 100 {
		t.Fatalf("state after flip = %+v", st)
	}

	// Idempotent re-run: no change, current epoch reported, no error.
	flipped, epoch, err = cutover.Flip(context.Background(), db, cutover.FlipSpec{
		Table: testStateTable, Floor: 999, Observe: observed,
	})
	if err != nil || flipped || epoch != 1 {
		t.Fatalf("re-flip = (%v,%d,%v), want (false,1,nil)", flipped, epoch, err)
	}
	if st := readRow(t, db); st.Floor != 100 {
		t.Fatalf("idempotent re-run must not move the floor: %+v", st)
	}
}

func TestFlipRefusesMissingRowAndUnknownMode(t *testing.T) {
	db := setupDB(t)
	if _, _, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{Table: testStateTable, Floor: 1}); !errors.Is(err, cutover.ErrStateMissing) {
		t.Fatalf("missing row: err=%v want ErrStateMissing", err)
	}

	seedState(t, db, 7, 0, 0)
	if _, _, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{Table: testStateTable, Floor: 1}); !errors.Is(err, cutover.ErrUnknownMode) {
		t.Fatalf("unknown mode: err=%v want ErrUnknownMode", err)
	}
}

func TestFlipFloorBounds(t *testing.T) {
	db := setupDB(t)
	seedState(t, db, cutover.ModeInactive, 0, 0)
	observed := func(*dbr.Tx) (int64, error) { return 100, nil }

	// Inclusive policy (#627): floor < observed refused, floor == observed ok.
	var fe *cutover.FloorError
	if _, _, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{
		Table: testStateTable, Floor: 99, Observe: observed,
	}); !errors.As(err, &fe) || fe.TooHigh || fe.Observed != 100 || fe.Floor != 99 {
		t.Fatalf("floor 99 vs observed 100: err=%v want FloorError{99,100}", err)
	}

	// Exclusive policy (#697): floor == observed refused too.
	if _, _, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{
		Table: testStateTable, Floor: 100, Observe: observed, FloorMustExceedObserved: true,
	}); !errors.As(err, &fe) || fe.TooHigh {
		t.Fatalf("exclusive floor == observed: err=%v want FloorError", err)
	}

	// Upper bound: floor above MaxFloor refused before any mutation.
	if _, _, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{
		Table: testStateTable, Floor: 501, MaxFloor: 500, Observe: observed,
	}); !errors.As(err, &fe) || !fe.TooHigh || fe.Max != 500 {
		t.Fatalf("floor above max: err=%v want FloorError{TooHigh,Max:500}", err)
	}

	// All three refusals must have left the row untouched.
	if st := readRow(t, db); st.Active() || st.Epoch != 0 || st.Floor != 0 {
		t.Fatalf("refused flips mutated state: %+v", st)
	}

	// Inclusive floor == observed proceeds.
	if flipped, _, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{
		Table: testStateTable, Floor: 100, Observe: observed,
	}); err != nil || !flipped {
		t.Fatalf("floor == observed (inclusive) = (%v,%v), want success", flipped, err)
	}
}

func TestFlipObserveErrorAborts(t *testing.T) {
	db := setupDB(t)
	seedState(t, db, cutover.ModeInactive, 0, 0)
	boom := errors.New("evidence unreadable")
	if _, _, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{
		Table: testStateTable, Floor: 1,
		Observe: func(*dbr.Tx) (int64, error) { return 0, boom },
	}); !errors.Is(err, boom) {
		t.Fatalf("observe error: err=%v want %v", err, boom)
	}
	if st := readRow(t, db); st.Active() {
		t.Fatalf("observe failure must not flip: %+v", st)
	}
}

func TestFlipWithSessionLockWaitTimeout(t *testing.T) {
	db := setupDB(t)
	seedState(t, db, cutover.ModeInactive, 0, 0)
	flipped, epoch, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{
		Table: testStateTable, Floor: 10,
		Observe:                func(*dbr.Tx) (int64, error) { return 10, nil },
		LockWaitTimeoutSeconds: 3,
	})
	if err != nil || !flipped || epoch != 1 {
		t.Fatalf("flip with lock-wait timeout = (%v,%d,%v), want (true,1,nil)", flipped, epoch, err)
	}
	// The pinned connection is closed on cleanup, so the pool never sees the
	// 3s session setting; a follow-up statement on the shared pool works.
	if st := readRow(t, db); !st.Active() || st.Floor != 10 {
		t.Fatalf("state after pinned flip = %+v", st)
	}
}
