package searchetl

import (
	"os"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// searchetl 消息检索 ETL（YUJ-4530）的运行参数。沿用 opanalytics 的 env 钳制范式。
//
// 阶段 1（本骨架）：仅 batch / lag / tick 三参就位，模块空跑游标、不接 Kafka。
// lag 的稳定性语义与 opanalytics 完全一致（见 envLagSeconds 注释），是防丢消息硬条件 C1。
const (
	// envBatch 单次 keyset 分页从 message 分片抽取的行数上限。
	envBatch     = "OCTO_SEARCHETL_BATCH"
	defaultBatch = 5000
	minBatch     = 100
	maxBatch     = 50000

	// envLagSeconds 抽取稳定性滞后窗口（秒）。message.id 在 INSERT 时分配、COMMIT 时才可见，
	// 提交顺序≠id 顺序：低 id 的事务可能晚于高 id 提交。若严格按 id>cursor 推进，会漏掉
	// 「已被游标越过、之后才提交」的低 id 行。对策：只处理 created_at ≤ DB_NOW-lag 的稳定
	// 前缀，游标只推进到该前缀末尾。要求 lag > 单条消息落库事务的最大时长。
	//
	// 🔴 硬条件 C1（STOP）：生产环境 lag 不得为 0、不得删除该过滤。设 0 仅限单实例可控测试。
	envLagSeconds     = "OCTO_SEARCHETL_LAG_SECONDS"
	defaultLagSeconds = 600
	maxLagSeconds     = 86400

	// envTickSeconds 增量 tick 周期（秒）。opanalytics 是每日 cron，searchetl 改分钟级 tick。
	envTickSeconds     = "OCTO_SEARCHETL_TICK_SECONDS"
	defaultTickSeconds = 60
	minTickSeconds     = 5
	maxTickSeconds     = 3600
)

// batchSize 返回 keyset 分页大小（钳制到 [min,max]）。
func batchSize() int {
	return clampInt(envBatch, defaultBatch, minBatch, maxBatch)
}

// lagSeconds 返回稳定性滞后窗口秒数（钳制到 [0,max]；0 仅测试用）。
func lagSeconds() int64 {
	v := os.Getenv(envLagSeconds)
	if v == "" {
		return defaultLagSeconds
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Warn("invalid OCTO_SEARCHETL_LAG_SECONDS, using default",
			zap.String("value", v), zap.Int64("default", defaultLagSeconds), zap.Error(err))
		return defaultLagSeconds
	}
	if n < 0 {
		return 0
	}
	if n > maxLagSeconds {
		return maxLagSeconds
	}
	return n
}

// tickInterval 返回增量 tick 周期。
func tickInterval() time.Duration {
	return time.Duration(clampInt(envTickSeconds, defaultTickSeconds, minTickSeconds, maxTickSeconds)) * time.Second
}

// clampInt 读取整型 env 并钳制到 [min,max]；缺省/非法回退 def。
func clampInt(env string, def, min, max int) int {
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Warn("invalid searchetl int config, using default",
			zap.String("env", env), zap.String("value", v), zap.Int("default", def), zap.Error(err))
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
