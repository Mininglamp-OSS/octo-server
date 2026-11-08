package user

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

func TestPermissionManagedIdentityClosedParser(t *testing.T) {
	for _, raw := range []string{
		`{"bot_token":"bf_synthetic","identity_version":2.0}`,
		`{"bot_token":"bf_synthetic","identity_version":1}`,
		`{"bot_token":"bf_synthetic","identity_version":null}`,
		`{"bot_token":"bf_synthetic","identity_version":2,"owner_uid":"forged"}`,
		`{"bot_token":"bf_synthetic","identity_version":2,"identity_versio\u006e":2}`,
		`{"bot_token":"bf_synthetic","identity_version":2,"include_mcp_policy":null}`,
		`{"bot_token":"bf_\ud800","identity_version":2}`,
	} {
		var req authVerifyBotReq
		require.Error(t, json.Unmarshal([]byte(raw), &req), raw)
	}
	var req authVerifyBotReq
	require.NoError(t, json.Unmarshal([]byte(`{"bot_token":"bf_synthetic","identity_version":2,"include_mcp_policy":false}`), &req))
}

func TestPermissionManagedIdentityMinimalProjection(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "positive", true: "app_conflict"}[invalid], func(t *testing.T) {
			db, mock, e := sqlmock.New()
			require.NoError(t, e)
			defer db.Close()
			conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
			u := &User{db: &DB{session: conn.NewSession(nil)}, Log: log.NewTLog("synthetic-identity-test")}
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT robot_id.*FROM robot WHERE bot_token=.*FOR SHARE NOWAIT").WithArgs("bf_synthetic").WillReturnRows(sqlmock.NewRows([]string{"robot_id", "creator_uid", "bot_token", "status"}).AddRow(strings.Repeat("😀", 40), "owner", "bf_synthetic", 1))
			mock.ExpectQuery("SELECT id,uid,status.*FROM user WHERE uid=.*FOR SHARE NOWAIT").WithArgs(strings.Repeat("😀", 40)).WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "status", "is_destroy", "robot"}).AddRow(1, strings.Repeat("😀", 40), 1, 0, 1))
			appRows := sqlmock.NewRows([]string{"id", "uid"})
			if invalid {
				appRows.AddRow(1, strings.Repeat("😀", 40))
			}
			mock.ExpectQuery("SELECT id,uid FROM app_bot WHERE uid=.*FOR SHARE").WillReturnRows(appRows)
			if invalid {
				mock.ExpectRollback()
			} else {
				mock.ExpectQuery("SELECT id,uid,status.*FROM user WHERE uid=.*FOR SHARE NOWAIT").WithArgs("owner").WillReturnRows(sqlmock.NewRows([]string{"id", "uid", "status", "is_destroy", "robot"}).AddRow(2, "owner", 1, 0, 0))
				mock.ExpectQuery("SELECT id,robot_id,status FROM robot WHERE robot_id=.*FOR SHARE").WithArgs("owner").WillReturnRows(sqlmock.NewRows([]string{"id", "robot_id", "status"}))
				mock.ExpectQuery("SELECT id,uid FROM app_bot WHERE uid=.*FOR SHARE").WithArgs("owner").WillReturnRows(sqlmock.NewRows([]string{"id", "uid"}))
				mock.ExpectCommit()
			}
			r := helperHarness(u.authVerifyBot)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/probe", strings.NewReader(`{"bot_token":"bf_synthetic","identity_version":2}`))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)
			if invalid {
				require.Equal(t, 401, rec.Code)
			} else {
				require.Equal(t, 200, rec.Code, rec.Body.String())
				var got map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
				require.Len(t, got, 3)
				require.Equal(t, float64(2), got["identity_version"])
				require.Equal(t, "user_bot", got["principal_kind"])
				require.Equal(t, strings.Repeat("😀", 40), got["bot_uid"])
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
