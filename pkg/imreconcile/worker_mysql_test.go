package imreconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

// These tests use a fresh database on the explicitly selected isolated MySQL.
// They never clean application tables or run the application's live workers.
func queueMySQL(t *testing.T) (*sql.DB, *dbr.Session) {
	t.Helper()
	dsn := os.Getenv("IM_RECONCILE_TEST_DSN")
	if dsn == "" {
		t.Skip("set IM_RECONCILE_TEST_DSN for transactional MySQL integration tests")
	}
	cfg, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:35906", cfg.Addr, "use the isolated reconciliation test MySQL")
	cfg.DBName = ""
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	admin.SetMaxOpenConns(2)
	name := fmt.Sprintf("im_reconcile_test_%d", time.Now().UnixNano())
	_, err = admin.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4")
	require.NoError(t, err)
	cfg.DBName = name
	conn, err := dbr.Open("mysql", cfg.FormatDSN(), nil)
	require.NoError(t, err)
	conn.SetMaxOpenConns(6)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
		_, err := admin.Exec("DROP DATABASE " + name)
		require.NoError(t, err)
		require.NoError(t, admin.Close())
	})
	for _, statement := range []string{
		"CREATE TABLE `group` (group_no VARCHAR(40) PRIMARY KEY, status INT NOT NULL, group_type INT NOT NULL DEFAULT 0)",
		"CREATE TABLE group_member (group_no VARCHAR(40), uid VARCHAR(40), status INT NOT NULL DEFAULT 1, is_deleted INT NOT NULL DEFAULT 0, forbidden_expir_time INT NOT NULL DEFAULT 0, PRIMARY KEY(group_no, uid))",
		"CREATE TABLE thread (id BIGINT PRIMARY KEY, group_no VARCHAR(40), short_id VARCHAR(32), status INT NOT NULL DEFAULT 1, KEY idx_group(group_no, id))",
	} {
		_, err := conn.Exec(statement)
		require.NoError(t, err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "modules", "group", "sql", "20260924000001_im_group_reconcile.sql"))
	require.NoError(t, err)
	up := strings.Split(string(data), "-- +migrate Down")[0]
	for _, statement := range strings.Split(up, ";") {
		if strings.TrimSpace(statement) != "" {
			_, err := conn.Exec(statement)
			require.NoError(t, err)
		}
	}
	t.Setenv("DM_IM_RECONCILE_ENABLED", "true")
	_, err = conn.Exec("INSERT INTO `group` (group_no,status) VALUES ('g', 1)")
	require.NoError(t, err)
	_, err = conn.Exec("INSERT INTO group_member(group_no,uid) VALUES ('g','a'), ('g','b')")
	require.NoError(t, err)
	return conn.DB, conn.NewSession(nil)
}

func TestMySQLMembershipAndIntentRollbackTogether(t *testing.T) {
	db, session := queueMySQL(t)
	tx, err := session.Begin()
	require.NoError(t, err)
	require.NoError(t, TouchGroupTx(tx, "g"))
	_, err = tx.Exec("UPDATE group_member SET is_deleted=1 WHERE group_no='g' AND uid='b'")
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM im_group_reconcile").Scan(&count))
	require.Zero(t, count)
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM group_member WHERE uid='b' AND is_deleted=0").Scan(&count))
	require.Equal(t, 1, count)
	// Enqueue failure must happen before the mutation, including for callers
	// which choose to continue processing other removals after an error.
	_, err = db.Exec("DROP TABLE im_group_reconcile")
	require.NoError(t, err)
	err = Mutate(session, "g", func(tx *dbr.Tx) error {
		t.Error("a failed intent must not execute its membership write")
		_, err := tx.Exec("UPDATE group_member SET is_deleted=1 WHERE uid='b'")
		return err
	})
	require.Error(t, err)
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM group_member WHERE uid='b' AND is_deleted=0").Scan(&count))
	require.Equal(t, 1, count)
}

func TestMySQLSnapshotReleasesLocksAndFencesStaleCheckpoint(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		_, err := tx.Exec("UPDATE group_member SET is_deleted=1 WHERE uid='b'")
		return err
	}))
	entered, release := make(chan Snapshot, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	old := &Worker{db: db, send: func(ctx context.Context, s Snapshot) error {
		entered <- s
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type outcome struct {
		changed bool
		err     error
	}
	oldDone := make(chan outcome, 1)
	go func() { changed, err := old.step(ctx, "g"); oldDone <- outcome{changed, err} }()
	first := <-entered
	require.Equal(t, []string{"a"}, first.Subscribers)
	// This transaction must commit while the old sender is still blocked.
	mutated := make(chan error, 1)
	go func() {
		mutated <- Mutate(session, "g", func(tx *dbr.Tx) error {
			_, err := tx.Exec("UPDATE group_member SET is_deleted=0 WHERE uid='b'")
			return err
		})
	}()
	select {
	case err := <-mutated:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("network wait retained a SQL lock")
	}
	var latest Snapshot
	newer := &Worker{db: db, send: func(_ context.Context, s Snapshot) error { latest = s; return nil }}
	changed, err := newer.step(ctx, "g")
	require.NoError(t, err)
	require.True(t, changed)
	require.Greater(t, latest.Revision, first.Revision)
	require.Equal(t, []string{"a", "b"}, latest.Subscribers)
	// A third transaction must remain pending even when the oldest sender
	// finally reports success. Its lease and checkpoint are obsolete.
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		_, err := tx.Exec("UPDATE group_member SET is_deleted=1 WHERE uid='a'")
		return err
	}))
	unblock()
	result := <-oldDone
	require.NoError(t, result.err)
	require.False(t, result.changed)
	var pending int
	require.NoError(t, db.QueryRow("SELECT pending FROM im_group_reconcile WHERE group_no='g'").Scan(&pending))
	require.Equal(t, 1, pending)
	changed, err = newer.step(ctx, "g")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []string{"b"}, latest.Subscribers)
}

