package bot_api

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/require"
)

func TestLegacyAuthorizationQueriesRejectExpiredGrants(t *testing.T) {
	groupType := common.ChannelTypeGroup.Uint8()
	for _, tc := range []struct {
		name          string
		call          func(*botAPIDB) error
		negativeProbe bool
	}{
		{"send", func(d *botAPIDB) error { _, err := d.findActiveGrantByGrantorBot("human-1", "bot-1"); return err }, true},
		{"grantor reply", func(d *botAPIDB) error { _, err := d.findGrantByGrantorBotActiveOnly("human-1", "bot-1"); return err }, false},
		{"explicit channel fan-out", func(d *botAPIDB) error { _, err := d.findActiveGrantsForChannel("group-1", groupType); return err }, false},
		{"mentioned grantor fan-out", func(d *botAPIDB) error {
			_, err := d.findActiveGrantsForChannelByGrantors("group-1", groupType, []string{"human-1"})
			return err
		}, false},
		{"implicit group fan-out", func(d *botAPIDB) error {
			_, err := d.findGlobalGrantsWithoutScope("group-1", "group-1", groupType)
			return err
		}, false},
		{"implicit DM fan-out", func(d *botAPIDB) error { _, err := d.findGlobalGrantsForDM("human-1", "peer-1"); return err }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock, closeDB := newSqlmockBotAPIDB(t)
			defer closeDB()
			mock.ExpectQuery("SELECT .* FROM obo_grants.*expires_at.*UTC_TIMESTAMP\\(6\\)").
				WillReturnRows(sqlmock.NewRows(grantRowCols()))
			if tc.negativeProbe {
				mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM obo_grants.*expires_at.*UTC_TIMESTAMP\\(6\\)").
					WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
			}
			require.NoError(t, tc.call(d))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
