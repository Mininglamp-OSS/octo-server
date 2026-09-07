package project

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testSrv *server.Server
	testCtx *config.Context
	testDB  *DB
)

// TestMain builds the shared test server for this package.
//
// No fixture DDLs. The external test package in this directory
// (project_external_test.go, package project_test) blank-imports
// octo-server/internal, and an in-package and an external test package compile into ONE
// test binary — so register.GetModules already holds every module here, and module.Setup
// applies the whole schema. Creating fixture tables on top of that is not merely
// redundant, it FAILS: the fixtures would win the race to CREATE TABLE `group` and the
// real migration would then abort with MySQL 1050.
//
// The two packages are therefore coupled through the binary, which is worth stating
// because deleting project_external_test.go would silently take the schema with it. It is
// also why there is exactly one TestMain here rather than one per package: a test binary
// may declare only one.
//
// OCTO_MASTER_KEY must be set before NewTestServer: modules/common (pulled in by the full
// registry, and also by modules/space which this package imports directly) encrypts the
// app config during Route() and panics without a key.
func TestMain(m *testing.M) {
	gin.SetMode(gin.ReleaseMode)
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		_ = os.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	}
	// The feature gate is fail-closed, so every write case would 403 without this. The
	// OFF behaviour is asserted by a case that flips p.cfg on a private router.
	_ = os.Setenv(envCreateEnabled, "true")

	srv, ctx := testutil.NewTestServer()
	// testutil.NewTestServer does not install an ErrorRenderer (main.go does), and without
	// one the route falls back to the legacy {msg,status} body with no error.code — every
	// assertion on a code would silently compare against "".
	srv.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	testSrv, testCtx = srv, ctx
	testDB = NewDB(ctx)
	os.Exit(m.Run())
}

// resetUIDRateLimit clears the shared UID token buckets.
//
// SharedUIDRateLimiter's buckets live in Redis under ratelimit:uid:* and are NOT
// cleared by CleanAllTables, so they accumulate across cases and a later case gets a
// 429 that has nothing to do with what it is testing.
func resetUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	cfg := ctx.GetConfig()
	client := redis.NewClient(&redis.Options{Addr: cfg.DB.RedisAddr, Password: cfg.DB.RedisPass})
	defer client.Close()
	keys, err := client.Keys("ratelimit:uid:*").Result()
	if err != nil {
		require.NoError(t, err, "Redis 不可用：本包测试需要 Redis，降级为静默跳过会让整包假绿")
	}
	if len(keys) > 0 {
		require.NoError(t, client.Del(keys...).Err())
	}
}

// redisKeys returns the keys matching pattern, for the namespace assertions.
func redisKeys(t *testing.T, pattern string) []string {
	t.Helper()
	cfg := testCtx.GetConfig()
	client := redis.NewClient(&redis.Options{Addr: cfg.DB.RedisAddr, Password: cfg.DB.RedisPass})
	defer client.Close()
	keys, err := client.Keys(pattern).Result()
	if err != nil {
		require.NoError(t, err, "Redis 不可用：本包测试需要 Redis，降级为静默跳过会让整包假绿")
	}
	return keys
}

// flushProjectCache clears this module's own membership cache between cases so a
// cached role from a previous case cannot answer for a fresh one.
func flushProjectCache(t *testing.T, ctx *config.Context) {
	t.Helper()
	cfg := ctx.GetConfig()
	client := redis.NewClient(&redis.Options{Addr: cfg.DB.RedisAddr, Password: cfg.DB.RedisPass})
	defer client.Close()
	for _, pattern := range []string{"project:member:*", "space:member:*"} {
		keys, err := client.Keys(pattern).Result()
		if err != nil {
			require.NoError(t, err, "Redis 不可用：本包测试需要 Redis，降级为静默跳过会让整包假绿")
		}
		if len(keys) > 0 {
			require.NoError(t, client.Del(keys...).Err())
		}
	}
}

// setup returns a clean server plus a fresh Project instance.
func setup(t *testing.T) (*server.Server, *Project) {
	t.Helper()
	require.NoError(t, testutil.CleanAllTables(testCtx))
	resetUIDRateLimit(t, testCtx)
	flushProjectCache(t, testCtx)
	// `cursors` is package-global and survives between cases, while CI runs -shuffle=on. A
	// truncated rotation left behind by whichever case ran before makes an "expect 0" reconcile
	// assertion pass for the wrong reason (PR #841 round 4, P2-3). Reset it here so no case has
	// to remember to.
	resetCursorsForTest()
	p := New(testCtx)
	p.cfg.CreateEnabled = true
	return testSrv, p
}

