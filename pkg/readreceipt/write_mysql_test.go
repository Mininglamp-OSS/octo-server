package readreceipt

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gocraft/dbr/v2"
)

// Run against an explicit local disposable DB or the existing Actions E2E DB.
// The fixture is a test assumption, not evidence of the production schema.
func TestWriteMySQLOverlappingBatches(t *testing.T) {
	dsn := os.Getenv("READ_RECEIPT_MYSQL_DSN")
	if dsn == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		// ci/run-e2e-shard.sh recreates this disposable DB before each package.
		dsn = "root:demo@tcp(127.0.0.1:3306)/test?parseTime=true"
	}
	if dsn == "" {
		t.Skip("set READ_RECEIPT_MYSQL_DSN to a disposable MySQL database")
	}
	open := func() *dbr.Connection {
		conn, err := dbr.Open("mysql", dsn, nil)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	left, right := open(), open()
	var leftID, rightID int64
	if err := left.QueryRow("SELECT CONNECTION_ID()").Scan(&leftID); err != nil {
		t.Fatal(err)
	}
	if err := right.QueryRow("SELECT CONNECTION_ID()").Scan(&rightID); err != nil {
		t.Fatal(err)
	}
	if leftID == rightID {
		t.Fatal("expected independent MySQL connections")
	}
	var version, isolation string
	if err := left.QueryRow("SELECT VERSION(), @@transaction_isolation").Scan(&version, &isolation); err != nil {
		t.Fatal(err)
	}
	t.Logf("MySQL %s; isolation=%s; connections=%d/%d", version, isolation, leftID, rightID)
	_, err := left.Exec(`CREATE TABLE IF NOT EXISTS member_readed (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		message_id VARCHAR(20) NOT NULL,
		channel_id VARCHAR(100) NOT NULL,
		channel_type TINYINT UNSIGNED NOT NULL,
		uid VARCHAR(100) NOT NULL,
		UNIQUE KEY message_uid (message_id, uid)
	) ENGINE=InnoDB`)
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range []int{0, 3, 7} {
		t.Run(fmt.Sprintf("existing_%d", existing), func(t *testing.T) {
			uid := fmt.Sprintf("receipt-test-%d-%d", time.Now().UnixNano(), existing)
			t.Cleanup(func() {
				if _, err := left.Exec("DELETE FROM member_readed WHERE uid=?", uid); err != nil {
					t.Error(err)
				}
			})
			for iteration := 0; iteration < 50; iteration++ {
				base := time.Now().UnixNano()
				all := make([]Row, 7)
				for i := range all {
					all[i] = Row{MessageID: base + int64(i), ChannelID: "normalized-channel", ChannelType: 2, UID: uid}
				}
				if err := Write(context.Background(), left.NewSession(nil), all[:existing]); err != nil {
					t.Fatal(err)
				}
				shuffled := []Row{all[6], all[2], all[0], all[5], all[1], all[4], all[3]}
				batches := [][]Row{all[:6], shuffled}
				if iteration%2 == 1 {
					batches[0], batches[1] = batches[1], batches[0]
				}
				start := make(chan struct{})
				errs := make(chan error, 2)
				var wg sync.WaitGroup
				for i, conn := range []*dbr.Connection{left, right} {
					wg.Add(1)
					go func(conn *dbr.Connection, rows []Row) {
						defer wg.Done()
						<-start
						errs <- Write(context.Background(), conn.NewSession(nil), rows)
					}(conn, batches[i])
				}
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatalf("iteration %d: %v", iteration, err)
					}
				}
				var count, distinct int
				if err := left.QueryRow("SELECT COUNT(*), COUNT(DISTINCT message_id) FROM member_readed WHERE uid=? AND message_id BETWEEN ? AND ?", uid, base, base+6).Scan(&count, &distinct); err != nil {
					t.Fatal(err)
				}
				if count != 7 || distinct != 7 {
					t.Fatalf("iteration %d: count=%d distinct=%d", iteration, count, distinct)
				}
			}
			t.Log("50 simultaneous ascending/shuffled 6/7 pairs; exactly seven unique rows per pair")
		})
	}
}
