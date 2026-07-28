package card_template_catalog

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/go-sql-driver/mysql"
)

func TestStorePublishCreatesClaimArtifactAndAuditAtomically(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_version_claim")).
		WithArgs("test.runtime-card", "1.0.0", claimSourceDynamic).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_artifact")).
		WithArgs(
			"test.runtime-card", "1.0.0", "ai", "private", "octo-json-template/v1",
			"octo-card@1.0", "1.0.0", []byte(`{"canonical":true}`), strings.Repeat("a", 64), "admin-1",
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WithArgs("admin-1", auditOperationPublish, "test.runtime-card", "1.0.0", strings.Repeat("a", 64), "created", "reviewed", "CHG-1").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	result, err := store.Publish(context.Background(), publishRequestFixture())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !result.Created || result.Idempotent {
		t.Fatalf("result = %+v, want created", result)
	}
	assertMockExpectations(t, mock)
}

func TestStorePublishSameHashIsIdempotentAndAudited(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_version_claim")).
		WithArgs("test.runtime-card", "1.0.0", claimSourceDynamic).
		WillReturnError(&mysql.MySQLError{Number: 1062, Message: "duplicate"})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.content_sha256, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.0.0").
		WillReturnRows(sqlmock.NewRows([]string{"source", "content_sha256", "blocked"}).
			AddRow(claimSourceDynamic, strings.Repeat("a", 64), false))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WithArgs("admin-1", auditOperationPublish, "test.runtime-card", "1.0.0", strings.Repeat("a", 64), "idempotent", "reviewed", "CHG-1").
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()

	result, err := store.Publish(context.Background(), publishRequestFixture())
	if err != nil {
		t.Fatalf("Publish idempotent: %v", err)
	}
	if result.Created || !result.Idempotent || result.Blocked {
		t.Fatalf("result = %+v, want unblocked idempotent", result)
	}
	assertMockExpectations(t, mock)
}

func TestStorePublishClassifiesExistingClaimLookupErrors(t *testing.T) {
	tests := []struct {
		name          string
		lookupErr     error
		wantIntegrity bool
	}{
		{name: "claim disappeared", lookupErr: sql.ErrNoRows, wantIntegrity: true},
		{name: "request deadline", lookupErr: context.DeadlineExceeded, wantIntegrity: false},
		{name: "connection dropped", lookupErr: errors.New("connection dropped"), wantIntegrity: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, mock, closeDB := newMockStore(t)
			defer closeDB()

			mock.ExpectBegin()
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_version_claim")).
				WillReturnError(&mysql.MySQLError{Number: 1062, Message: "duplicate"})
			mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.content_sha256, a.blocked_at IS NOT NULL")).
				WillReturnError(tt.lookupErr)
			mock.ExpectRollback()

			_, err := store.Publish(context.Background(), publishRequestFixture())
			if got := errors.Is(err, ErrCatalogIntegrity); got != tt.wantIntegrity {
				t.Fatalf("errors.Is(ErrCatalogIntegrity) = %v, want %v; err=%v", got, tt.wantIntegrity, err)
			}
			if !tt.wantIntegrity && !errors.Is(err, tt.lookupErr) {
				t.Fatalf("lookup cause was not preserved: %v", err)
			}
			assertMockExpectations(t, mock)
		})
	}
}

func TestStorePublishRetriesTransientDuplicateResolutionFailure(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	deadlock := &mysql.MySQLError{Number: 1213, Message: "deadlock"}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_version_claim")).
		WillReturnError(&mysql.MySQLError{Number: 1062, Message: "duplicate"})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.content_sha256, a.blocked_at IS NOT NULL")).
		WillReturnError(deadlock)
	mock.ExpectRollback()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_version_claim")).
		WillReturnError(&mysql.MySQLError{Number: 1062, Message: "duplicate"})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.content_sha256, a.blocked_at IS NOT NULL")).
		WillReturnRows(sqlmock.NewRows([]string{"source", "content_sha256", "blocked"}).
			AddRow(claimSourceDynamic, strings.Repeat("a", 64), false))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()

	result, err := store.Publish(context.Background(), publishRequestFixture())
	if err != nil {
		t.Fatalf("Publish after transient deadlock: %v", err)
	}
	if !result.Idempotent {
		t.Fatalf("result = %+v, want idempotent", result)
	}
	assertMockExpectations(t, mock)
}

