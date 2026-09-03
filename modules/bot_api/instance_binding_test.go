package bot_api

import (
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func bindingRows(fingerprint []byte, kind, robotID, instanceID, imToken string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"token_fingerprint", "bot_kind", "robot_id", "instance_id", "im_token"}).
		AddRow(fingerprint, kind, robotID, instanceID, imToken)
}

func TestValidInstanceID(t *testing.T) {
	t.Parallel()
	require.True(t, validInstanceID("550e8400-e29b-41d4-a716-446655440000"))
	require.True(t, validInstanceID("instance_node-01:primary"))
	require.False(t, validInstanceID("too-short"))
	require.False(t, validInstanceID(strings.Repeat("a", 129)))
	require.False(t, validInstanceID("550e8400-e29b-41d4-a716-44665544/000"))
}

func TestBotTokenFingerprintDoesNotContainRawToken(t *testing.T) {
	t.Parallel()
	token := "bf_super_secret_token"
	fingerprint := botBindingFingerprint(token)
	require.Len(t, fingerprint, 32)
	require.NotContains(t, string(fingerprint), token)
}

func TestResolveRegistrationIMTokenFirstClaimReturnsStoredToken(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	fingerprint := botBindingFingerprint("bf_token")
	mock.ExpectExec("INSERT INTO bot_instance_binding").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT .* FROM bot_instance_binding").
		WillReturnRows(bindingRows(fingerprint, botKindUser, "bot_1", "instance_0000001", "im_winner"))

	imToken, bound, err := d.resolveRegistrationIMToken("bf_token", botKindUser, "bot_1", "instance_0000001")
	require.NoError(t, err)
	require.True(t, bound)
	require.Equal(t, "im_winner", imToken)
	require.NotEqual(t, "bf_token", imToken)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestResolveRegistrationIMTokenSameInstanceIsIdempotent(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	fingerprint := botBindingFingerprint("app_token")
	mock.ExpectExec("INSERT INTO bot_instance_binding").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT .* FROM bot_instance_binding").
		WillReturnRows(bindingRows(fingerprint, botKindApp, "app_1_bot", "instance_0000001", "im_existing"))

	imToken, bound, err := d.resolveRegistrationIMToken("app_token", botKindApp, "app_1_bot", "instance_0000001")
	require.NoError(t, err)
	require.True(t, bound)
	require.Equal(t, "im_existing", imToken)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestResolveRegistrationIMTokenRejectsDifferentInstance(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	fingerprint := botBindingFingerprint("bf_token")
	mock.ExpectExec("INSERT INTO bot_instance_binding").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT .* FROM bot_instance_binding").
		WillReturnRows(bindingRows(fingerprint, botKindUser, "bot_1", "instance_0000001", "im_existing"))

	_, _, err := d.resolveRegistrationIMToken("bf_token", botKindUser, "bot_1", "instance_0000002")
	require.ErrorIs(t, err, errBotInstanceConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestResolveRegistrationIMTokenLegacyTransition(t *testing.T) {
	t.Run("unclaimed token keeps legacy credential", func(t *testing.T) {
		d, mock, closer := newSqlmockBotAPIDB(t)
		defer closer()

		mock.ExpectQuery("SELECT .* FROM bot_instance_binding").
			WillReturnRows(sqlmock.NewRows([]string{"token_fingerprint", "bot_kind", "robot_id", "instance_id", "im_token"}))

		imToken, bound, err := d.resolveRegistrationIMToken("bf_legacy", botKindUser, "bot_1", "")
		require.NoError(t, err)
		require.False(t, bound)
		require.Equal(t, "bf_legacy", imToken)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("claimed token rejects a legacy client", func(t *testing.T) {
		d, mock, closer := newSqlmockBotAPIDB(t)
		defer closer()

		fingerprint := botBindingFingerprint("bf_claimed")
		mock.ExpectQuery("SELECT .* FROM bot_instance_binding").
			WillReturnRows(bindingRows(fingerprint, botKindUser, "bot_1", "instance_0000001", "im_existing"))

		_, _, err := d.resolveRegistrationIMToken("bf_claimed", botKindUser, "bot_1", "")
		require.ErrorIs(t, err, errBotInstanceConflict)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestBotInstanceBindingMigrationPinsAtomicOwnership(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("sql/20260903000001_bot_instance_binding.sql")
	require.NoError(t, err)
	sql := string(data)
	require.Contains(t, sql, "token_fingerprint BINARY(32)")
	require.Contains(t, sql, "PRIMARY KEY (token_fingerprint)")
	require.Contains(t, sql, "instance_id       VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin")
	require.NotContains(t, sql, "bot_token ")
}
