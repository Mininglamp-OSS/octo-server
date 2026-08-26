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

func TestStoreActivateRejects129thActiveTargetRealMySQL(t *testing.T) {
	db := newRuntimeCatalogIntegrationDB(t)
	seedActiveTargets(t, db, startupActiveTargetLimit)
	candidate := cardtmpl.ID("capacity.candidate-129")
	insertStaticCapacityClaim(t, db, candidate)

	_, err := newStore(db).Activate(context.Background(), capacityActivationRequest(candidate))
	if !errors.Is(err, ErrActiveTargetCapacity) {
		t.Fatalf("129th Activate error=%v, want typed capacity conflict", err)
	}
	assertActiveTargetCount(t, db, startupActiveTargetLimit)
	assertStateAuditCount(t, db, []cardtmpl.ID{candidate}, 0)
}

func TestStoreRollbackRejectsDisabledTargetAtCapacityRealMySQL(t *testing.T) {
	db := newRuntimeCatalogIntegrationDB(t)
	seedActiveTargets(t, db, startupActiveTargetLimit)
	candidate := cardtmpl.ID("capacity.rollback-disabled")
	insertStaticCapacityClaim(t, db, candidate)
	if _, err := db.Exec(`INSERT INTO card_template_activation
        (template_id, active_version, status, revision, updated_by, reason, change_ticket)
        VALUES (?, NULL, 'disabled', 2, 'admin-1', 'disabled after incident', 'CAP-ROLLBACK')`, candidate); err != nil {
		t.Fatalf("insert disabled activation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO card_template_audit
        (actor_uid, operation, template_id, version, resulting_version,
         previous_revision, resulting_revision, result, reason, change_ticket)
        VALUES ('admin-1', 'activate', ?, '1.0.0', '1.0.0', 0, 1, 'ok',
                'prior activation proof', 'CAP-ROLLBACK')`, candidate); err != nil {
		t.Fatalf("insert prior-active audit: %v", err)
	}

	_, err := newStore(db).Rollback(context.Background(), RollbackRequest{
		TemplateID: candidate, TargetVersion: "1.0.0", ExpectedRevision: 2,
		ActorUID: "admin-1", Reason: "restore disabled target", ChangeTicket: "CAP-ROLLBACK",
		targetReceipt: staticCapacityReceipt(candidate),
	})
	if !errors.Is(err, ErrActiveTargetCapacity) {
		t.Fatalf("disabled-to-active Rollback error=%v, want typed capacity conflict", err)
	}
	assertActiveTargetCount(t, db, startupActiveTargetLimit)
	assertStateAuditCount(t, db, []cardtmpl.ID{candidate}, 1)
}

func TestStoreConcurrentActivationsShareLastCapacitySlotRealMySQL(t *testing.T) {
	db := newRuntimeCatalogIntegrationDB(t)
	seedActiveTargets(t, db, startupActiveTargetLimit-1)
	candidates := []cardtmpl.ID{"capacity.concurrent-a", "capacity.concurrent-b"}
	for _, candidate := range candidates {
		insertStaticCapacityClaim(t, db, candidate)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	outcomes := make(chan error, len(candidates))
	var callers sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			_, err := newStore(db).Activate(ctx, capacityActivationRequest(candidate))
			outcomes <- err
		}()
	}
	close(start)
	callers.Wait()
	close(outcomes)

	successes, conflicts := 0, 0
	for err := range outcomes {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrActiveTargetCapacity):
			conflicts++
		default:
			t.Fatalf("concurrent capacity activation error=%v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent last-slot successes/conflicts=%d/%d, want 1/1", successes, conflicts)
	}
	assertActiveTargetCount(t, db, startupActiveTargetLimit)
	assertStateAuditCount(t, db, candidates, 1)
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
	// Derive the expected count from the source rather than hardcoding it. A
	// hardcoded 2 meant that adding the grant migration broke four unrelated
	// tests with a message about migration counts — and only on a machine with
	// MySQL, so the breakage was invisible everywhere else.
	migrations, err := source.FindMigrations()
	if err != nil {
		_ = db.Close()
		_ = bootstrap.Close()
		t.Fatalf("find catalog migrations: %v", err)
	}
	if applied, err := migrate.Exec(db, "mysql", source, migrate.Up); err != nil || applied != len(migrations) {
		_ = db.Close()
		_ = bootstrap.Close()
		t.Fatalf("apply catalog migrations: applied=%d want=%d err=%v", applied, len(migrations), err)
	}
	t.Cleanup(func() {
		if reverted, err := migrate.Exec(db, "mysql", source, migrate.Down); err != nil || reverted != len(migrations) {
			t.Errorf("revert catalog migrations: reverted=%d want=%d err=%v",
				reverted, len(migrations), err)
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

func seedActiveTargets(t *testing.T, db *sql.DB, count int) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin active-target seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("capacity.seed-%03d", index)
		if _, err := tx.Exec(`INSERT INTO card_template_version_claim
            (template_id, version, source) VALUES (?, '1.0.0', 'static')`, id); err != nil {
			t.Fatalf("insert active-target claim %s: %v", id, err)
		}
		if _, err := tx.Exec(`INSERT INTO card_template_activation
            (template_id, active_version, status, revision, updated_by, reason, change_ticket)
            VALUES (?, '1.0.0', 'active', 1, 'capacity-seed', 'capacity fixture', 'CAP-SEED')`, id); err != nil {
			t.Fatalf("insert active-target activation %s: %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit active-target seed: %v", err)
	}
}

func insertStaticCapacityClaim(t *testing.T, db *sql.DB, id cardtmpl.ID) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO card_template_version_claim
        (template_id, version, source) VALUES (?, '1.0.0', 'static')`, id); err != nil {
		t.Fatalf("insert capacity candidate %s: %v", id, err)
	}
}

func capacityActivationRequest(id cardtmpl.ID) ActivationRequest {
	return ActivationRequest{
		TemplateID: id, TargetVersion: "1.0.0", ExpectedRevision: 0,
		ActorUID: "admin-1", Reason: "capacity test activation", ChangeTicket: "CAP-ACTIVATE",
		targetReceipt: staticCapacityReceipt(id),
	}
}

func staticCapacityReceipt(id cardtmpl.ID) stateTargetReceipt {
	return stateTargetReceipt{meta: cardtmpl.RuntimeArtifactMeta{
		ID: id, Version: "1.0.0", Source: cardtmpl.RuntimeSourceStatic,
	}}
}

func assertActiveTargetCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM card_template_activation WHERE status = 'active'`).Scan(&got); err != nil {
		t.Fatalf("count active targets: %v", err)
	}
	if got != want {
		t.Fatalf("active target count=%d, want %d", got, want)
	}
}

func assertStateAuditCount(t *testing.T, db *sql.DB, ids []cardtmpl.ID, want int) {
	t.Helper()
	if len(ids) == 0 || len(ids) > 2 {
		t.Fatalf("unsupported audit identity count %d", len(ids))
	}
	query := `SELECT COUNT(*) FROM card_template_audit WHERE template_id = ?`
	args := []any{ids[0]}
	if len(ids) == 2 {
		query += ` OR template_id = ?`
		args = append(args, ids[1])
	}
	var got int
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("count state audits: %v", err)
	}
	if got != want {
		t.Fatalf("state audit count=%d, want %d", got, want)
	}
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
		AuthorizeDynamic: func(context.Context, cardtmpl.CatalogAccess, cardtmpl.RuntimeArtifactMeta,
			cardtmpl.RuntimeGrant) error {
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