// ---------- fixtures ----------

const (
	spaceA = "space_a"
	spaceB = "space_b"
)

// seedUser creates a user row and a token so the uid can authenticate.
//
// short_no must be distinct: the real `user` table carries a UNIQUE index on it
// (short_no_udx), so two rows defaulting to ” collide with MySQL 1062. Setting it to the
// uid keeps the fixture honest without inventing a second identifier.
func seedUser(t *testing.T, uid string) string {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, "user-"+uid, uid,
	).Exec()
	require.NoError(t, err)
	token := "tok-" + uid + "-" + util.GenerUUID()[:8]
	require.NoError(t, testCtx.Cache().Set(
		testCtx.GetConfig().Cache.TokenCachePrefix+token, uid+"@test"))
	return token
}

// seedSpace creates an active Space.
func seedSpace(t *testing.T, spaceID string, status int) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, ?)",
		spaceID, spaceID, "creator", status,
	).Exec()
	require.NoError(t, err)
}

// seedSpaceMember gives uid a seat in spaceID.
func seedSpaceMember(t *testing.T, spaceID, uid string, role, status int) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, ?, ?)",
		spaceID, uid, role, status,
	).Exec()
	require.NoError(t, err)
}

// rejoinSpaceMember reactivates a previously-removed seat.
//
// A separate helper from seedSpaceMember because removeSpaceMember flips status rather than
// deleting the row (matching what the Space module actually does), so re-seeding would hit the
// unique key on (space_id, uid) with error 1062 instead of modelling a rejoin.
func rejoinSpaceMember(t *testing.T, spaceID, uid string) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE space_member SET status = 1 WHERE space_id = ? AND uid = ?", spaceID, uid,
	).Exec()
	require.NoError(t, err)
}

// setSpaceStatus flips a Space's status (1 normal, 2 banned, 0 disbanded).
func setSpaceStatus(t *testing.T, spaceID string, status int) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql("UPDATE `space` SET status = ? WHERE space_id = ?",
		status, spaceID).Exec()
	require.NoError(t, err)
}

// removeSpaceMember flips a space_member seat to removed, WITHOUT enqueueing a cleanup
// job — the raw state a Space removal leaves behind once the job has run, and the state
// the reconcile job must flag when no job exists.
func removeSpaceMember(t *testing.T, spaceID, uid string) {
	t.Helper()
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?", spaceID, uid).Exec()
	require.NoError(t, err)
}

// ---------- HTTP helpers ----------