func TestMySQLRestartRecoversPagedChildrenAndPhysicalDeletion(t *testing.T) {
	db, session := queueMySQL(t)
	for i := int64(1); i <= 33; i++ {
		_, err := db.Exec("INSERT INTO thread(id,group_no,short_id) VALUES (?,'g',?)", i, fmt.Sprint(i))
		require.NoError(t, err)
	}
	_, err := db.Exec("UPDATE thread SET status=3 WHERE id=14")
	require.NoError(t, err)
	_, err = db.Exec("UPDATE thread SET status=2 WHERE id=15")
	require.NoError(t, err)
	tx, err := session.Begin()
	require.NoError(t, err)
	require.NoError(t, RetireChildTx(tx, 22, "g", "22"))
	_, err = tx.Exec("DELETE FROM thread WHERE id=22")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	// Simulate a process dying after its claim was committed and before send.
	old := &Worker{db: db}
	claimed, err := old.claim(context.Background(), "g")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	_, err = db.Exec("UPDATE im_group_reconcile SET lease_until=DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 1 SECOND)")
	require.NoError(t, err)
	var mu sync.Mutex
	seen := make(map[string]Snapshot)
	restarted := &Worker{db: db, send: func(_ context.Context, s Snapshot) error {
		mu.Lock()
		defer mu.Unlock()
		seen[s.ChannelID] = s
		return nil
	}}
	for i := 0; i < 3; i++ {
		changed, err := restarted.step(context.Background(), "g")
		require.NoError(t, err)
		require.True(t, changed)
	}
	require.Len(t, seen, 34)
	for channel, s := range seen {
		if channel == "g" {
			require.EqualValues(t, 2, s.ChannelType)
		} else {
			require.EqualValues(t, 5, s.ChannelType)
		}
		if channel == "g____22" {
			require.Empty(t, s.Subscribers)
		} else {
			require.Equal(t, []string{"a", "b"}, s.Subscribers)
		}
		if channel == "g____14" {
			require.Equal(t, 1, s.Ban)
		}
	}
	var pending int
	require.NoError(t, db.QueryRow("SELECT pending FROM im_group_reconcile").Scan(&pending))
	require.Zero(t, pending)
}

func TestMySQLFailedReconcileRemainsRetryableAndNewMutationRevivesIt(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(*dbr.Tx) error { return nil }))
	failure := errors.New("lost IM response")
	w := &Worker{db: db, send: func(context.Context, Snapshot) error { return failure }}
	_, err := w.step(context.Background(), "g")
	require.ErrorIs(t, err, failure)
	var pending, attempts int
	require.NoError(t, db.QueryRow("SELECT pending, attempts FROM im_group_reconcile").Scan(&pending, &attempts))
	require.Equal(t, 1, pending)
	require.Equal(t, 1, attempts)
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		_, err := tx.Exec("UPDATE group_member SET is_deleted=1 WHERE uid='b'")
		return err
	}))
	var sent Snapshot
	w.send = func(_ context.Context, s Snapshot) error { sent = s; return nil }
	changed, err := w.step(context.Background(), "g")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []string{"a"}, sent.Subscribers)
	require.NoError(t, db.QueryRow("SELECT pending, attempts FROM im_group_reconcile").Scan(&pending, &attempts))
	require.Zero(t, pending)
	require.Zero(t, attempts)
}

