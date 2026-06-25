package bot_api

// Regression test for issue #309: App Bot token revocation must take effect on a
// PEER replica, not linger until that replica restarts.
//
// The pre-fix bug: the auth registry was a per-process in-memory map, mutated
// only on the instance that handled the admin request, so a revoked token kept
// authenticating on every other replica. The fix backs the registry with a
// SHARED Redis cache (RedisAppBotRegistry), so a revoke (DEL of the shared key)
// is visible to every replica immediately; the DB remains the authority via the
// existing fallback.
//
// This test models two replicas as two RedisAppBotRegistry instances over the
// same Redis + a shared DB, and drives the REAL authBot middleware. It runs
// against the same MySQL+Redis the suite uses (testutil defaults); override with
// DM_REPRO_MYSQL / DM_REPRO_REDIS, and it skips if neither is reachable.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/gin-gonic/gin"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func redisRegCtx(t *testing.T) *config.Context {
	t.Helper()
	cfg := config.New()
	cfg.DB.MySQLAddr = envOr("DM_REPRO_MYSQL", "root:demo@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=true")
	cfg.DB.RedisAddr = envOr("DM_REPRO_REDIS", "127.0.0.1:6379")
	ctx := config.NewContext(cfg)
	if err := ctx.DB().DB.Ping(); err != nil {
		t.Skipf("MySQL unreachable (%s): %v", cfg.DB.MySQLAddr, err)
	}
	if _, err := ctx.GetRedisConn().GetKeys("appbot_auth_probe_nonexist:*"); err != nil {
		t.Skipf("Redis unreachable (%s): %v", cfg.DB.RedisAddr, err)
	}
	return ctx
}