func doJSON(t *testing.T, srv *server.Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequest(method, path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("token", token)
	}
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

// mountProject installs p's routes on a private router so each case gets a clean
// chain. The shared testSrv router already carries the routes registered by
// module.Setup; a private router is what lets a case swap p.cfg (the feature gate)
// without racing other cases.
func mountProject(t *testing.T, p *Project) *wkhttp.WKHttp {
	t.Helper()
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	// No SetTokenParser: AuthMiddleware falls back to octo-lib's legacyTokenParser,
	// which reads "{prefix}{token}" from the same Redis cache seedUser writes to. That
	// is also what the shared test server uses, so tokens work identically on both.
	p.Route(r)
	return r
}

func doOn(t *testing.T, r *wkhttp.WKHttp, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequest(method, path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("token", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeResp(t *testing.T, w *httptest.ResponseRecorder) *Resp {
	t.Helper()
	var resp Resp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body: %s", w.Body.String())
	return &resp
}

// createProjectVia creates a project through the HTTP surface, exercising the real DAO
// (which is what the "insert through the real DAO succeeds with the generated
// active_name column present" acceptance needs — a hand-written INSERT would not).
func createProjectVia(t *testing.T, srv *server.Server, spaceID, token, name string) *Resp {
	t.Helper()
	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceID+"/projects", token,
		map[string]any{"name": name})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	return decodeResp(t, w)
}

func epochOf(t *testing.T, projectID string) int64 {
	t.Helper()
	row, err := testDB.queryByProjectID(projectID)
	require.NoError(t, err)
	require.NotNil(t, row)
	return row.MemberEpoch
}

// ---------- schema ----------

// TestSchemaColumnsMatchSpaceAndUser is the 1267 guard. The new tables pin
// utf8mb4_general_ci while space / space_member / user declare no COLLATE and inherit
// the database default, so a JOIN across them is only safe when those actually agree.
func TestSchemaColumnsMatchSpaceAndUser(t *testing.T) {
	setup(t)
	type colInfo struct {
		Table     string `db:"table_name"`
		Column    string `db:"column_name"`
		Type      string `db:"column_type"`
		Collation string `db:"collation_name"`
	}
	var cols []*colInfo
	_, err := testCtx.DB().SelectBySql(
		"SELECT LOWER(TABLE_NAME) AS table_name, LOWER(COLUMN_NAME) AS column_name, " +
			"COLUMN_TYPE AS column_type, COLLATION_NAME AS collation_name " +
			"FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND (" +
			"  (TABLE_NAME='space' AND COLUMN_NAME='space_id') OR " +
			"  (TABLE_NAME='space_member' AND COLUMN_NAME IN ('space_id','uid')) OR " +
			"  (TABLE_NAME='user' AND COLUMN_NAME='uid') OR " +
			"  (TABLE_NAME='octo_project' AND COLUMN_NAME IN ('space_id','project_id','creator')) OR " +
			"  (TABLE_NAME='octo_project_member' AND COLUMN_NAME IN ('space_id','uid','project_id')))",
	).Load(&cols)
	require.NoError(t, err)
	require.NotEmpty(t, cols)

	// Both project tables must be present: NotEmpty alone would pass if
	// octo_project_member were renamed away and only the legacy rows came back.
	tables := map[string]bool{}
	for _, c := range cols {
		tables[c.Table] = true
		assert.Equal(t, "varchar(40)", c.Type, "%s.%s width must match space/user", c.Table, c.Column)
		assert.Equal(t, "utf8mb4_general_ci", c.Collation,
			"%s.%s collation must match space/user or every reconcile JOIN hits MySQL 1267",
			c.Table, c.Column)
	}
	assert.True(t, tables["octo_project"], "octo_project columns must be in the result")
	assert.True(t, tables["octo_project_member"], "octo_project_member columns must be in the result")
}

// TestJoinAcrossSpaceMemberAndUserHasNoCollationError runs the shape the reconcile scan
// uses. A 1267 would surface here rather than in production at 3am.
func TestJoinAcrossSpaceMemberAndUserHasNoCollationError(t *testing.T) {
	setup(t)
	var count int
	err := testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member` pm " +
			"INNER JOIN space_member sm ON sm.space_id = pm.space_id AND sm.uid = pm.uid " +
			"INNER JOIN `user` u ON u.uid = pm.uid " +
			"INNER JOIN `space` s ON s.space_id = pm.space_id",
	).LoadOne(&count)
	require.NoError(t, err, "collation mismatch across the reconcile JOIN (MySQL 1267)")
}

// TestGeneratedActiveNameFreesTheNameOnDisband covers D4 end to end, through the real
// DAO: create -> disband -> recreate the same name succeeds, and a duplicate ACTIVE
// name is rejected with the registered code. It also proves no statement names the
// generated column (that would be MySQL 3105).
func TestGeneratedActiveNameFreesTheNameOnDisband(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	first := createProjectVia(t, srv, spaceA, token, "Q3 delivery")

	// A duplicate ACTIVE name is rejected.
	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": "Q3 delivery"})
	assertProjectErrorCode(t, w, "err.server.project.name_duplicated")

	// Disband, then the name is free again.
	w = doJSON(t, srv, http.MethodDelete, "/v1/projects/"+first.ProjectID, token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	second := createProjectVia(t, srv, spaceA, token, "Q3 delivery")
	assert.NotEqual(t, first.ProjectID, second.ProjectID)

	// And a second disbanded row with the same name coexists with the first: repeated
	// NULLs do not collide in a unique index.
	w = doJSON(t, srv, http.MethodDelete, "/v1/projects/"+second.ProjectID, token, nil)
	require.Equal(t, http.StatusOK, w.Code)
	third := createProjectVia(t, srv, spaceA, token, "Q3 delivery")
	assert.NotEqual(t, second.ProjectID, third.ProjectID)
}

// TestUpdateThroughRealDAOSucceedsWithGeneratedColumn is the 3105 guard on the UPDATE
// side: a struct-driven SET clause would name active_name and fail.
func TestUpdateThroughRealDAOSucceedsWithGeneratedColumn(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "before")

	w := doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID, token,
		map[string]any{"name": "after", "description": "d", "discoverability": DiscoverabilityUnlisted})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	updated := decodeResp(t, w)
	assert.Equal(t, "after", updated.Name)
	assert.Equal(t, DiscoverabilityUnlisted, updated.Discoverability)
}

// TestIsOfficialStaysZeroThroughCRUD is the behavioural half of D6.
func TestIsOfficialStaysZeroThroughCRUD(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	other := seedUser(t, "member1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceA, "member1", 0, 1)

	created := createProjectVia(t, srv, spaceA, token, "p")
	doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID, token,
		map[string]any{"name": "p2"})
	doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add", token,
		map[string]any{"uids": []string{"member1"}})
	doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID+"/members/member1/role", token,
		map[string]any{"role": RoleAdmin})
	doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/leave", other, nil)
	doJSON(t, srv, http.MethodDelete, "/v1/projects/"+created.ProjectID, token, nil)

	flags, err := testDB.queryIsOfficialFlags(spaceA)
	require.NoError(t, err)
	require.NotEmpty(t, flags)
	for _, f := range flags {
		assert.Equal(t, 0, f.IsOfficial, "is_official must never be written in P0 (D6)")
	}
}

// TestMigrationUpDownUpLeavesNoResidue drives the migration file's OWN Up and Down
// sections, in both directions, twice.
//
// The earlier version of this case substituted two hand-written `DROP TABLE IF EXISTS`
// statements for the Down section. That verifies that *a* drop is complete, not that the
// migration's Down is: a Down that forgot a table, or ordered its drops so a constraint
// blocked one, would have passed. Reading Down from the same embedded file as Up is what
// makes the assertion about the artifact that actually ships.
//
// The second up→down→up lap is not redundant. The first Up runs against a database
// sql-migrate already built; only a lap that starts from the state Down left can show
// that Down is a true inverse — the STORED generated column and the unique index over it
// are the parts that would survive a partial drop and then collide on re-create.
func TestMigrationUpDownUpLeavesNoResidue(t *testing.T) {
	setup(t)
	db, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true")
	require.NoError(t, err)
	defer db.Close()

	tableCount := func() int {
		var n int
		require.NoError(t, db.QueryRow(
			"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() "+
				"AND TABLE_NAME IN ('octo_project','octo_project_member')").Scan(&n))
		return n
	}
	// generatedColumns counts the STORED generated column, and indexOverGenerated the
	// unique index built on it. Table presence alone would not notice either going missing.
	generatedColumns := func() int {
		var n int
		require.NoError(t, db.QueryRow(
			"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() "+
				"AND TABLE_NAME = 'octo_project' AND COLUMN_NAME = 'active_name' "+
				"AND EXTRA LIKE '%GENERATED%'").Scan(&n))
		return n
	}
	indexOverGenerated := func() int {
		var n int
		require.NoError(t, db.QueryRow(
			"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() "+
				"AND TABLE_NAME = 'octo_project' AND INDEX_NAME = 'uk_octo_project_space_active_name'").Scan(&n))
		return n
	}

	assertApplied := func(stage string) {
		assert.Equal(t, 2, tableCount(), "%s: both tables must exist", stage)
		assert.Equal(t, 1, generatedColumns(), "%s: active_name must be a generated column", stage)
		assert.Greater(t, indexOverGenerated(), 0, "%s: the unique index over active_name must exist", stage)
	}
	assertGone := func(stage string) {
		assert.Equal(t, 0, tableCount(), "%s: the Down section must leave no residue", stage)
		assert.Equal(t, 0, generatedColumns(), "%s: no generated column may survive Down", stage)
		assert.Equal(t, 0, indexOverGenerated(), "%s: no index may survive Down", stage)
	}

	// Starting state: sql-migrate applied Up during setup.
	assertApplied("after sql-migrate Up")

	for lap := 1; lap <= 2; lap++ {
		applyProjectMigration(t, db, migrateDown)
		assertGone(fmt.Sprintf("lap %d after Down", lap))
		applyProjectMigration(t, db, migrateUp)
		assertApplied(fmt.Sprintf("lap %d after Up", lap))
	}

	// A row must be insertable through the real DAO on the re-created schema — the point of
	// re-applying rather than just counting tables. setup() first: it truncates data (the
	// tables the loop just rebuilt stay), and seeding before it would be wiped.
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "afterMigrate")
	seedSpaceMember(t, spaceA, "afterMigrate", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "after-migrate")
	assert.NotEmpty(t, created.ProjectID, "the re-created schema must accept a real insert")
}

// migrateUp / migrateDown select which section of the migration file to execute.
type migrateDirection int

const (
	migrateUp migrateDirection = iota
	migrateDown
)

// projectMigrationFiles are this module's migration files, in APPLY order.
//
// EDITED IN P1. The helper below used to read one hard-coded filename, which
// encoded the assumption that modules/project has exactly one migration. P1
// breaks that assumption by adding octo_project_member.removing and the cascade
// outbox in a second file, and the consequence was not subtle: the Down/Up lap
// dropped both tables and rebuilt them from P0's file alone, so
// octo_project_member came back WITHOUT `removing`, and every test that ran
// after this one in the same binary failed with
// `Error 1054: Unknown column 'removing'`. 82 failures from one stale helper.
//
// Down is applied in REVERSE order for the usual reason: the later file's Down
// drops objects the earlier file's Down would otherwise pull out from under it.
var projectMigrationFiles = []string{
	"sql/20260904000001_project_core.sql",
	"sql/20260906000001_project_group_binding.sql",
}

// applyProjectMigration executes one section of every migration file this module
// ships.
//
// Splitting on the `-- +migrate Up` / `-- +migrate Down` markers is safe for THESE
// files specifically: they use neither StatementBegin/StatementEnd nor a semicolon
// inside any string literal, both of which are asserted below so a future edit that
// breaks the assumption fails here rather than silently executing a truncated
// statement.
func applyProjectMigration(t *testing.T, db *sql.DB, dir migrateDirection) {
	t.Helper()
	files := append([]string(nil), projectMigrationFiles...)
	if dir == migrateDown {
		for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
			files[i], files[j] = files[j], files[i]
		}
	}
	for _, name := range files {
		applyOneProjectMigrationFile(t, db, name, dir)
	}
}

func applyOneProjectMigrationFile(t *testing.T, db *sql.DB, name string, dir migrateDirection) {
	t.Helper()
	raw, err := sqlFS.ReadFile(name)
	require.NoError(t, err)
	body := string(raw)

	require.NotContains(t, body, "StatementBegin",
		"this helper splits on semicolons; a StatementBegin block would be split wrongly")
	for _, lit := range regexp.MustCompile(`'[^']*'`).FindAllString(body, -1) {
		require.NotContains(t, lit, ";",
			"a semicolon inside a string literal breaks the naive split: %s", lit)
	}

	downAt := mustIndex(t, body, "-- +migrate Down")
	section := body[:downAt]
	if dir == migrateDown {
		section = body[downAt:]
	}
	for _, stmt := range splitSQLStatements(section) {
		if stmt == "" {
			continue
		}
		_, err := db.Exec(stmt)
		require.NoError(t, err, "stmt: %s", stmt)
	}
}

func mustIndex(t *testing.T, s, sub string) int {
	t.Helper()
	i := bytes.Index([]byte(s), []byte(sub))
	require.GreaterOrEqual(t, i, 0, "missing %q in migration", sub)
	return i
}

// splitSQLStatements splits on ';' at end of line, dropping comment-only lines.
func splitSQLStatements(body string) []string {
	var stmts []string
	var cur bytes.Buffer
	for _, line := range bytes.Split([]byte(body), []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("--")) {
			continue
		}
		cur.Write(line)
		cur.WriteByte('\n')
		if bytes.HasSuffix(trimmed, []byte(";")) {
			stmts = append(stmts, string(bytes.TrimSpace(bytes.TrimSuffix(bytes.TrimSpace(cur.Bytes()), []byte(";")))))
			cur.Reset()
		}
	}
	return stmts
}

