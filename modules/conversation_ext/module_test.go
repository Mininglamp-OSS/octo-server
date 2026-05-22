//go:build integration

package conversation_ext

import (
	"os"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetGlobalConvExtServiceOnce 是测试专用 helper：重置 sync.Once，让每个
// test 可以独立 Init 全局 singleton。声明在 _test.go 中保证不会被生产代码引用
// （PR #21 review I2 by Jerry-Xin —— 之前放在 1module.go 用 *testing.T 占位，
// 仍然让 testing 包污染生产构建）。
func resetGlobalConvExtServiceOnce(_ *testing.T) {
	globalConvExtServiceOnce = sync.Once{}
	globalConvExtService = nil
}

// newCtxForTest builds a *config.Context pointing at the test MySQL instance.
// It does NOT run migrations (cfg.DB.Migration = false) — the table must
// already exist (created by the CI test-setup or by the sql/ migration file).
func newCtxForTest(t *testing.T) *config.Context {
	t.Helper()
	return newCtxForTestTB(t)
}

// newCtxForTestTB 是 newCtxForTest 的 testing.TB 版本，供 benchmark 使用，
// 避免在 *testing.B 里塞 *testing.T{}。Jerry-Xin review (round-1) 指出的反模式。
func newCtxForTestTB(tb testing.TB) *config.Context {
	tb.Helper()
	addr := os.Getenv("CONV_EXT_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = "root:demo@tcp(127.0.0.1)/conv_ext_test?charset=utf8mb4&parseTime=true"
	}
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = addr
	cfg.DB.Migration = false
	return config.NewContext(cfg)
}

// ---------------------------------------------------------------------------
// Global singleton
// ---------------------------------------------------------------------------

// TestInitGlobalConvExtService_NonNil verifies that after calling
// InitGlobalConvExtService the package-level singleton is reachable and non-nil.
func TestInitGlobalConvExtService_NonNil(t *testing.T) {
	ctx := newCtxForTest(t)

	// Reset the once so this test is hermetic when run in isolation or
	// in the full package test suite after another test has initialised it.
	resetGlobalConvExtServiceOnce(t)

	InitGlobalConvExtService(ctx)

	svc := GetGlobalConvExtService()
	require.NotNil(t, svc, "global ConvExtService must be non-nil after Init")
}

// TestInitGlobalConvExtService_Idempotent verifies that calling Init twice
// does not panic and returns the same pointer (sync.Once guarantee).
func TestInitGlobalConvExtService_Idempotent(t *testing.T) {
	ctx := newCtxForTest(t)

	resetGlobalConvExtServiceOnce(t)

	InitGlobalConvExtService(ctx)
	first := GetGlobalConvExtService()
	require.NotNil(t, first)

	// Second call must be a no-op; the pointer must remain the same.
	InitGlobalConvExtService(ctx)
	second := GetGlobalConvExtService()
	assert.Same(t, first, second, "Init must be idempotent — same pointer after two calls")
}

// ---------------------------------------------------------------------------
// Table sanity: the migration SQL must have created user_conversation_ext
// ---------------------------------------------------------------------------

// TestTableExists verifies that the user_conversation_ext table is present in
// the test database. This acts as a sanity check that the SQL migration file
// embedded in the module was applied before the tests ran.
func TestTableExists(t *testing.T) {
	ctx := newCtxForTest(t)

	// A lightweight way to assert the table exists: run a COUNT(*) query.
	// If the table is missing the driver returns an error.
	var count int
	_, err := ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?",
		table,
	).Load(&count)
	require.NoError(t, err, "information_schema query must not error")
	assert.Equal(t, 1, count, "table %q must exist in the test database", table)
}
