package category

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/module"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain ensures OCTO_MASTER_KEY is set before any test boots. common.Setup
// (called transitively from module.Setup → user module init) refuses to start
// without a 32-byte master key. Mirrors modules/user/main_test.go: don't
// overwrite an externally-provided key so CI / dev shells can pin one.
func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		_ = os.Setenv("OCTO_MASTER_KEY", hex.EncodeToString(key))
	}
	os.Exit(m.Run())
}

// toctouDefaultDB is the isolated MySQL database this test file owns. We
// don't reuse the project-wide `test` database because testutil.NewTestServer
// hardcodes that name and the TOCTOU helper drops all tables to recover
// from leftover stub schemas — running the helper against the shared `test`
// DB would race destructively with any other package's tests under
// `go test ./...` (which runs package binaries concurrently).
const toctouDefaultDB = "octo_toctou_test"

// newToctouTestServer mirrors testutil.NewTestServer but reads the MySQL DSN
// from OCTO_TEST_MYSQL_ADDR so local podman / docker setups with different
// credentials can run these tests without patching the testutil hardcoded
// DSN. Default uses an isolated DB name (toctouDefaultDB) which the helper
// CREATEs lazily if missing, so CI's mysql:8 service doesn't need any
// MYSQL_DATABASE config beyond what the existing Test job already gives us.
//
// Returns the test server, context, and a fresh *Category bound to ctx so
// callers can poke its db helpers (seedSpace, seedGroup) in the same way the
// existing api_test.go helpers do.
func newToctouTestServer(t *testing.T) (*server.Server, *config.Context, *Category) {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1)/" + toctouDefaultDB + "?charset=utf8mb4&parseTime=true"
	}
	ensureToctouDB(t, addr)

	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = addr
	cfg.DB.Migration = false
	ctx := config.NewContext(cfg)

	// Drop all tables in OUR isolated DB (including gorp_migrations) so
	// module.Setup re-creates the schema from scratch every test. Safe
	// because `(SELECT DATABASE())` resolves to whichever DB the helper
	// connected to, and the default is the isolated one — never the shared
	// `test` DB the rest of the project uses.
	var dropSqls []string
	_, err := ctx.DB().SelectBySql(
		"SELECT CONCAT('DROP TABLE IF EXISTS ','`', table_name,'`') FROM information_schema.tables WHERE table_schema = (SELECT DATABASE())",
	).Load(&dropSqls)
	require.NoError(t, err, "list tables")
	for _, stmt := range dropSqls {
		_, err = ctx.DB().UpdateBySql(stmt).Exec()
		require.NoError(t, err, "drop table: "+stmt)
	}

	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test"), "seed token")

	s := server.New(ctx)
	s.GetRoute().UseGin(ctx.Tracer().GinMiddle())
	ctx.SetHttpRoute(s.GetRoute())
	require.NoError(t, module.Setup(ctx), "module setup")

	return s, ctx, New(ctx)
}

