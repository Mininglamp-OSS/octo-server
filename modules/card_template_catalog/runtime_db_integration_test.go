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
	mysqldriver "github.com/go-sql-driver/mysql"
	migrate "github.com/rubenv/sql-migrate"
)

const runtimeCatalogIntegrationDBPrefix = "octo_card_catalog_runtime_test"

var runtimeCatalogIntegrationDBSequence atomic.Uint64

func TestRuntimeCatalogMultiReplicaActivationRollbackBlockAndRestart(t *testing.T) {
	db := newRuntimeCatalogIntegrationDB(t)
	storeA, storeB := newStore(db), newStore(db)
	artifactV1 := integrationArtifactForVersion(t, "1.0.0")
	artifactV2 := integrationArtifactForVersion(t, "1.1.0")
	for _, artifact := range []*cardtmpl.CompiledArtifact{artifactV1, artifactV2} {
		if _, err := storeA.Publish(context.Background(), PublishRequest{
			Artifact: artifact, ActorUID: "admin-1", Reason: "integration publish", ChangeTicket: "TEST-1",
		}); err != nil {
			t.Fatalf("Publish %s: %v", artifact.Meta.Version, err)
		}
	}

	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	catalogA := integrationRuntimeCatalog(t, registry, storeA)
	catalogB := integrationRuntimeCatalog(t, registry, storeB)
	access := cardtmpl.CatalogAccess{
		Purpose: cardtmpl.CatalogPurposeNewSend,
		Principal: cardtmpl.CatalogPrincipal{
			Kind: cardtmpl.CatalogPrincipalSystem, ID: "integration", SpaceID: "space-1",
		},
	}
	defaultRequest := cardtmpl.CatalogDefaultRequest{Access: access, ID: artifactV1.Meta.ID}

	firstRequest := ActivationRequest{
		TemplateID: artifactV1.Meta.ID, TargetVersion: artifactV1.Meta.Version,
		ExpectedRevision: 0, ActorUID: "admin-1", Reason: "activate v1", ChangeTicket: "TEST-2",
		targetReceipt: receiptForIntegrationArtifact(artifactV1),
	}
	type activationOutcome struct {
		result ActivationResult
		err    error
	}
	begin := make(chan struct{})
	outcomes := make(chan activationOutcome, 2)
	var callers sync.WaitGroup
	for _, stateStore := range []*store{storeA, storeB} {
		callers.Add(1)
		go func(stateStore *store) {
			defer callers.Done()
			<-begin
			result, err := stateStore.Activate(context.Background(), firstRequest)
			outcomes <- activationOutcome{result: result, err: err}
		}(stateStore)
	}
	close(begin)
	callers.Wait()
	close(outcomes)
	successes, conflicts := 0, 0
	for outcome := range outcomes {
		switch {
		case outcome.err == nil && outcome.result.Revision == 1:
			successes++
		case errors.Is(outcome.err, ErrActivationConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent activation outcome: result=%+v err=%v", outcome.result, outcome.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent first activation successes/conflicts=%d/%d, want 1/1", successes, conflicts)
	}
	assertCatalogDefaultVersion(t, catalogA, defaultRequest, artifactV1.Meta.Version)
	assertCatalogDefaultVersion(t, catalogB, defaultRequest, artifactV1.Meta.Version)

	second, err := storeA.Activate(context.Background(), ActivationRequest{
		TemplateID: artifactV2.Meta.ID, TargetVersion: artifactV2.Meta.Version,
		ExpectedRevision: 1, ActorUID: "admin-1", Reason: "activate v2", ChangeTicket: "TEST-3",
		targetReceipt: receiptForIntegrationArtifact(artifactV2),
	})
	if err != nil || second.Revision != 2 {
		t.Fatalf("activate v2 result=%+v err=%v", second, err)
	}
	assertCatalogDefaultVersion(t, catalogB, defaultRequest, artifactV2.Meta.Version)

	rolledBack, err := storeA.Rollback(context.Background(), RollbackRequest{
		TemplateID: artifactV1.Meta.ID, TargetVersion: artifactV1.Meta.Version,
		ExpectedRevision: 2, ActorUID: "admin-1", Reason: "rollback v1", ChangeTicket: "TEST-4",
		targetReceipt: receiptForIntegrationArtifact(artifactV1),
	})
	if err != nil || rolledBack.Revision != 3 {
		t.Fatalf("rollback v1 result=%+v err=%v", rolledBack, err)
	}
	assertCatalogDefaultVersion(t, catalogB, defaultRequest, artifactV1.Meta.Version)

	blockedV1, err := storeA.Block(context.Background(), BlockRequest{
		TemplateID: artifactV1.Meta.ID, Version: artifactV1.Meta.Version,
		ExpectedRevision: 3, FallbackVersion: artifactV2.Meta.Version,
		ActorUID: "admin-1", Reason: "block v1", ChangeTicket: "TEST-5",
		fallbackReceipt: receiptForIntegrationArtifact(artifactV2),
	})
	if err != nil || blockedV1.Revision != 4 || blockedV1.ActiveVersion != artifactV2.Meta.Version {
		t.Fatalf("block v1 result=%+v err=%v", blockedV1, err)
	}
	_, err = catalogB.MetaExact(context.Background(), cardtmpl.CatalogExactRequest{
		Access: access, ID: artifactV1.Meta.ID, Version: artifactV1.Meta.Version,
	})
	if !errors.Is(err, cardtmpl.ErrRuntimeCatalogBlocked) {
		t.Fatalf("hot-cache blocked v1 error=%v, want ErrRuntimeCatalogBlocked", err)
	}
	assertCatalogDefaultVersion(t, catalogB, defaultRequest, artifactV2.Meta.Version)

	// A new process has an empty compiled cache but must recover the same DB truth.
	catalogAfterRestart := integrationRuntimeCatalog(t, registry, newStore(db))
	assertCatalogDefaultVersion(t, catalogAfterRestart, defaultRequest, artifactV2.Meta.Version)

	blockedV2, err := storeA.Block(context.Background(), BlockRequest{
		TemplateID: artifactV2.Meta.ID, Version: artifactV2.Meta.Version,
		ExpectedRevision: 4, ActorUID: "admin-1", Reason: "disable catalog", ChangeTicket: "TEST-6",
	})
	if err != nil || blockedV2.Revision != 5 || blockedV2.Status != activationStatusDisabled {
		t.Fatalf("block v2 result=%+v err=%v", blockedV2, err)
	}
	for name, catalog := range map[string]*cardtmpl.RuntimeCatalog{"replica-b": catalogB, "restart": catalogAfterRestart} {
		if _, err := catalog.MetaDefault(context.Background(), defaultRequest); !errors.Is(err, cardtmpl.ErrRuntimeCatalogDisabled) {
			t.Fatalf("%s disabled default error=%v, want ErrRuntimeCatalogDisabled", name, err)
		}
	}

}

func newRuntimeCatalogIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	databaseName := fmt.Sprintf("%s_%d_%d", runtimeCatalogIntegrationDBPrefix,
		os.Getpid(), runtimeCatalogIntegrationDBSequence.Add(1))
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
	parsed.DBName = databaseName
	db, err := sql.Open("mysql", parsed.FormatDSN())
	if err != nil {
		_ = bootstrap.Close()
		t.Fatalf("open integration database: %v", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		_ = bootstrap.Close()
		t.Fatalf("ping integration database: %v", err)
	}
	source := runtimeCatalogMigrationSource{}
	if applied, err := migrate.Exec(db, "mysql", source, migrate.Up); err != nil || applied != 2 {
		_ = db.Close()
		_ = bootstrap.Close()
		t.Fatalf("apply catalog migrations: applied=%d err=%v", applied, err)
	}
	t.Cleanup(func() {
		if reverted, err := migrate.Exec(db, "mysql", source, migrate.Down); err != nil || reverted != 2 {
			t.Errorf("revert catalog migrations: reverted=%d err=%v", reverted, err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("close integration database: %v", err)
		}
		if _, err := bootstrap.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`"); err != nil {
			t.Errorf("drop integration database: %v", err)
		}
		if err := bootstrap.Close(); err != nil {
			t.Errorf("close bootstrap database: %v", err)
		}
	})
	return db
}

type runtimeCatalogMigrationSource struct{}

func (runtimeCatalogMigrationSource) FindMigrations() ([]*migrate.Migration, error) {
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

func integrationRuntimeCatalog(t *testing.T, registry *cardtmpl.Registry, store *store) *cardtmpl.RuntimeCatalog {
	t.Helper()
	catalog, err := cardtmpl.NewRuntimeCatalog(registry, store, cardtmpl.RuntimeCatalogConfig{
		MaxCacheEntries: 8, MaxCacheBytes: 8 << 20, CompileTimeout: 5 * time.Second,
		AuthorizeDynamic: func(context.Context, cardtmpl.CatalogAccess, cardtmpl.RuntimeArtifactMeta) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog: %v", err)
	}
	return catalog
}

func integrationArtifactForVersion(t *testing.T, version string) *cardtmpl.CompiledArtifact {
	t.Helper()
	root, err := cardtmpl.DecodeStrictJSONObject(validControlRequestJSON(t, false))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := cardtmpl.DecodeBundleValue(root["bundle"])
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(bundle.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["version"] = version
	bundle.Manifest, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := cardtmpl.CompileJSONArtifact(context.Background(), bundle, cardtmpl.DefaultCompileLimits())
	if err != nil {
		t.Fatalf("CompileJSONArtifact %s: %v", version, err)
	}
	return artifact
}

func receiptForIntegrationArtifact(artifact *cardtmpl.CompiledArtifact) stateTargetReceipt {
	return stateTargetReceipt{meta: cardtmpl.RuntimeArtifactMeta{
		ID: artifact.Meta.ID, Version: artifact.Meta.Version, Source: cardtmpl.RuntimeSourceDynamic,
		Engine: artifact.Engine, Hash: artifact.Hash, Owner: artifact.Owner,
		Visibility: artifact.Visibility, Protocol: artifact.Meta.Protocol,
		ContractVersion: artifact.ContractVersion,
	}}
}

func assertCatalogDefaultVersion(
	t *testing.T,
	catalog *cardtmpl.RuntimeCatalog,
	request cardtmpl.CatalogDefaultRequest,
	want string,
) {
	t.Helper()
	meta, err := catalog.MetaDefault(context.Background(), request)
	if err != nil {
		t.Fatalf("MetaDefault want %s: %v", want, err)
	}
	if meta.Version != want {
		t.Fatalf("MetaDefault version=%s, want %s", meta.Version, want)
	}
}