// ---------- authorization / enumeration ----------

// TestRoutesRejectWithoutSpaceHeaderOrQuery pins that the Project routes do NOT inherit
// SpaceMiddleware's pass-through: it reads only ?space_id and X-Space-ID and calls
// c.Next() when neither is present, so a path-parameter route mounted under it would be
// wide open. Every route here is called with neither.
func TestRoutesRejectWithoutSpaceHeaderOrQuery(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	insider := seedUser(t, "insider")
	outsider := seedUser(t, "outsider")
	seedSpaceMember(t, spaceA, "insider", 0, 1)
	seedSpaceMember(t, spaceB, "outsider", 0, 1)

	created := createProjectVia(t, srv, spaceA, insider, "p")

	routes := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/v1/space/" + spaceA + "/projects", map[string]any{"name": "x"}},
		{http.MethodGet, "/v1/space/" + spaceA + "/projects", nil},
		{http.MethodGet, "/v1/projects/" + created.ProjectID, nil},
		{http.MethodPut, "/v1/projects/" + created.ProjectID, map[string]any{"name": "x"}},
		{http.MethodDelete, "/v1/projects/" + created.ProjectID, nil},
		{http.MethodGet, "/v1/projects/" + created.ProjectID + "/members", nil},
		{http.MethodPost, "/v1/projects/" + created.ProjectID + "/members/add", map[string]any{"uids": []string{"x"}}},
		{http.MethodPost, "/v1/projects/" + created.ProjectID + "/members/remove", map[string]any{"uids": []string{"x"}}},
		{http.MethodPost, "/v1/projects/" + created.ProjectID + "/leave", nil},
		{http.MethodPut, "/v1/projects/" + created.ProjectID + "/members/x/role", map[string]any{"role": 0}},
	}

	for _, rt := range routes {
		t.Run("anonymous "+rt.method+" "+rt.path, func(t *testing.T) {
			w := doJSON(t, srv, rt.method, rt.path, "", rt.body)
			env := decodeProjectEnvelope(t, w.Body.Bytes())
			// AuthMiddleware answers first for a tokenless request. The envelope must carry a
			// REGISTERED code — the earlier form only asserted NotEqual(200) and discarded env,
			// so deleting the middleware (which routes the request to the 500 fallback, also
			// wire-400) kept every case green (yujiawei Q9).
			assert.NotEqual(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.NotEmpty(t, env.Error.Code, "the refusal must carry a registered error code")
		})
		t.Run("foreign space "+rt.method+" "+rt.path, func(t *testing.T) {
			w := doJSON(t, srv, rt.method, rt.path, outsider, rt.body)
			require.NotEqual(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			env := decodeProjectEnvelope(t, w.Body.Bytes())
			// The cross-Space case must be the folded not-found answer, not an auth failure:
			// the caller IS authenticated, the project is simply invisible.
			assert.Contains(t, []string{"err.server.project.not_found", "err.shared.auth.forbidden"},
				env.Error.Code, "body: %s", w.Body.String())
		})
	}
}

// TestUnlistedProjectIsIndistinguishableFromNonexistent pins the anti-enumeration
// contract: a non-member must not be able to tell an unlisted project apart from one
// that never existed, while members and Space admins get the real payload.
func TestUnlistedProjectIsIndistinguishableFromNonexistent(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	strangerTok := seedUser(t, "stranger")
	adminTok := seedUser(t, "spaceadmin")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceA, "stranger", 0, 1)
	seedSpaceMember(t, spaceA, "spaceadmin", 1, 1)

	created := createProjectVia(t, srv, spaceA, ownerTok, "secret")
	w := doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID, ownerTok,
		map[string]any{"discoverability": DiscoverabilityUnlisted})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	unlisted := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, strangerTok, nil)
	nonexistent := doJSON(t, srv, http.MethodGet, "/v1/projects/"+util.GenerUUID(), strangerTok, nil)

	assert.Equal(t, nonexistent.Code, unlisted.Code)
	assert.JSONEq(t, nonexistent.Body.String(), unlisted.Body.String(),
		"an unlisted project must be byte-identical to a nonexistent one for a non-member")
	assertProjectErrorCode(t, unlisted, "err.server.project.not_found")

	// A member sees it.
	member := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, member.Code, "body: %s", member.Body.String())
	// And so does a Space admin who never joined.
	admin := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, adminTok, nil)
	require.Equal(t, http.StatusOK, admin.Code, "body: %s", admin.Body.String())
	assert.Equal(t, roleNonMember, decodeResp(t, admin).MyRole)
}

