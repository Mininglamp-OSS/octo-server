package card_template_catalog

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	mysqldriver "github.com/go-sql-driver/mysql"
	migrate "github.com/rubenv/sql-migrate"
)

const catalogStoreIntegrationDBPrefix = "octo_card_catalog_store_test"

var catalogStoreIntegrationDBSequence atomic.Uint64

func TestStoreReconcileStaticV3ClaimsExactVersionAndRejectsDynamicCollisionRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	registry := cardtmpl.NewRegistry()
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV3)
	registry.Freeze()
	catalogStore := newStore(db)

	if err := catalogStore.ReconcileStatic(context.Background(), registry.List()); err != nil {
		t.Fatalf("ReconcileStatic V3: %v", err)
	}
	var source string
	if err := db.QueryRow(`SELECT source FROM card_template_version_claim
        WHERE template_id = ? AND version = ?`,
		aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV3).Scan(&source); err != nil {
		t.Fatalf("query V3 static claim: %v", err)
	}
	if source != claimSourceStatic {
		t.Fatalf("V3 claim source = %q, want %q", source, claimSourceStatic)
	}
	var artifactCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM card_template_artifact
        WHERE template_id = ? AND version = ?`,
		aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV3).Scan(&artifactCount); err != nil {
		t.Fatalf("count V3 dynamic artifacts: %v", err)
	}
	if artifactCount != 0 {
		t.Fatalf("V3 dynamic artifact count = %d, want 0", artifactCount)
	}

	if _, err := db.Exec(`DELETE FROM card_template_version_claim
        WHERE template_id = ? AND version = ?`,
		aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV3); err != nil {
		t.Fatalf("remove V3 static claim fixture: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO card_template_version_claim
        (template_id, version, source) VALUES (?, ?, 'dynamic')`,
		aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV3); err != nil {
		t.Fatalf("seed V3 dynamic collision: %v", err)
	}
	if err := catalogStore.ReconcileStatic(context.Background(), registry.List()); !errors.Is(err, ErrVersionClaimConflict) {
		t.Fatalf("ReconcileStatic V3 collision error = %v, want ErrVersionClaimConflict", err)
	}
}

func TestStorePublishRealMySQLImmutableConcurrency(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	artifact := compileStoreIntegrationArtifact(t, "hello")
	stores := []*store{newStore(db), newStore(db)}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	type outcome struct {
		result PublishResult
		err    error
	}
	outcomes := make(chan outcome, len(stores))
	var publishers sync.WaitGroup
	for index, catalogStore := range stores {
		publishers.Add(1)
		go func(index int, catalogStore *store) {
			defer publishers.Done()
			<-start
			result, err := catalogStore.Publish(ctx, PublishRequest{
				Artifact: artifact, ActorUID: fmt.Sprintf("admin-%d", index+1),
				Reason: "integration publish", ChangeTicket: "TEST-IMMUTABLE",
			})
			outcomes <- outcome{result: result, err: err}
		}(index, catalogStore)
	}
	close(start)
	publishers.Wait()
	close(outcomes)

	created, idempotent := 0, 0
	for result := range outcomes {
		if result.err != nil {
			t.Fatalf("concurrent same-hash publish: %v", result.err)
		}
		if result.result.Created {
			created++
		}
		if result.result.Idempotent {
			idempotent++
		}
	}
	if created != 1 || idempotent != 1 {
		t.Fatalf("same-hash outcomes created/idempotent=%d/%d, want 1/1", created, idempotent)
	}

	conflicting := compileStoreIntegrationArtifact(t, "different")
	if conflicting.Hash == artifact.Hash {
		t.Fatal("conflicting fixture unexpectedly produced the same hash")
	}
	_, err := stores[0].Publish(ctx, PublishRequest{
		Artifact: conflicting, ActorUID: "admin-3",
		Reason: "integration conflict", ChangeTicket: "TEST-IMMUTABLE",
	})
	if !errors.Is(err, ErrImmutableVersionConflict) {
		t.Fatalf("different-hash publish error = %v, want ErrImmutableVersionConflict", err)
	}

	assertCatalogStoreIntegrationRows(t, ctx, db, artifact)
}

