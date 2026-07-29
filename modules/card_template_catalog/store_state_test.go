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

func TestStoreActivateCreatesFirstRevisionAndAuditAtomically(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WithArgs("test.runtime-card").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.0.0").
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_activation")).
		WithArgs("test.runtime-card", "1.0.0", activationStatusActive, "admin-1", "activate reviewed artifact", "CHG-2").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	result, err := store.Activate(context.Background(), ActivationRequest{
		TemplateID:       "test.runtime-card",
		TargetVersion:    "1.0.0",
		ExpectedRevision: 0,
		ActorUID:         "admin-1",
		Reason:           "activate reviewed artifact",
		ChangeTicket:     "CHG-2",
		targetReceipt:    dynamicTargetReceiptFixture("test.runtime-card", "1.0.0", strings.Repeat("a", 64)),
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if result.Status != activationStatusActive || result.ActiveVersion != "1.0.0" || result.Revision != 1 {
		t.Fatalf("Activate result = %+v, want active 1.0.0 revision 1", result)
	}
	assertMockExpectations(t, mock)
}

func TestStoreActivateRejectsStaleRevisionBeforeTargetMutation(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WithArgs("test.runtime-card").
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.0.0", activationStatusActive, 2))
	mock.ExpectRollback()

	_, err := store.Activate(context.Background(), ActivationRequest{
		TemplateID:       "test.runtime-card",
		TargetVersion:    "1.1.0",
		ExpectedRevision: 1,
		ActorUID:         "admin-1",
		Reason:           "stale request",
		ChangeTicket:     "CHG-3",
		targetReceipt:    dynamicTargetReceiptFixture("test.runtime-card", "1.1.0", strings.Repeat("a", 64)),
	})
	if !errors.Is(err, ErrActivationConflict) {
		t.Fatalf("Activate error = %v, want ErrActivationConflict", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreActivateRollsBackStateWhenAuditFails(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WithArgs("test.runtime-card").
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.0.0", activationStatusActive, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.1.0").
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE card_template_activation")).
		WithArgs("1.1.0", activationStatusActive, "admin-1", "activate next version", "CHG-4", "test.runtime-card", 1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WillReturnError(errors.New("audit unavailable"))
	mock.ExpectRollback()

	_, err := store.Activate(context.Background(), ActivationRequest{
		TemplateID:       "test.runtime-card",
		TargetVersion:    "1.1.0",
		ExpectedRevision: 1,
		ActorUID:         "admin-1",
		Reason:           "activate next version",
		ChangeTicket:     "CHG-4",
		targetReceipt:    dynamicTargetReceiptFixture("test.runtime-card", "1.1.0", strings.Repeat("a", 64)),
	})
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("Activate error = %v, want audit failure", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreActivateRejectsArtifactMetadataChangedAfterPrevalidation(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WithArgs("test.runtime-card").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.0.0").
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("b", 64)))
	mock.ExpectRollback()

	request := activationRequestFixture()
	request.targetReceipt = dynamicTargetReceiptFixture("test.runtime-card", "1.0.0", strings.Repeat("a", 64))
	_, err := store.Activate(context.Background(), request)
	if !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("Activate error = %v, want ErrCatalogIntegrity", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreBlockCurrentVersionWithoutFallbackDisablesAtomically(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WithArgs("test.runtime-card").
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.1.0", activationStatusActive, 3))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.1.0").
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE card_template_artifact")).
		WithArgs("admin-1", "security incident", "test.runtime-card", "1.1.0").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE card_template_activation")).
		WithArgs(nil, activationStatusDisabled, "admin-1", "security incident", "SEC-1", "test.runtime-card", 3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	result, err := store.Block(context.Background(), BlockRequest{
		TemplateID:       "test.runtime-card",
		Version:          "1.1.0",
		ExpectedRevision: 3,
		ActorUID:         "admin-1",
		Reason:           "security incident",
		ChangeTicket:     "SEC-1",
	})
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if !result.Blocked || result.Status != activationStatusDisabled || result.Revision != 4 || result.ActiveVersion != "" {
		t.Fatalf("Block result = %+v, want blocked disabled revision 4", result)
	}
	assertMockExpectations(t, mock)
}

func TestStoreRollbackRequiresPriorActiveTargetAndAuditsTransition(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WithArgs("test.runtime-card").
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.1.0", activationStatusActive, 4))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.0.0").
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM card_template_audit")).
		WithArgs("test.runtime-card", "1.0.0", "1.0.0").
		WillReturnRows(sqlmock.NewRows([]string{"previously_active"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE card_template_activation")).
		WithArgs("1.0.0", activationStatusActive, "admin-1", "rollback incident", "INC-1", "test.runtime-card", 4).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	result, err := store.Rollback(context.Background(), RollbackRequest{
		TemplateID:       "test.runtime-card",
		TargetVersion:    "1.0.0",
		ExpectedRevision: 4,
		ActorUID:         "admin-1",
		Reason:           "rollback incident",
		ChangeTicket:     "INC-1",
		targetReceipt:    dynamicTargetReceiptFixture("test.runtime-card", "1.0.0", strings.Repeat("a", 64)),
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if result.ActiveVersion != "1.0.0" || result.Revision != 5 || result.Status != activationStatusActive {
		t.Fatalf("Rollback result = %+v, want active 1.0.0 revision 5", result)
	}
	assertMockExpectations(t, mock)
}

func TestStoreRollbackRejectsNeverActiveTarget(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.1.0", activationStatusActive, 4))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM card_template_audit")).
		WillReturnRows(sqlmock.NewRows([]string{"previously_active"}).AddRow(false))
	mock.ExpectRollback()

	_, err := store.Rollback(context.Background(), RollbackRequest{
		TemplateID:       "test.runtime-card",
		TargetVersion:    "0.9.0",
		ExpectedRevision: 4,
		ActorUID:         "admin-1",
		Reason:           "invalid rollback",
		ChangeTicket:     "INC-2",
		targetReceipt:    dynamicTargetReceiptFixture("test.runtime-card", "0.9.0", strings.Repeat("a", 64)),
	})
	if !errors.Is(err, ErrRollbackTargetInvalid) {
		t.Fatalf("Rollback error = %v, want ErrRollbackTargetInvalid", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreBlockCurrentVersionSwitchesToFallbackAtomically(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.1.0", activationStatusActive, 3))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.1.0").
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE card_template_artifact")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.0.0").
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM card_template_audit")).
		WithArgs("test.runtime-card", "1.0.0", "1.0.0").
		WillReturnRows(sqlmock.NewRows([]string{"previously_active"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE card_template_activation")).
		WithArgs("1.0.0", activationStatusActive, "admin-1", "block with fallback", "SEC-2", "test.runtime-card", 3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	result, err := store.Block(context.Background(), BlockRequest{
		TemplateID:       "test.runtime-card",
		Version:          "1.1.0",
		FallbackVersion:  "1.0.0",
		ExpectedRevision: 3,
		ActorUID:         "admin-1",
		Reason:           "block with fallback",
		ChangeTicket:     "SEC-2",
		fallbackReceipt:  dynamicTargetReceiptFixture("test.runtime-card", "1.0.0", strings.Repeat("a", 64)),
	})
	if err != nil {
		t.Fatalf("Block with fallback: %v", err)
	}
	if result.ActiveVersion != "1.0.0" || result.Status != activationStatusActive || result.Revision != 4 {
		t.Fatalf("Block result = %+v, want fallback 1.0.0 revision 4", result)
	}
	assertMockExpectations(t, mock)
}

func TestStoreBlockRejectsNeverActiveDynamicFallbackAtomically(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.1.0", activationStatusActive, 3))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.1.0").
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE card_template_artifact")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WithArgs("test.runtime-card", "1.0.0").
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM card_template_audit")).
		WithArgs("test.runtime-card", "1.0.0", "1.0.0").
		WillReturnRows(sqlmock.NewRows([]string{"previously_active"}).AddRow(false))
	mock.ExpectRollback()

	_, err := store.Block(context.Background(), BlockRequest{
		TemplateID: "test.runtime-card", Version: "1.1.0", FallbackVersion: "1.0.0",
		ExpectedRevision: 3, ActorUID: "admin-1", Reason: "block with bad fallback", ChangeTicket: "SEC-4",
		fallbackReceipt: dynamicTargetReceiptFixture("test.runtime-card", "1.0.0", strings.Repeat("a", 64)),
	})
	if !errors.Is(err, ErrBlockTargetInvalid) {
		t.Fatalf("Block error = %v, want ErrBlockTargetInvalid", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreBlockNonActiveVersionPreservesActivationRevision(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.1.0", activationStatusActive, 3))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE card_template_artifact")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	result, err := store.Block(context.Background(), BlockRequest{
		TemplateID:       "test.runtime-card",
		Version:          "1.0.0",
		ExpectedRevision: 3,
		ActorUID:         "admin-1",
		Reason:           "block old version",
		ChangeTicket:     "SEC-3",
	})
	if err != nil {
		t.Fatalf("Block non-active: %v", err)
	}
	if result.ActiveVersion != "1.1.0" || result.Revision != 3 {
		t.Fatalf("Block result = %+v, want unchanged active revision", result)
	}
	assertMockExpectations(t, mock)
}

func TestStoreActivateMapsConcurrentFirstInsertToConflict(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_activation")).
		WillReturnError(&mysql.MySQLError{Number: 1062, Message: "duplicate"})
	mock.ExpectRollback()

	_, err := store.Activate(context.Background(), activationRequestFixture())
	if !errors.Is(err, ErrActivationConflict) {
		t.Fatalf("Activate error = %v, want ErrActivationConflict", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreActivateRetriesTransientTransactionFailure(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin().WillReturnError(&mysql.MySQLError{Number: 1213, Message: "deadlock"})
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
		WillReturnRows(dynamicStateTargetRows(strings.Repeat("a", 64)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_activation")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO card_template_audit")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	result, err := store.Activate(context.Background(), activationRequestFixture())
	if err != nil {
		t.Fatalf("Activate after deadlock: %v", err)
	}
	if result.Revision != 1 {
		t.Fatalf("result = %+v", result)
	}
	assertMockExpectations(t, mock)
}

func TestActivationMigrationDefinesCASStateAndAuditColumns(t *testing.T) {
	raw, err := sqlFS.ReadFile("sql/20260728000002_card_template_catalog_activation.sql")
	if err != nil {
		t.Fatalf("read activation migration: %v", err)
	}
	sqlText := string(raw)
	for _, required := range []string{
		"CREATE TABLE card_template_activation",
		"active_version",
		"status",
		"revision BIGINT UNSIGNED NOT NULL",
		"KEY idx_card_template_activation_status (status, template_id)",
		"FOREIGN KEY (template_id, active_version)",
		"ADD COLUMN previous_version",
		"ADD COLUMN resulting_version",
		"ADD COLUMN previous_revision",
		"ADD COLUMN resulting_revision",
		"ADD KEY idx_card_template_audit_page (template_id, id)",
		"ADD KEY idx_card_template_audit_previous (template_id, result, operation, previous_version)",
		"ADD KEY idx_card_template_audit_resulting (template_id, result, operation, resulting_version)",
	} {
		if !strings.Contains(sqlText, required) {
			t.Errorf("activation migration missing %q", required)
		}
	}
	if strings.Contains(strings.ToUpper(sqlText), "ON DELETE CASCADE") {
		t.Fatal("activation must not cascade-delete immutable claims")
	}
}

func activationRequestFixture() ActivationRequest {
	return ActivationRequest{
		TemplateID:       cardtmpl.ID("test.runtime-card"),
		TargetVersion:    "1.0.0",
		ExpectedRevision: 0,
		ActorUID:         "admin-1",
		Reason:           "activate reviewed artifact",
		ChangeTicket:     "CHG-2",
		targetReceipt:    dynamicTargetReceiptFixture("test.runtime-card", "1.0.0", strings.Repeat("a", 64)),
	}
}

func dynamicStateTargetRows(hash string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"source", "has_artifact", "blocked", "owner", "visibility", "engine_contract",
		"protocol", "contract_version", "content_sha256",
	}).AddRow(
		claimSourceDynamic, true, false, "ai", cardtmpl.CatalogVisibilityPrivate,
		cardtmpl.JSONTemplateEngineV1, cardtmpl.Protocol, "1.0.0", hash,
	)
}

func dynamicTargetReceiptFixture(id cardtmpl.ID, version, hash string) stateTargetReceipt {
	return stateTargetReceipt{meta: cardtmpl.RuntimeArtifactMeta{
		ID: id, Version: version, Source: cardtmpl.RuntimeSourceDynamic,
		Owner: "ai", Visibility: cardtmpl.CatalogVisibilityPrivate,
		Engine: cardtmpl.JSONTemplateEngineV1, Protocol: cardtmpl.Protocol,
		ContractVersion: "1.0.0", Hash: hash,
	}}
}
