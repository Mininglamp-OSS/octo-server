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
	"github.com/stretchr/testify/require"
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

	// Missing table → still satisfies ErrStateMissing (so every existing caller
	// is unaffected), but is ALSO distinguishable as ErrStateTableMissing.
	//
	// The distinction is not cosmetic: a domain whose runtime reader defaults to
	// legacy for a missing row may fail every write closed on a missing table —
	// msgextraseq does exactly that — so an operator surface reporting the
	// friendlier of the two states tells them writes are flowing while they are
	// erroring. Deriving it here, where MySQL 1146 is already in hand, saves
	// every domain from re-querying information_schema to recover it.
	_, tableErr := cutover.ReadState(context.Background(), db, "octo_cutover_test_absent")
	if !errors.Is(tableErr, cutover.ErrStateMissing) {
		t.Fatalf("missing table: err=%v must still satisfy ErrStateMissing", tableErr)
	}
	if !errors.Is(tableErr, cutover.ErrStateTableMissing) {
		t.Fatalf("missing table: err=%v want ErrStateTableMissing", tableErr)
	}

	// A missing ROW is ErrStateMissing and must NOT look like a missing table.
	_, rowErr := cutover.ReadState(context.Background(), db, testStateTable)
	if !errors.Is(rowErr, cutover.ErrStateMissing) {
		t.Fatalf("missing row: err=%v want ErrStateMissing", rowErr)
	}
	if errors.Is(rowErr, cutover.ErrStateTableMissing) {
		t.Fatalf("missing row reported as a missing table: %v", rowErr)
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

// TestFlipWithSessionLockWaitTimeout covers the pinned-connection path and,
// more importantly, what keeps it from poisoning the pool.
//
// An earlier version of this test asserted nothing about the session variable
// and explained itself with "the pinned connection is closed on cleanup, so the
// pool never sees the 3s setting" — which is backwards. sql.Conn.Close()
// RETURNS the connection to the pool; that is the whole hazard. Three things
// keep it clean, in order: restoreBeforeCommit on the success path,
// restoreDetached in cleanup for every other path, and the ErrBadConn discard
// when neither restore succeeds.
//
// So the assertion is the round trip rather than any one of those mechanisms:
// whatever pooled connections carried before the flip, they must still carry
// after it. Removing a single restore path leaves the other covering it;
// removing both makes this fail with the flip's own timeout leaked into the
// pool, which is the outcome that actually matters.
func TestFlipWithSessionLockWaitTimeout(t *testing.T) {
	db := setupDB(t)
	seedState(t, db, cutover.ModeInactive, 0, 0)

	// The baseline is whatever pooled connections already carry — a SET GLOBAL
	// here would prove nothing, because it only reaches connections opened
	// afterwards while the flip pins one that already exists.
	const flipTimeout = 3
	var baseline int64
	_, err := db.SelectBySql("SELECT @@SESSION.innodb_lock_wait_timeout").Load(&baseline)
	require.NoError(t, err)
	if baseline == flipTimeout {
		// Not a failure: a legitimately-configured server can sit on this value,
		// and the test simply cannot distinguish a restored connection from a
		// leaked one when it does.
		t.Skipf("server default innodb_lock_wait_timeout is %d, the same value the flip "+
			"sets — a leak would be indistinguishable from a restore", baseline)
	}

	flipped, epoch, err := cutover.Flip(context.Background(), db, cutover.FlipSpec{
		Table: testStateTable, Floor: 10,
		Observe:                func(*dbr.Tx) (int64, error) { return 10, nil },
		LockWaitTimeoutSeconds: flipTimeout,
	})
	if err != nil || !flipped || epoch != 1 {
		t.Fatalf("flip with lock-wait timeout = (%v,%d,%v), want (true,1,nil)", flipped, epoch, err)
	}
	if st := readRow(t, db); !st.Active() || st.Floor != 10 {
		t.Fatalf("state after pinned flip = %+v", st)
	}

	// The connection the flip pinned is back in the pool by now. Sample enough
	// connections that a leaked setting cannot hide behind pool ordering: a
	// writer that drew the poisoned one would silently get a lock-wait budget
	// nobody configured.
	for i := 0; i < 8; i++ {
		var got int64
		_, err := db.SelectBySql("SELECT @@SESSION.innodb_lock_wait_timeout").Load(&got)
		require.NoError(t, err)
		require.EqualValues(t, baseline, got,
			"a pooled connection reports innodb_lock_wait_timeout=%d after the flip (baseline %d); "+
				"the flip's session value leaked into the pool — Close returns the connection, "+
				"it does not discard it, so one of the restore paths has to run", got, baseline)
	}
}

// TestFlipRestoresThroughACancelledContext pins that cleanup detaches from the
// caller's context.
//
// Cancelling mid-flip means the deferred restore would run against a dead
// deadline if it inherited it — failing instantly and discarding a connection a
// fresh two-second attempt returns to the pool cleanly. restoreDetached exists
// for that, and a regression dropping it surfaces here as a restore error
// joined onto an otherwise ordinary cancellation.
//
// Note what this does NOT cover: the driver.ErrBadConn discard branch, which
// only runs when the restore itself fails. That branch has no test in either
// package.
func TestFlipRestoresThroughACancelledContext(t *testing.T) {
	db := setupDB(t)
	seedState(t, db, cutover.ModeInactive, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	flipped, _, err := cutover.Flip(ctx, db, cutover.FlipSpec{
		Table: testStateTable, Floor: 5,
		Observe: func(*dbr.Tx) (int64, error) {
			// Cancel mid-flip: the commit still runs on the pinned connection,
			// and the cleanup restore must not inherit this cancellation.
			cancel()
			return 5, nil
		},
		LockWaitTimeoutSeconds: 3,
	})
	// The cancellation lands before the commit, so this flip is expected to
	// fail — the point is that it fails cleanly, with no restore error joined
	// on top from a cleanup that inherited the dead context.
	require.False(t, flipped)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "restore session lock-wait timeout",
		"cleanup inherited the cancelled context instead of detaching from it")

	if st := readRow(t, db); st.Active() {
		t.Fatal("a cancelled flip must not have activated the domain")
	}
}