func TestMySQLMemberPagesResumeAfterWorkerRestart(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		for i := 0; i < 270; i++ {
			if _, err := tx.Exec("INSERT INTO group_member(group_no,uid) VALUES ('g',?)", fmt.Sprintf("u%03d", i)); err != nil {
				return err
			}
		}
		return nil
	}))
	var pages []Snapshot
	sender := func(_ context.Context, s Snapshot) error { pages = append(pages, s); return nil }
	old := &Worker{db: db, send: sender}
	changed, err := old.step(context.Background(), "g")
	require.NoError(t, err)
	require.True(t, changed)
	var cursor, pending int
	require.NoError(t, db.QueryRow("SELECT member_page,pending FROM im_group_reconcile").Scan(&cursor, &pending))
	require.Equal(t, 1, cursor)
	require.Equal(t, 1, pending)
	restarted := &Worker{db: db, send: sender}
	for i := 0; i < 2; i++ {
		changed, err = restarted.step(context.Background(), "g")
		require.NoError(t, err)
		require.True(t, changed)
	}
	require.Len(t, pages, 3)
	all := map[string]bool{}
	for i, s := range pages {
		require.EqualValues(t, i, s.PageIndex)
		require.EqualValues(t, 3, s.PageCount)
		require.Equal(t, pages[0].SnapshotID, s.SnapshotID)
		require.LessOrEqual(t, len(s.Subscribers), memberPageSize)
		if i > 0 {
			require.Equal(t, pages[i-1].RangeEnd, s.RangeStart)
		}
		for _, uid := range s.Subscribers {
			require.False(t, all[uid], "duplicate page member")
			all[uid] = true
		}
		encoded, err := json.Marshal(s)
		require.NoError(t, err)
		require.Less(t, len(encoded), 128<<10)
	}
	require.Len(t, all, 272)
	require.Empty(t, pages[0].RangeStart)
	require.Empty(t, pages[2].RangeEnd)
	require.NoError(t, db.QueryRow("SELECT member_page,pending FROM im_group_reconcile").Scan(&cursor, &pending))
	require.Zero(t, cursor)
	require.Zero(t, pending)
}

func TestMySQLAdoptionDoesNotResetAuthorityAndCanonicalizesIdentity(t *testing.T) {
	db, session := queueMySQL(t)
	w := &Worker{db: db, send: func(context.Context, Snapshot) error { return nil }}
	_, done, err := w.adoptPage(context.Background(), "")
	require.NoError(t, err)
	require.True(t, done)
	changed, err := w.step(context.Background(), "G")
	require.NoError(t, err)
	require.True(t, changed)
	_, _, err = w.adoptPage(context.Background(), "")
	require.NoError(t, err)
	var revision uint64
	var pending int
	require.NoError(t, db.QueryRow("SELECT revision,pending FROM im_group_reconcile").Scan(&revision, &pending))
	require.EqualValues(t, 1, revision)
	require.Zero(t, pending)
	require.NoError(t, Mutate(session, "G", func(*dbr.Tx) error { return nil }))
	var snapshot Snapshot
	w.send = func(_ context.Context, s Snapshot) error { snapshot = s; return nil }
	changed, err = w.step(context.Background(), "G")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "g", snapshot.ChannelID)
	require.EqualValues(t, 2, snapshot.Revision)
}

func TestMySQLDisbandRetainsHistoricalMembership(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(tx *dbr.Tx) error {
		_, err := tx.Exec("UPDATE `group` SET status=2 WHERE group_no='g'")
		return err
	}))
	var snapshot Snapshot
	w := &Worker{db: db, send: func(_ context.Context, s Snapshot) error { snapshot = s; return nil }}
	changed, err := w.step(context.Background(), "g")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 1, snapshot.Disband)
	require.Equal(t, []string{"a", "b"}, snapshot.Subscribers)
}

func TestMySQLConcurrentMutationsAndClaimsKeepEveryRevision(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(*dbr.Tx) error { return nil }))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	worker := &Worker{db: db, send: func(context.Context, Snapshot) error { return nil }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_, _ = worker.step(ctx, "g")
			time.Sleep(time.Millisecond)
		}
	}()
	var wg sync.WaitGroup
	results := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- Mutate(session, "g", func(tx *dbr.Tx) error {
				_, err := tx.Exec("UPDATE group_member SET is_deleted=1-is_deleted WHERE group_no='g' AND uid='a'")
				return err
			})
		}()
	}
	wg.Wait()
	cancel()
	<-done
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	var revision uint64
	require.NoError(t, db.QueryRow("SELECT revision FROM im_group_reconcile").Scan(&revision))
	require.EqualValues(t, 33, revision)
	var deleted int
	require.NoError(t, db.QueryRow("SELECT is_deleted FROM group_member WHERE uid='a'").Scan(&deleted))
	require.Zero(t, deleted)
}

func TestMySQLForegroundRetriesWithinBudgetAndLeavesTimeoutPending(t *testing.T) {
	db, session := queueMySQL(t)
	require.NoError(t, Mutate(session, "g", func(*dbr.Tx) error { return nil }))
	calls := 0
	id := ""
	transient := errors.New("lost successful reply")
	worker := &Worker{db: db, send: func(_ context.Context, s Snapshot) error {
		calls++
		if id == "" {
			id = s.OperationID
		} else {
			require.Equal(t, id, s.OperationID)
		}
		if calls == 1 {
			return transient
		}
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, worker.flush(ctx, "g"))
	require.Equal(t, 2, calls)
	require.NoError(t, Mutate(session, "g", func(*dbr.Tx) error { return nil }))
	worker.send = func(context.Context, Snapshot) error { return transient }
	failedCtx, stop := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer stop()
	err := worker.flush(failedCtx, "g")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var pending int
	require.NoError(t, db.QueryRow("SELECT pending FROM im_group_reconcile").Scan(&pending))
	require.Equal(t, 1, pending)
}
