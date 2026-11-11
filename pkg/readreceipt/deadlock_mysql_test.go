package readreceipt

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

// This test creates a real row-lock cycle, not the production supremum cycle.
// The test-only driver wrapper prelocks row 9 on Write's first transaction;
// the heavier adversary holds row 10 then requests row 9. Write's normal
// upsert requests row 10, making its smaller transaction the 1213 victim.
func TestWriteMySQLRecoversForcedDeadlock(t *testing.T) {
	adversary := openReceiptMySQL(t)
	ensureReceiptFixture(t, adversary)
	uid := fmt.Sprintf("deadlock-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if _, err := adversary.Exec("DELETE FROM member_readed WHERE uid IN (?, ?)", uid, uid+"-bulk"); err != nil {
			t.Error(err)
		}
	})
	rows := make([]Row, 7)
	for i := range rows {
		rows[i] = Row{MessageID: int64(9 + i), UID: uid, ChannelID: "channel", ChannelType: 2}
	}
	if err := Write(context.Background(), adversary.NewSession(nil), rows[:2]); err != nil {
		t.Fatal(err)
	}
	bulk := make([]Row, 100)
	for i := range bulk {
		bulk[i] = Row{MessageID: int64(100 + i), UID: uid + "-bulk", ChannelID: "channel", ChannelType: 2}
	}
	if err := Write(context.Background(), adversary.NewSession(nil), bulk); err != nil {
		t.Fatal(err)
	}
	var id9, id10 int64
	if err := adversary.QueryRow("SELECT id FROM member_readed WHERE message_id='9' AND uid=?", uid).Scan(&id9); err != nil {
		t.Fatal(err)
	}
	if err := adversary.QueryRow("SELECT id FROM member_readed WHERE message_id='10' AND uid=?", uid).Scan(&id10); err != nil {
		t.Fatal(err)
	}
	var bulkIDs []int64
	resultRows, err := adversary.Query("SELECT id FROM member_readed WHERE uid=?", uid+"-bulk")
	if err != nil {
		t.Fatal(err)
	}
	for resultRows.Next() {
		var id int64
		if err := resultRows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		bulkIDs = append(bulkIDs, id)
	}
	if err := resultRows.Err(); err != nil {
		t.Fatal(err)
	}
	resultRows.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := adversary.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var adversaryID int64
	if err := tx.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&adversaryID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE member_readed SET clone_no='held' WHERE id=?", id10); err != nil {
		t.Fatal(err)
	}
	// Actual updates, not just reads, give the adversary a much higher weight.
	for _, id := range bulkIDs {
		if _, err := tx.ExecContext(ctx, "UPDATE member_readed SET clone_no='held' WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
	}

	parent, disconnect := context.WithCancel(ctx)
	defer disconnect()
	hook := &deadlockCycle{id9: id9, adversaryID: adversaryID, locked: make(chan struct{}), disconnect: disconnect}
	cfg, err := mysql.ParseDSN(receiptMySQLDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	base, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(&deadlockConnector{Connector: base, cycle: hook})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer db.Close()
	conn := &dbr.Connection{DB: db, Dialect: dialect.MySQL, EventReceiver: &dbr.NullEventReceiver{}}
	competitorDone := make(chan error, 1)
	go func() {
		select {
		case <-hook.locked:
		case <-ctx.Done():
			competitorDone <- ctx.Err()
			return
		}
		_, err := tx.ExecContext(ctx, "UPDATE member_readed SET clone_no='held' WHERE id=?", id9)
		if err == nil {
			err = tx.Commit()
		}
		competitorDone <- err
	}()
	writeErr := Write(parent, conn.NewSession(nil), rows)
	// Always collect the adversary, even when the writer unexpectedly fails.
	if writeErr != nil {
		cancel()
	}
	competitorErr := <-competitorDone
	if writeErr != nil {
		t.Fatalf("writer: %v; adversary: %v", writeErr, competitorErr)
	}
	if competitorErr != nil {
		t.Fatal(competitorErr)
	}
	if hook.deadlocks.Load() < 1 || hook.starts.Load() < 2 {
		t.Fatalf("no actual service retry: starts=%d, server1213=%d", hook.starts.Load(), hook.deadlocks.Load())
	}
	if parent.Err() == nil {
		t.Fatal("parent disconnect not exercised")
	}
	var count, unique int
	if err := adversary.QueryRow("SELECT COUNT(*), COUNT(DISTINCT message_id) FROM member_readed WHERE uid=?", uid).Scan(&count, &unique); err != nil {
		t.Fatal(err)
	}
	if count != 7 || unique != 7 {
		t.Fatalf("lost receipts: count=%d distinct=%d", count, unique)
	}
	if db.Stats().InUse != 0 {
		t.Fatalf("writer connection leaked: %+v", db.Stats())
	}
	t.Logf("server1213=%d; attempts=%d; final unique receipts=%d; parent disconnected", hook.deadlocks.Load(), hook.starts.Load(), unique)
}

type deadlockCycle struct {
	id9         int64
	uid         string
	adversaryID int64
	starts      atomic.Int32
	deadlocks   atomic.Int32
	locked      chan struct{}
	disconnect  context.CancelFunc
}

type deadlockConnector struct {
	driver.Connector
	cycle *deadlockCycle
}

func (c *deadlockConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &deadlockConn{Conn: conn, cycle: c.cycle}, nil
}

type deadlockConn struct {
	driver.Conn
	cycle *deadlockCycle
}

func (c *deadlockConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }
func (c *deadlockConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}
func (c *deadlockConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	exec := c.Conn.(driver.ExecerContext)
	result, err := exec.ExecContext(ctx, query, args)
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1213 {
		c.cycle.deadlocks.Add(1)
	}
	if err != nil || query != "START TRANSACTION" {
		return result, err
	}
	if c.cycle.starts.Add(1) != 1 {
		return result, nil
	}
	lock := fmt.Sprintf("UPDATE member_readed SET clone_no=clone_no WHERE id=%d", c.cycle.id9)
	if _, err := exec.ExecContext(ctx, lock, nil); err != nil {
		return nil, fmt.Errorf("prelock row9: %w", err)
	}
	close(c.cycle.locked)
	c.cycle.disconnect()
	// Wait for the adversary's request to be genuinely blocked on our row,
	// rather than assume a scheduler-dependent sleep creates the cycle.
	query = fmt.Sprintf(`SELECT COUNT(*) FROM performance_schema.data_lock_waits w JOIN performance_schema.threads t ON w.REQUESTING_THREAD_ID=t.THREAD_ID JOIN performance_schema.threads b ON w.BLOCKING_THREAD_ID=b.THREAD_ID WHERE t.PROCESSLIST_ID=%d AND b.PROCESSLIST_ID=CONNECTION_ID()`, c.cycle.adversaryID)
	for {
		rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, nil)
		if err != nil {
			return nil, err
		}
		values := make([]driver.Value, 1)
		err = rows.Next(values)
		rows.Close()
		if err != nil && err != io.EOF {
			return nil, err
		}
		var count string
		if bytes, ok := values[0].([]byte); ok {
			count = string(bytes)
		} else {
			count = fmt.Sprint(values[0])
		}
		if strings.TrimSpace(count) == "1" {
			return result, nil
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("waiting for adversary lock, last count=%s: %w", count, ctx.Err())
		case <-timer.C:
		}
	}
}