func TestStorePublishRejectsImmutableAndStaticConflicts(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		storedHash any
		want       error
	}{
		{name: "different dynamic hash", source: claimSourceDynamic, storedHash: strings.Repeat("b", 64), want: ErrImmutableVersionConflict},
		{name: "static claim", source: claimSourceStatic, storedHash: nil, want: ErrVersionClaimConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, mock, closeDB := newMockStore(t)
			defer closeDB()

			mock.ExpectBegin()
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_version_claim")).
				WithArgs("test.runtime-card", "1.0.0", claimSourceDynamic).
				WillReturnError(&mysql.MySQLError{Number: 1062, Message: "duplicate"})
			mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.content_sha256, a.blocked_at IS NOT NULL")).
				WithArgs("test.runtime-card", "1.0.0").
				WillReturnRows(sqlmock.NewRows([]string{"source", "content_sha256", "blocked"}).
					AddRow(tt.source, tt.storedHash, false))
			mock.ExpectRollback()

			_, err := store.Publish(context.Background(), publishRequestFixture())
			if !errors.Is(err, tt.want) {
				t.Fatalf("Publish error = %v, want %v", err, tt.want)
			}
			assertMockExpectations(t, mock)
		})
	}
}

func TestStorePublishRollsBackWhenAuditFails(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_version_claim")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_artifact")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WillReturnError(errors.New("audit unavailable"))
	mock.ExpectRollback()

	if _, err := store.Publish(context.Background(), publishRequestFixture()); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("Publish error = %v, want audit failure", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreReconcileStaticInventoryClaimsExactVersions(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectExec(staticClaimUpsertPattern).
		WithArgs("ai.reasoning-process", "0.2.0", claimSourceStatic).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, EXISTS(SELECT 1 FROM card_template_artifact")).
		WithArgs("ai.reasoning-process", "0.2.0").
		WillReturnRows(sqlmock.NewRows([]string{"source", "has_artifact"}).AddRow(claimSourceStatic, false))
	mock.ExpectCommit()

	err := store.ReconcileStatic(context.Background(), []cardtmpl.TemplateMeta{{
		ID: "ai.reasoning-process", Version: "0.2.0",
	}})
	if err != nil {
		t.Fatalf("ReconcileStatic: %v", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreReconcileStaticRejectsDynamicClaim(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectExec(staticClaimUpsertPattern).
		WithArgs("ai.reasoning-process", "0.2.0", claimSourceStatic).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, EXISTS(SELECT 1 FROM card_template_artifact")).
		WithArgs("ai.reasoning-process", "0.2.0").
		WillReturnRows(sqlmock.NewRows([]string{"source", "has_artifact"}).AddRow(claimSourceDynamic, true))
	mock.ExpectRollback()

	err := store.ReconcileStatic(context.Background(), []cardtmpl.TemplateMeta{{
		ID: "ai.reasoning-process", Version: "0.2.0",
	}})
	if !errors.Is(err, ErrVersionClaimConflict) {
		t.Fatalf("ReconcileStatic error = %v, want ErrVersionClaimConflict", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreReconcileStaticAcceptsExistingStaticClaim(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectExec(staticClaimUpsertPattern).
		WithArgs("ai.reasoning-process", "0.2.0", claimSourceStatic).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, EXISTS(SELECT 1 FROM card_template_artifact")).
		WithArgs("ai.reasoning-process", "0.2.0").
		WillReturnRows(sqlmock.NewRows([]string{"source", "has_artifact"}).AddRow(claimSourceStatic, false))
	mock.ExpectCommit()

	err := store.ReconcileStatic(context.Background(), []cardtmpl.TemplateMeta{{
		ID: "ai.reasoning-process", Version: "0.2.0",
	}})
	if err != nil {
		t.Fatalf("ReconcileStatic existing claim: %v", err)
	}
	assertMockExpectations(t, mock)
}

func TestValidatePublishRequestRejectsMalformedInputs(t *testing.T) {
	valid := publishRequestFixture()
	if err := validatePublishRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*PublishRequest)
	}{
		{name: "nil artifact", mutate: func(request *PublishRequest) { request.Artifact = nil }},
		{name: "unknown engine", mutate: func(request *PublishRequest) { request.Artifact.Engine = "unknown/v1" }},
		{name: "bad hash", mutate: func(request *PublishRequest) { request.Artifact.Hash = "not-sha256" }},
		{name: "uppercase hash", mutate: func(request *PublishRequest) { request.Artifact.Hash = strings.Repeat("A", 64) }},
		{name: "empty bundle", mutate: func(request *PublishRequest) { request.Artifact.Bundle = nil }},
		{name: "missing actor", mutate: func(request *PublishRequest) { request.ActorUID = "" }},
		{name: "untrimmed reason", mutate: func(request *PublishRequest) { request.Reason = " reviewed " }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := publishRequestFixture()
			artifactCopy := *request.Artifact
			request.Artifact = &artifactCopy
			tt.mutate(&request)
			if err := validatePublishRequest(request); !errors.Is(err, ErrPublishRequestInvalid) {
				t.Fatalf("error = %v, want ErrPublishRequestInvalid", err)
			}
		})
	}
}

func TestStorePublishReturnsTransactionErrors(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		store, mock, closeDB := newMockStore(t)
		defer closeDB()
		mock.ExpectBegin().WillReturnError(errors.New("begin unavailable"))
		if _, err := store.Publish(context.Background(), publishRequestFixture()); err == nil || !strings.Contains(err.Error(), "begin") {
			t.Fatalf("error = %v", err)
		}
		assertMockExpectations(t, mock)
	})

	t.Run("commit", func(t *testing.T) {
		store, mock, closeDB := newMockStore(t)
		defer closeDB()
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_version_claim")).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_artifact")).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit().WillReturnError(errors.New("commit unavailable"))
		if _, err := store.Publish(context.Background(), publishRequestFixture()); err == nil || !strings.Contains(err.Error(), "commit") {
			t.Fatalf("error = %v", err)
		}
		assertMockExpectations(t, mock)
	})
}

func TestCatalogMigrationDefinesImmutableCaseSensitiveTables(t *testing.T) {
	raw, err := sqlFS.ReadFile("sql/20260728000001_card_template_catalog.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sqlText := string(raw)
	for _, required := range []string{
		"CREATE TABLE card_template_version_claim",
		"CREATE TABLE card_template_artifact",
		"CREATE TABLE card_template_audit",
		"COLLATE ascii_bin",
		"MEDIUMBLOB NOT NULL",
		"FOREIGN KEY (template_id, version)",
	} {
		if !strings.Contains(sqlText, required) {
			t.Errorf("migration missing %q", required)
		}
	}
	if strings.Contains(strings.ToUpper(sqlText), "ON UPDATE CASCADE") {
		t.Fatal("immutable identity must not have ON UPDATE CASCADE")
	}
}

func newMockStore(t *testing.T) (*store, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return newStore(rawDB), mock, func() { _ = rawDB.Close() }
}

func publishRequestFixture() PublishRequest {
	return PublishRequest{
		Artifact: &cardtmpl.CompiledArtifact{
			Meta: cardtmpl.TemplateMeta{
				ID: "test.runtime-card", Version: "1.0.0", Protocol: cardtmpl.Protocol,
			},
			Hash:            strings.Repeat("a", 64),
			Bundle:          cardtmpl.CanonicalBundle(`{"canonical":true}`),
			Engine:          cardtmpl.JSONTemplateEngineV1,
			Visibility:      cardtmpl.CatalogVisibilityPrivate,
			Owner:           "ai",
			ContractVersion: "1.0.0",
		},
		ActorUID:     "admin-1",
		Reason:       "reviewed",
		ChangeTicket: "CHG-1",
	}
}

func assertMockExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

const staticClaimUpsertPattern = `INSERT INTO card_template_version_claim[\s\S]+ON DUPLICATE KEY UPDATE source = source`