// ensureToctouDB parses the DSN, opens a connection WITHOUT a DB name, and
// runs CREATE DATABASE IF NOT EXISTS for the target DB. This lets a fresh
// MySQL (e.g. CI's mysql:8 service container, or a freshly-restarted local
// podman container) bootstrap the isolated DB without operator intervention.
// Idempotent.
func ensureToctouDB(t *testing.T, addr string) {
	t.Helper()
	parsed, err := mysqldriver.ParseDSN(addr)
	require.NoError(t, err, "parse DSN")
	dbName := parsed.DBName
	if dbName == "" {
		return
	}
	parsed.DBName = ""
	bootstrapAddr := parsed.FormatDSN()
	boot, err := sql.Open("mysql", bootstrapAddr)
	require.NoError(t, err, "open bootstrap conn")
	defer boot.Close()
	_, err = boot.Exec("CREATE DATABASE IF NOT EXISTS `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err, "create isolated DB %s", dbName)
}

// toctouDoRequest mirrors doRequest from api_test.go — kept local to avoid
// any subtle interaction with that helper's test scaffolding.
func toctouDoRequest(t *testing.T, route *wkhttp.WKHttp, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader([]byte(util.ToJson(body)))
	} else {
		reqBody = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	req, err := http.NewRequest(method, path, reqBody)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	return w
}

// TestMoveGroupToCategory_TOCTOU_DanglingReference reproduces issue #75 by
// racing the move handler against a half-committed delete and asserting the
// terminal DB state contains no dangling category_id reference.
//
// Before the fix: queryCategoryByID reads status=1 outside the tx, then the
// handler opens its tx and writes group_setting.category_id=X even though
// the deleter committed status=2 in between. End state has
// group_setting.category_id=X AND group_category.status=2.
//
// After the fix: handler's in-tx SELECT ... FOR UPDATE on group_category
// blocks on the deleter's X lock. Once the deleter commits, the handler
// sees status=2 in its own tx, rolls back, and returns a 4xx without
// touching group_setting.
//
// Mechanism: the test holds an external sql.Tx that has UPDATEd
// group_category SET status=2 but has NOT committed (so it holds the X
// lock). The handler is launched in a goroutine; we sleep ~200ms to let it
// reach the lock-acquisition point, then commit the deleter. After-fix
// behaviour observes status=2 once the lock is released.
func TestMoveGroupToCategory_TOCTOU_DanglingReference(t *testing.T) {
	s, ctx, f := newToctouTestServer(t)
	route := s.GetRoute()

	const (
		spaceID = "space-toctou-001"
		groupNo = "group-toctou-001"
		catID   = "cat-toctou-001"
	)

	seedSpaceAndMember(t, f, spaceID, 1)
	seedGroup(t, f, groupNo, spaceID)
	_, err := f.db.session.InsertBySql(
		"INSERT INTO group_category (category_id, space_id, uid, name, sort, status, is_default) VALUES (?, ?, ?, ?, ?, 1, NULL)",
		catID, spaceID, testutil.UID, "工作", 0,
	).Exec()
	require.NoError(t, err, "seed category")

	rawDB := ctx.DB().DB
	deleterTx, err := rawDB.BeginTx(context.Background(), &sql.TxOptions{})
	require.NoError(t, err, "begin deleter tx")
	_, err = deleterTx.Exec("UPDATE group_category SET status=2 WHERE category_id=?", catID)
	require.NoError(t, err, "deleter UPDATE")
	// deleter now holds X lock on the group_category PK row for catID but has
	// NOT committed. Concurrent reads with SELECT ... FOR UPDATE will block.

	type moverResult struct {
		resp *httptest.ResponseRecorder
		err  interface{}
	}
	moverDone := make(chan moverResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				moverDone <- moverResult{err: r}
			}
		}()
		w := toctouDoRequest(t, route, "PUT", "/v1/groups/"+groupNo+"/category", map[string]string{
			"category_id": catID,
		})
		moverDone <- moverResult{resp: w}
	}()

	// Give the mover ~200ms to reach the FOR UPDATE point (after fix) or
	// to fully complete (before fix — the unprotected path doesn't block).
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, deleterTx.Commit(), "commit deleter")

	var result moverResult
	select {
	case result = <-moverDone:
	case <-time.After(10 * time.Second):
		t.Fatal("mover did not complete within 10s — likely deadlock")
	}
	require.Nil(t, result.err, "mover panicked: %v", result.err)
	require.NotNil(t, result.resp, "mover returned nil response")

	var catStatus int
	err = rawDB.QueryRow("SELECT status FROM group_category WHERE category_id=?", catID).Scan(&catStatus)
	require.NoError(t, err, "query group_category status after deleter commit")
	require.Equal(t, 2, catStatus, "deleter should have committed status=2")

	var settingCategoryID sql.NullString
	err = rawDB.QueryRow(
		"SELECT category_id FROM group_setting WHERE group_no=? AND uid=?",
		groupNo, testutil.UID,
	).Scan(&settingCategoryID)
	if err != nil && err != sql.ErrNoRows {
		t.Fatalf("query group_setting: %v", err)
	}

	dangling := settingCategoryID.Valid && settingCategoryID.String == catID
	assert.False(t, dangling,
		"DANGLING REFERENCE: group_setting.category_id=%q points at deleted category (status=%d). mover HTTP=%d body=%s",
		settingCategoryID.String, catStatus, result.resp.Code, result.resp.Body.String())
}

