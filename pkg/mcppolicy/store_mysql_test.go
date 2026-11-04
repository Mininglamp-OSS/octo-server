package mcppolicy

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	migrate "github.com/rubenv/sql-migrate"
)

// This suite creates and drops only a fresh mcp_policy_test_* database. The
// opt-in DSN must point at a disposable test server, never a deployed database.
func mysqlStore(t *testing.T) (Store, *sql.DB, *migrate.MemoryMigrationSource) {
	t.Helper()
	dsn := os.Getenv("MCP_POLICY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set MCP_POLICY_TEST_MYSQL_DSN to a disposable MySQL 8 server")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	cfg.DBName = ""
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("mcp_policy_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(6)
	db.SetMaxIdleConns(6)
	t.Cleanup(func() {
		db.Close()
		_, err := admin.Exec("DROP DATABASE `" + name + "`")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	raw, err := os.ReadFile("../../modules/robot/sql/20261030000001_mcp_context_policy.sql")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.ParseMigration("20261030000001_mcp_context_policy.sql", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	source := &migrate.MemoryMigrationSource{Migrations: []*migrate.Migration{m}}
	if n, err := migrate.Exec(db, "mysql", source, migrate.Up); err != nil || n != 1 {
		t.Fatalf("migration up: n=%d err=%v", n, err)
	}
	conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return Store{Session: conn.NewSession(nil)}, db, source
}

func TestMySQLMigrationSeedAndProtection(t *testing.T) {
	for _, edited := range []string{"none", "default", "override", "removed_override"} {
		t.Run(edited, func(t *testing.T) {
			s, db, source := mysqlStore(t)
			ctx := context.Background()
			p, err := s.GetDefault(ctx)
			if err != nil || p.ContextMode != Compatible || p.Generation != "1" {
				t.Fatalf("seed: %+v %v", p, err)
			}
			if n, err := migrate.Exec(db, "mysql", source, migrate.Up); err != nil || n != 0 {
				t.Fatalf("ledger repeat: %d %v", n, err)
			}
			switch edited {
			case "default":
				if _, err := s.PutDefault(ctx, "admin_U", Strict, 1); err != nil {
					t.Fatal(err)
				}
			case "override", "removed_override":
				if _, err := s.PutBot(ctx, "bot_B", "owner_U", Compatible, 1, allowOwner); err != nil {
					t.Fatal(err)
				}
				if edited == "removed_override" {
					if _, err := s.DeleteBot(ctx, "bot_B", "owner_U", 2, allowOwner); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Replay the actual Up statements to prove seed never overwrites edits.
			for _, stmt := range source.Migrations[0].Up {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			if edited == "default" {
				p, err := s.GetDefault(ctx)
				if err != nil || p.ContextMode != Strict || p.Generation != "2" {
					t.Fatalf("reseed destroyed config: %+v %v", p, err)
				}
			}
			_, err = migrate.Exec(db, "mysql", source, migrate.Down)
			if edited == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.GetDefault(ctx); err != ErrUnavailable {
					t.Fatal("down did not remove tables")
				}
			} else {
				if err == nil {
					t.Fatal("edited policy was destroyed")
				}
				if _, err := s.GetDefault(ctx); err != nil {
					t.Fatalf("protected data lost: %v", err)
				}
				var count int
				if err := db.QueryRow("SELECT COUNT(*) FROM mcp_bot_policy").Scan(&count); err != nil {
					t.Fatal(err)
				}
				if edited == "override" && count != 1 {
					t.Fatal("override lost")
				}
			}
		})
	}
}

func TestMySQLConstraintsAndCorruptState(t *testing.T) {
	s, db, _ := mysqlStore(t)
	for _, stmt := range []string{
		"INSERT INTO mcp_policy_default(singleton_id,context_mode,generation) VALUES(2,'compatible',1)",
		"UPDATE mcp_policy_default SET context_mode='invalid'",
		"UPDATE mcp_policy_default SET generation=0",
		"INSERT INTO mcp_bot_policy(bot_uid,context_mode,revision,updated_by) VALUES('','strict',1,'owner_U')",
		"INSERT INTO mcp_bot_policy(bot_uid,context_mode,revision,updated_by) VALUES('bot_B','STRICT',1,'owner_U')",
		"INSERT INTO mcp_bot_policy(bot_uid,context_mode,revision,updated_by) VALUES('bot_B','strict',0,'owner_U')",
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Fatalf("constraint accepted %s", stmt)
		}
	}
	if _, err := s.PutBot(context.Background(), "bot_B", "owner_U", Compatible, 1, allowOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO mcp_bot_policy(bot_uid,context_mode,revision,updated_by) VALUES('bot_B','strict',1,'owner_U')"); err == nil {
		t.Fatal("duplicate Bot accepted")
	}
	// CHECK cannot enforce cross-table revisions; the store still must.
	if _, err := db.Exec("UPDATE mcp_bot_policy SET revision=99"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(context.Background(), "bot_B"); err != ErrUnavailable {
		t.Fatal("future revision accepted")
	}
	if _, err := db.Exec("DELETE FROM mcp_policy_default"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(context.Background(), "bot_B"); err != ErrUnavailable {
		t.Fatal("missing mandatory default accepted")
	}
}

func TestMySQLCASAndDefaultTransition(t *testing.T) {
	s, _, _ := mysqlStore(t)
	ctx := context.Background()
	p, err := s.PutBot(ctx, "bot_B", "owner_U", Compatible, 1, allowOwner)
	if err != nil || p.Generation != "2" || p.Override == nil {
		t.Fatalf("explicit compatible: %+v %v", p, err)
	}
	if p, err := s.PutBot(ctx, "bot_B", "owner_U", Compatible, 2, allowOwner); err != nil || p.Generation != "2" {
		t.Fatalf("no-op: %+v %v", p, err)
	}
	if _, err := s.PutDefault(ctx, "admin_U", Strict, 2); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Resolve(ctx, "bot_B"); err != nil || p.ContextMode != Compatible || p.StageDefault != Strict {
		t.Fatalf("exception lost: %+v %v", p, err)
	}
	if p, err := s.Resolve(ctx, "new_B"); err != nil || p.ContextMode != Strict || p.Override != nil {
		t.Fatalf("default ignored: %+v %v", p, err)
	}
	if _, err := s.DeleteBot(ctx, "bot_B", "owner_U", 2, allowOwner); err != ErrConflict {
		t.Fatal("stale delete accepted")
	}
	if p, err := s.DeleteBot(ctx, "bot_B", "owner_U", 3, allowOwner); err != nil || p.ContextMode != Strict || p.Override != nil || p.Generation != "4" {
		t.Fatalf("delete: %+v %v", p, err)
	}
	if p, err := s.DeleteBot(ctx, "bot_B", "owner_U", 4, allowOwner); err != nil || p.Generation != "4" {
		t.Fatalf("delete no-op: %+v %v", p, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, uid := range []string{"race_A", "race_B"} {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			_, err := s.PutBot(ctx, uid, "owner_U", Compatible, 4, allowOwner)
			errs <- err
		}(uid)
	}
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if err == ErrConflict {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
}

func TestMySQLReadSnapshotAndWriteTimeout(t *testing.T) {
	s, _, _ := mysqlStore(t)
	ctx := context.Background()
	tx, err := s.begin(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.RollbackUnlessCommitted()
	def, err := readDefault(ctx, tx, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBot(ctx, "bot_B", "owner_U", Strict, 1, allowOwner); err != nil {
		t.Fatal(err)
	}
	bot, err := readBot(ctx, tx, "bot_B", def, false)
	if err != nil || bot != nil || def.generation != 1 {
		t.Fatalf("torn snapshot: %+v %+v %v", def, bot, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	lock, err := s.begin(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.RollbackUnlessCommitted()
	if _, err := readDefault(ctx, lock, true); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := s.PutDefault(deadline, "admin_U", Strict, 2); err != ErrUnavailable {
		t.Fatalf("blocked write: %v", err)
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if p, err := s.GetDefault(ctx); err != nil || p.Generation != "2" || p.ContextMode != Compatible {
		t.Fatalf("timed-out write changed data: %+v %v", p, err)
	}
}

func TestMySQLInterruptedAndIncompatibleMigration(t *testing.T) {
	s, db, source := mysqlStore(t)
	if _, err := migrate.Exec(db, "mysql", source, migrate.Down); err != nil {
		t.Fatal(err)
	}
	// Simulate an interruption after the first DDL, before seed completion.
	if _, err := db.Exec(source.Migrations[0].Up[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(context.Background(), "bot_B"); err != ErrUnavailable {
		t.Fatal("partial migration failed open")
	}
	if _, err := migrate.Exec(db, "mysql", source, migrate.Up); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDefault(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Exec(db, "mysql", source, migrate.Down); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE mcp_policy_default (unrelated INT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Exec(db, "mysql", source, migrate.Up); err == nil {
		t.Fatal("incompatible existing table accepted")
	}
	if _, err := s.GetDefault(context.Background()); err != ErrUnavailable {
		t.Fatal("incompatible table failed open")
	}
}

func TestMySQLReadOnlyAndGenerationLimit(t *testing.T) {
	s, db, _ := mysqlStore(t)
	ctx := context.Background()
	tx, err := s.begin(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.RollbackUnlessCommitted()
	if _, err := tx.ExecContext(ctx, "UPDATE mcp_policy_default SET context_mode='strict'"); err == nil {
		t.Fatal("snapshot transaction allowed writes")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE mcp_policy_default SET generation=18446744073709551615"); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetDefault(ctx)
	if err != nil || p.Generation != "18446744073709551615" {
		t.Fatalf("lossy generation: %+v %v", p, err)
	}
	if _, err := s.PutDefault(ctx, "admin_U", Strict, ^uint64(0)); err != ErrUnavailable {
		t.Fatal("generation wrapped")
	}
	if p, err := s.PutDefault(ctx, "admin_U", Compatible, ^uint64(0)); err != nil || p.Generation != "18446744073709551615" {
		t.Fatalf("no-op at limit: %+v %v", p, err)
	}
}
