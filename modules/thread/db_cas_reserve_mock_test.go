package thread

import (
	"strconv"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

// 这组用例不连数据库，用 sqlmock 固定「CAS 重试时版本号来自共享序列」这一步
// 的语句顺序（octo-Q P1 的回归）：GenSeq 落后于行内版本时，重试必须先在 seq 表上
// 预留（BEGIN / INSERT IGNORE / UPDATE min_seq / SELECT min_seq / COMMIT），再用
// 预留到的值重试 UPDATE。旧实现只会把行内 version+1 直接写回，不会碰 seq 表，
// 因此这里任何一步缺失或顺序错乱都会让 ExpectationsWereMet 失败。
func newMockThreadDB(t *testing.T) (*DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	return &DB{session: conn.NewSession(nil)}, mock, func() { _ = rawDB.Close() }
}

func expectReserveThreadVersion(mock sqlmock.Sqlmock, floor int64, reserved int64) {
	mock.ExpectBegin()
	mock.ExpectExec("INSERT IGNORE INTO seq").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE seq SET `min_seq`=GREATEST\\(`min_seq`\\+1, " + strconv.FormatInt(floor, 10) + "\\) WHERE `key`='seq:thread'").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT `min_seq` FROM seq WHERE `key`='seq:thread'").
		WillReturnRows(sqlmock.NewRows([]string{"min_seq"}).AddRow(reserved))
	mock.ExpectCommit()
}

func TestCASRetryReservesVersionInSharedSeq(t *testing.T) {
	staleGen := func() (int64, error) { return 50, nil }

	cases := []struct {
		name string
		// firstUpdate / retriedUpdate 匹配两次 CAS UPDATE 的语句前缀（含版本号），
		// 用来证明重试用的是预留值 5001 而不是 GenSeq 的 50。
		firstUpdate   string
		retriedUpdate string
		call          func(db *DB) error
	}{
		{
			name:          "UpdateStatusFrom",
			firstUpdate:   "UPDATE thread SET status=2, version=50,",
			retriedUpdate: "UPDATE thread SET status=2, version=5001,",
			call: func(db *DB) error {
				return db.UpdateStatusFrom("cas", ThreadStatusActive, ThreadStatusArchived, staleGen)
			},
		},
		{
			name:          "MarkDeleted",
			firstUpdate:   "UPDATE thread SET status=3, version=50,",
			retriedUpdate: "UPDATE thread SET status=3, version=5001,",
			call:          func(db *DB) error { return db.MarkDeleted("cas", staleGen) },
		},
		{
			name:          "UpdateName",
			firstUpdate:   "UPDATE thread SET name='renamed', version=50,",
			retriedUpdate: "UPDATE thread SET name='renamed', version=5001,",
			call: func(db *DB) error {
				return db.UpdateName("cas", "renamed", staleGen)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, cleanup := newMockThreadDB(t)
			defer cleanup()

			// 第一次 CAS：GenSeq=50 < 行内 5000，命中 0 行。
			mock.ExpectExec("UPDATE thread SET .+version=50,").WillReturnResult(sqlmock.NewResult(0, 0))
			// probe 读回行内版本 5000。
			mock.ExpectQuery("SELECT status, version FROM thread WHERE short_id='cas'").
				WillReturnRows(sqlmock.NewRows([]string{"status", "version"}).AddRow(ThreadStatusActive, 5000))
			// 第二次 GenSeq 仍是 50 → 在共享序列上预留 5001。
			expectReserveThreadVersion(mock, 5001, 5001)
			// 用预留值重试成功。
			mock.ExpectExec("UPDATE thread SET .+version=5001,").WillReturnResult(sqlmock.NewResult(0, 1))

			require.NoError(t, tc.call(db))
			require.NoError(t, mock.ExpectationsWereMet(),
				"CAS retry must reserve the retry version in seq:thread before writing it")
		})
	}
}

// TestCASRetryUsesNewerGenSeqWithoutReserving 反向用例：GenSeq 已经大于行内版本时，
// 重试直接用它，不必额外占用共享序列（避免把正常路径变成每次都写 seq 行）。
func TestCASRetryUsesNewerGenSeqWithoutReserving(t *testing.T) {
	db, mock, cleanup := newMockThreadDB(t)
	defer cleanup()

	gen := func() (int64, error) { return 200, nil }

	mock.ExpectExec("UPDATE thread SET status=2, version=200,").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT status, version FROM thread WHERE short_id='cas'").
		WillReturnRows(sqlmock.NewRows([]string{"status", "version"}).AddRow(ThreadStatusActive, 100))
	// 第二次 GenSeq=200 > 100：直接用，无 BEGIN/seq 语句。
	mock.ExpectExec("UPDATE thread SET status=2, version=200,").WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, db.UpdateStatusFrom("cas", ThreadStatusActive, ThreadStatusArchived, gen))
	require.NoError(t, mock.ExpectationsWereMet())
}
