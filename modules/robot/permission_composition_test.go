package robot

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

func TestPermissionMyBotsExactSpaceAccess(t *testing.T) {
	for _, state := range []string{"member", "nonmember", "query_failure", "legacy"} {
		t.Run(state, func(t *testing.T) {
			db, mock, e := sqlmock.New()
			require.NoError(t, e)
			defer db.Close()
			conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
			rb := &Robot{db: robotDB{session: conn.NewSession(nil)}, Log: log.NewTLog("synthetic-scope-test")}
			if state != "legacy" {
				q := mock.ExpectQuery("SELECT COUNT.*FROM space_member sm.*INNER JOIN space.*WHERE sm.uid = 'caller' AND sm.space_id = 'space'")
				if state == "query_failure" {
					q.WillReturnError(errors.New("private cause"))
				} else {
					n := 0
					if state == "member" {
						n = 1
					}
					q.WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
				}
			}
			if state == "member" || state == "legacy" {
				mock.ExpectQuery("SELECT f.to_uid.*FROM friend f").WillReturnRows(sqlmock.NewRows([]string{"uid", "name", "description", "creator_uid", "bot_commands"}))
			}
			r := helperHarness(func(c *wkhttp.Context) { c.Set("uid", "caller"); rb.myBots(c) })
			rec := httptest.NewRecorder()
			path := "/probe?space_id=space"
			if state == "legacy" {
				path = "/probe"
			}
			r.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
			if state == "member" || state == "legacy" {
				require.Equal(t, 200, rec.Code, rec.Body.String())
				require.Equal(t, "[]", rec.Body.String())
			} else {
				require.Equal(t, 400, rec.Code)
				require.NotContains(t, rec.Body.String(), "private cause")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