func assertCatalogStoreIntegrationRows(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	artifact *cardtmpl.CompiledArtifact,
) {
	t.Helper()
	for table, want := range map[string]int{
		"card_template_version_claim": 1,
		"card_template_artifact":      1,
	} {
		var count int
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE template_id = ? AND version = ?", table)
		if err := db.QueryRowContext(ctx, query, string(artifact.Meta.ID), artifact.Meta.Version).Scan(&count); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s row count = %d, want %d", table, count, want)
		}
	}

	rows, err := db.QueryContext(ctx, `SELECT result FROM card_template_audit
        WHERE template_id = ? AND version = ? ORDER BY id`, string(artifact.Meta.ID), artifact.Meta.Version)
	if err != nil {
		t.Fatalf("query audit results: %v", err)
	}
	defer rows.Close()
	results := make(map[string]int)
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			t.Fatalf("scan audit result: %v", err)
		}
		results[result]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit results: %v", err)
	}
	for result, want := range map[string]int{"created": 1, "idempotent": 1, "immutable_conflict": 1} {
		if results[result] != want {
			t.Fatalf("audit result %q count = %d, want %d; all=%v", result, results[result], want, results)
		}
	}
}

func compileStoreIntegrationArtifact(t *testing.T, title string) *cardtmpl.CompiledArtifact {
	t.Helper()
	root, err := cardtmpl.DecodeStrictJSONObject(validControlRequestJSON(t, false))
	if err != nil {
		t.Fatalf("decode control request: %v", err)
	}
	bundle, err := cardtmpl.DecodeBundleValue(root["bundle"])
	if err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	bundle.Samples["shown"], err = json.Marshal(map[string]string{"title": title})
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}
	artifact, err := cardtmpl.CompileJSONArtifact(context.Background(), bundle, cardtmpl.DefaultCompileLimits())
	if err != nil {
		t.Fatalf("compile integration artifact: %v", err)
	}
	return artifact
}

func newCatalogStoreIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	databaseName := fmt.Sprintf("%s_%d_%d", catalogStoreIntegrationDBPrefix,
		os.Getpid(), catalogStoreIntegrationDBSequence.Add(1))
	baseDSN := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if baseDSN == "" {
		baseDSN = "root:demo@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=true"
	}
	parsed, err := mysqldriver.ParseDSN(baseDSN)
	if err != nil {
		t.Fatalf("parse MySQL DSN: %v", err)
	}
	parsed.DBName = ""
	bootstrap, err := sql.Open("mysql", parsed.FormatDSN())
	if err != nil {
		t.Fatalf("open MySQL bootstrap connection: %v", err)
	}
	bootstrap.SetMaxOpenConns(2)
	bootstrap.SetMaxIdleConns(1)
	if _, err := bootstrap.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`"); err != nil {
		_ = bootstrap.Close()
		t.Fatalf("drop stale integration database: %v", err)
	}
	if _, err := bootstrap.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		_ = bootstrap.Close()
		t.Fatalf("create integration database: %v", err)
	}

	var db *sql.DB
	source := catalogStoreMigrationSource{}
	appliedMigrations := 0
	t.Cleanup(func() {
		if db != nil {
			if appliedMigrations > 0 {
				if reverted, err := migrate.Exec(db, "mysql", source, migrate.Down); err != nil || reverted != appliedMigrations {
					t.Errorf("revert catalog migrations: reverted=%d want=%d err=%v", reverted, appliedMigrations, err)
				}
			}
			if err := db.Close(); err != nil {
				t.Errorf("close integration database: %v", err)
			}
		}
		if _, err := bootstrap.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`"); err != nil {
			t.Errorf("drop integration database: %v", err)
		}
		if err := bootstrap.Close(); err != nil {
			t.Errorf("close bootstrap database: %v", err)
		}
	})

	parsed.DBName = databaseName
	db, err = sql.Open("mysql", parsed.FormatDSN())
	if err != nil {
		t.Fatalf("open integration database: %v", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping integration database: %v", err)
	}

	migrations, err := source.FindMigrations()
	if err != nil {
		t.Fatalf("find catalog migrations: %v", err)
	}
	applied, err := migrate.Exec(db, "mysql", source, migrate.Up)
	appliedMigrations = applied
	if err != nil || applied != len(migrations) {
		t.Fatalf("apply catalog migrations: applied=%d want=%d err=%v", applied, len(migrations), err)
	}
	return db
}

type catalogStoreMigrationSource struct{}

func (catalogStoreMigrationSource) FindMigrations() ([]*migrate.Migration, error) {
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		return nil, err
	}
	migrations := make([]*migrate.Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		raw, err := sqlFS.ReadFile(path.Join("sql", entry.Name()))
		if err != nil {
			return nil, err
		}
		migration, err := migrate.ParseMigration(entry.Name(), bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("parse migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Less(migrations[j]) })
	return migrations, nil
}