// TestCrossSpaceProjectLooksNonexistent covers the second anti-enumeration case: a
// caller in Space B probing a real project id from Space A.
func TestCrossSpaceProjectLooksNonexistent(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	ownerTok := seedUser(t, "owner1")
	foreignTok := seedUser(t, "foreigner")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceB, "foreigner", 0, 1)

	created := createProjectVia(t, srv, spaceA, ownerTok, "p")
	real := doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, foreignTok, nil)
	fake := doJSON(t, srv, http.MethodGet, "/v1/projects/"+util.GenerUUID(), foreignTok, nil)
	assert.Equal(t, fake.Code, real.Code)
	assert.JSONEq(t, fake.Body.String(), real.Body.String())
}

// ---------- feature gate ----------

// TestCreateDisabledFreezesWritesButNotReads pins D1's shape: with the flag off every
// write returns 403 with the registered code while list and detail still work, so
// existing data stays observable during a rollback.
func TestCreateDisabledFreezesWritesButNotReads(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	other := seedUser(t, "member1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceA, "member1", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "p")
	_ = other

	// Flag OFF on a private router, so the shared one keeps working for other cases.
	p.cfg.CreateEnabled = false
	r := mountProject(t, p)

	writes := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/v1/space/" + spaceA + "/projects", map[string]any{"name": "n"}},
		{http.MethodPut, "/v1/projects/" + created.ProjectID, map[string]any{"name": "n"}},
		{http.MethodDelete, "/v1/projects/" + created.ProjectID, nil},
		{http.MethodPost, "/v1/projects/" + created.ProjectID + "/members/add", map[string]any{"uids": []string{"member1"}}},
		{http.MethodPost, "/v1/projects/" + created.ProjectID + "/members/remove", map[string]any{"uids": []string{"member1"}}},
		{http.MethodPost, "/v1/projects/" + created.ProjectID + "/leave", nil},
		{http.MethodPut, "/v1/projects/" + created.ProjectID + "/members/member1/role", map[string]any{"role": 0}},
	}
	for _, wr := range writes {
		t.Run("blocked "+wr.method+" "+wr.path, func(t *testing.T) {
			resetUIDRateLimit(t, testCtx)
			w := doOn(t, r, wr.method, wr.path, token, wr.body)
			assertProjectErrorCode(t, w, "err.server.project.disabled")
		})
	}

	resetUIDRateLimit(t, testCtx)
	w := doOn(t, r, http.MethodGet, "/v1/space/"+spaceA+"/projects", token, nil)
	assert.Equal(t, http.StatusOK, w.Code, "list must keep working: %s", w.Body.String())
	w = doOn(t, r, http.MethodGet, "/v1/projects/"+created.ProjectID, token, nil)
	assert.Equal(t, http.StatusOK, w.Code, "detail must keep working: %s", w.Body.String())
}

