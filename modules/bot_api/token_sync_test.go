package bot_api

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/require"
)

func robotSyncRows(robotID, botToken, imTokenCache string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"robot_id", "bot_token", "im_token_cache", "status"}).
		AddRow(robotID, botToken, imTokenCache, 1)
}

func successfulTokenRecorder(tokens *[]string) func(config.UpdateIMTokenReq) (*config.UpdateIMTokenResp, error) {
	return func(req config.UpdateIMTokenReq) (*config.UpdateIMTokenResp, error) {
		*tokens = append(*tokens, req.Token)
		return &config.UpdateIMTokenResp{Status: config.UpdateTokenStatusSuccess}, nil
	}
}

func TestResolveRobotSyncTokenPrefersBindingOverLegacyState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cachedToken string
	}{
		{name: "raw token cache", cachedToken: "bf_token"},
		{name: "empty cache after disconnect", cachedToken: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock, closer := newSqlmockBotAPIDB(t)
			defer closer()

			const (
				robotID  = "bot_1"
				botToken = "bf_token"
			)
			mock.ExpectQuery("SELECT .* FROM robot").
				WillReturnRows(robotSyncRows(robotID, botToken, tc.cachedToken))
			mock.ExpectQuery("SELECT .* FROM bot_instance_binding").
				WillReturnRows(bindingRows(botBindingFingerprint(botToken), botKindUser, robotID,
					"550e8400-e29b-41d4-a716-446655440000", "im_bound"))

			ba := &BotAPI{db: d}
			token, active, err := ba.resolveRobotSyncToken(robotID)
			require.NoError(t, err)
			require.True(t, active)
			require.Equal(t, "im_bound", token)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestSyncRobotIMTokenRepairsBindingClaimedDuringPush(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	const (
		robotID  = "bot_1"
		botToken = "bf_token"
	)
	// First resolution sees the legacy state.
	mock.ExpectQuery("SELECT .* FROM robot").
		WillReturnRows(robotSyncRows(robotID, botToken, botToken))
	mock.ExpectQuery("SELECT .* FROM bot_instance_binding").
		WillReturnRows(sqlmock.NewRows([]string{"token_fingerprint", "bot_kind", "robot_id", "instance_id", "im_token"}))
	// The post-update resolution observes a concurrently committed first claim.
	mock.ExpectQuery("SELECT .* FROM robot").
		WillReturnRows(robotSyncRows(robotID, botToken, "im_bound"))
	mock.ExpectQuery("SELECT .* FROM bot_instance_binding").
		WillReturnRows(bindingRows(botBindingFingerprint(botToken), botKindUser, robotID,
			"550e8400-e29b-41d4-a716-446655440000", "im_bound"))

	var installed []string
	ba := &BotAPI{db: d, updateIMToken: successfulTokenRecorder(&installed)}
	require.NoError(t, ba.syncRobotIMToken(robotID))
	require.Equal(t, []string{botToken, "im_bound"}, installed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReconcileBindingAfterUnboundUpdateRepairsAndConflicts(t *testing.T) {
	d, mock, closer := newSqlmockBotAPIDB(t)
	defer closer()

	const (
		robotID  = "bot_1"
		botToken = "bf_token"
	)
	mock.ExpectQuery("SELECT .* FROM bot_instance_binding").
		WillReturnRows(bindingRows(botBindingFingerprint(botToken), botKindUser, robotID,
			"550e8400-e29b-41d4-a716-446655440000", "im_bound"))

	var installed []string
	ba := &BotAPI{db: d, updateIMToken: successfulTokenRecorder(&installed)}
	conflict, err := ba.reconcileBindingAfterUnboundUpdate(botToken, botKindUser, robotID, botToken)
	require.NoError(t, err)
	require.True(t, conflict)
	require.Equal(t, []string{"im_bound"}, installed)
	require.NoError(t, mock.ExpectationsWereMet())
}
