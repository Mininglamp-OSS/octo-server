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

// TestMessageReactionDB_SetReactionIdempotent pins the Bot-API explicit add/remove
// primitive: unlike toggleReaction's blind flip, setReaction writes is_deleted to
// the requested target value (VALUES(is_deleted), not 1 - is_deleted) so a retried
// add never cancels a live reaction. It also does NOT read back — the final state
// equals the requested value, returned directly.
func TestMessageReactionDB_SetReactionIdempotent(t *testing.T) {
	model := func() *reactionModel {
		return &reactionModel{
			ChannelID: "group-a", ChannelType: 2, UID: "u1", Name: "User 1",
			MessageID: "9001", Emoji: "👍", Seq: 7,
		}
	}

	t.Run("add writes is_deleted=0 and returns it without a read-back", func(t *testing.T) {
		db, mock, cleanup := newMockReactionDB(t)
		defer cleanup()
		mock.ExpectExec("INSERT INTO reaction_users .*VALUES \\('9001',7,'group-a',2,'u1','User 1','👍',0\\) ON DUPLICATE KEY UPDATE is_deleted = VALUES\\(is_deleted\\), seq = VALUES\\(seq\\), name = VALUES\\(name\\)").
			WillReturnResult(sqlmock.NewResult(1, 1))
		isDeleted, err := db.setReaction(model(), 0)
		require.NoError(t, err)
		require.Equal(t, 0, isDeleted)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("remove writes is_deleted=1", func(t *testing.T) {
		db, mock, cleanup := newMockReactionDB(t)
		defer cleanup()
		mock.ExpectExec("INSERT INTO reaction_users .*VALUES \\('9001',7,'group-a',2,'u1','User 1','👍',1\\) ON DUPLICATE KEY UPDATE is_deleted = VALUES\\(is_deleted\\)").
			WillReturnResult(sqlmock.NewResult(1, 1))
		isDeleted, err := db.setReaction(model(), 1)
		require.NoError(t, err)
		require.Equal(t, 1, isDeleted)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}
