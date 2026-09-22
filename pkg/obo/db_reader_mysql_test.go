package obo

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// This regression uses a temporary table so it can exercise the production
// MySQL collation without changing persistent rows in the configured database.
func TestBotTokenLookupRequiresExactBytesMySQL(t *testing.T) {
	dsn, explicitDSN := os.LookupEnv("OCTO_OBO_TEST_MYSQL_DSN")
	if !explicitDSN {
		dsn = "root:demo@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=true"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		if !explicitDSN {
			t.Skipf("default MySQL test service is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "CREATE TEMPORARY TABLE robot (robot_id VARCHAR(64), creator_uid VARCHAR(64), bot_token VARCHAR(255) COLLATE utf8mb4_general_ci, status INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO robot (robot_id,creator_uid,bot_token,status) VALUES ('bot-1','human-1','bf_AbC123',1)"); err != nil {
		t.Fatal(err)
	}
	const exactToken = "bf_AbC123"
	for _, token := range asciiCaseMutations(exactToken) {
		var botID, ownerUID string
		err := conn.QueryRowContext(ctx, botTokenLookupSQL, token, token).Scan(&botID, &ownerUID)
		if token == exactToken {
			if err != nil || botID != "bot-1" || ownerUID != "human-1" {
				t.Fatalf("exact token lookup: bot=%q owner=%q err=%v", botID, ownerUID, err)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("case-variant token %q must be rejected, got bot=%q err=%v", token, botID, err)
		}
	}
}

func asciiCaseMutations(value string) []string {
	positions := make([]int, 0, len(value))
	for i := range value {
		if value[i] >= 'a' && value[i] <= 'z' || value[i] >= 'A' && value[i] <= 'Z' {
			positions = append(positions, i)
		}
	}
	mutations := make([]string, 0, 1<<len(positions))
	for mask := 0; mask < 1<<len(positions); mask++ {
		candidate := []byte(value)
		for bit, position := range positions {
			if mask&(1<<bit) == 0 {
				continue
			}
			if candidate[position] >= 'a' && candidate[position] <= 'z' {
				candidate[position] -= 'a' - 'A'
			} else {
				candidate[position] += 'a' - 'A'
			}
		}
		mutations = append(mutations, string(candidate))
	}
	return mutations
}
