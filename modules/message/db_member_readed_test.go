package message

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func TestMemberReadedWriteBoundary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	defer db.Close()
	conn := &dbr.Connection{DB: db, Dialect: dialect.MySQL, EventReceiver: &dbr.NullEventReceiver{}}
	store := &memberReadedDB{session: conn.NewSession(nil)}
	mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO member_readed .*VALUES \(6, 'c', 2, 'u'\),\(7, 'c', 2, 'u'\) ON DUPLICATE KEY UPDATE message_id=VALUES\(message_id\)`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("^COMMIT$").WillReturnResult(sqlmock.NewResult(0, 0))
	models := []*memberReadedModel{{MessageID: 7, UID: "u", ChannelID: "c", ChannelType: 2}, {MessageID: 6, UID: "u", ChannelID: "c", ChannelType: 2}, {MessageID: 7, UID: "u", ChannelID: "c", ChannelType: 2}}
	if err := store.batchInsertOrUpdate(context.Background(), models); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
