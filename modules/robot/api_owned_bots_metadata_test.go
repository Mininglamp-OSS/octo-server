package robot

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Exercises the actual handler and MySQL joins. Authentication is deliberately
// injected: this is not a replacement for the Session/Redis middleware suite.
// Only the randomly named database created here is changed or dropped.
func TestOwnedBotsMetadataMySQLContract(t *testing.T) {
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
	databaseName := "octo_assistant_" + util.GenerUUID()[:12]
	_, err = bootstrap.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4")
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
	// Minimal projections of the production tables; migration coverage is a
	// separate concern. No shared schema or testutil.CleanAllTables is used.
	exec("CREATE TABLE space (space_id VARCHAR(40) PRIMARY KEY, status INT NOT NULL)")
	exec("CREATE TABLE space_member (space_id VARCHAR(40), uid VARCHAR(40), status INT NOT NULL, PRIMARY KEY(space_id,uid))")
	exec("CREATE TABLE user (uid VARCHAR(40) PRIMARY KEY, name VARCHAR(40), robot INT NOT NULL, status INT NOT NULL)")
	exec("CREATE TABLE robot (robot_id VARCHAR(40) PRIMARY KEY, creator_uid VARCHAR(40), status INT NOT NULL, description TEXT, bot_commands TEXT, agent_platform VARCHAR(40), agent_hosting VARCHAR(40), created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)")
	exec("INSERT INTO space VALUES ('space-a',1),('space-b',1)")
	exec("INSERT INTO space_member VALUES ('space-a','owner',1),('space-b','owner',1),('space-a','other-owner',1)")
	for _, bot := range []struct {
		uid, owner, space, platform, hosting         string
		userStatus, robotStatus, memberStatus, robot int
	}{
		{"local", "owner", "space-a", "OpenClaw", "self_hosted", 1, 1, 1, 1},
		{"cloud", "owner", "space-a", "OpenClaw", "octo_hosted", 1, 1, 1, 1},
		{"other-platform", "owner", "space-a", "Codex", "self_hosted", 1, 1, 1, 1},
		{"no-platform", "owner", "space-a", "", "self_hosted", 1, 1, 1, 1},
		{"unknown-platform", "owner", "space-a", "custom-runtime", "self_hosted", 1, 1, 1, 1},
		{"foreign", "other-owner", "space-a", "OpenClaw", "self_hosted", 1, 1, 1, 1},
		{"other-space", "owner", "space-b", "OpenClaw", "self_hosted", 1, 1, 1, 1},
		{"disabled-user", "owner", "space-a", "OpenClaw", "self_hosted", 0, 1, 1, 1},
		{"disabled-robot", "owner", "space-a", "OpenClaw", "self_hosted", 1, 0, 1, 1},
		{"removed-bot", "owner", "space-a", "OpenClaw", "self_hosted", 1, 1, 0, 1},
		{"human", "owner", "space-a", "OpenClaw", "self_hosted", 1, 1, 1, 0},
	} {
		exec("INSERT INTO user VALUES (?,?,?,?)", bot.uid, bot.uid, bot.robot, bot.userStatus)
		exec("INSERT INTO robot(robot_id,creator_uid,status,description,bot_commands,agent_platform,agent_hosting) VALUES (?,?,?,'description','[]',?,?)", bot.uid, bot.owner, bot.robotStatus, bot.platform, bot.hosting)
		exec("INSERT INTO space_member VALUES (?,?,?)", bot.space, bot.uid, bot.memberStatus)
	}
	// Prove the entire endpoint works without querying the unrelated platform column.
	exec("ALTER TABLE robot DROP COLUMN agent_platform")
	rb := &Robot{ctx: ctx, Log: log.NewTLog("OwnedBotsMetadataTest")}
	router := wkhttp.New()
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	loginUID := "owner"
	router.GET("/v1/robot/owned_bots", func(c *wkhttp.Context) {
		c.Set("uid", loginUID)
		rb.ownedBots(c)
	})
	get := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/v1/robot/owned_bots"+query, nil))
		return w
	}
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"default includes hosting", "?space_id=space-a", []string{"local", "cloud", "other-platform", "no-platform", "unknown-platform"}},
		{"unknown include is ignored", "?space_id=space-a&include=future", []string{"local", "cloud", "other-platform", "no-platform", "unknown-platform"}},
		{"old include cannot change owner", "?space_id=space-a&include=agent_metadata&owner_uid=other-owner", []string{"local", "cloud", "other-platform", "no-platform", "unknown-platform"}},
		{"space-bound", "?space_id=space-b", []string{"other-space"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := get(tc.query)
			require.Equal(t, 200, w.Code, w.Body.String())
			var rows []map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
			var ids []string
			for _, row := range rows {
				require.Len(t, row, 5)
				ids = append(ids, row["uid"].(string))
				for _, field := range []string{"uid", "name", "description", "bot_commands"} {
					require.Contains(t, row, field)
				}
				require.NotContains(t, row, "agent_platform")
				require.NotEmpty(t, row["agent_hosting"])
			}
			require.ElementsMatch(t, tc.want, ids)
		})
	}
	assertDenied := func(query string, semanticStatus int) {
		t.Helper()
		w := get(query)
		require.Equal(t, 400, w.Code, w.Body.String()) // Legacy D14 envelope.
		var envelope struct {
			Error struct {
				HTTPStatus int `json:"http_status"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
		require.Equal(t, semanticStatus, envelope.Error.HTTPStatus, w.Body.String())
		require.NotContains(t, w.Body.String(), "agent_hosting")
	}
	assertDenied("", 400)
	assertDenied("?space_id=unknown", 403)
	loginUID = "outsider"
	assertDenied("?space_id=space-a", 403)
	loginUID = "owner"
	exec("UPDATE space_member SET status=0 WHERE space_id='space-a' AND uid='owner'")
	assertDenied("?space_id=space-a", 403)
	exec("UPDATE space_member SET status=1 WHERE space_id='space-a' AND uid='owner'")
	exec("UPDATE space SET status=0 WHERE space_id='space-a'")
	assertDenied("?space_id=space-a", 403)
	exec("UPDATE space SET status=1 WHERE space_id='space-a'")
	exec("ALTER TABLE robot RENAME COLUMN agent_hosting TO missing_hosting")
	assertDenied("?space_id=space-a", 500)
}
