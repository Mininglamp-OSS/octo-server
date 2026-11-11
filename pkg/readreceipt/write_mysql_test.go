package readreceipt

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gocraft/dbr/v2"
)

// Run against an explicit local disposable DB or the existing Actions E2E DB.
// The fixture is a test assumption, not evidence of the production schema.
func TestWriteMySQLOverlappingBatches(t *testing.T) {
	left, right := openReceiptMySQL(t), openReceiptMySQL(t)
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
	ensureReceiptFixture(t, left)
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

func receiptMySQLDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("READ_RECEIPT_MYSQL_DSN")
	if dsn == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		// This package is in the E2E lane, whose runner recreates this DB.
		dsn = "root:demo@tcp(127.0.0.1:3306)/test?parseTime=true"
	}
	if dsn == "" {
		if os.Getenv("READ_RECEIPT_REQUIRE_MYSQL") == "1" {
			t.Fatal("E2E requires READ_RECEIPT_MYSQL_DSN; refusing to skip")
		}
		t.Skip("set READ_RECEIPT_MYSQL_DSN to a disposable MySQL database")
	}
	return dsn
}

func openReceiptMySQL(t *testing.T) *dbr.Connection {
	t.Helper()
	conn, err := dbr.Open("mysql", receiptMySQLDSN(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { conn.Close() })
	if err := conn.Ping(); err != nil {
		t.Fatalf("required MySQL unavailable: %v", err)
	}
	return conn
}

func ensureReceiptFixture(t *testing.T, conn *dbr.Connection) {
	t.Helper()
	var exists int
	if err := conn.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='member_readed'").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists == 1 {
		validateReceiptFixture(t, conn)
		return
	}
	migration, err := os.ReadFile("../../modules/message/sql/20210407000001_message_legacy01.sql")
	if err != nil {
		t.Fatal(err)
	}
	ddl := string(migration)
	start := strings.Index(ddl, "CREATE TABLE `member_readed`")
	if start < 0 {
		t.Fatal("member_readed migration section not found")
	}
	end := strings.Index(ddl[start:], "-- 最近会话扩展表")
	if end < 0 {
		t.Fatal("member_readed migration section not found")
	}
	// Execute the actual repository table and all three indexes, not a reduced
	// lookalike. This is still not evidence of the deployed production DDL.
	for _, statement := range strings.Split(ddl[start:start+end], ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := conn.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	validateReceiptFixture(t, conn)
}
