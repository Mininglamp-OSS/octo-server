package user

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func TestQueryByPhoneFiltersDestroyedRowsFromBlindIndexLookup(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	d, mock := newPhoneLookupMock(t)

	mock.ExpectQuery(`SELECT \* FROM user WHERE \(phone_hash='[^']+' AND is_destroy<>2\) ORDER BY id ASC LIMIT 1$`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`SELECT \* FROM user WHERE \(zone='0086' and phone='13800001234'\) ORDER BY id ASC LIMIT 1$`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	got, err := d.QueryByPhone("0086", "13800001234")
	if err != nil {
		t.Fatalf("QueryByPhone: %v", err)
	}
	if got != nil {
		t.Fatalf("QueryByPhone returned destroyed row: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestQueryByPhoneReturnsActiveBlindIndexMatch(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	d, mock := newPhoneLookupMock(t)

	mock.ExpectQuery(`SELECT \* FROM user WHERE \(phone_hash='[^']+' AND is_destroy<>2\) ORDER BY id ASC LIMIT 1$`).
		WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("u-active"))

	got, err := d.QueryByPhone("0086", "13800001234")
	if err != nil {
		t.Fatalf("QueryByPhone: %v", err)
	}
	if got == nil || got.UID != "u-active" {
		t.Fatalf("QueryByPhone = %+v, want u-active", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestQueryByPhonePropagatesBlindIndexQueryError(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	d, mock := newPhoneLookupMock(t)
	wantErr := errors.New("blind lookup unavailable")

	mock.ExpectQuery(`SELECT \* FROM user WHERE \(phone_hash='[^']+' AND is_destroy<>2\) ORDER BY id ASC LIMIT 1$`).
		WillReturnError(wantErr)

	if _, err := d.QueryByPhone("0086", "13800001234"); !errors.Is(err, wantErr) {
		t.Fatalf("QueryByPhone error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestQueryByPhoneFallsBackWhenEncryptionKeyUnavailable(t *testing.T) {
	withPhoneSecretForTest(t, "")
	d, mock := newPhoneLookupMock(t)
	wantErr := errors.New("plaintext lookup unavailable")

	mock.ExpectQuery(`SELECT \* FROM user WHERE \(zone='0086' and phone='13800001234'\) ORDER BY id ASC LIMIT 1$`).
		WillReturnError(wantErr)

	if _, err := d.QueryByPhone("0086", "13800001234"); !errors.Is(err, wantErr) {
		t.Fatalf("QueryByPhone error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func newPhoneLookupMock(t *testing.T) (*DB, sqlmock.Sqlmock) {
	t.Helper()

	rawDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return &DB{session: conn.NewSession(nil)}, mock
}