// ---------- quotas ----------

// TestQuotasRejectAtTheirBoundary drives each quota to its configured limit and asserts
// its own registered code. Limits come from p.cfg, not from literals in the handler.
func TestQuotasRejectAtTheirBoundary(t *testing.T) {
	t.Run("projects per creator per space", func(t *testing.T) {
		_, p := setup(t)
		p.cfg.MaxPerCreator = 2
		r := mountProject(t, p)
		seedSpace(t, spaceA, 1)
		token := seedUser(t, "owner1")
		seedSpaceMember(t, spaceA, "owner1", 0, 1)

		for i := 0; i < 2; i++ {
			w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
				map[string]any{"name": fmt.Sprintf("p%d", i)})
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		}
		w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
			map[string]any{"name": "p-over"})
		assertProjectErrorCode(t, w, "err.server.project.quota_per_creator")
		env := decodeProjectEnvelope(t, w.Body.Bytes())
		assert.Equal(t, float64(2), env.Error.Details["max"], "the cap must come from config")
	})

	t.Run("projects per space", func(t *testing.T) {
		_, p := setup(t)
		p.cfg.MaxPerSpace = 1
		p.cfg.MaxPerCreator = 100
		r := mountProject(t, p)
		seedSpace(t, spaceA, 1)
		token := seedUser(t, "owner1")
		seedSpaceMember(t, spaceA, "owner1", 0, 1)

		w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
			map[string]any{"name": "p0"})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		w = doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
			map[string]any{"name": "p1"})
		assertProjectErrorCode(t, w, "err.server.project.quota_per_space")
	})

	t.Run("daily creation cap", func(t *testing.T) {
		_, p := setup(t)
		p.cfg.MaxDailyCreate = 1
		r := mountProject(t, p)
		seedSpace(t, spaceA, 1)
		token := seedUser(t, "owner1")
		seedSpaceMember(t, spaceA, "owner1", 0, 1)

		w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
			map[string]any{"name": "p0"})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		created := decodeResp(t, w)
		w = doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
			map[string]any{"name": "p1"})
		assertProjectErrorCode(t, w, "err.server.project.quota_daily_create")

		// Disbanding does not buy another create: the cap bounds creation RATE, so
		// create-then-disband must not be a free bypass. countCreatedInWindowTx counts
		// disbanded rows on purpose; this pins it.
		w = doOn(t, r, http.MethodDelete, "/v1/projects/"+created.ProjectID, token, nil)
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		w = doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
			map[string]any{"name": "p2"})
		assertProjectErrorCode(t, w, "err.server.project.quota_daily_create")
	})

	t.Run("members per project", func(t *testing.T) {
		_, p := setup(t)
		// The owner seat already occupies one, so a cap of 2 admits exactly one more.
		p.cfg.MaxMembers = 2
		r := mountProject(t, p)
		seedSpace(t, spaceA, 1)
		token := seedUser(t, "owner1")
		seedSpaceMember(t, spaceA, "owner1", 0, 1)
		for _, uid := range []string{"m1", "m2"} {
			seedUser(t, uid)
			seedSpaceMember(t, spaceA, uid, 0, 1)
		}
		w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
			map[string]any{"name": "p"})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		created := decodeResp(t, w)

		w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add", token,
			map[string]any{"uids": []string{"m1", "m2"}})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		var outcomes []memberOutcome
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes))
		require.Len(t, outcomes, 2)
		assert.True(t, outcomes[0].OK)
		assert.False(t, outcomes[1].OK)
		assert.Equal(t, reasonQuotaMembers, outcomes[1].Reason)
	})
}

