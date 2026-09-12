package bot_api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

// newBotAllMemberGuardCollationContext builds minimal tables containing only
// the columns consumed by the Bot guard. The Project table keeps
// utf8mb4_general_ci while the legacy group table uses utf8mb4_0900_ai_ci,
// matching the production mixed-collation shape that made the old JOIN fail.
func newBotAllMemberGuardCollationContext(t *testing.T, base *config.Context) *config.Context {
	t.Helper()
	addr := base.GetConfig().DB.MySQLAddr
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected MySQL DSN shape")
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}
	dbName := "octo_bot_guard_collation_" + strings.ReplaceAll(util.GenerUUID(), "-", "")

	admin, err := dbr.Open("mysql", addr[:slash+1]+opts, nil)
	require.NoError(t, err)
	adminSess := admin.NewSession(nil)
	exec := func(sess *dbr.Session, stmt string, args ...interface{}) {
		_, execErr := sess.UpdateBySql(stmt, args...).Exec()
		require.NoError(t, execErr, stmt)
	}
	exec(adminSess, "DROP DATABASE IF EXISTS `"+dbName+"`")
	exec(adminSess, "CREATE DATABASE `"+dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")

	conn, err := dbr.Open("mysql", addr[:slash+1]+dbName+opts, nil)
	require.NoError(t, err)
	sess := conn.NewSession(nil)
	exec(sess, "CREATE TABLE `octo_project` ("+
		"`project_id` VARCHAR(40) NOT NULL DEFAULT '', "+
		"`status` TINYINT NOT NULL DEFAULT 1, "+
		"`all_member_group_no` VARCHAR(40) NOT NULL DEFAULT '', "+
		"PRIMARY KEY (`project_id`)"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci")
	exec(sess, "CREATE TABLE `group` ("+
		"`group_no` VARCHAR(40) NOT NULL DEFAULT '', "+
		"`status` TINYINT NOT NULL DEFAULT 1, "+
		"`project_id` VARCHAR(40) NOT NULL DEFAULT '', "+
		"PRIMARY KEY (`group_no`)"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci")

	cfg := *base.GetConfig()
	cfg.Test = true
	cfg.DB.Migration = false
	cfg.DB.MySQLAddr = addr[:slash+1] + dbName + opts
	cfg.DB.MySQLMaxOpenConns = 2
	cfg.DB.MySQLMaxIdleConns = 1
	ctx := config.NewContext(&cfg)
	t.Cleanup(func() {
		_ = ctx.DB().DB.Close()
		_ = conn.Close()
		_, _ = adminSess.UpdateBySql("DROP DATABASE IF EXISTS `" + dbName + "`").Exec()
		_ = admin.Close()
	})
	return ctx
}

func seedBotAllMemberGuardCollationFixture(t *testing.T, ctx *config.Context) (projectID, dedicatedNo, ordinaryNo string) {
	t.Helper()
	projectID = "project-bot-guard-collation-" + util.GenerUUID()[:8]
	dedicatedNo = "group-bot-dedicated-" + util.GenerUUID()[:8]
	ordinaryNo = "group-bot-ordinary-" + util.GenerUUID()[:8]

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project` (project_id, status, all_member_group_no) VALUES (?, 1, ?)",
		projectID, dedicatedNo,
	).Exec()
	require.NoError(t, err)
	for _, groupNo := range []string{dedicatedNo, ordinaryNo} {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO `group` (group_no, status, project_id) VALUES (?, 1, ?)",
			groupNo, projectID,
		).Exec()
		require.NoError(t, err)
	}
	return projectID, dedicatedNo, ordinaryNo
}

func TestBotAllMemberGuardSurvivesMixedCollation(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.DB.Migration = false
	cfg.DB.MySQLMaxOpenConns = 2
	cfg.DB.MySQLMaxIdleConns = 1
	base := config.NewContext(cfg)
	t.Cleanup(func() { _ = base.DB().DB.Close() })
	ctx := newBotAllMemberGuardCollationContext(t, base)
	_, dedicatedNo, ordinaryNo := seedBotAllMemberGuardCollationFixture(t, ctx)
	ba := &BotAPI{ctx: ctx, db: newBotAPIDB(ctx), Log: log.NewTLog("BotAPI-all-member-collation-test")}

	probe := func(groupNo string) int {
		r := helperHarness(func(c *wkhttp.Context) {
			ba.refuseIfAllMemberGroup(c, groupNo)
		})
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	require.Equal(t, http.StatusForbidden, probe(dedicatedNo),
		"the dedicated group must remain protected when group uses 0900_ai_ci")
	require.Equal(t, http.StatusForbidden, probe(strings.ToUpper(dedicatedNo)),
		"case-insensitive group lookup must not bypass dedicated protection")
	_, err := ctx.DB().Exec("ALTER TABLE `group` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, probe(dedicatedNo+" "),
		"PAD SPACE group lookup must not bypass dedicated protection")
	require.Equal(t, http.StatusOK, probe(ordinaryNo),
		"a normal project-linked group must remain mutable when group uses 0900_ai_ci")
}
