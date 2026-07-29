package card_template_catalog

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestStoreListActiveTargetsReturnsOnlyValidatedIdentities(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	mock.ExpectQuery("SELECT template_id, active_version").
		WillReturnRows(sqlmock.NewRows([]string{"template_id", "active_version"}).
			AddRow("test.runtime-a", "1.0.0").
			AddRow("test.runtime-b", "2.1.0"))
	targets, err := store.ListActiveTargets(context.Background())
	if err != nil {
		t.Fatalf("ListActiveTargets: %v", err)
	}
	if len(targets) != 2 || targets[0].ID != "test.runtime-a" || targets[1].Version != "2.1.0" {
		t.Fatalf("targets = %+v", targets)
	}
	assertMockExpectations(t, mock)
}

func TestStoreListActiveTargetsRejectsMalformedPersistentIdentity(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	mock.ExpectQuery("SELECT template_id, active_version").
		WillReturnRows(sqlmock.NewRows([]string{"template_id", "active_version"}).
			AddRow("INVALID", "1.0.0"))
	_, err := store.ListActiveTargets(context.Background())
	if !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("ListActiveTargets error = %v, want ErrCatalogIntegrity", err)
	}
	assertMockExpectations(t, mock)
}

func TestStoreListActiveTargetsRejectsCapacityOverflow(t *testing.T) {
	const activeTargetLimit = 128
	if !strings.Contains(selectActiveTargetsSQL, "LIMIT 129") {
		t.Fatalf("active target query is not bounded to limit+1: %s", selectActiveTargetsSQL)
	}
	store, mock, closeDB := newMockStore(t)
	defer closeDB()
	rows := sqlmock.NewRows([]string{"template_id", "active_version"})
	for i := 0; i <= activeTargetLimit; i++ {
		rows.AddRow("test.runtime-a", "1.0.0")
	}
	mock.ExpectQuery("SELECT template_id, active_version").WillReturnRows(rows)
	targets, err := store.ListActiveTargets(context.Background())
	if !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("ListActiveTargets overflow error = %v, want ErrCatalogIntegrity", err)
	}
	if len(targets) != activeTargetLimit+1 {
		t.Fatalf("ListActiveTargets overflow count = %d, want %d", len(targets), activeTargetLimit+1)
	}
	assertMockExpectations(t, mock)
}
