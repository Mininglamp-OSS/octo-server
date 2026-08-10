package user

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func TestQueryByPhoneFiltersDestroyedRowsFromBlindIndexLookup(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")

	rawDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	d := &DB{session: conn.NewSession(nil)}

	mock.ExpectQuery(`SELECT \* FROM user WHERE \(phone_hash='[^']+' AND is_destroy<>2\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`SELECT \* FROM user WHERE \(zone='0086' and phone='13800001234'\)`).
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
