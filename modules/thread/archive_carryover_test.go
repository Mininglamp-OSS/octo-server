package thread

// =============================================================================
// PR #679 结转项（task per-user-hiding-window / P2）
//
//   - parseDays 的溢出钳制：该函数只服务 policy 未注入的单测路径，但一个只在测试里
//     才会回绕的换算同样会把测试变成谎言。
//   - logOrderingAtStart 的判定：管理写路径的守卫只覆盖 DB→DB，且
//     ViolatesThreadArchiveOrdering 在 !ArchiveEnabled 时短路，于是纯 env 配出来的
//     违规状态不写一次管理接口就永远不暴露。启动日志补的正是这个洞。
//
// 纯逻辑测试，无需 DB / IM。
// =============================================================================

import (
	"math"
	"testing"
	"time"

	commonapi "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// parseDays —— 与生产路径共用同一个溢出上限
// ---------------------------------------------------------------------------

func TestParseDays_NormalValues(t *testing.T) {
	assert.Equal(t, 3*24*time.Hour, parseDays("3", 7))
	assert.Equal(t, 14*24*time.Hour, parseDays("14", 3))
	assert.Equal(t, 3650*24*time.Hour, parseDays("3650", 3))
}

func TestParseDays_FallbackBranches(t *testing.T) {
	assert.Equal(t, 3*24*time.Hour, parseDays("", 3), "空 → 默认")
	assert.Equal(t, 3*24*time.Hour, parseDays("abc", 3), "非数字 → 默认")
	assert.Equal(t, 3*24*time.Hour, parseDays("-1", 3), "负数 → 默认")
	assert.Equal(t, time.Duration(0), parseDays("0", 3), "0 是合法的『禁用阈值』")
}

// 与 common.threadAutoArchiveDaysFromEnv 的空白符语义必须一致：都不 TrimSpace。
// 对应侧的断言在 modules/common/thread_archive_env_parity_test.go。
func TestParseDays_DoesNotTrimWhitespace(t *testing.T) {
	assert.Equal(t, 3*24*time.Hour, parseDays(" 9999 ", 3),
		"带空格必须解析失败回落默认——与 common 侧逐字节一致")
	assert.Equal(t, 3*24*time.Hour, parseDays("9999 ", 3))
}

// 核心回归：会让 int64 回绕的取值必须被钳住，不论它来自 raw 还是来自 defaultDays。
func TestParseDays_OverflowClamped(t *testing.T) {
	want := time.Duration(maxArchiveDays) * 24 * time.Hour

	for _, raw := range []string{"100000", "213504", "2147483647"} {
		got := parseDays(raw, 3)
		assert.Equal(t, want, got, "raw=%s 必须钳到上限", raw)
		assert.Greater(t, got, 365*24*time.Hour,
			"raw=%s 钳制后必须仍是远大于一年的阈值，绝不能回绕成小正数", raw)
	}

	// 默认值路径同样要走钳制——否则调用方传一个巨大的 defaultDays 就能绕过。
	assert.Equal(t, want, parseDays("", math.MaxInt32))
	assert.Equal(t, want, parseDays("abc", math.MaxInt32))
}

// ---------------------------------------------------------------------------
// logOrderingAtStart 的判定谓词
// ---------------------------------------------------------------------------

func TestOrderingViolatesIfEnabled_DetectsLatentViolation(t *testing.T) {
	// 归档 3 天 < 隐藏 7 天：子区在隐藏窗口走完前就被归档，运维调宽隐藏窗口看不到效果。
	latent := commonapi.ThreadArchiveOrdering{
		ArchiveEnabled: false, // 关着 —— 守卫本人会在这里短路
		ArchiveDays:    3,
		RecentDays:     7,
	}
	assert.False(t, commonapi.ViolatesThreadArchiveOrdering(latent),
		"前提：守卫在 !ArchiveEnabled 时短路，这正是启动日志要绕开的洞")
	assert.True(t, orderingViolatesIfEnabled(latent),
		"关着也必须报——那是『打开开关的瞬间就生效』的潜伏状态")
}

func TestOrderingViolatesIfEnabled_LiveViolation(t *testing.T) {
	live := commonapi.ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 3, RecentDays: 7}
	assert.True(t, commonapi.ViolatesThreadArchiveOrdering(live))
	assert.True(t, orderingViolatesIfEnabled(live))
}

func TestOrderingViolatesIfEnabled_HealthyAndInert(t *testing.T) {
	// 产品拍板的 14/7：归档 >= 隐藏，合法。
	assert.False(t, orderingViolatesIfEnabled(
		commonapi.ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 14, RecentDays: 7}))
	// 相等是允许的（现网 3/3）。
	assert.False(t, orderingViolatesIfEnabled(
		commonapi.ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 3, RecentDays: 3}))
	// 任一侧为 0 = 该级不生效，无从违反。
	assert.False(t, orderingViolatesIfEnabled(
		commonapi.ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 0, RecentDays: 7}))
	assert.False(t, orderingViolatesIfEnabled(
		commonapi.ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 3, RecentDays: 0}))
}

// 未注入 ordering（单测默认）时 Start 不得 panic —— 既有 worker 测试全部走这条路径。
func TestLogOrderingAtStart_NilResolverIsNoop(t *testing.T) {
	w := newTestWorker(defaultCfg(), &stubArchiveDB{}, &stubVersionGen{}, time.Now())
	require.Nil(t, w.ordering)
	assert.NotPanics(t, w.logOrderingAtStart)
}

func TestLogOrderingAtStart_ResolvesOnceWhenInjected(t *testing.T) {
	w := newTestWorker(defaultCfg(), &stubArchiveDB{}, &stubVersionGen{}, time.Now())
	calls := 0
	w.ordering = func() commonapi.ThreadArchiveOrdering {
		calls++
		return commonapi.ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 3, RecentDays: 7}
	}
	w.logOrderingAtStart()
	assert.Equal(t, 1, calls, "启动时读一次即可，不该是每 tick 的开销")
}
