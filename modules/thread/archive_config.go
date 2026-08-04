package thread

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// ArchiveConfig 子区自动归档 worker 的运行参数。
//
// 只有 Interval / BatchSize / BatchSleep 是**运维调优**项，仍以 env 为唯一来源。
// Enabled / Threshold 是**策略**项，已迁往 system_settings（task
// inactive-hiding-user-control / P1），在生产路径上不再被读取 —— 见下方各自的注释。
type ArchiveConfig struct {
	// Enabled 在生产路径上是**死字段**。
	//
	// 它曾经是启动门（"disabled 时 worker 不启动 ticker"），该语义已被移除：策略要能
	// 热开启，卡在启动期会让「管理台打开归档」必须重启进程才生效。现在 Start() 只卡
	// Interval，开关每 tick 由 ArchiveWorker.policy 从 system_settings 重读。
	//
	// 真源：system_setting `thread.auto_archive_enabled`，回落
	// DM_THREAD_AUTO_ARCHIVE_ENABLED，再回落代码默认（modules/common）。
	// 本字段只在 policy 未注入时（即单测）被 resolvePolicy 读到。
	Enabled bool
	// Threshold 同为生产路径的**死字段**，语义与 Enabled 相同：真源是 system_setting
	// `thread.auto_archive_days`，经 archiveThresholdFromDays 换算。
	// 只在 policy 未注入时（单测）生效。
	Threshold time.Duration
	// Interval 两次 cron tick 之间的间隔。<=0 视为禁用。
	Interval time.Duration
	// BatchSize 单次 UPDATE 的最大行数。<=0 时回退默认值。
	BatchSize int
	// BatchSleep 两次批之间的 sleep，给 DB 喘息。<0 视为 0。
	BatchSleep time.Duration
}

const (
	envArchiveEnabled    = "DM_THREAD_AUTO_ARCHIVE_ENABLED"
	envArchiveDays       = "DM_THREAD_AUTO_ARCHIVE_DAYS"
	envArchiveInterval   = "DM_THREAD_AUTO_ARCHIVE_INTERVAL"
	envArchiveBatchSize  = "DM_THREAD_AUTO_ARCHIVE_BATCH_SIZE"
	envArchiveBatchSleep = "DM_THREAD_AUTO_ARCHIVE_BATCH_SLEEP"

	defaultArchiveDays       = 3
	defaultArchiveInterval   = time.Hour
	defaultArchiveBatchSize  = 500
	defaultArchiveBatchSleep = 100 * time.Millisecond
	// maxArchiveBatchSize 防止运维误填巨数把单次 UPDATE 变成长事务，超出即截断到上限。
	// 5000 行单次 UPDATE 在 InnoDB 上的锁/binlog 体量仍可控。
	maxArchiveBatchSize = 5000
)

// LoadArchiveConfig 从环境变量装载配置。
// 错误 / 越界值一律回退默认值，避免运维误填导致 worker 行为失控。
// `DM_THREAD_AUTO_ARCHIVE_DAYS=0` 显式禁用阈值（threshold 归零），但 Enabled 仍可为 true。
func LoadArchiveConfig() ArchiveConfig {
	cfg := ArchiveConfig{
		Enabled:    parseBool(os.Getenv(envArchiveEnabled)),
		Threshold:  parseDays(os.Getenv(envArchiveDays), defaultArchiveDays),
		Interval:   parseDuration(os.Getenv(envArchiveInterval), defaultArchiveInterval),
		BatchSize:  parseBoundedPositiveInt(os.Getenv(envArchiveBatchSize), defaultArchiveBatchSize, maxArchiveBatchSize),
		BatchSleep: parseNonNegativeDuration(os.Getenv(envArchiveBatchSleep), defaultArchiveBatchSleep),
	}
	return cfg
}

func parseBool(raw string) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	return v == "true" || v == "1"
}

// parseDays 把字符串当"天数"解析。负数视为非法，回默认；0 是合法的"禁用"。
//
// 天数 → Duration 的换算走 archiveThresholdFromDays，与生产路径共用同一个溢出钳制：
// time.Duration 是 int64 纳秒，约 106,751 天即回绕成一个**小的正数**阈值
// （~213,504 天 ≈ 25 分钟），把几乎所有子区归档掉。本函数只服务于 policy 未注入的
// 单测路径，但一个只在测试里才会回绕的换算同样会把测试变成谎言
// （PR #679 review, yujiawei）。
//
// 解析语义（空/非法/负 → 默认，0 合法，其余原样）必须与 common.threadAutoArchiveDaysFromEnv
// 逐字节一致，包括**不做 TrimSpace** —— 同一个 env 变量现在有两个解析器，它们分歧
// 就等于同一份配置有两种含义。等 Enabled/Threshold 死字段被删掉之后这个约束才会消失。
func parseDays(raw string, defaultDays int) time.Duration {
	if raw == "" {
		return archiveThresholdFromDays(defaultDays)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return archiveThresholdFromDays(defaultDays)
	}
	return archiveThresholdFromDays(n)
}

func parseDuration(raw string, def time.Duration) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func parseNonNegativeDuration(raw string, def time.Duration) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return def
	}
	return d
}

func parsePositiveInt(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// parseBoundedPositiveInt 解析正整数并截断到上限。<=0 或非法值回默认；>max 截到 max。
func parseBoundedPositiveInt(raw string, def, max int) int {
	n := parsePositiveInt(raw, def)
	if n > max {
		return max
	}
	return n
}
