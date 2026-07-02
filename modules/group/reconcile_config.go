package group

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// ReconcileConfig 孤儿群频道 reconcile worker 的运行参数（#394）。
type ReconcileConfig struct {
	// Enabled 总开关。disabled 时 worker 不启动 ticker。
	Enabled bool
	// Interval 两次 tick 之间的间隔。<=0 视为禁用。
	Interval time.Duration
	// Grace 宽限期：只 reconcile created_at 早于 (now-Grace) 的未同步群，避免和正在
	// 进行中的建群事务（尚未走到 channel_synced=1）竞争。<=0 时回退默认值。
	Grace time.Duration
	// BatchSize 单个 tick 扫描 / 处理的孤儿上限。<=0 或越界回退 / 截断。
	BatchSize int
}

const (
	envReconcileEnabled   = "DM_GROUP_CHANNEL_RECONCILE_ENABLED"
	envReconcileInterval  = "DM_GROUP_CHANNEL_RECONCILE_INTERVAL"
	envReconcileGrace     = "DM_GROUP_CHANNEL_RECONCILE_GRACE"
	envReconcileBatchSize = "DM_GROUP_CHANNEL_RECONCILE_BATCH_SIZE"

	defaultReconcileInterval  = 5 * time.Minute
	defaultReconcileGrace     = 2 * time.Minute
	defaultReconcileBatchSize = 200
	// maxReconcileBatchSize 防止运维误填巨数把单 tick 变成对 WuKongIM 的洪峰调用。
	maxReconcileBatchSize = 2000
)

// LoadReconcileConfig 从环境变量装载配置。
// 错误 / 越界值一律回退默认值，避免运维误填导致 worker 行为失控。
// 默认 Enabled=false：新兜底路径显式开启（灰度友好），与 #394「不引入非预期后台流量」一致。
func LoadReconcileConfig() ReconcileConfig {
	return ReconcileConfig{
		Enabled:   parseReconcileBool(os.Getenv(envReconcileEnabled)),
		Interval:  parseReconcileDuration(os.Getenv(envReconcileInterval), defaultReconcileInterval),
		Grace:     parseReconcileDuration(os.Getenv(envReconcileGrace), defaultReconcileGrace),
		BatchSize: parseReconcileBoundedInt(os.Getenv(envReconcileBatchSize), defaultReconcileBatchSize, maxReconcileBatchSize),
	}
}

func parseReconcileBool(raw string) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	return v == "true" || v == "1"
}

func parseReconcileDuration(raw string, def time.Duration) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func parseReconcileBoundedInt(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