// ensureAppBotTable creates the app_bot table if the suite migrations haven't
// (CREATE TABLE IF NOT EXISTS is a no-op when the real schema is already there).
func ensureAppBotTable(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().DB.Exec(`CREATE TABLE IF NOT EXISTS app_bot (
		id           VARCHAR(40) PRIMARY KEY,
		uid          VARCHAR(40) UNIQUE NOT NULL,
		display_name VARCHAR(100) NOT NULL,
		description  VARCHAR(500) DEFAULT '',
		avatar       VARCHAR(200) DEFAULT '',
		scope        VARCHAR(20) NOT NULL DEFAULT 'platform',
		space_id     VARCHAR(40) DEFAULT NULL,
		status       TINYINT NOT NULL DEFAULT 0,
		token        VARCHAR(100) UNIQUE NOT NULL,
		welcome_msg  VARCHAR(500) DEFAULT '',
		created_by   VARCHAR(40) NOT NULL,
		created_at   DATETIME NOT NULL DEFAULT NOW(),
		updated_at   DATETIME NOT NULL DEFAULT NOW() ON UPDATE NOW()
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		t.Fatalf("create app_bot table: %v", err)
	}
}

// authStatus drives the REAL authBot middleware on the replica holding registry
// reg, with the given bearer token. 200 = authorized, non-200 = rejected.
func authStatus(t *testing.T, ba *BotAPI, reg AppBotRegistryInterface, token string) int {
	t.Helper()
	SetAppBotRegistry(reg) // route the request to "this replica"

	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", ba.authBot(), func(c *wkhttp.Context) {
		c.JSON(http.StatusOK, gin.H{"uid": getRobotIDFromContext(c)})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)
	return w.Code
}

func ttl5min() time.Duration { return 5 * time.Minute }

// TestAppBotTokenRevocationPropagatesToPeer asserts the #309 fix: a token revoked
// on one replica is rejected on a PEER replica immediately (via the shared Redis
// store), while a newly rotated token is accepted on the peer.
func TestAppBotTokenRevocationPropagatesToPeer(t *testing.T) {
	ctx := redisRegCtx(t)
	ensureAppBotTable(t, ctx)
	ba := &BotAPI{ctx: ctx, db: newBotAPIDB(ctx), Log: log.NewTLog("repro-309")}
	sqlDB := ctx.DB().DB

	const (
		id   = "repro309_bot"
		uid  = "repro309_uid"
		tOld = "app_old_309_tok_aaaaaaaa"
		tNew = "app_new_309_tok_bbbbbbbb"
	)
	spec := &AppBotRegistrySpec{UID: uid, Scope: "platform"}

	// Two replicas over the SAME Redis + DB.
	regA := NewRedisAppBotRegistry(ctx, ttl5min)
	regB := NewRedisAppBotRegistry(ctx, ttl5min)

	// cleanup
	cleanup := func() {
		_, _ = sqlDB.Exec("DELETE FROM app_bot WHERE id=? OR uid=?", id, uid)
		regA.Remove(tOld)
		regA.Remove(tNew)
	}
	cleanup()
	defer cleanup()

	// Publish: DB row (published) + warm the shared cache (as the publishing
	// replica's syncAuthRegistry would).
	if _, err := sqlDB.Exec(
		"INSERT INTO app_bot (id,uid,display_name,scope,space_id,status,token,created_by) "+
			"VALUES (?,?,?,?,'',1,?,?)",
		id, uid, "Repro 309 Bot", "platform", tOld, "admin",
	); err != nil {
		t.Fatalf("insert app_bot: %v", err)
	}
	regA.Add(tOld, spec)

	// Baseline: peer replica B authorizes the published token (shared cache hit).
	if got := authStatus(t, ba, regB, tOld); got != http.StatusOK {
		t.Fatalf("baseline: peer should authorize published token, got HTTP %d", got)
	}

	// ---- Rotate on replica A: DB swap + shared-cache Update (DEL old + SET new) ----
	if _, err := sqlDB.Exec("UPDATE app_bot SET token=? WHERE id=? AND token=?", tNew, id, tOld); err != nil {
		t.Fatalf("rotate DB: %v", err)
	}
	regA.Update(tOld, tNew, spec)

	// FIX assertion: peer replica B now REJECTS the revoked old token
	// (shared Redis miss -> DB fallback -> old token no longer in DB).
	if got := authStatus(t, ba, regB, tOld); got == http.StatusOK {
		t.Errorf("🔴 #309 NOT fixed: peer replica still authorizes the revoked old token (HTTP %d)", got)
	}
	// Availability preserved: peer replica B accepts the new token (shared hit).
	if got := authStatus(t, ba, regB, tNew); got != http.StatusOK {
		t.Errorf("peer replica should authorize the rotated new token, got HTTP %d", got)
	}
}

// TestAppBotAuthFailsSafeWhenRedisDown asserts that with the cache backend
// unavailable, auth degrades to the authoritative DB (never serves stale): a
// valid token is accepted via DB fallback and an unknown token rejected.
func TestAppBotAuthFailsSafeWhenRedisDown(t *testing.T) {
	ctx := redisRegCtx(t)
	ensureAppBotTable(t, ctx)
	ba := &BotAPI{ctx: ctx, db: newBotAPIDB(ctx), Log: log.NewTLog("repro-309-faildown")}
	sqlDB := ctx.DB().DB

	const (
		id    = "repro309fs_bot"
		uid   = "repro309fs_uid"
		token = "app_fs_309_tok_cccccccc"
	)
	cleanup := func() { _, _ = sqlDB.Exec("DELETE FROM app_bot WHERE id=? OR uid=?", id, uid) }
	cleanup()
	defer cleanup()
	if _, err := sqlDB.Exec(
		"INSERT INTO app_bot (id,uid,display_name,scope,space_id,status,token,created_by) "+
			"VALUES (?,?,?,?,'',1,?,?)",
		id, uid, "Repro 309 FS Bot", "platform", token, "admin",
	); err != nil {
		t.Fatalf("insert app_bot: %v", err)
	}

	// A registry pointed at a dead Redis: every FindByToken errors -> nil -> DB fallback.
	deadCfg := config.New()
	deadCfg.DB.RedisAddr = "127.0.0.1:1" // nothing listening
	deadCtx := config.NewContext(deadCfg)
	deadReg := NewRedisAppBotRegistry(deadCtx, ttl5min)

	// FindByToken on a dead backend must be a safe miss (nil), not an error/panic.
	if spec := deadReg.FindByToken(token); spec != nil {
		t.Fatalf("dead-Redis FindByToken should miss (nil), got %+v", spec)
	}

	// Auth still works via the DB fallback for a valid token...
	if got := authStatus(t, ba, deadReg, token); got != http.StatusOK {
		t.Errorf("with Redis down, valid token should auth via DB fallback, got HTTP %d", got)
	}
	// ...and an unknown token is still rejected.
	if got := authStatus(t, ba, deadReg, "app_unknown_309_tok_dddddddd"); got == http.StatusOK {
		t.Errorf("with Redis down, unknown token must be rejected, got HTTP %d", got)
	}
}
