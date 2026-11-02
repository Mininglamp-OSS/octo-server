package conversation_ext

import (
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestFoldDBScopesEveryOperation(t *testing.T) {
	raw, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer raw.Close()
	conn := &dbr.Connection{DB: raw, Dialect: dialect.MySQL, EventReceiver: &dbr.NullEventReceiver{}}
	db := &DB{session: conn.NewSession(nil)}
	mock.ExpectExec("INSERT IGNORE INTO user_folded_chat .*'u', 's', 5, 'g:t'").WillReturnResult(sqlmock.NewResult(1, 1))
	require.NoError(t, db.SetFolded("u", "s", FoldedChat{ChannelID: "g:t", ChannelType: 5}, true))
	mock.ExpectQuery("SELECT channel_id, channel_type FROM user_folded_chat WHERE uid='u' AND space_id='s'").WillReturnRows(sqlmock.NewRows([]string{"channel_id", "channel_type"}).AddRow("g:t", 5))
	rows, err := db.ListFolded("u", "s")
	require.NoError(t, err)
	require.Equal(t, []FoldedChat{{ChannelID: "g:t", ChannelType: 5}}, rows)
	mock.ExpectExec("DELETE FROM `user_folded_chat` WHERE .*uid='u' AND space_id='s' AND channel_type=5 AND channel_id='g:t'").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, db.SetFolded("u", "s", rows[0], false))
	require.NoError(t, mock.ExpectationsWereMet())
}
