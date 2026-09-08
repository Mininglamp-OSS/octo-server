package user

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Pin every SQL predicate that gates owner Project facts. The tests below also
// inject each computed boolean to exercise the Go-level fail-closed logic.
const botContextQueryPattern = `(?s)SELECT IFNULL.*` +
	`bu.status = 1 AND bu.robot = 1 AND COALESCE\(bu.is_destroy, 0\) = 0.*` +
	`ou.status = 1 AND ou.robot = 0 AND COALESCE\(ou.is_destroy, 0\) = 0.*` +
	`s.space_id = 'space' AND s.status = 1.*` +
	`bm.uid = bu.uid AND bm.status = 1.*` +
	`om.uid = ou.uid AND om.status = 1.*` +
	`WHERE r.robot_id = 'bot' AND r.creator_uid = 'owner' AND r.bot_token = 'bf_test' AND r.status = 1`

type botContextUserNames struct{ IService }

func (botContextUserNames) GetUser(uid string) (*Resp, error) {
	return &Resp{Name: uid + " name"}, nil
}

// Real HTTP handler and SQL contract; this does not replace MySQL integration.
// Opt-in fields must never change default verifier clients.
func TestBotOwnerContextHTTPContract(t *testing.T) {
	for _, tc := range []struct {
		name, query, body          string
		invalid, extended, dbError bool
	}{
		{"default", "", `{"bot_token":"bf_test"}`, false, false, false},
		{"legacy context", "?include=context", `{"bot_token":"bf_test","owner_uid":"ignored"}`, false, false, false},
		{"scoped", "?include=owner_context", `{"bot_token":"bf_test","space_id":"space","project_ids":["p"]}`, false, true, false},
		{"unavailable", "?include=owner_context", `{"bot_token":"bf_test","space_id":"space","project_ids":["p"]}`, false, true, true},
		{"missing space", "?include=owner_context", `{"bot_token":"bf_test"}`, true, false, false},
		{"forged owner", "?include=owner_context", `{"bot_token":"bf_test","space_id":"space","owner_uid":"victim"}`, true, false, false},
		{"too many projects", "?include=owner_context", `{"bot_token":"bf_test","space_id":"space","project_ids":[` + strings.Repeat(`"p",`, maxVerifyProjectIDs) + `"p"]}`, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newPhoneLookupMock(t)
			u := &User{db: db, userService: botContextUserNames{}, Log: log.NewTLog("bot-context-test")}
			if !tc.invalid {
				mock.ExpectQuery("SELECT.*FROM robot.*bot_token = 'bf_test'").WillReturnRows(sqlmock.NewRows([]string{"robot_id", "creator_uid"}).AddRow("bot", "owner"))
				mock.ExpectQuery("SELECT.*FROM space_member.*uid = 'bot'").WillReturnRows(sqlmock.NewRows([]string{"space_id"}).AddRow("first-space"))
				if tc.extended {
					q := mock.ExpectQuery(botContextQueryPattern)
					if tc.dbError {
						q.WillReturnError(errors.New("database unavailable"))
					} else {
						q.WillReturnRows(sqlmock.NewRows([]string{"hosting", "hosting_reported_at", "bot_active", "owner_active", "bot_member", "owner_member"}).AddRow("self_hosted", nil, true, true, true, true))
						mock.ExpectQuery("SELECT pm.project_id.*WHERE pm.uid = 'owner'.*pm.space_id = 'space'").WillReturnRows(sqlmock.NewRows([]string{"project_id", "role", "member_epoch"}).AddRow("p", 2, 7))
					}
				}
			}
			r := wkhttp.New()
			r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
			r.POST("/v1/auth/verify-bot", u.authVerifyBot)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/verify-bot"+tc.query, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			var response map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			if tc.invalid {
				require.Equal(t, http.StatusBadRequest, w.Code)
				require.Contains(t, response, "error")
			} else {
				require.Equal(t, http.StatusOK, w.Code)
				require.Equal(t, "first-space", response["space_id"])
				require.NotContains(t, response, "projects")
				require.NotContains(t, w.Body.String(), "bf_test")
				if !tc.extended {
					require.Len(t, response, 5)
				} else if tc.dbError {
					require.Equal(t, true, response["context_included"])
					require.Equal(t, true, response["context_error"])
					require.NotContains(t, response, "bot_context")
					require.NotContains(t, response, "owner_context")
				} else {
					require.Equal(t, true, response["context_included"])
					require.Contains(t, response, "bot_context")
					require.Contains(t, response, "owner_context")
					require.NotContains(t, response["bot_context"], "agent_platform")
					botContext := response["bot_context"].(map[string]any)
					require.Equal(t, "self_hosted", botContext["agent_hosting"])
					require.Contains(t, botContext, "agent_reported_hosting_at")
					require.Nil(t, botContext["agent_reported_hosting_at"])
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestBotOwnerContextDefaultSchema(t *testing.T) {
	data, err := json.Marshal(authVerifyBotResp{BotUID: "bot", BotName: "Bot", OwnerUID: "owner", OwnerName: "Owner", SpaceID: "legacy-first-space"})
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	require.Len(t, fields, 5)
	for _, key := range []string{"bot_uid", "bot_name", "owner_uid", "owner_name", "space_id"} {
		require.Contains(t, fields, key)
	}
	data, err = json.Marshal(ownedBot{UID: "bot", Name: "Bot"})
	require.NoError(t, err)
	require.JSONEq(t, `{"uid":"bot","name":"Bot"}`, string(data))
}

func TestBotOwnerContextPlatformNeutralAndSpaceScoped(t *testing.T) {
	for _, tc := range []struct {
		name, hosting                                  string
		botActive, ownerActive, botMember, ownerMember bool
	}{
		{"assistant without platform", "self_hosted", true, true, true, true},
		{"cloud facts are not filtered by Server", "octo_hosted", true, true, true, true},
		{"unknown hosting remains an identity fact", "", true, true, true, true},
		{"bot disabled", "self_hosted", false, true, true, true},
		{"owner disabled", "self_hosted", true, false, true, true},
		{"bot removed from Space", "self_hosted", true, true, false, true},
		{"owner removed from Space", "self_hosted", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newPhoneLookupMock(t)
			mock.ExpectQuery(botContextQueryPattern).
				WillReturnRows(sqlmock.NewRows([]string{"hosting", "hosting_reported_at", "bot_active", "owner_active", "bot_member", "owner_member"}).AddRow(tc.hosting, nil, tc.botActive, tc.ownerActive, tc.botMember, tc.ownerMember))
			allowed := tc.botActive && tc.ownerActive && tc.botMember && tc.ownerMember
			if allowed {
				mock.ExpectQuery("SELECT pm.project_id.*WHERE pm.uid = 'owner'.*pm.space_id = 'space'").
					WillReturnRows(sqlmock.NewRows([]string{"project_id", "role", "member_epoch"}).AddRow("p", 2, 7))
			}
			resp := authVerifyBotResp{BotUID: "bot", OwnerUID: "owner", SpaceID: "legacy-first-space"}
			err := (&User{db: db}).fillBotProjectContext(&resp, authVerifyBotReq{BotToken: "bf_test", SpaceID: "space", ProjectIDs: []string{"p", "unknown", "p"}})
			require.NoError(t, err)
			require.Equal(t, "legacy-first-space", resp.SpaceID)
			require.True(t, resp.ContextIncluded)
			require.Equal(t, tc.botMember, resp.BotContext.SpaceMember)
			require.Equal(t, tc.ownerMember, resp.OwnerContext.SpaceMember)
			require.Len(t, resp.OwnerContext.Projects, 2)
			require.Equal(t, allowed, resp.OwnerContext.Projects[0].Member)
			require.False(t, resp.OwnerContext.Projects[1].Member)
			require.Nil(t, resp.OwnerContext.Projects[1].Role)
			require.Nil(t, resp.OwnerContext.Projects[1].MemberEpoch)
			if !allowed {
				require.Nil(t, resp.OwnerContext.Projects[0].Role)
				require.Nil(t, resp.OwnerContext.Projects[0].MemberEpoch)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestBotOwnerContextFailsClosed(t *testing.T) {
	db, mock := newPhoneLookupMock(t)
	mock.ExpectQuery(botContextQueryPattern).WillReturnError(errors.New("database unavailable"))
	resp := authVerifyBotResp{BotUID: "bot", OwnerUID: "owner", SpaceID: "legacy"}
	err := (&User{db: db}).fillBotProjectContext(&resp, authVerifyBotReq{BotToken: "bf_test", SpaceID: "space", ProjectIDs: []string{"p"}})
	require.Error(t, err)
	require.Empty(t, resp.OwnerContext.Projects)
	require.False(t, resp.BotContext.SpaceMember)
	require.Equal(t, "legacy", resp.SpaceID)
	require.NoError(t, mock.ExpectationsWereMet())
	err = (&User{}).fillBotProjectContext(&resp, authVerifyBotReq{ProjectIDs: make([]string, maxVerifyProjectIDs+1)})
	require.ErrorIs(t, err, errTooManyProjectIDs)
}

func TestBotOwnerContextMissingFactsIsAnError(t *testing.T) {
	db, mock := newPhoneLookupMock(t)
	mock.ExpectQuery(botContextQueryPattern).
		WillReturnRows(sqlmock.NewRows([]string{"hosting", "hosting_reported_at", "bot_active", "owner_active", "bot_member", "owner_member"}))
	resp := authVerifyBotResp{BotUID: "bot", OwnerUID: "owner"}
	err := (&User{db: db}).fillBotProjectContext(&resp, authVerifyBotReq{
		BotToken: "bf_test", SpaceID: "space", ProjectIDs: []string{"p"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "disappeared after credential verification")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestBotOwnerContextMySQLLivenessContract executes the authorization-shaped
// predicates in MySQL instead of injecting their already-computed booleans.
// Every case gets distinct rows in a randomly named database; no shared schema
// or testutil.CleanAllTables call is involved.
func TestBotOwnerContextMySQLLivenessContract(t *testing.T) {
	dsn := os.Getenv("OCTO_ASSISTANT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set OCTO_ASSISTANT_TEST_MYSQL_DSN for an isolated MySQL server")
	}
	dbConfig, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	dbConfig.DBName = "information_schema"
	bootstrap, err := sql.Open("mysql", dbConfig.FormatDSN())
	require.NoError(t, err)
	bootstrap.SetMaxOpenConns(2)
	bootstrap.SetMaxIdleConns(1)
	t.Cleanup(func() { require.NoError(t, bootstrap.Close()) })
	require.NoError(t, bootstrap.Ping())
	databaseName := "octo_bot_context_" + util.GenerUUID()[:12]
	_, err = bootstrap.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := bootstrap.Exec("DROP DATABASE `" + databaseName + "`")
		require.NoError(t, err)
	})
	dbConfig.DBName = databaseName
	cfg := config.New()
	cfg.DB.MySQLAddr = dbConfig.FormatDSN()
	cfg.DB.MySQLMaxOpenConns = 2
	cfg.DB.MySQLMaxIdleConns = 1
	ctx := testutil.NewTestContext(cfg)
	database := ctx.DB().DB
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := database.Exec(query, args...)
		require.NoError(t, err)
	}
	exec("CREATE TABLE `user` (uid VARCHAR(40) PRIMARY KEY, status INT NOT NULL, robot INT NOT NULL, is_destroy INT NULL)")
	exec("CREATE TABLE robot (robot_id VARCHAR(40) PRIMARY KEY, creator_uid VARCHAR(40) NOT NULL, bot_token VARCHAR(80) NOT NULL, status INT NOT NULL, agent_hosting VARCHAR(40) NOT NULL DEFAULT '', agent_reported_hosting_at TIMESTAMP NULL)")
	exec("CREATE TABLE space (space_id VARCHAR(40) PRIMARY KEY, status INT NOT NULL)")
	exec("CREATE TABLE space_member (space_id VARCHAR(40), uid VARCHAR(40), status INT NOT NULL, PRIMARY KEY(space_id,uid))")
	exec("CREATE TABLE octo_project (project_id VARCHAR(40) PRIMARY KEY, status INT NOT NULL, member_epoch BIGINT NOT NULL)")
	exec("CREATE TABLE octo_project_member (project_id VARCHAR(40), uid VARCHAR(40), space_id VARCHAR(40), role INT NOT NULL, status INT NOT NULL, removing INT NOT NULL, PRIMARY KEY(project_id,uid))")

	for i, tc := range []struct {
		name                                                           string
		botStatus, botDestroy, ownerStatus, ownerDestroy               int
		botMemberStatus, ownerMemberStatus, spaceStatus, projectStatus int
		wantMember                                                     bool
	}{
		{"live principals", 1, 0, 1, 0, 1, 1, 1, 1, true},
		{"disabled bot", 0, 0, 1, 0, 1, 1, 1, 1, false},
		{"destroying bot", 1, 1, 1, 0, 1, 1, 1, 1, false},
		{"destroyed bot", 1, 2, 1, 0, 1, 1, 1, 1, false},
		{"disabled owner", 1, 0, 0, 0, 1, 1, 1, 1, false},
		{"destroying owner", 1, 0, 1, 1, 1, 1, 1, 1, false},
		{"destroyed owner", 1, 0, 1, 2, 1, 1, 1, 1, false},
		{"bot removed from Space", 1, 0, 1, 0, 0, 1, 1, 1, false},
		{"owner removed from Space", 1, 0, 1, 0, 1, 0, 1, 1, false},
		{"disbanded Space", 1, 0, 1, 0, 1, 1, 0, 1, false},
		{"disbanded Project", 1, 0, 1, 0, 1, 1, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suffix := fmt.Sprintf("%02d", i)
			botID, ownerID := "bot"+suffix, "owner"+suffix
			spaceID, projectID, token := "space"+suffix, "project"+suffix, "bf_"+suffix
			exec("INSERT INTO `user` VALUES (?,?,1,?),(?,?,0,?)", botID, tc.botStatus, tc.botDestroy, ownerID, tc.ownerStatus, tc.ownerDestroy)
			exec("INSERT INTO robot VALUES (?,?,?,1,'self_hosted',UTC_TIMESTAMP())", botID, ownerID, token)
			exec("INSERT INTO space VALUES (?,?)", spaceID, tc.spaceStatus)
			exec("INSERT INTO space_member VALUES (?,?,?),(?,?,?)", spaceID, botID, tc.botMemberStatus, spaceID, ownerID, tc.ownerMemberStatus)
			exec("INSERT INTO octo_project VALUES (?,?,7)", projectID, tc.projectStatus)
			exec("INSERT INTO octo_project_member VALUES (?,?,?,2,1,0)", projectID, ownerID, spaceID)

			resp := authVerifyBotResp{BotUID: botID, OwnerUID: ownerID}
			err := (&User{db: NewDB(ctx)}).fillBotProjectContext(&resp, authVerifyBotReq{
				BotToken: token, SpaceID: spaceID, ProjectIDs: []string{projectID},
			})
			require.NoError(t, err)
			require.Len(t, resp.OwnerContext.Projects, 1)
			require.Equal(t, tc.wantMember, resp.OwnerContext.Projects[0].Member)
			if tc.wantMember {
				require.True(t, resp.BotContext.Active)
				require.True(t, resp.OwnerContext.Active)
				require.True(t, resp.BotContext.SpaceMember)
				require.True(t, resp.OwnerContext.SpaceMember)
				require.NotNil(t, resp.OwnerContext.Projects[0].Role)
			} else {
				require.Nil(t, resp.OwnerContext.Projects[0].Role)
				require.Nil(t, resp.OwnerContext.Projects[0].MemberEpoch)
				require.Empty(t, resp.OwnerContext.Projects[0].Capabilities)
			}
		})
	}
}
