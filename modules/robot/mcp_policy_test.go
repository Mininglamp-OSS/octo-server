package robot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/mcppolicy"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func TestMCPPolicyOwnerGuard(t *testing.T) {
	for _, tc := range []string{"owner", "nonowner", "missing", "empty_creator", "db_failure", "locked_owner_recheck"} {
		t.Run(tc, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			q := "SELECT IFNULL.*FROM robot WHERE robot_id=.*BINARY robot_id=BINARY .*status=1"
			lock := tc == "locked_owner_recheck"
			if lock {
				q += " FOR UPDATE"
			}
			expect := mock.ExpectQuery(q).WithArgs("bot_B", "bot_B")
			want := error(nil)
			switch tc {
			case "db_failure":
				expect.WillReturnError(errors.New("private cause"))
				want = mcppolicy.ErrUnavailable
			case "missing":
				expect.WillReturnRows(sqlmock.NewRows([]string{"creator_uid"}))
				want = errMCPRobotNotFound
			case "empty_creator":
				expect.WillReturnRows(sqlmock.NewRows([]string{"creator_uid"}).AddRow(""))
				want = errMCPRobotNotFound
			case "nonowner", "locked_owner_recheck":
				expect.WillReturnRows(sqlmock.NewRows([]string{"creator_uid"}).AddRow("other_U"))
				want = errMCPRobotForbidden
			default:
				expect.WillReturnRows(sqlmock.NewRows([]string{"creator_uid"}).AddRow("owner_U"))
			}
			if err := mcpRobotOwner(context.Background(), db, "bot_B", "owner_U", lock); err != want {
				t.Fatalf("%v != %v", err, want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMCPPolicyErrorEnvelope(t *testing.T) {
	rb := &Robot{Log: log.NewTLog("mcp-test")}
	for _, tc := range []struct {
		err            error
		wire, semantic int
		code           string
	}{
		{errMCPRobotNotFound, 404, 404, "err.server.robot.not_found"},
		{errMCPRobotForbidden, 403, 403, "err.server.robot.creator_only"},
		{mcppolicy.ErrInvalid, 400, 400, "err.server.mcp.policy_invalid"},
		{mcppolicy.ErrConflict, 400, 409, "err.server.mcp.policy_conflict"},
		{mcppolicy.ErrUnavailable, 400, 503, "err.server.mcp.policy_unavailable"},
	} {
		r := helperHarness(func(c *wkhttp.Context) { rb.respondMCPPolicyError(c, tc.err) })
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", "/probe", nil))
		env := decodeRobotEnvelope(t, rec.Body.Bytes())
		if rec.Code != tc.wire || env.Error.HTTPStatus != tc.semantic || env.Error.Code != tc.code || len(env.Error.Details) != 0 {
			t.Fatalf("%s", rec.Body.String())
		}
	}
}

// Assemble the actual route tree: all Bot wildcard names must remain
// :robot_id, and Bearer credentials cannot enter the human owner API.
func TestMCPPolicyRouteAssemblyAndBearerDenial(t *testing.T) {
	ctx := config.NewContext(config.New())
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	// Route's existing system-Bot initialization sees a preexisting row.
	mock.ExpectQuery("FROM robot").WillReturnRows(sqlmock.NewRows([]string{"robot_id"}).AddRow(ctx.GetConfig().Account.SystemUID))
	rb := &Robot{ctx: ctx, db: robotDB{session: conn.NewSession(nil)}, Log: log.NewTLog("mcp-test")}
	r := wkhttp.New()
	rb.Route(r)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/robot/bot_B/mcp-policy", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer synthetic-bot-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("%s: %d %s", method, rec.Code, rec.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
