package group

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
)

// newTestServer wraps testutil.NewTestServer with a test-scoped connection
// pool bound.
//
// testutil.NewTestServer builds a fresh *config.Context per call, and each
// Context lazily opens its own *sql.DB pool (default 100 max / 10 idle conns)
// that nothing ever closes. This package has 200+ tests that each call it, so
// under `go test -race -shuffle=on` the abandoned pools accumulated idle
// connections until the CI MySQL service hit max_connections and the suite
// panicked with "Error 1040: Too many connections" (OCT-8).
//
// We bound each pool small and, crucially, give idle connections a short
// lifetime so an abandoned pool drains to zero connections shortly after its
// test finishes. We deliberately do NOT Close() the pool: config.NewContext
// starts background dispatchers (EventPool/PushPool/RobotEventPool) and a
// timing wheel that outlive the test and may still touch that Context's DB;
// closing it out from under them panics a later test with "sql: database is
// closed".
func newTestServer(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	bindTestDBPool(ctx)
	// The per-UID bucket behind SharedUIDRateLimiter is process-wide, lives in
	// Redis, and is NOT reset by CleanAllTables. Every authenticated route in this
	// package draws on the same `ratelimit:uid:{UID}` bucket, so a case that
	// spends the tokens leaves the next one — in `-shuffle=on` order, any case —
	// receiving `err.shared.rate.limited` instead of the response it asserts.
	// CI hit exactly that in TestGroupInviteAuthorize_Expired (429 where the case
	// expected "邀请链接已过期"). Resetting per test makes each case start from a
	// full bucket; cases that assert 429 (api_project_create_test.go) drain their
	// own quota inside the case and are unaffected.
	resetGroupUIDRateLimit(t, ctx)
	return s, ctx
}

func bindTestDBPool(ctx *config.Context) {
	conn := ctx.DB()
	conn.SetMaxOpenConns(12)
	conn.SetMaxIdleConns(2)
	// Reap idle connections fast so leaked per-test pools shed their
	// connections back to MySQL instead of pinning them until ConnMaxLifetime.
	conn.SetConnMaxIdleTime(time.Second)
	conn.SetConnMaxLifetime(30 * time.Second)
}
