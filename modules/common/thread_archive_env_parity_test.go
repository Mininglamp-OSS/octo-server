package common

// =============================================================================
// DM_THREAD_AUTO_ARCHIVE_DAYS 的两个解析器必须逐字节一致
// （task per-user-hiding-window / P2，结转自 PR #679 review 第 6-7 轮）
//
// 迁移把归档天数的真源移到 system_settings，env 降级为兼容垫，兼容垫的全部价值就是
// 「上线瞬间行为与现网逐字节相同」。但同一个 env 变量现在有两个解析器 ——
// common.threadAutoArchiveDaysFromEnv 与 thread.parseDays —— 它们分歧就等于同一份
// 配置有两种含义，而那个保证也就不成立了。
//
// 第一版在 common 侧加了 strings.TrimSpace，看着像无害的便利，实际是整个迁移中**唯一**
// 一个行为不相同的输入：legacy parseDays 把 " 9999 " 直接喂给 Atoi（失败 → 回落 3 天，
// 按 3 天归档），加了 TrimSpace 则解析成 9999 天（实质不归档）。评审把「grep 部署配置里
// 有没有带空格的取值」列进了人工核查清单 —— 这个测试让那次 grep 不必再有第二回。
//
// 纯逻辑测试，无需 DB / IM。
// =============================================================================

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

// legacyParseDaysDays 是 modules/thread/archive_config.go 里 parseDays 解析部分的逐字
// 复制（只保留「字符串 → 天数」，去掉 → time.Duration 的换算，便于直接比对天数）。
//
// 有意复制而不是 import：thread 依赖 common，反向 import 会成环；而
// threadAutoArchiveDaysFromEnv 不导出，也无法从 thread 侧调用。复制的代价是它可能与
// 本体漂移 —— 但下面的表覆盖了 parseDays 的每一条分支，漂移会在这里而不是在生产上暴露。
// 等 ArchiveConfig.Enabled/Threshold 两个死字段被删除、parseDays 随之消失，这个副本
// 和整个文件都应当一起删掉。
func legacyParseDaysDays(raw string, defaultDays int) int {
	if raw == "" {
		return defaultDays
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultDays
	}
	return n
}

func TestThreadAutoArchiveDaysFromEnv_MatchesLegacyParseDays(t *testing.T) {
	cases := []struct {
		raw  string
		want int
		why  string
	}{
		{"", defaultThreadAutoArchiveDays, "未设置 → 代码默认"},
		{"0", 0, "0 是合法的『禁用时间阈值』，不是未设置"},
		{"3", 3, "常规取值"},
		{"14", 14, "产品拍板的两阶段衰减归档侧取值"},
		{"9999", 9999, "远超 settingIntMax，env 层有意不设上界（实质不归档）"},
		{"-1", defaultThreadAutoArchiveDays, "负数非法 → 默认"},
		{"abc", defaultThreadAutoArchiveDays, "非数字 → 默认"},
		{"3.5", defaultThreadAutoArchiveDays, "Atoi 不接受小数 → 默认"},

		// 回归核心：带空白的取值必须与 legacy 一致地**失败**，而不是被 trim 后解析成功。
		{" 9999 ", defaultThreadAutoArchiveDays, "首尾空格：Atoi 失败 → 默认（不得 TrimSpace）"},
		{" 3", defaultThreadAutoArchiveDays, "前导空格同理"},
		{"3 ", defaultThreadAutoArchiveDays, "尾随空格同理"},
		{"\t3", defaultThreadAutoArchiveDays, "制表符同理"},
	}

	for _, c := range cases {
		t.Run(strconv.Quote(c.raw), func(t *testing.T) {
			t.Setenv(envThreadAutoArchiveDays, c.raw)

			got := threadAutoArchiveDaysFromEnv()
			assert.Equal(t, c.want, got, c.why)
			assert.Equal(t, legacyParseDaysDays(c.raw, defaultThreadAutoArchiveDays), got,
				"两个解析器必须给出同一个天数——分歧会让『上线逐字节相同』的保证失效")
		})
	}
}

// enabled 侧的对照：它**应当** TrimSpace，因为它镜像的 legacy parseBool 也 TrimSpace。
// 规则是「与 legacy 一致」，不是「一律 trim」或「一律不 trim」，这条钉住方向不被写反。
func TestThreadAutoArchiveEnabledFromEnv_TrimsLikeLegacyParseBool(t *testing.T) {
	for _, raw := range []string{"true", " true ", "TRUE", "1", " 1 "} {
		t.Run(strconv.Quote(raw), func(t *testing.T) {
			t.Setenv(envThreadAutoArchiveEnabled, raw)
			assert.True(t, threadAutoArchiveEnabledFromEnv(),
				"legacy parseBool 做 TrimSpace+ToLower，这一侧必须跟随")
		})
	}
	for _, raw := range []string{"", "false", "0", "yes"} {
		t.Run("false/"+strconv.Quote(raw), func(t *testing.T) {
			t.Setenv(envThreadAutoArchiveEnabled, raw)
			assert.False(t, threadAutoArchiveEnabledFromEnv())
		})
	}
}
