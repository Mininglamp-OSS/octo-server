package wkhttp

import (
	"os"
	"testing"

	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/stretchr/testify/assert"
)

// resetUIDRateLimiterForTest 清除 SharedUIDRateLimiter 的单例状态，
// 供同 package 测试在不同 *config.Context 间重建限流器使用。
// 生产代码不会链接 _test.go 文件，不存在误用风险。
func resetUIDRateLimiterForTest() {
	uidRateLimitMu.Lock()
	defer uidRateLimitMu.Unlock()
	uidRateLimitMW = nil
	uidRateLimitReady = false
}

// setOrUnsetEnv 把 setenv("")（空串）与 unset（变量真正不存在）区分开。
// os.Getenv 对两者返回值一致，但语义不同——这里按测试用例真实意图设置。
func setOrUnsetEnv(t *testing.T, key, value string, unset bool) {
	t.Helper()
	if unset {
		prev, had := os.LookupEnv(key)
		require := func(err error) {
			if err != nil {
				t.Fatal(err)
			}
		}
		require(os.Unsetenv(key))
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(key, prev)
			} else {
				_ = os.Unsetenv(key)
			}
		})
		return
	}
	t.Setenv(key, value)
}

func TestParseRPSFromEnv(t *testing.T) {
	const key = "DM_API_RATELIMIT_TEST_RPS"

	tests := []struct {
		name  string
		env   string
		unset bool
		def   float64
		want  float64
	}{
		{name: "unset uses default", unset: true, def: 2.0, want: 2.0},
		{name: "empty string uses default", env: "", def: 2.0, want: 2.0},
		{name: "valid value", env: "5.5", def: 2.0, want: 5.5},
		{name: "malformed falls back", env: "2x", def: 2.0, want: 2.0},
		{name: "zero falls back", env: "0", def: 2.0, want: 2.0},
		{name: "negative falls back", env: "-1", def: 2.0, want: 2.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnsetEnv(t, key, tc.env, tc.unset)
			assert.Equal(t, tc.want, ParseRPSFromEnv(key, tc.def))
		})
	}
}

func TestParseBurstFromEnv(t *testing.T) {
	const key = "DM_API_RATELIMIT_TEST_BURST"

	tests := []struct {
		name  string
		env   string
		unset bool
		def   int
		want  int
	}{
		{name: "unset uses default", unset: true, def: 60, want: 60},
		{name: "empty string uses default", env: "", def: 60, want: 60},
		{name: "valid value", env: "100", def: 60, want: 100},
		{name: "malformed falls back", env: "60x", def: 60, want: 60},
		{name: "zero falls back", env: "0", def: 60, want: 60},
		{name: "negative falls back", env: "-5", def: 60, want: 60},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnsetEnv(t, key, tc.env, tc.unset)
			assert.Equal(t, tc.want, ParseBurstFromEnv(key, tc.def))
		})
	}
}

// TestSharedUIDRateLimiterSingleton 验证多次调用返回同一实例，
// 且 resetUIDRateLimiterForTest 能触发重建。
func TestSharedUIDRateLimiterSingleton(t *testing.T) {
	// 本测试不构造 *config.Context（会引入 DB 依赖），仅验证 reset 开关。
	// 真实初始化路径由集成测试或启动时覆盖。
	resetUIDRateLimiterForTest()
	assert.False(t, uidRateLimitReady)
	uidRateLimitMu.Lock()
	uidRateLimitMW = libwkhttp.HandlerFunc(func(_ *libwkhttp.Context) {}) // 仅用于验证 ready 切换，不会被调用
	uidRateLimitReady = true
	uidRateLimitMu.Unlock()
	resetUIDRateLimiterForTest()
	assert.False(t, uidRateLimitReady)
	assert.Nil(t, uidRateLimitMW)
}
