package bot_api

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

func TestRevokedGrantReauthorizationClearsLegacyPersonaMySQL(t *testing.T) {
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
	if _, err := conn.ExecContext(ctx, "CREATE TEMPORARY TABLE obo_grants (id BIGINT PRIMARY KEY, mode VARCHAR(16), active INT, global_enabled INT, revoked_at DATETIME NULL, persona_prompt TEXT, expires_at DATETIME NULL, policy_version BIGINT, updated_at DATETIME)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO obo_grants VALUES (1,'auto',0,0,'2026-09-21 00:00:00','revoked persona',NULL,3,UTC_TIMESTAMP())"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, updateGenericGrantSQL, policyGrantMode, 0, 0, 0, 0, 0, nil, 1); err != nil {
		t.Fatal(err)
	}
	var revoked sql.NullTime
	var persona string
	if err := conn.QueryRowContext(ctx, "SELECT revoked_at,persona_prompt FROM obo_grants WHERE id=1").Scan(&revoked, &persona); err != nil {
		t.Fatal(err)
	}
	if !revoked.Valid || persona != "revoked persona" {
		t.Fatalf("disable-only PUT must preserve revocation and persona, revoked=%v persona=%q", revoked.Valid, persona)
	}
	if _, err := conn.ExecContext(ctx, updateGenericGrantSQL, policyGrantMode, 1, 1, 1, 1, 0, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT revoked_at,persona_prompt FROM obo_grants WHERE id=1").Scan(&revoked, &persona); err != nil {
		t.Fatal(err)
	}
	if revoked.Valid || persona != "" {
		t.Fatalf("reauthorization must clear revocation and the stale legacy persona, revoked=%v persona=%q", revoked.Valid, persona)
	}
}

func TestUsableGrantPredicateMySQL(t *testing.T) {
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
	for _, statement := range []string{
		"CREATE TEMPORARY TABLE obo_grants (id BIGINT PRIMARY KEY, mode VARCHAR(16), active INT, revoked_at DATETIME(6) NULL, expires_at DATETIME(6) NULL)",
		"INSERT INTO obo_grants VALUES (1,'auto',1,NULL,NULL)",
		"INSERT INTO obo_grants VALUES (2,'auto',1,NULL,UTC_TIMESTAMP(6) + INTERVAL 1 HOUR)",
		"INSERT INTO obo_grants VALUES (3,'auto',1,NULL,UTC_TIMESTAMP(6) - INTERVAL 1 HOUR)",
		"INSERT INTO obo_grants VALUES (4,'auto',1,UTC_TIMESTAMP(6),NULL)",
		"INSERT INTO obo_grants VALUES (5,'policy',1,NULL,NULL)",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := conn.QueryContext(ctx, "SELECT id FROM obo_grants WHERE "+usableGrantPredicate("")+" ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("usable Grant predicate returned ids %v, want [1 2]", ids)
	}
}

func TestManagementReadRejectsInconsistentBotOwnerRecordMySQL(t *testing.T) {
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
	for _, statement := range []string{
		"CREATE TEMPORARY TABLE obo_grants (id BIGINT PRIMARY KEY, grantor_uid VARCHAR(64), grantee_bot_uid VARCHAR(64), mode VARCHAR(16), active INT, global_enabled INT, revoked_at DATETIME NULL, expires_at DATETIME NULL, policy_version BIGINT)",
		"CREATE TEMPORARY TABLE robot (robot_id VARCHAR(64) PRIMARY KEY, creator_uid VARCHAR(64), status INT)",
		"CREATE TEMPORARY TABLE user (uid VARCHAR(64) PRIMARY KEY, robot INT, status INT, is_destroy INT)",
		"INSERT INTO obo_grants VALUES (7,'human-1','bot-1','policy',1,1,NULL,NULL,3)",
		"INSERT INTO robot VALUES ('bot-1','human-1',1)",
		"INSERT INTO user VALUES ('human-1',0,1,0)",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		var value int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM ("+genericGrantForOwnerSQL+") owned", "human-1", "human-1", 7, "human-1").Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if count() != 1 {
		t.Fatal("current owner should be able to read the Grant")
	}
	if _, err := conn.ExecContext(ctx, "UPDATE robot SET creator_uid='human-2' WHERE robot_id='bot-1'"); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatal("Grant access must fail when the Bot owner record no longer matches the grantor")
	}
}
