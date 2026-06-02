package app_bot

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func TestApplyBotUsesSharedUIDRateLimiter(t *testing.T) {
	route, ctx, mock, cleanup := newApplyBotRateLimitTestRoute(t)
	defer cleanup()

	oldKey := "app_bot_apply_rate:" + testutil.UID
	if err := ctx.GetRedisConn().Del(oldKey); err != nil {
		t.Fatalf("delete old app_bot apply rate-limit key: %v", err)
	}
	if err := ctx.GetRedisConn().Del("ratelimit:uid:" + testutil.UID); err != nil {
		t.Fatalf("delete shared uid rate-limit key: %v", err)
	}
	mock.ExpectQuery(`SELECT \* FROM app_bot WHERE \(uid='app_missing_bot'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	body := bytes.NewBufferString(`{"robot_uid":"app_missing_bot"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/app_bot/apply", body)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	route.ServeHTTP(w, req)

	if got := w.Header().Get("X-RateLimit-Scope"); got != "uid" {
		t.Fatalf("X-RateLimit-Scope = %q, want uid", got)
	}
	if got := w.Header().Get("X-RateLimit-Limit"); got == "" {
		t.Fatal("X-RateLimit-Limit is empty; shared UID limiter was not applied")
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got == "" {
		t.Fatal("X-RateLimit-Remaining is empty; shared UID limiter was not applied")
	}
	if got, err := ctx.GetRedisConn().GetString(oldKey); err != nil {
		t.Fatalf("read old app_bot apply rate-limit key: %v", err)
	} else if got != "" {
		t.Fatalf("old app_bot apply rate-limit key = %q, want absent", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func newApplyBotRateLimitTestRoute(t *testing.T) (*wkhttp.WKHttp, *config.Context, sqlmock.Sqlmock, func()) {
	t.Helper()

	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	if err := ctx.Cache().Set(cfg.Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test"); err != nil {
		t.Fatalf("seed auth token: %v", err)
	}

	rawDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}

	route := wkhttp.New()
	ab := &AppBot{
		ctx: ctx,
		db: &appBotDB{
			ctx:     ctx,
			session: conn.NewSession(nil),
		},
		Log: log.NewTLog("AppBotRateLimitTest"),
	}
	ab.Route(route)

	return route, ctx, mock, func() {
		_ = rawDB.Close()
	}
}