// TestFollowDM_TOCTOU_DanglingReference is the convext counterpart to
// TestMoveGroupToCategory_TOCTOU_DanglingReference: same mid-flight delete
// race, asserted against user_conversation_ext.dm_category_id.
//
// Lives in the category package (not conversation_ext) so it can reuse
// newToctouTestServer's full module.Setup — convext's own test binary
// cannot blank-import category (cycle: category → convext), and trying to
// re-run convext's migrations standalone hits sql-migrate + PROCEDURE
// edge-cases on a previously-bootstrapped DB.
//
// Before fix: the old DMCategoryChecker pre-check (in FollowDM, outside
// withTx) read status=1 (deleter uncommitted, invisible), withTx upserted
// dm_category_id=X, deleter committed status=2 → dangling reference.
// PR #79 fix removed the checker and moved the validation into the
// authoritative in-tx SELECT ... FOR UPDATE; the test now exercises that
// path directly (no DMCategoryChecker injection needed — the in-tx check
// is the sole authority).
//
// After fix: FollowDM's in-tx SELECT ... FOR UPDATE blocks on deleter,
// observes status=2 once committed, returns ErrDMCategoryForbidden without
// touching user_conversation_ext.
func TestFollowDM_TOCTOU_DanglingReference(t *testing.T) {
	_, ctx, _ := newToctouTestServer(t)

	const (
		uid     = "u-toctou-follow"
		peerUID = "u-toctou-peer"
		spaceID = "s-toctou-follow"
		catID   = "cat-toctou-follow"
	)

	svc := convext.NewService(ctx)

	rawDB := ctx.DB().DB

	_, err := rawDB.Exec(
		"INSERT INTO group_category (category_id, space_id, uid, name, sort, status, is_default) VALUES (?, ?, ?, ?, ?, 1, NULL)",
		catID, spaceID, uid, "工作", 0,
	)
	require.NoError(t, err, "seed group_category")

	deleterTx, err := rawDB.BeginTx(context.Background(), &sql.TxOptions{})
	require.NoError(t, err, "begin deleter tx")
	_, err = deleterTx.Exec("UPDATE group_category SET status=2 WHERE category_id=?", catID)
	require.NoError(t, err, "deleter UPDATE")

	type followResult struct {
		err   error
		panic interface{}
	}
	moverDone := make(chan followResult, 1)
	cat := catID
	go func() {
		defer func() {
			if r := recover(); r != nil {
				moverDone <- followResult{panic: r}
			}
		}()
		moverDone <- followResult{err: svc.FollowDM(uid, spaceID, peerUID, &cat)}
	}()

	time.Sleep(200 * time.Millisecond)
	require.NoError(t, deleterTx.Commit(), "commit deleter")

	var result followResult
	select {
	case result = <-moverDone:
	case <-time.After(10 * time.Second):
		t.Fatal("follower did not complete within 10s — likely deadlock")
	}
	require.Nil(t, result.panic, "follower panicked: %v", result.panic)

	var catStatus int
	require.NoError(t, rawDB.QueryRow(
		"SELECT status FROM group_category WHERE category_id=?", catID,
	).Scan(&catStatus), "query group_category status")
	require.Equal(t, 2, catStatus, "deleter should have committed status=2")

	// target_type = 1 for DM; literal because the constant in convext is
	// package-private. Schema comment confirms: "1私聊 2群 5子区".
	const targetTypeDM = 1
	var dmCategoryID sql.NullString
	err = rawDB.QueryRow(
		"SELECT dm_category_id FROM user_conversation_ext WHERE uid=? AND space_id=? AND target_type=? AND target_id=?",
		uid, spaceID, targetTypeDM, peerUID,
	).Scan(&dmCategoryID)
	if err != nil && err != sql.ErrNoRows {
		t.Fatalf("query user_conversation_ext: %v", err)
	}

	dangling := dmCategoryID.Valid && dmCategoryID.String == catID
	assert.False(t, dangling,
		"DANGLING REFERENCE: user_conversation_ext.dm_category_id=%q points at deleted category (status=%d). follower err=%v",
		dmCategoryID.String, catStatus, result.err)
}
