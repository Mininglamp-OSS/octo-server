package group

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

// newAllMemberGuardCollationContext clones only the two tables consumed by the
// guard into a disposable database, then gives group's guard a connection pool
// pointed at it. The source Project table keeps utf8mb4_general_ci while the
// legacy group table is deliberately converted to utf8mb4_0900_ai_ci, matching
// the production shape that exposed MySQL error 1267 in the old JOIN.
func newAllMemberGuardCollationContext(t *testing.T, base *config.Context) *config.Context {
	t.Helper()
	addr := base.GetConfig().DB.MySQLAddr
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected MySQL DSN shape")
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}
	dbName := "octo_group_guard_collation_" + strings.ReplaceAll(util.GenerUUID(), "-", "")

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
	var sourceSchema string
	require.NoError(t, base.DB().SelectBySql("SELECT DATABASE()").LoadOne(&sourceSchema))
	require.NotEmpty(t, sourceSchema)
	for _, table := range []string{"octo_project", "group"} {
		exec(sess, "CREATE TABLE `"+table+"` LIKE `"+sourceSchema+"`.`"+table+"`")
	}
	exec(sess, "ALTER TABLE `group` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")

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

func seedAllMemberGuardCollationFixture(t *testing.T, ctx *config.Context) (projectID, dedicatedNo, ordinaryNo string) {
	t.Helper()
	projectID = "project-guard-collation-" + util.GenerUUID()[:8]
	dedicatedNo = "group-guard-dedicated-" + util.GenerUUID()[:8]
	ordinaryNo = "group-guard-ordinary-" + util.GenerUUID()[:8]
	spaceID := "space-guard-collation-" + util.GenerUUID()[:8]

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `octo_project` (project_id, space_id, name, creator, status, all_member_group_no, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 1, ?, NOW(3), NOW(3))",
		projectID, spaceID, projectID, "guard-owner", dedicatedNo,
	).Exec()
	require.NoError(t, err)
	for _, groupNo := range []string{dedicatedNo, ordinaryNo} {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) VALUES (?, ?, ?, 1, ?, ?)",
			groupNo, groupNo, "guard-owner", spaceID, projectID,
		).Exec()
		require.NoError(t, err)
	}
	return projectID, dedicatedNo, ordinaryNo
}

func TestGroupAllMemberGuardSurvivesMixedCollation(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	cfg.DB.Migration = false
	cfg.DB.MySQLMaxOpenConns = 2
	cfg.DB.MySQLMaxIdleConns = 1
	base := config.NewContext(cfg)
	t.Cleanup(func() { _ = base.DB().DB.Close() })
	ctx := newAllMemberGuardCollationContext(t, base)
	projectID, dedicatedNo, ordinaryNo := seedAllMemberGuardCollationFixture(t, ctx)
	g := &Group{ctx: ctx, Log: log.NewTLog("Group-all-member-collation-test")}

	probe := func(groupNo string) int {
		r := helperHarness(func(c *wkhttp.Context) {
			g.refuseIfAllMemberGroupFields(c, groupNo, projectID, allMemberGroupActionExit)
		})
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	require.Equal(t, http.StatusBadRequest, probe(dedicatedNo),
		"the dedicated group must remain protected when group uses 0900_ai_ci")
	require.Equal(t, http.StatusOK, probe(ordinaryNo),
		"a normal project-linked group must remain mutable when group uses 0900_ai_ci")
}
