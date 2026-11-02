package user

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

type mcpIdentityNames struct{ IService }

func (mcpIdentityNames) GetUser(string) (*Resp, error) { return nil, nil }

func TestMCPVerifyBotCompatibility(t *testing.T) {
	for _, tc := range []string{"legacy", "false", "true_default", "true_override", "policy_failure", "invalid_token", "malformed"} {
		t.Run(tc, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
			u := &User{db: &DB{session: conn.NewSession(nil)}, userService: mcpIdentityNames{}, Log: log.NewTLog("mcp-test")}
			body := `{"bot_token":"synthetic-test-token"}`
			if tc == "false" {
				body = `{"bot_token":"synthetic-test-token","include_mcp_policy":false}`
			}
			if strings.HasPrefix(tc, "true_") || tc == "policy_failure" {
				body = `{"bot_token":"synthetic-test-token","include_mcp_policy":true}`
			}
			if tc == "malformed" {
				body = `{"bot_token":"synthetic-test-token","include_mcp_policy":null}`
			} else {
				lookup := mock.ExpectQuery("FROM robot")
				if tc == "invalid_token" {
					lookup.WillReturnError(errors.New("not found"))
				} else {
					lookup.WillReturnRows(sqlmock.NewRows([]string{"robot_id", "creator_uid"}).AddRow("bot_B", "owner_U"))
					if strings.HasPrefix(tc, "true_") || tc == "policy_failure" {
						mock.ExpectBegin()
						if tc == "policy_failure" {
							mock.ExpectQuery("FROM mcp_policy_default").WillReturnError(errors.New("private SQL cause"))
							mock.ExpectRollback()
						} else {
							mock.ExpectQuery("FROM mcp_policy_default").WillReturnRows(sqlmock.NewRows([]string{"context_mode", "generation"}).AddRow("strict", 7))
							rows := sqlmock.NewRows([]string{"context_mode", "revision"})
							if tc == "true_override" {
								rows.AddRow("compatible", 6)
							}
							mock.ExpectQuery("FROM mcp_bot_policy").WithArgs("bot_B").WillReturnRows(rows)
							mock.ExpectCommit()
						}
					}
					if tc != "policy_failure" {
						mock.ExpectQuery("FROM space_member").WillReturnRows(sqlmock.NewRows([]string{"space_id"}).AddRow("space_S"))
					}
				}
			}
			r := helperHarness(u.authVerifyBot)
			req := httptest.NewRequest(http.MethodGet, "/probe", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("cacheable identity")
			}
			switch tc {
			case "invalid_token":
				if rec.Code != 401 {
					t.Fatal(rec.Code)
				}
			case "malformed":
				if rec.Code != 400 {
					t.Fatal(rec.Code)
				}
			case "policy_failure":
				env := decodeEnvelope(t, rec.Body.Bytes())
				if rec.Code != 400 || env.Error.Code != "err.server.mcp.policy_unavailable" || env.Error.HTTPStatus != 503 || len(env.Error.Details) != 0 || strings.Contains(rec.Body.String(), "private SQL cause") {
					t.Fatal(rec.Body.String())
				}
			default:
				if rec.Code != 200 {
					t.Fatal(rec.Body.String())
				}
				var got map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if tc == "legacy" || tc == "false" {
					if len(got) != 5 || got["mcp_policy"] != nil {
						t.Fatal("legacy shape changed", rec.Body.String())
					}
				} else {
					var p map[string]any
					if err := json.Unmarshal(got["mcp_policy"], &p); err != nil {
						t.Fatal(err)
					}
					if len(p) != 7 || p["version"] != float64(1) || p["generation"] != "7" {
						t.Fatal(rec.Body.String())
					}
					if tc == "true_default" && (p["source"] != "stage_default" || p["override"] != nil || p["bot_revision"] != nil) {
						t.Fatal(rec.Body.String())
					}
					if tc == "true_override" && (p["source"] != "bot" || p["context_mode"] != "compatible" || p["bot_revision"] != "6") {
						t.Fatal(rec.Body.String())
					}
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMCPVerifyBotOptInDecoder(t *testing.T) {
	for _, body := range []string{
		`{"bot_token":"synthetic","include_mcp_policy":null}`,
		`{"bot_token":"synthetic","include_mcp_policy":0}`,
		`{"bot_token":"synthetic","include_mcp_policy":"true"}`,
		`{"bot_token":"synthetic","include_mcp_policy":false,"include_mcp_polic\u0079":true}`,
	} {
		var req authVerifyBotReq
		if err := json.Unmarshal([]byte(body), &req); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
