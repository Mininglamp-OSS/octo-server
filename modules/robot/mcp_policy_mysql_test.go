package robot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/mcppolicy"
	"github.com/go-sql-driver/mysql"
	migrate "github.com/rubenv/sql-migrate"
)

func TestMCPPolicyHandlersMySQL(t *testing.T) {
	dsn := os.Getenv("MCP_POLICY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set MCP_POLICY_TEST_MYSQL_DSN to a disposable MySQL 8 server")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	cfg.DBName = ""
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("mcp_policy_test_http_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	appcfg := config.New()
	appcfg.DB.MySQLAddr = cfg.FormatDSN()
	appcfg.DB.MySQLMaxOpenConns = 4
	appcfg.DB.MySQLMaxIdleConns = 4
	ctx := config.NewContext(appcfg)
	db := ctx.DB().DB
	t.Cleanup(func() {
		db.Close()
		_, err := admin.Exec("DROP DATABASE `" + name + "`")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	raw, err := sqlFS.ReadFile("sql/20261030000001_mcp_context_policy.sql")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.ParseMigration("20261030000001_mcp_context_policy.sql", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Exec(db, "mysql", &migrate.MemoryMigrationSource{Migrations: []*migrate.Migration{m}}, migrate.Up); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE robot(robot_id VARCHAR(40) PRIMARY KEY,creator_uid VARCHAR(40),status INT) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO robot VALUES('bot_B','owner_U',1),('disabled_B','owner_U',0)"); err != nil {
		t.Fatal(err)
	}
	rb := &Robot{ctx: ctx, Log: log.NewTLog("mcp-test")}
	call := func(method, uid, actor string, body io.Reader) *httptest.ResponseRecorder {
		r := wkhttp.New()
		r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
		g := r.Group("/v1", func(c *wkhttp.Context) { c.Set("uid", actor); c.Set("role", string(wkhttp.SuperAdmin)); c.Next() })
		g.GET("/robot/:robot_id/mcp-policy", rb.getMCPPolicy)
		g.PUT("/robot/:robot_id/mcp-policy", rb.putMCPPolicy)
		g.DELETE("/robot/:robot_id/mcp-policy", rb.deleteMCPPolicy)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, "/v1/robot/"+uid+"/mcp-policy", body))
		return rec
	}
	for _, tc := range []struct {
		uid, actor string
		want       int
	}{
		{"bot_B", "owner_U", 200}, {"bot_B", "nonowner_U", 403}, {"bot_B", "group_admin", 403}, {"bot_B", "superadmin_U", 403},
		{"BOT_B", "owner_U", 404}, {"disabled_B", "owner_U", 404}, {"missing_B", "owner_U", 404},
	} {
		rec := call("GET", tc.uid, tc.actor, nil)
		if rec.Code != tc.want || rec.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("%+v: %d %s", tc, rec.Code, rec.Body.String())
		}
	}
	// Even malformed JSON cannot expose field validation before ownership.
	if rec := call("PUT", "bot_B", "nonowner_U", strings.NewReader(`{}`)); rec.Code != 403 {
		t.Fatal(rec.Body.String())
	}
	if rec := call("PUT", "bot_B", "owner_U", strings.NewReader(`{}`)); rec.Code != 400 || !strings.Contains(rec.Body.String(), "err.server.mcp.policy_invalid") {
		t.Fatal(rec.Body.String())
	}
	rec := call("PUT", "bot_B", "owner_U", strings.NewReader(`{"context_mode":"compatible","if_generation":"1"}`))
	var p mcppolicy.BotPolicy
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &p) != nil || p.Generation != "2" || p.BotUID != "bot_B" || p.Override == nil {
		t.Fatal(rec.Body.String())
	}
	if rec := call("PUT", "bot_B", "owner_U", strings.NewReader(`{"context_mode":"strict","if_generation":"1"}`)); rec.Code != 400 || decodeRobotEnvelope(t, rec.Body.Bytes()).Error.HTTPStatus != 409 {
		t.Fatal(rec.Body.String())
	}
	// Change creator only after the external owner check, while reading body.
	body := &mcpReadHook{Reader: strings.NewReader(`{"context_mode":"strict","if_generation":"2"}`), hook: func() {
		if _, err := db.Exec("UPDATE robot SET creator_uid='other_U' WHERE robot_id='bot_B'"); err != nil {
			t.Fatal(err)
		}
	}}
	if rec := call("PUT", "bot_B", "owner_U", body); rec.Code != 403 {
		t.Fatal("transaction owner recheck missing", rec.Body.String())
	}
	store := mcppolicy.Store{Session: ctx.DB()}
	if p, err := store.Resolve(context.Background(), "bot_B"); err != nil || p.Generation != "2" || p.ContextMode != mcppolicy.Compatible {
		t.Fatalf("unauthorized change: %+v %v", p, err)
	}
	if _, err := db.Exec("UPDATE robot SET creator_uid='owner_U' WHERE robot_id='bot_B'"); err != nil {
		t.Fatal(err)
	}
	rec = call("DELETE", "bot_B", "owner_U", strings.NewReader(`{"if_generation":"2"}`))
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &p) != nil || p.Generation != "3" || p.Override != nil {
		t.Fatal(rec.Body.String())
	}
}

type mcpReadHook struct {
	io.Reader
	hook func()
}

func (r *mcpReadHook) Read(p []byte) (int, error) {
	if r.hook != nil {
		hook := r.hook
		r.hook = nil
		hook()
	}
	return r.Reader.Read(p)
}
