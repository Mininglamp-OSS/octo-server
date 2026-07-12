package message

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

func newMockReactionDB(t *testing.T) (*messageReactionDB, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return &messageReactionDB{session: conn.NewSession(nil)}, mock, func() { _ = rawDB.Close() }
}

func reactionRows() *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{
		"id", "message_id", "seq", "channel_id", "channel_type", "uid", "name", "emoji", "is_deleted", "created_at", "updated_at",
	}).AddRow(1, "9001", 7, "group-a", 2, "u1", "User 1", "👍", 0, now, now)
}

func TestMessageReactionDB_QueryWithMessageIDsScopedToChannel(t *testing.T) {
	db, mock, cleanup := newMockReactionDB(t)
	defer cleanup()

	mock.ExpectQuery("SELECT \\* FROM reaction_users WHERE \\(channel_id='group-a' and channel_type=2 and message_id in \\('9001'\\)\\)").
		WillReturnRows(reactionRows())

	got, err := db.queryWithMessageIDsInChannel("group-a", 2, []string{"9001"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "group-a", got[0].ChannelID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMessageReactionDB_ToggleReactionUpsertsAndReadsBack(t *testing.T) {
	db, mock, cleanup := newMockReactionDB(t)
	defer cleanup()

	// 原子 upsert：INSERT ... ON DUPLICATE KEY UPDATE is_deleted = 1 - is_deleted。
	// dbr 默认客户端插值，sqlmock 收到的是已内联值的语句（无占位参数）。
	mock.ExpectExec("INSERT INTO reaction_users .*VALUES \\('9001',7,'group-a',2,'u1','User 1','👍',0\\) ON DUPLICATE KEY UPDATE is_deleted = 1 - is_deleted").
		WillReturnResult(sqlmock.NewResult(1, 1))
	// 回读该 (channel, message, uid, emoji) 行的最终 is_deleted。
	mock.ExpectQuery("SELECT is_deleted FROM reaction_users WHERE \\(channel_id='group-a' and channel_type=2 and message_id='9001' and uid='u1' and emoji='👍'\\)").
		WillReturnRows(sqlmock.NewRows([]string{"is_deleted"}).AddRow(1))

	isDeleted, err := db.toggleReaction(&reactionModel{
		ChannelID: "group-a", ChannelType: 2, UID: "u1", Name: "User 1",
		MessageID: "9001", Emoji: "👍", Seq: 7,
	})
	require.NoError(t, err)
	require.Equal(t, 1, isDeleted)
	require.NoError(t, mock.ExpectationsWereMet())
}
