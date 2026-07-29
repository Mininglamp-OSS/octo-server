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
)

func TestStateTargetReceiptCoversStaticAndCorruptSourceCases(t *testing.T) {
	staticReceipt := stateTargetReceipt{meta: cardtmpl.RuntimeArtifactMeta{
		ID: "test.static", Version: "1.0.0", Source: cardtmpl.RuntimeSourceStatic,
	}}
	if err := requireStateTargetReceipt(stateTarget{source: claimSourceStatic}, staticReceipt); err != nil {
		t.Fatalf("valid static receipt: %v", err)
	}
	if err := requireStateTargetReceipt(stateTarget{source: claimSourceStatic}, stateTargetReceipt{}); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("missing receipt error=%v, want ErrCatalogIntegrity", err)
	}
	wrongSource := staticReceipt
	wrongSource.meta.Source = cardtmpl.RuntimeSourceDynamic
	if err := requireStateTargetReceipt(stateTarget{source: claimSourceStatic}, wrongSource); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("wrong static source error=%v, want ErrCatalogIntegrity", err)
	}
	if err := requireStateTargetReceipt(stateTarget{
		source: claimSourceStatic, meta: cardtmpl.RuntimeArtifactMeta{ID: "corrupt", Version: "1.0.0"},
	}, staticReceipt); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("static metadata error=%v, want ErrCatalogIntegrity", err)
	}
	if err := requireStateTargetReceipt(stateTarget{source: "unknown"}, staticReceipt); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("unknown source error=%v, want ErrCatalogIntegrity", err)
	}
}

func TestStateRequestValidationRequiresBoundedReceipts(t *testing.T) {
	valid := activationRequestFixture()
	if err := validateActivationRequest(valid); err != nil {
		t.Fatalf("valid activation request: %v", err)
	}
	missingReceipt := valid
	missingReceipt.targetReceipt = stateTargetReceipt{}
	if err := validateActivationRequest(missingReceipt); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("missing activation receipt error=%v, want ErrCatalogIntegrity", err)
	}
	invalidText := valid
	invalidText.Reason = " padded "
	if err := validateActivationRequest(invalidText); !errors.Is(err, ErrPublishRequestInvalid) {
		t.Fatalf("invalid activation text error=%v, want ErrPublishRequestInvalid", err)
	}

	block := BlockRequest{
		TemplateID: "test.runtime-card", Version: "1.1.0", FallbackVersion: "1.0.0",
		ExpectedRevision: 2, ActorUID: "admin-1", Reason: "incident", ChangeTicket: "INC-1",
		fallbackReceipt: dynamicTargetReceiptFixture("test.runtime-card", "1.0.0", strings.Repeat("a", 64)),
	}
	if err := validateBlockRequest(block); err != nil {
		t.Fatalf("valid block request: %v", err)
	}
	block.fallbackReceipt = stateTargetReceipt{}
	if err := validateBlockRequest(block); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("missing fallback receipt error=%v, want ErrCatalogIntegrity", err)
	}
	block.FallbackVersion = block.Version
	if err := validateBlockRequest(block); !errors.Is(err, ErrPublishRequestInvalid) {
		t.Fatalf("self fallback error=%v, want ErrPublishRequestInvalid", err)
	}
}

func TestLoadStateTargetForUpdateRejectsPersistentCorruption(t *testing.T) {
	tests := []struct {
		name string
		rows *sqlmock.Rows
		err  error
	}{
		{name: "missing", err: sql.ErrNoRows},
		{name: "valid static", rows: stateTargetRows(claimSourceStatic, false, false, nil, nil, nil, nil, nil, nil)},
		{name: "static with artifact", rows: stateTargetRows(claimSourceStatic, true, false, "ai", "private", "engine", "protocol", "1.0.0", strings.Repeat("a", 64)), err: ErrCatalogIntegrity},
		{name: "dynamic incomplete", rows: stateTargetRows(claimSourceDynamic, true, false, nil, nil, nil, nil, nil, nil), err: ErrCatalogIntegrity},
		{name: "dynamic blocked", rows: dynamicStateTargetRowsWithBlocked(true), err: ErrArtifactBlocked},
		{name: "unknown source", rows: stateTargetRows("other", false, false, nil, nil, nil, nil, nil, nil), err: ErrCatalogIntegrity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, mock, closeDB := newMockStore(t)
			defer closeDB()
			mock.ExpectBegin()
			expect := mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.template_id IS NOT NULL, a.blocked_at IS NOT NULL")).
				WithArgs("test.runtime-card", "1.0.0")
			if test.rows != nil {
				expect.WillReturnRows(test.rows)
			} else {
				expect.WillReturnError(test.err)
			}
			mock.ExpectRollback()
			tx, err := store.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			target, gotErr := loadStateTargetForUpdate(context.Background(), tx, "test.runtime-card", "1.0.0")
			if test.name == "valid static" {
				if gotErr != nil || target.source != claimSourceStatic {
					t.Fatalf("target=%+v err=%v", target, gotErr)
				}
			} else {
				want := test.err
				if test.name == "missing" {
					want = ErrActivationTargetInvalid
				}
				if !errors.Is(gotErr, want) {
					t.Fatalf("error=%v, want %v", gotErr, want)
				}
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			assertMockExpectations(t, mock)
		})
	}
}

func stateTargetRows(source string, hasArtifact, blocked bool, owner, visibility, engine, protocol, contractVersion, hash any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"source", "has_artifact", "blocked", "owner", "visibility", "engine_contract",
		"protocol", "contract_version", "content_sha256",
	}).AddRow(source, hasArtifact, blocked, owner, visibility, engine, protocol, contractVersion, hash)
}

func dynamicStateTargetRowsWithBlocked(blocked bool) *sqlmock.Rows {
	return stateTargetRows(
		claimSourceDynamic, true, blocked, "ai", cardtmpl.CatalogVisibilityPrivate,
		cardtmpl.JSONTemplateEngineV1, cardtmpl.Protocol, "1.0.0", strings.Repeat("a", 64),
	)
}
