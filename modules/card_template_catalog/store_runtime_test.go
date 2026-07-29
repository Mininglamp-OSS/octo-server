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

func TestStoreLoadActivationDistinguishesAbsentActiveAndDisabled(t *testing.T) {
	tests := []struct {
		name string
		rows *sqlmock.Rows
		want cardtmpl.RuntimeActivation
	}{
		{name: "absent", rows: sqlmock.NewRows([]string{"active_version", "status", "revision"}), want: cardtmpl.RuntimeActivation{}},
		{name: "active", rows: sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.1.0", "active", 7), want: cardtmpl.RuntimeActivation{
			Exists: true, Version: "1.1.0", Status: cardtmpl.RuntimeActivationActive, Revision: 7,
		}},
		{name: "disabled", rows: sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow(nil, "disabled", 8), want: cardtmpl.RuntimeActivation{
			Exists: true, Status: cardtmpl.RuntimeActivationDisabled, Revision: 8,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, mock, closeDB := newMockStore(t)
			defer closeDB()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
				WithArgs("test.runtime-card").WillReturnRows(test.rows)

			got, err := store.LoadActivation(context.Background(), "test.runtime-card")
			if err != nil {
				t.Fatalf("LoadActivation: %v", err)
			}
			if got != test.want {
				t.Fatalf("LoadActivation = %+v, want %+v", got, test.want)
			}
			assertMockExpectations(t, mock)
		})
	}
}

func TestStoreLoadActivationRejectsMalformedShape(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT active_version, status, revision")).
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow(nil, "active", 1))

	_, err := store.LoadActivation(context.Background(), "test.runtime-card")
	if !errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
		t.Fatalf("LoadActivation error = %v, want ErrRuntimeCatalogIntegrity", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreLoadArtifactMetaReturnsAuthoritativeDynamicFields(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	hash := strings.Repeat("a", 64)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.owner, a.visibility, a.engine_contract")).
		WithArgs("test.runtime-card", "1.1.0").
		WillReturnRows(sqlmock.NewRows([]string{
			"source", "owner", "visibility", "engine_contract", "protocol", "contract_version", "content_sha256", "blocked",
		}).AddRow("dynamic", "ai", "private", cardtmpl.JSONTemplateEngineV1, cardtmpl.Protocol, "1.0.0", hash, true))

	got, err := store.LoadArtifactMeta(context.Background(), "test.runtime-card", "1.1.0")
	if err != nil {
		t.Fatalf("LoadArtifactMeta: %v", err)
	}
	want := cardtmpl.RuntimeArtifactMeta{
		ID: "test.runtime-card", Version: "1.1.0", Source: cardtmpl.RuntimeSourceDynamic,
		Owner: "ai", Visibility: "private", Engine: cardtmpl.JSONTemplateEngineV1,
		Protocol: cardtmpl.Protocol, ContractVersion: "1.0.0", Hash: hash, Blocked: true,
	}
	if got != want {
		t.Fatalf("LoadArtifactMeta = %+v, want %+v", got, want)
	}
	assertMockExpectations(t, mock)
}

func TestStoreLoadArtifactMetaClassifiesMissingAndCorruptRows(t *testing.T) {
	t.Run("unknown claim", func(t *testing.T) {
		store, mock, closeDB := newMockStore(t)
		defer closeDB()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.owner, a.visibility, a.engine_contract")).
			WillReturnError(sql.ErrNoRows)
		_, err := store.LoadArtifactMeta(context.Background(), "test.unknown", "1.0.0")
		if !errors.Is(err, cardtmpl.ErrTemplateUnknown) {
			t.Fatalf("error = %v, want ErrTemplateUnknown", err)
		}
		assertMockExpectations(t, mock)
	})

	t.Run("dynamic claim without artifact", func(t *testing.T) {
		store, mock, closeDB := newMockStore(t)
		defer closeDB()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT c.source, a.owner, a.visibility, a.engine_contract")).
			WillReturnRows(sqlmock.NewRows([]string{
				"source", "owner", "visibility", "engine_contract", "protocol", "contract_version", "content_sha256", "blocked",
			}).AddRow("dynamic", nil, nil, nil, nil, nil, nil, false))
		_, err := store.LoadArtifactMeta(context.Background(), "test.runtime-card", "1.0.0")
		if !errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
			t.Fatalf("error = %v, want ErrRuntimeCatalogIntegrity", err)
		}
		assertMockExpectations(t, mock)
	})
}

func TestStoreLoadArtifactBundleRequiresExactIdentityAndHash(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	hash := strings.Repeat("b", 64)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT canonical_bundle FROM card_template_artifact")).
		WithArgs("test.runtime-card", "1.1.0", hash).
		WillReturnRows(sqlmock.NewRows([]string{"canonical_bundle"}).AddRow([]byte(`{"catalog":{}}`)))

	got, err := store.LoadArtifactBundle(context.Background(), "test.runtime-card", "1.1.0", hash)
	if err != nil {
		t.Fatalf("LoadArtifactBundle: %v", err)
	}
	if string(got) != `{"catalog":{}}` {
		t.Fatalf("bundle = %q", got)
	}
	assertMockExpectations(t, mock)
}

func TestStoreLoadArtifactBundleMissingRowIsIntegrityFailure(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT canonical_bundle FROM card_template_artifact")).
		WillReturnError(sql.ErrNoRows)

	_, err := store.LoadArtifactBundle(context.Background(), "test.runtime-card", "1.1.0", strings.Repeat("c", 64))
	if !errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
		t.Fatalf("error = %v, want ErrRuntimeCatalogIntegrity", err)
	}
	assertMockExpectations(t, mock)
}
