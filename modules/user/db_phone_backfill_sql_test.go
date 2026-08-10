package user

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCountPhoneShadowPendingIncludesLegacyHashVersions(t *testing.T) {
	d, mock := newPhoneLookupMock(t)
	mock.ExpectQuery(`SELECT count\(\*\) FROM user WHERE \(phone<>'' AND phone_hash NOT LIKE '2:%' AND is_destroy<>2\)$`).
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(1))

	got, err := d.CountPhoneShadowPending()
	if err != nil {
		t.Fatalf("CountPhoneShadowPending: %v", err)
	}
	if got != 1 {
		t.Fatalf("CountPhoneShadowPending = %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}