// TestMemberBatchIsStructurallyBounded pins the payload bound: a well-formed but
// pathological uid list must be refused before it becomes one transaction per uid.
func TestMemberBatchIsStructurallyBounded(t *testing.T) {
	_, p := setup(t)
	p.cfg.MemberBatchMax = 3
	r := mountProject(t, p)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", token, map[string]any{"name": "p"})
	require.Equal(t, http.StatusOK, w.Code)
	created := decodeResp(t, w)

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add", token,
		map[string]any{"uids": []string{"a", "b", "c", "d"}})
	assertProjectErrorCode(t, w, "err.server.project.batch_too_large")
	env := decodeProjectEnvelope(t, w.Body.Bytes())
	assert.Equal(t, float64(3), env.Error.Details["max"])
}

// TestValidationRejectsOversizeFields covers the name / description / logo caps.
func TestValidationRejectsOversizeFields(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	long := func(n int) string {
		b := make([]rune, n)
		for i := range b {
			b[i] = '字'
		}
		return string(b)
	}
	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": ""})
	assertProjectErrorCode(t, w, "err.server.project.name_invalid")

	w = doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": long(maxNameChars + 1)})
	assertProjectErrorCode(t, w, "err.server.project.name_invalid")

	w = doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": "ok", "description": long(maxDescriptionChars + 1)})
	assertProjectErrorCode(t, w, "err.server.project.field_too_long")

	w = doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": "ok2", "discoverability": 7})
	assertProjectErrorCode(t, w, "err.server.project.request_invalid")
}

// TestDayWindowUsesTheBusinessTimezone pins that the per-day quota boundary is local
// midnight in the configured zone while the stored values stay UTC. Getting this wrong
// silently resets the quota at 08:00 local.
func TestDayWindowUsesTheBusinessTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	cfg := Config{DayBoundary: loc}

	// 2026-09-04 00:30 Asia/Shanghai is 2026-09-03 16:30 UTC: the window must start at
	// the LOCAL midnight (2026-09-03 16:00 UTC), not at UTC midnight.
	now := time.Date(2026, 9, 4, 0, 30, 0, 0, loc)
	from, to := cfg.dayWindow(now)
	assert.Equal(t, time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC), from.UTC())
	assert.Equal(t, time.Date(2026, 9, 4, 16, 0, 0, 0, time.UTC), to.UTC())
	assert.True(t, from.Before(now.UTC()) && to.After(now.UTC()))
}
